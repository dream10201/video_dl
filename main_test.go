package main

import (
	"os"
	"path/filepath"
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
