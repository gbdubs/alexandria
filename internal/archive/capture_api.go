package archive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
)

func runCaptureCLI(config Config, names []string) error {
	sources, err := captureSources(config, names)
	if err != nil {
		return err
	}
	summary, err := Capture(context.Background(), config, sources, func(result CaptureSourceResult) {
		if result.State != "running" {
			fmt.Fprintf(os.Stderr, "%s: %s; %d files (%d bytes) and %d snapshots (%d bytes) captured, %d errors\n", result.Name, result.State,
				result.FilesCopied, result.BytesCopied, result.SnapshotsTaken, result.SnapshotBytes, len(result.Errors))
		}
	})
	if err != nil {
		return err
	}
	if err := printJSON(summary); err != nil {
		return err
	}
	if !summary.OK {
		return errors.New("capture finished with errors")
	}
	return nil
}

// CaptureRun is a background capture's progress, shaped like SyncRun.
type CaptureRun struct {
	ID               string                `json:"id"`
	Kind             string                `json:"kind"`
	State            string                `json:"state"`
	Sources          []string              `json:"sources"`
	CurrentSource    any                   `json:"current_source"`
	CompletedSources int                   `json:"completed_sources"`
	TotalSources     int                   `json:"total_sources"`
	FilesCopied      int                   `json:"files_copied"`
	BytesCopied      int64                 `json:"bytes_copied"`
	SnapshotsTaken   int                   `json:"snapshots_taken"`
	Results          []CaptureSourceResult `json:"results"`
	Errors           []captureError        `json:"errors"`
	StartedAt        string                `json:"started_at"`
	UpdatedAt        string                `json:"updated_at"`
	CompletedAt      any                   `json:"completed_at"`
}

type captureRuns struct {
	mu   sync.Mutex
	runs []*CaptureRun
}

func (r *captureRuns) start(sources []SourceConfig) *CaptureRun {
	timestamp := now()
	run := &CaptureRun{ID: randomToken(), Kind: "capture", State: "running", Sources: []string{}, TotalSources: len(sources),
		Results: []CaptureSourceResult{}, Errors: []captureError{}, StartedAt: timestamp, UpdatedAt: timestamp}
	for _, source := range sources {
		run.Sources = append(run.Sources, source.Name)
		run.Results = append(run.Results, CaptureSourceResult{Name: source.Name, Kind: source.Kind, State: "pending"})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append([]*CaptureRun{run}, r.runs...)
	if len(r.runs) > 20 {
		r.runs = r.runs[:20]
	}
	return run
}

func (r *captureRuns) update(id string, apply func(*CaptureRun)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		if run.ID == id {
			apply(run)
			run.FilesCopied, run.BytesCopied, run.SnapshotsTaken, run.CompletedSources = 0, 0, 0, 0
			for _, result := range run.Results {
				run.FilesCopied += result.FilesCopied
				run.BytesCopied += result.BytesCopied + result.SnapshotBytes
				run.SnapshotsTaken += result.SnapshotsTaken
				if result.State == "complete" || result.State == "failed" {
					run.CompletedSources++
				}
			}
			run.UpdatedAt = now()
			return
		}
	}
}

func (r *captureRuns) progress(id string, result CaptureSourceResult) {
	r.update(id, func(run *CaptureRun) {
		if index := slices.IndexFunc(run.Results, func(item CaptureSourceResult) bool { return item.Name == result.Name }); index >= 0 {
			run.Results[index] = result
		}
		run.CurrentSource = result.Name
	})
}

func (r *captureRuns) finish(id string, summary CaptureSummary, failure error) {
	r.update(id, func(run *CaptureRun) {
		for _, result := range summary.Sources {
			if index := slices.IndexFunc(run.Results, func(item CaptureSourceResult) bool { return item.Name == result.Name }); index >= 0 {
				run.Results[index] = result
			}
		}
		run.Errors = append(run.Errors, summary.Errors...)
		if failure != nil {
			run.Errors = append(run.Errors, captureError{Error: failure.Error()})
		}
		run.State = "complete"
		if !summary.OK || failure != nil {
			run.State = "failed"
		}
		run.CurrentSource = nil
		run.CompletedAt = now()
	})
}

func (r *captureRuns) list() []CaptureRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := make([]CaptureRun, 0, len(r.runs))
	for _, run := range r.runs {
		copied := *run
		copied.Sources = slices.Clone(run.Sources)
		copied.Errors = slices.Clone(run.Errors)
		copied.Results = make([]CaptureSourceResult, len(run.Results))
		for index, result := range run.Results {
			copied.Results[index] = result.clone()
		}
		runs = append(runs, copied)
	}
	return runs
}

func (r *captureRuns) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.runs, func(run *CaptureRun) bool { return run.State == "running" })
}

// captureActive reports a capture by this service or, through the capture
// lock, by any other process on this Mac such as the CLI.
func (s *Server) captureActive() bool {
	if s.captures.active() {
		return true
	}
	lock, err := os.Open(filepath.Join(s.Config().CaptureRoot, currentHost().ID, captureLockName))
	if err != nil {
		return false
	}
	defer lock.Close()
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

// captureStatus's safe_to_unplug is the library status's: nothing at all is
// running (see library_status.go). It does not make unplugging without an
// eject safe, since the service still has the catalog open.
func (s *Server) captureStatus() map[string]any {
	capturing, syncing := s.captureActive(), s.syncActive()
	runs := s.captures.list()
	var latest any
	if len(runs) > 0 {
		latest = runs[0]
	}
	return map[string]any{"active": capturing, "sync_active": syncing, "safe_to_unplug": len(s.libraryActivities()) == 0,
		"capture_root": s.Config().CaptureRoot, "host": currentHost(), "run": latest, "runs": runs}
}

// startCapture begins a capture in the background and returns at once;
// progress is polled with GET /api/capture.
func (s *Server) startCapture(w http.ResponseWriter, body map[string]any) {
	names, ok := sliceValue(body["sources"])
	if !ok {
		writeError(w, errors.New("sources must be an array of source names"), http.StatusBadRequest)
		return
	}
	config := s.Config()
	sources, err := captureSources(config, stringSlice(names))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if s.captures.active() {
		writeError(w, errCaptureBusy, http.StatusConflict)
		return
	}
	session, err := beginCapture(config)
	if errors.Is(err, errCaptureBusy) {
		writeError(w, err, http.StatusConflict)
		return
	} else if err != nil {
		writeError(w, err, http.StatusServiceUnavailable)
		return
	}
	run := s.captures.start(sources)
	// A release (before an eject) cancels the capture between files; every
	// file and manifest is replaced atomically, so the next run resumes.
	started := s.spawn(func(ctx context.Context) {
		defer session.Close()
		summary, failure := CaptureSummary{OK: true}, error(nil)
		defer func() {
			if value := recover(); value != nil {
				failure = fmt.Errorf("capture stopped: %v", value)
			}
			s.captures.finish(run.ID, summary, failure)
		}()
		summary = session.Run(ctx, sources, func(result CaptureSourceResult) { s.captures.progress(run.ID, result) })
	})
	if !started {
		// A stop began after this request was admitted.
		session.Close()
		stopping := errors.New("Pharos is stopping, so the capture did not start")
		s.captures.finish(run.ID, CaptureSummary{}, stopping)
		writeError(w, stopping, http.StatusServiceUnavailable)
		return
	}
	for _, item := range s.captures.list() {
		if item.ID == run.ID {
			writeJSON(w, map[string]any{"ok": true, "run": item}, http.StatusAccepted)
			return
		}
	}
}
