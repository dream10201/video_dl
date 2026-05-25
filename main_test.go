package main

import "testing"

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
