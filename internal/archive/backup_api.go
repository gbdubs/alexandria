package archive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// backupRuns tracks the service's backups; one runs at a time.
type backupRuns struct {
	mu     sync.Mutex
	runs   []*BackupRun
	cancel context.CancelFunc
}

var errBackupBusy = errors.New("a backup is already running")

func (r *backupRuns) begin(run BackupRun, cancel context.CancelFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return errBackupBusy
	}
	r.cancel = cancel
	r.runs = append([]*BackupRun{&run}, r.runs...)
	if len(r.runs) > 10 {
		r.runs = r.runs[:10]
	}
	return nil
}

func (r *backupRuns) update(run BackupRun, done bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.runs {
		if item.ID == run.ID {
			*item = run
		}
	}
	if done {
		r.cancel = nil
	}
}

func (r *backupRuns) stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel == nil {
		return false
	}
	r.cancel()
	return true
}

func (r *backupRuns) status() (bool, []BackupRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := make([]BackupRun, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run.clone())
	}
	return r.cancel != nil, runs
}

func (s *Server) backupStatus() map[string]any {
	active, runs := s.backups.status()
	var latest any
	if len(runs) > 0 {
		latest = runs[0]
	}
	history := loadBackupHistory(s.Config())
	var last any
	if len(history.Backups) > 0 {
		last = history.Backups[0]
	}
	return map[string]any{"active": active, "run": latest, "runs": runs, "last_success": last, "record": backupRecordPath(s.Config()),
		"suggested_destination": suggestedBackupDestination(s.Config(), history)}
}

// suggestedBackupDestination is where the UI proposes to back up: the last
// destination, or for a library on another drive a folder in this Mac's home.
// A library or per-user catalog on this Mac's own disk has none.
func suggestedBackupDestination(config Config, history backupHistory) string {
	if len(history.Backups) > 0 {
		return history.Backups[0].Destination
	}
	home, err := os.UserHomeDir()
	if !config.Library || err != nil || libraryDriveOf(config).Location == "internal" {
		return ""
	}
	return filepath.Join(home, "Pharos Backup")
}

// startBackup checks the destination, then backs up in the background;
// progress is polled with GET /api/backup, and POST /api/backup/cancel or a
// release (before an eject) stops it between files or catalog pages.
func (s *Server) startBackup(w http.ResponseWriter, body map[string]any) {
	destination, _ := body["destination"].(string)
	prune, _ := body["prune"].(bool)
	if _, ok := body["prune"]; ok && body["prune"] != true && body["prune"] != false {
		writeError(w, fmt.Errorf("prune must be true or false"), http.StatusBadRequest)
		return
	}
	job, err := prepareBackup(s.Config(), destination, prune)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	started := job.run.clone()
	ctx, cancel := context.WithCancel(s.life.ctx)
	if err := s.backups.begin(started, cancel); err != nil {
		cancel()
		writeError(w, err, http.StatusConflict)
		return
	}
	s.spawn(func(context.Context) {
		defer cancel()
		run := started
		defer func() {
			if value := recover(); value != nil {
				s.exitIfFault(value)
				run.State, run.Phase, run.CompletedAt = "failed", "failed", now()
				run.Errors = append(run.Errors, captureError{Error: fmt.Sprintf("backup stopped: %v", value)})
			}
			s.backups.update(run, true)
		}()
		run = job.Run(ctx, func(progress BackupRun) { s.backups.update(progress, false) })
	})
	writeJSON(w, map[string]any{"ok": true, "run": started}, http.StatusAccepted)
}
