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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Status    TaskStatus `json:"status"`
	Progress  float64    `json:"progress"`
	FilePath  string     `json:"file_path,omitempty"`
	Error     string     `json:"error,omitempty"`
	Logs      []string   `json:"logs"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt time.Time  `json:"started_at,omitempty"`
	EndedAt   time.Time  `json:"ended_at,omitempty"`
}

type Server struct {
	mu          sync.Mutex
	tasks       map[string]*Task
	order       []string
	queue       chan string
	cancelFuncs map[string]context.CancelFunc
	downloadDir string
	ytDLP       string
	ffmpeg      string
	apiToken    string
}

var progressRE = regexp.MustCompile(`\[download\]\s+([0-9]+(?:\.[0-9]+)?)%`)

func main() {
	port := env("PORT", "8080")
	downloadDir := env("DOWNLOAD_DIR", "downloads")
	ytDLP := env("YT_DLP_BIN", "yt-dlp")
	ffmpeg := env("FFMPEG_BIN", "ffmpeg")
	apiToken := env("API_TOKEN", "")
	workers := envInt("WORKERS", max(1, min(2, runtime.NumCPU())))

	if apiToken == "" {
		log.Fatal("API_TOKEN is required")
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Fatalf("create download dir: %v", err)
	}

	srv := &Server{
		tasks:       make(map[string]*Task),
		queue:       make(chan string, 100),
		cancelFuncs: make(map[string]context.CancelFunc),
		downloadDir: downloadDir,
		ytDLP:       ytDLP,
		ffmpeg:      ffmpeg,
		apiToken:    apiToken,
	}
	for i := 0; i < workers; i++ {
		go srv.worker()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/downloads", srv.withToken(srv.handleCreateDownload))
	mux.HandleFunc("/ui/tasks", srv.handleUITasks)
	mux.HandleFunc("/ui/tasks/", srv.handleUITask)
	mux.Handle("/downloads/", http.StripPrefix("/downloads/", http.FileServer(http.Dir(downloadDir))))

	log.Printf("video_dl listening on :%s, workers=%d, download_dir=%s", port, workers, downloadDir)
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

func (s *Server) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL string `json:"url"`
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
	task := s.enqueue(req.URL)
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
			URL string `json:"url"`
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
		task := s.enqueue(req.URL)
		writeJSON(w, http.StatusCreated, task)
	default:
		w.Header().Set("Allow", "GET, POST")
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

func (s *Server) enqueue(rawURL string) *Task {
	task := &Task{
		ID:        newID(),
		URL:       rawURL,
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
	task.Logs = append(task.Logs, "开始调用 yt-dlp")
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelFuncs[id] = cancel
	s.mu.Unlock()

	outputTemplate := filepath.Join(s.downloadDir, "%(title).200B [%(id)s].%(ext)s")
	args := []string{
		"--newline",
		"--no-warnings",
		"--continue",
		"--ignore-errors",
		"--format", "bestvideo*+bestaudio/best",
		"--merge-output-format", "mp4",
		"--ffmpeg-location", s.ffmpeg,
		"--output", outputTemplate,
		"--print", "after_move:filepath",
		task.URL,
	}
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
		s.finishTask(id, StatusFailed, filePath, err.Error())
		return
	}
	s.finishTask(id, StatusSucceeded, filePath, "")
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
			if maybeFilePath(line) {
				task.FilePath = line
			}
		}
		s.mu.Unlock()
	}
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

func maybeFilePath(line string) bool {
	if strings.Contains(line, "[") || strings.Contains(line, "]") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(line))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".m4a", ".mp3", ".opus":
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
