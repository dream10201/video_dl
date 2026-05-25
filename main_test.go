package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEntryScorePrefersLargestVideoAudioEstimate(t *testing.T) {
	smallPreview := probeEntry{
		Duration: 10,
		Formats: []probeFormat{
			{Filesize: 1_000, VCodec: "avc1", ACodec: "none"},
			{Filesize: 200, VCodec: "none", ACodec: "mp4a"},
		},
	}
	mainVideo := probeEntry{
		Duration: 120,
		Formats: []probeFormat{
			{Filesize: 10_000, VCodec: "avc1", ACodec: "none"},
			{Filesize: 1_000, VCodec: "none", ACodec: "mp4a"},
		},
	}

	if entryScore(mainVideo) <= entryScore(smallPreview) {
		t.Fatalf("main video should score higher than preview")
	}
}

func TestEntryScoreFallsBackToBitrateAndDuration(t *testing.T) {
	entry := probeEntry{
		Duration: 100,
		Formats: []probeFormat{
			{TBR: 800, VCodec: "avc1", ACodec: "mp4a"},
		},
	}

	if got := entryScore(entry); got <= 0 {
		t.Fatalf("entryScore() = %v, want positive fallback score", got)
	}
}

func TestProxyForTaskRequiresConfiguredProxy(t *testing.T) {
	srv := &Server{}

	if got, err := srv.proxyForTask(false); err != nil || got != "" {
		t.Fatalf("proxyForTask(false) = %q, %v; want empty proxy and nil error", got, err)
	}
	if _, err := srv.proxyForTask(true); err == nil {
		t.Fatal("proxyForTask(true) without proxy URL should fail")
	}

	srv.proxyURL = "socks5://127.0.0.1:1080"
	got, err := srv.proxyForTask(true)
	if err != nil {
		t.Fatalf("proxyForTask(true) returned error: %v", err)
	}
	if got != srv.proxyURL {
		t.Fatalf("proxyForTask(true) = %q, want %q", got, srv.proxyURL)
	}
}

func TestFindFinalMediaPrefersVideoOverAudioSidecar(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "sample.m4a")
	video := filepath.Join(dir, "sample.mp4")
	if err := os.WriteFile(audio, []byte("larger-audio-sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findFinalMedia(dir)
	if err != nil {
		t.Fatalf("findFinalMedia returned error: %v", err)
	}
	if got != video {
		t.Fatalf("findFinalMedia() = %q, want %q", got, video)
	}
}

func TestRemoveString(t *testing.T) {
	got := removeString([]string{"a", "b", "c"}, "b")
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("removeString length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("removeString()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBrowserContextSanitizedYTDLPArgs(t *testing.T) {
	ctx := BrowserContext{
		Cookie:    "sid=abc",
		UserAgent: "Agent\nBad",
		Referer:   "https://example.com/watch",
		Headers: map[string]string{
			"Accept-Language": "zh-CN,zh;q=0.9",
			"Authorization":   "Bearer secret",
			"Bad\nName":       "bad",
		},
	}.Sanitized()

	if ctx.Cookie != "sid=abc" {
		t.Fatalf("Cookie = %q", ctx.Cookie)
	}
	if _, ok := ctx.Headers["Cookie"]; ok {
		t.Fatal("Cookie should not be forwarded with --add-header")
	}
	if ctx.Headers["User-Agent"] != "AgentBad" {
		t.Fatalf("User-Agent header = %q", ctx.Headers["User-Agent"])
	}
	if _, ok := ctx.Headers["Authorization"]; ok {
		t.Fatal("Authorization header should not be forwarded")
	}

	args := ctx.YTDLPArgs()
	if len(args) == 0 {
		t.Fatal("YTDLPArgs should include add-header args")
	}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "--add-header" {
			t.Fatalf("args[%d] = %q, want --add-header", i, args[i])
		}
	}
}

func TestWriteCookieFile(t *testing.T) {
	ctx := BrowserContext{Cookie: "sid=abc; token=a=b=c"}.Sanitized()
	path, err := ctx.WriteCookieFile("https://www.bilibili.com/video/BV1", t.TempDir())
	if err != nil {
		t.Fatalf("WriteCookieFile returned error: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "www.bilibili.com\tTRUE\t/\tTRUE\t0\tsid\tabc") {
		t.Fatalf("cookie file missing sid cookie:\n%s", text)
	}
	if !strings.Contains(text, "www.bilibili.com\tTRUE\t/\tTRUE\t0\ttoken\ta=b=c") {
		t.Fatalf("cookie file missing token cookie:\n%s", text)
	}
}

func TestMediaContentType(t *testing.T) {
	cases := map[string]string{
		"video.mp4": "video/mp4",
		"clip.webm": "video/webm",
		"audio.m4a": "audio/mp4",
	}
	for name, want := range cases {
		if got := mediaContentType(name); got != want {
			t.Fatalf("mediaContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestTitleFromFilePath(t *testing.T) {
	got := titleFromFilePath("/downloads/My Video [abc123].mp4")
	if got != "My Video [abc123]" {
		t.Fatalf("titleFromFilePath() = %q", got)
	}
}

func TestParseRawHeadersFromBrowserCopy(t *testing.T) {
	raw := "GET / HTTP/2\n" +
		"Host: www.bilibili.com\n" +
		"User-Agent: Mozilla/5.0\n" +
		"Accept-Language: zh-CN\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\n" +
		"Connection: keep-alive\n" +
		"Cookie: sid=secret; bili_jct=token\n" +
		"Sec-Fetch-Dest: document\n" +
		"Priority: u=0, i\n" +
		"TE: trailers\n"

	headers := parseRawHeaders(raw)
	if headers["User-Agent"] != "Mozilla/5.0" {
		t.Fatalf("User-Agent = %q", headers["User-Agent"])
	}
	if headers["Cookie"] != "sid=secret; bili_jct=token" {
		t.Fatalf("Cookie = %q", headers["Cookie"])
	}
	if _, ok := headers["Host"]; ok {
		t.Fatal("Host should be filtered")
	}
	if _, ok := headers["Connection"]; ok {
		t.Fatal("Connection should be filtered")
	}
	if _, ok := headers["Accept-Encoding"]; ok {
		t.Fatal("Accept-Encoding should be filtered")
	}
	if _, ok := headers["Sec-Fetch-Dest"]; ok {
		t.Fatal("Sec-Fetch-Dest should be filtered")
	}
	if _, ok := headers["Te"]; ok {
		t.Fatal("TE should be filtered")
	}
}

func TestDownloadRequestDefaultsRefererToURL(t *testing.T) {
	ctx := DownloadRequest{URL: "https://example.com/video"}.BrowserContext()
	if ctx.Headers["Referer"] != "https://example.com/video" {
		t.Fatalf("Referer = %q", ctx.Headers["Referer"])
	}
}
