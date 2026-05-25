package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

type TaskStatus string

const (
	StatusQueued    TaskStatus = "queued"
	StatusRunning   TaskStatus = "running"
	StatusSucceeded TaskStatus = "succeeded"
	StatusFailed    TaskStatus = "failed"
	StatusCanceled  TaskStatus = "canceled"
)

type Task struct {
	ID        string         `json:"id"`
	URL       string         `json:"url"`
	UseProxy  bool           `json:"use_proxy"`
	Context   BrowserContext `json:"-"`
	Status    TaskStatus     `json:"status"`
	Progress  float64        `json:"progress"`
	FilePath  string         `json:"file_path,omitempty"`
	Error     string         `json:"error,omitempty"`
	Logs      []string       `json:"logs"`
	CreatedAt time.Time      `json:"created_at"`
	StartedAt time.Time      `json:"started_at,omitempty"`
	EndedAt   time.Time      `json:"ended_at,omitempty"`
}

type DownloadRequest struct {
	URL       string            `json:"url"`
	UseProxy  bool              `json:"proxy"`
	Cookie    string            `json:"cookie"`
	UserAgent string            `json:"user_agent"`
	Referer   string            `json:"referer"`
	Headers   map[string]string `json:"headers"`
}

type BrowserContext struct {
	Cookie    string
	UserAgent string
	Referer   string
	Headers   map[string]string
}

func (r DownloadRequest) BrowserContext() BrowserContext {
	return BrowserContext{
		Cookie:    r.Cookie,
		UserAgent: r.UserAgent,
		Referer:   r.Referer,
		Headers:   r.Headers,
	}.Sanitized()
}

func (c BrowserContext) Sanitized() BrowserContext {
	headers := make(map[string]string)
	for name, value := range c.Headers {
		name = cleanHeaderName(name)
		value = cleanHeaderValue(value)
		if name == "" || value == "" || blockedForwardHeader(name) {
			continue
		}
		headers[name] = value
	}
	if value := cleanHeaderValue(c.Cookie); value != "" {
		headers["Cookie"] = value
	}
	if value := cleanHeaderValue(c.UserAgent); value != "" {
		headers["User-Agent"] = value
	}
	if value := cleanHeaderValue(c.Referer); value != "" {
		headers["Referer"] = value
	}
	return BrowserContext{Headers: headers}
}

func (c BrowserContext) HasHeaders() bool {
	return len(c.Headers) > 0
}

func (c BrowserContext) YTDLPArgs() []string {
	if len(c.Headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Headers))
	for name := range c.Headers {
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]string, 0, len(names)*2)
	for _, name := range names {
		args = append(args, "--add-header", name+":"+c.Headers[name])
	}
	return args
}

type Server struct {
	mu          sync.Mutex
	tasks       map[string]*Task
	order       []string
	queue       chan string
	cancelFuncs map[string]context.CancelFunc
	downloadDir string
	tempRoot    string
	ytDLP       string
	ffmpeg      string
	apiToken    string
	proxyURL    string
}

type probeInfo struct {
	Entries []probeEntry `json:"entries"`
}

type probeEntry struct {
	Title          string        `json:"title"`
	URL            string        `json:"url"`
	Duration       float64       `json:"duration"`
	Filesize       float64       `json:"filesize"`
	FilesizeApprox float64       `json:"filesize_approx"`
	Formats        []probeFormat `json:"formats"`
}

type probeFormat struct {
	Filesize       float64 `json:"filesize"`
	FilesizeApprox float64 `json:"filesize_approx"`
	TBR            float64 `json:"tbr"`
	VBR            float64 `json:"vbr"`
	ABR            float64 `json:"abr"`
	Width          float64 `json:"width"`
	Height         float64 `json:"height"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
}

type downloadTarget struct {
	URL          string
	PlaylistItem int
	EntriesCount int
	Title        string
	Score        float64
}

var progressRE = regexp.MustCompile(`\[download\]\s+([0-9]+(?:\.[0-9]+)?)%`)

func main() {
	port := env("PORT", "8080")
	downloadDir := env("DOWNLOAD_DIR", "downloads")
	tempRoot := env("TEMP_DIR", defaultTempRoot())
	ytDLP := env("YT_DLP_BIN", "yt-dlp")
	ffmpeg := env("FFMPEG_BIN", "ffmpeg")
	apiToken := env("API_TOKEN", "")
	proxyURL := env("PROXY_URL", "")
	workers := envInt("WORKERS", max(1, min(2, runtime.NumCPU())))

	if apiToken == "" {
		log.Fatal("API_TOKEN is required")
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Fatalf("create download dir: %v", err)
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		log.Fatalf("create temp dir: %v", err)
	}

	srv := &Server{
		tasks:       make(map[string]*Task),
		queue:       make(chan string, 100),
		cancelFuncs: make(map[string]context.CancelFunc),
		downloadDir: downloadDir,
		tempRoot:    tempRoot,
		ytDLP:       ytDLP,
		ffmpeg:      ffmpeg,
		apiToken:    apiToken,
		proxyURL:    proxyURL,
	}
	for i := 0; i < workers; i++ {
		go srv.worker()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/downloads", srv.withToken(srv.handleCreateDownload))
	mux.HandleFunc("/ui/tasks", srv.handleUITasks)
	mux.HandleFunc("/ui/tasks/", srv.handleUITask)
	mux.HandleFunc("/downloads/", srv.handleDownloadFile)

	log.Printf("video_dl listening on :%s, workers=%d, download_dir=%s, temp_dir=%s", port, workers, downloadDir, tempRoot)
	if err := http.ListenAndServe(":"+port, logging(mux)); err != nil {
		log.Fatal(err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/downloads/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.downloadDir, name)
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mediaContentType(name))
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", contentDisposition(disposition, name))
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *Server) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if err := validateURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task := s.enqueue(req.URL, req.UseProxy, req.BrowserContext())
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) handleUITasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		tasks := make([]*Task, 0, len(s.order))
		for i := len(s.order) - 1; i >= 0; i-- {
			if t, ok := s.tasks[s.order[i]]; ok {
				tasks = append(tasks, cloneTask(t))
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	case http.MethodPost:
		var req struct {
			URL      string `json:"url"`
			UseProxy bool   `json:"proxy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		req.URL = strings.TrimSpace(req.URL)
		if err := validateURL(req.URL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		task := s.enqueue(req.URL, req.UseProxy, BrowserContext{})
		writeJSON(w, http.StatusCreated, task)
	case http.MethodDelete:
		deleted := s.deleteAllTasks()
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUITask(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ui/tasks/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]

	if len(parts) == 1 && r.Method == http.MethodGet {
		s.mu.Lock()
		task, ok := s.tasks[id]
		if ok {
			task = cloneTask(task)
		}
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if !s.deleteTask(id) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if !s.cancelTask(id) {
			writeError(w, http.StatusNotFound, "task not found or already finished")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
		return
	}

	http.NotFound(w, r)
}

func (s *Server) enqueue(rawURL string, useProxy bool, browserContext BrowserContext) *Task {
	task := &Task{
		ID:        newID(),
		URL:       rawURL,
		UseProxy:  useProxy,
		Context:   browserContext.Sanitized(),
		Status:    StatusQueued,
		CreatedAt: time.Now().UTC(),
		Logs:      []string{"任务已创建，等待下载"},
	}
	s.mu.Lock()
	s.tasks[task.ID] = task
	s.order = append(s.order, task.ID)
	cloned := cloneTask(task)
	s.mu.Unlock()
	s.queue <- task.ID
	return cloned
}

func (s *Server) worker() {
	for id := range s.queue {
		s.runTask(id)
	}
}

func (s *Server) runTask(id string) {
	s.mu.Lock()
	task, ok := s.tasks[id]
	if !ok || task.Status != StatusQueued {
		s.mu.Unlock()
		return
	}
	task.Status = StatusRunning
	task.StartedAt = time.Now().UTC()
	task.Logs = append(task.Logs, "开始分析页面视频")
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFuncs[id] = cancel
	useProxy := task.UseProxy
	browserContext := task.Context
	s.mu.Unlock()

	proxyURL, err := s.proxyForTask(useProxy)
	if err != nil {
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	if proxyURL != "" {
		s.addLog(id, "本任务启用代理")
	}
	if browserContext.HasHeaders() {
		s.addLog(id, "本任务携带浏览器上下文")
	}

	target, err := s.selectDownloadTarget(ctx, task.URL, proxyURL, browserContext)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.finishTask(id, StatusCanceled, "", "任务已取消")
			return
		}
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	s.addLog(id, target.describe())

	tempDir, err := os.MkdirTemp(s.tempRoot, id+"-")
	if err != nil {
		s.finishTask(id, StatusFailed, "", fmt.Sprintf("create temp dir: %v", err))
		return
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("remove temp dir %s: %v", tempDir, err)
		}
	}()

	outputTemplate := filepath.Join(tempDir, "%(title).200B [%(id)s].%(ext)s")
	args := []string{
		"--newline",
		"--no-warnings",
		"--continue",
		"--ignore-errors",
		"--no-keep-video",
		"--windows-filenames",
		"--format", "bestvideo*+bestaudio/best",
		"--merge-output-format", "mp4",
		"--ffmpeg-location", s.ffmpeg,
		"--output", outputTemplate,
	}
	if proxyURL != "" {
		args = append(args, "--proxy", proxyURL)
	}
	args = append(args, browserContext.YTDLPArgs()...)
	if target.PlaylistItem > 0 {
		args = append(args, "--playlist-items", strconv.Itoa(target.PlaylistItem))
	}
	args = append(args, target.URL)
	cmd := exec.CommandContext(ctx, s.ytDLP, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		s.finishTask(id, StatusFailed, "", fmt.Sprintf("start yt-dlp: %v", err))
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.scanOutput(id, stdout)
	}()
	go func() {
		defer wg.Done()
		s.scanOutput(id, stderr)
	}()
	err = cmd.Wait()
	wg.Wait()

	s.mu.Lock()
	delete(s.cancelFuncs, id)
	filePath := ""
	if t := s.tasks[id]; t != nil {
		filePath = t.FilePath
	}
	s.mu.Unlock()

	if errors.Is(ctx.Err(), context.Canceled) {
		s.finishTask(id, StatusCanceled, filePath, "任务已取消")
		return
	}
	if err != nil {
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	finalPath, err := s.moveFinalDownload(tempDir)
	if err != nil {
		s.finishTask(id, StatusFailed, "", err.Error())
		return
	}
	s.finishTask(id, StatusSucceeded, finalPath, "")
}

func (s *Server) proxyForTask(useProxy bool) (string, error) {
	if !useProxy {
		return "", nil
	}
	if s.proxyURL == "" {
		return "", errors.New("proxy requested but PROXY_URL is not configured")
	}
	return s.proxyURL, nil
}

func (s *Server) selectDownloadTarget(ctx context.Context, rawURL, proxyURL string, browserContext BrowserContext) (downloadTarget, error) {
	target := downloadTarget{URL: rawURL}
	args := []string{"-J", "--no-warnings", "--ignore-errors", rawURL}
	if proxyURL != "" {
		args = []string{"-J", "--no-warnings", "--ignore-errors", "--proxy", proxyURL, rawURL}
	}
	args = append(args[:len(args)-1], append(browserContext.YTDLPArgs(), rawURL)...)
	cmd := exec.CommandContext(ctx, s.ytDLP, args...)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return target, ctx.Err()
		}
		return target, fmt.Errorf("probe page with yt-dlp: %w", err)
	}

	var info probeInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return target, fmt.Errorf("parse yt-dlp probe output: %w", err)
	}
	if len(info.Entries) == 0 {
		target.EntriesCount = 1
		return target, nil
	}

	bestIndex := -1
	bestScore := -1.0
	for i, entry := range info.Entries {
		score := entryScore(entry)
		if score > bestScore {
			bestScore = score
			bestIndex = i
		}
	}
	if bestIndex < 0 {
		return target, nil
	}

	best := info.Entries[bestIndex]
	target.PlaylistItem = bestIndex + 1
	target.EntriesCount = len(info.Entries)
	target.Title = best.Title
	target.Score = bestScore
	return target, nil
}

func (t downloadTarget) describe() string {
	if t.EntriesCount <= 1 || t.PlaylistItem == 0 {
		return "未发现多个候选视频，按原链接下载最高质量"
	}
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = "未命名视频"
	}
	return fmt.Sprintf("发现 %d 个候选视频，选择估算体积最大的第 %d 个：%s", t.EntriesCount, t.PlaylistItem, title)
}

func entryScore(entry probeEntry) float64 {
	if entry.Filesize > 0 {
		return entry.Filesize
	}
	if entry.FilesizeApprox > 0 {
		return entry.FilesizeApprox
	}

	bestVideo := 0.0
	bestAudio := 0.0
	bestAny := 0.0
	for _, format := range entry.Formats {
		score := formatScore(format, entry.Duration)
		if score <= 0 {
			continue
		}
		if score > bestAny {
			bestAny = score
		}
		if format.VCodec != "" && format.VCodec != "none" {
			if score > bestVideo {
				bestVideo = score
			}
			continue
		}
		if format.ACodec != "" && format.ACodec != "none" {
			if score > bestAudio {
				bestAudio = score
			}
		}
	}
	if bestVideo > 0 {
		return bestVideo + bestAudio
	}
	return bestAny
}

func formatScore(format probeFormat, duration float64) float64 {
	if format.Filesize > 0 {
		return format.Filesize
	}
	if format.FilesizeApprox > 0 {
		return format.FilesizeApprox
	}
	bitrate := maxFloat(format.TBR, format.VBR+format.ABR)
	if bitrate > 0 && duration > 0 {
		return bitrate * 1000 / 8 * duration
	}
	if format.Width > 0 && format.Height > 0 {
		return format.Width * format.Height
	}
	return 0
}

func (s *Server) scanOutput(id string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.mu.Lock()
		if task := s.tasks[id]; task != nil {
			task.Logs = appendBounded(task.Logs, line, 300)
			if m := progressRE.FindStringSubmatch(line); len(m) == 2 {
				if p, err := strconv.ParseFloat(m[1], 64); err == nil && p > task.Progress {
					task.Progress = p
				}
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) moveFinalDownload(tempDir string) (string, error) {
	source, err := findFinalMedia(tempDir)
	if err != nil {
		return "", err
	}
	dest := uniquePath(filepath.Join(s.downloadDir, filepath.Base(source)))
	if err := moveFile(source, dest); err != nil {
		return "", fmt.Errorf("move final file: %w", err)
	}
	return dest, nil
}

func moveFile(source, dest string) error {
	if err := os.Rename(source, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	return os.Remove(source)
}

func findFinalMedia(root string) (string, error) {
	var bestVideo fileCandidate
	var bestAny fileCandidate
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if isTempDownloadFile(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		candidate := fileCandidate{path: path, size: info.Size()}
		if isMediaFile(path) && candidate.size > bestAny.size {
			bestAny = candidate
		}
		if isVideoFile(path) && candidate.size > bestVideo.size {
			bestVideo = candidate
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("scan downloaded files: %w", err)
	}
	if bestVideo.path != "" {
		return bestVideo.path, nil
	}
	if bestAny.path != "" {
		return bestAny.path, nil
	}
	return "", errors.New("no downloaded media file found")
}

type fileCandidate struct {
	path string
	size int64
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func (s *Server) addLog(id, line string) {
	s.mu.Lock()
	if task := s.tasks[id]; task != nil {
		task.Logs = appendBounded(task.Logs, line, 300)
	}
	s.mu.Unlock()
}

func (s *Server) finishTask(id string, status TaskStatus, filePath, errText string) {
	s.mu.Lock()
	if task := s.tasks[id]; task != nil {
		task.Status = status
		task.EndedAt = time.Now().UTC()
		if status == StatusSucceeded {
			task.Progress = 100
			task.Logs = appendBounded(task.Logs, "下载完成", 300)
		}
		if filePath != "" {
			task.FilePath = filePath
		}
		if errText != "" {
			task.Error = errText
			task.Logs = appendBounded(task.Logs, errText, 300)
		}
	}
	delete(s.cancelFuncs, id)
	s.mu.Unlock()
}

func (s *Server) cancelTask(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return false
	}
	switch task.Status {
	case StatusSucceeded, StatusFailed, StatusCanceled:
		return false
	case StatusQueued:
		task.Status = StatusCanceled
		task.EndedAt = time.Now().UTC()
		task.Error = "任务已取消"
		task.Logs = appendBounded(task.Logs, "任务已取消", 300)
		return true
	case StatusRunning:
		if cancel, ok := s.cancelFuncs[id]; ok {
			cancel()
			task.Logs = appendBounded(task.Logs, "正在取消任务", 300)
			return true
		}
	}
	return false
}

func (s *Server) deleteTask(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return false
	}
	if cancel, ok := s.cancelFuncs[id]; ok {
		cancel()
		delete(s.cancelFuncs, id)
	}
	delete(s.tasks, id)
	s.order = removeString(s.order, id)
	return true
}

func (s *Server) deleteAllTasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := len(s.tasks)
	for _, cancel := range s.cancelFuncs {
		cancel()
	}
	s.tasks = make(map[string]*Task)
	s.order = nil
	s.cancelFuncs = make(map[string]context.CancelFunc)
	return deleted
}

func validateURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http/https urls are supported")
	}
	return nil
}

func cleanHeaderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, r := range name {
		if r <= 32 || r >= 127 || r == ':' {
			return ""
		}
	}
	parts := strings.Split(strings.ToLower(name), "-")
	for i, part := range parts {
		if part == "" {
			return ""
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}

func cleanHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return value
}

func mediaContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".opus":
		return "audio/ogg"
	default:
		if value := mime.TypeByExtension(filepath.Ext(name)); value != "" {
			return value
		}
		return "application/octet-stream"
	}
}

func contentDisposition(disposition, name string) string {
	escaped := strings.ReplaceAll(name, `"`, `'`)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disposition, escaped, url.PathEscape(name))
}

func blockedForwardHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "host", "content-length", "connection", "transfer-encoding", "proxy-authorization":
		return true
	default:
		return false
	}
}

func (s *Server) withToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r) {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	}
}

func (s *Server) validToken(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	} else {
		token = r.Header.Get("X-API-Token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiToken)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.Method == http.MethodGet && r.URL.Path == "/ui/tasks" {
			return
		}
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func cloneTask(t *Task) *Task {
	cp := *t
	cp.Logs = append([]string(nil), t.Logs...)
	return &cp
}

func appendBounded(logs []string, line string, maxLines int) []string {
	logs = append(logs, line)
	if len(logs) > maxLines {
		return logs[len(logs)-maxLines:]
	}
	return logs
}

func removeString(values []string, target string) []string {
	for i, value := range values {
		if value == target {
			return append(values[:i], values[i+1:]...)
		}
	}
	return values
}

func isTempDownloadFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".temp") {
		return true
	}
	return strings.Contains(name, ".part.")
}

func isMediaFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".m4v", ".m4a", ".mp3", ".opus":
		return true
	default:
		return false
	}
}

func isVideoFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".m4v":
		return true
	default:
		return false
	}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func defaultTempRoot() string {
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		return "/dev/shm/video_dl"
	}
	return filepath.Join(os.TempDir(), "video_dl")
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
