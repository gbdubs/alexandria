package archive

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCaptureBenchmark captures this Mac's real sources (read-only) into a
// scratch directory and removes the capture afterwards. It runs only when
// PHAROS_CAPTURE_BENCH_ROOT names an existing scratch directory
// (PHAROS_CAPTURE_BENCH_SOURCES=claude,codex limits the sources):
//
//	PHAROS_CAPTURE_BENCH_ROOT=/Volumes/euclid/.pharos-capture-bench-XXXX \
//	  go test ./internal/archive -run TestCaptureBenchmark -v -timeout 30m
func TestCaptureBenchmark(t *testing.T) {
	root := os.Getenv("PHAROS_CAPTURE_BENCH_ROOT")
	if root == "" {
		t.Skip("set PHAROS_CAPTURE_BENCH_ROOT to a scratch directory to benchmark real sources")
	}
	useHost(t, "bench")
	captures, err := os.MkdirTemp(root, "captures-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(captures) })
	config := defaultConfig(filepath.Join(root, "bench.toml"))
	config.CaptureRoot = captures
	for _, source := range []SourceConfig{
		{Name: "claude", Kind: "claude", Path: expandPath("~/.claude/projects")},
		{Name: "codex", Kind: "codex", Path: expandPath("~/.codex")},
		{Name: "conductor", Kind: "conductor", Path: expandPath("~/Library/Application Support/com.conductor.app")},
		{Name: "tl1", Kind: "tl1", Path: expandPath("~/.tl1/registry.json")},
	} {
		if only := os.Getenv("PHAROS_CAPTURE_BENCH_SOURCES"); only != "" && !slices.Contains(strings.Split(only, ","), source.Name) {
			continue
		}
		if _, err := os.Stat(source.Path); err == nil {
			source.Enabled = true
			config.Sources = append(config.Sources, source)
		}
	}
	for _, run := range []string{"full", "incremental"} {
		started := time.Now()
		summary, err := Capture(context.Background(), config, config.Sources, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s capture: %v, %d files (%.2f GB) and %d snapshots, ok=%v", run, time.Since(started).Round(time.Millisecond),
			summary.FilesCopied, float64(summary.BytesCopied)/1e9, summary.SnapshotsTaken, summary.OK)
		for _, result := range summary.Sources {
			start, _ := time.Parse(time.RFC3339Nano, result.StartedAt)
			finish, _ := time.Parse(time.RFC3339Nano, result.FinishedAt)
			seconds := finish.Sub(start).Seconds()
			bytes := float64(result.BytesCopied + result.SnapshotBytes)
			t.Logf("  %-9s %7.2fs  seen=%d copied=%d unchanged=%d  %.2f GB  %.2f GB/s  snapshots=%d/%d unchanged=%d  errors=%d warnings=%d",
				result.Name, seconds, result.FilesSeen, result.FilesCopied, result.FilesUnchanged, bytes/1e9, bytes/1e9/max(seconds, 1e-9),
				result.SnapshotsTaken, result.DatabasesSeen, result.SnapshotsUnchanged, len(result.Errors), len(result.Warnings))
			for _, problem := range append(result.Errors, result.Warnings...) {
				t.Logf("    %s: %s", problem.Path, problem.Error)
			}
		}
	}
	manifest, err := os.Stat(filepath.Join(captures, "bench", "codex", captureManifestName))
	if err == nil {
		t.Logf("codex manifest: %.2f MB", float64(manifest.Size())/1e6)
	}
}
