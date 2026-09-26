package archive

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const indexRunKind = "capture-index"

// runIndexCLI: pharos index [--host ID | --all-hosts] [SOURCE ...]
func runIndexCLI(config Config, catalog *Catalog, args []string) error {
	flags := flag.NewFlagSet("index", flag.ContinueOnError)
	host := flags.String("host", "", "index this host's captures (default: this Mac's)")
	all := flags.Bool("all-hosts", false, "index every captured host")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *host != "" && *all {
		return errors.New("usage: pharos index [--host ID | --all-hosts] [SOURCE ...]")
	}
	hosts := []string{}
	if *host != "" {
		hosts = append(hosts, *host)
	}
	targets, err := captureTargets(config.CaptureRoot, hosts, *all, flags.Args())
	if err != nil {
		return err
	}
	results, links, err := catalog.indexCaptureTargets(context.Background(), targets, func(target captureTarget, result IngestResult, elapsed time.Duration) {
		fmt.Fprintf(os.Stderr, "%s: %d workspaces written, %d parts parsed, %d unchanged in %.1fs; error: %v\n", target.label(),
			result.Workspaces, result.Parsed, result.Unchanged, elapsed.Seconds(), result.Error)
	})
	if err != nil {
		return err
	}
	if err := printJSON(map[string]any{"host": currentHost(), "sources": results, "identity_links_added": links}); err != nil {
		return err
	}
	for _, result := range results {
		if result.Error != nil {
			return errors.New("index finished with errors")
		}
	}
	return nil
}

// startIndex indexes captures in the background: body {"host": ID} or
// {"all_hosts": true}, default this Mac, and optionally {"sources": [...]}.
// It shares the sync lock, so a sync and an index never run at once.
func (s *Server) startIndex(w http.ResponseWriter, body map[string]any) {
	names, ok := sliceValue(body["sources"])
	all, allOK := valueOr(body["all_hosts"], false).(bool)
	onlyNeeded, neededOK := valueOr(body["only_needed"], false).(bool)
	host, hostOK := valueOr(body["host"], "").(string)
	if !ok || !allOK || !hostOK || !neededOK || (all && host != "") {
		writeError(w, errors.New(`body is {"host": ID} or {"all_hosts": true}, with optional "sources": [names] and "only_needed": true`), http.StatusBadRequest)
		return
	}
	hosts := []string{}
	if host != "" {
		hosts = append(hosts, host)
	}
	config := s.Config()
	targets, err := captureTargets(config.CaptureRoot, hosts, all, stringSlice(names))
	if err != nil {
		status := http.StatusBadRequest
		if _, statErr := os.Stat(config.CaptureRoot); statErr != nil {
			status = http.StatusServiceUnavailable
		}
		writeError(w, err, status)
		return
	}
	if onlyNeeded {
		hosts, err := s.Catalog.capturedHosts(config.CaptureRoot)
		if err != nil {
			writeError(w, err, http.StatusServiceUnavailable)
			return
		}
		needed := map[string]bool{}
		for _, host := range hosts {
			for _, source := range host["sources"].([]map[string]any) {
				if source["needs_index"] == true {
					needed[firstString(host["id"])+"/"+firstString(source["name"])] = true
				}
			}
		}
		targets = slices.DeleteFunc(targets, func(target captureTarget) bool { return !needed[target.label()] })
	}
	if !s.ingestMu.TryLock() {
		writeError(w, errors.New("source indexing is already running"), http.StatusConflict)
		return
	}
	labels := []string{}
	for _, target := range targets {
		labels = append(labels, target.label())
	}
	run := s.startRunNamed(indexRunKind, labels)
	started := s.spawn(func(ctx context.Context) {
		defer s.ingestMu.Unlock()
		s.indexTargets(ctx, run.ID, targets)
	})
	if !started {
		// A stop began after this request was admitted.
		s.ingestMu.Unlock()
		stopping := errors.New("Pharos is stopping, so the index did not start")
		s.updateRun(run.ID, func(run *SyncRun) {
			run.State, run.Phase, run.Error, run.CompletedAt = "failed", "failed", stopping.Error(), now()
		})
		writeError(w, stopping, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "run": s.indexRuns()[0]}, http.StatusAccepted)
}

func (s *Server) indexTargets(ctx context.Context, runID string, targets []captureTarget) {
	results := []IngestResult{}
	var failure any
	defer func() {
		if value := recover(); value != nil {
			s.exitIfFault(value)
			failure = fmt.Sprintf("index stopped: %v", value)
		}
		ok := failure == nil && ctx.Err() == nil
		for _, result := range results {
			if result.Error != nil {
				ok = false
				failure = valueOr(failure, result.Error)
			}
		}
		s.updateRun(runID, func(run *SyncRun) {
			run.State, run.Phase = "complete", "complete"
			if ctx.Err() != nil {
				run.State, run.Phase = "interrupted", "interrupted"
			} else if !ok {
				run.State, run.Phase = "failed", "failed"
			}
			run.CurrentSource, run.CompletedAt, run.Error = nil, now(), failure
		})
	}()
	for index, target := range targets {
		if ctx.Err() != nil {
			break
		}
		label := target.label()
		base := IngestResult{}
		for _, result := range results {
			base.Workspaces += result.Workspaces
			base.Conversations += result.Conversations
			base.Messages += result.Messages
			base.SkippedCurrent += result.SkippedCurrent
		}
		result := s.Catalog.IndexCapture(ctx, target, func(phase string, w, c, m, skipped int) {
			s.updateRun(runID, func(run *SyncRun) {
				run.CurrentSource, run.Phase, run.CompletedSources = label, phase, index
				run.Workspaces, run.Conversations = base.Workspaces+w, base.Conversations+c
				run.Messages, run.SkippedCurrent = base.Messages+m, base.SkippedCurrent+skipped
			})
		})
		results = append(results, result)
		s.updateRun(runID, func(run *SyncRun) {
			run.Results = slices.Clone(results)
			run.CompletedSources = index + 1
			run.Workspaces, run.Conversations = base.Workspaces+result.Workspaces, base.Conversations+result.Conversations
			run.Messages, run.SkippedCurrent = base.Messages+result.Messages, base.SkippedCurrent+result.SkippedCurrent
		})
	}
	if !wroteRecords(results) {
		return
	}
	if ctx.Err() == nil {
		if _, err := s.Catalog.ReconcileIdentities(); err != nil {
			failure = err.Error()
		}
	}
	_ = s.Catalog.Checkpoint()
	s.refreshGitInBackground(false)
}

func wroteRecords(results []IngestResult) bool {
	return slices.ContainsFunc(results, func(result IngestResult) bool { return result.Workspaces > 0 })
}

// indexCaptureTargets indexes targets in order, reporting each as it
// finishes, then links identities and refreshes the Library. Linking and the
// Git merge scan cover the whole catalog (minutes on a large one), so they run
// only when the index wrote something. A cancelled ctx stops between records.
func (c *Catalog) indexCaptureTargets(ctx context.Context, targets []captureTarget, done func(captureTarget, IngestResult, time.Duration)) ([]IngestResult, int, error) {
	results := []IngestResult{}
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		started := time.Now()
		result := c.IndexCapture(ctx, target, nil)
		if done != nil {
			done(target, result, time.Since(started))
		}
		results = append(results, result)
	}
	links := 0
	if wroteRecords(results) && ctx.Err() == nil {
		var err error
		if links, err = c.ReconcileIdentities(); err != nil {
			return results, 0, err
		}
		if err := c.refreshMainIntegrations(ctx); err != nil && ctx.Err() == nil {
			return results, links, err
		}
	}
	if ctx.Err() != nil {
		return results, links, nil
	}
	return results, links, c.refreshAllLibrary(ctx)
}

func (s *Server) indexRuns() []SyncRun {
	s.runsMu.RLock()
	defer s.runsMu.RUnlock()
	runs := []SyncRun{}
	for _, run := range s.runs {
		if run.Kind == indexRunKind {
			copied := *run
			copied.Sources, copied.Results = slices.Clone(run.Sources), slices.Clone(run.Results)
			runs = append(runs, copied)
		}
	}
	return runs
}

func (s *Server) indexStatus() map[string]any {
	config := s.Config()
	runs := s.indexRuns()
	var latest any
	active := false
	if len(runs) > 0 {
		latest, active = runs[0], runs[0].State == "running"
	}
	status := map[string]any{"active": active, "sync_active": s.syncActive(), "capture_root": config.CaptureRoot,
		"host": currentHost(), "run": latest, "runs": runs}
	hosts, err := s.Catalog.capturedHosts(config.CaptureRoot)
	status["hosts"] = hosts
	if err != nil {
		status["error"] = err.Error()
	}
	return status
}

// capturedHosts describes each host's captures and how current their index
// is, for choosing what to index.
func (c *Catalog) capturedHosts(root string) ([]map[string]any, error) {
	hosts := []map[string]any{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return hosts, fmt.Errorf("capture root %s is unavailable: %w", root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validCaptureName(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		host := capturedHost(dir, entry.Name())
		sources := []map[string]any{}
		items, _ := os.ReadDir(dir)
		for _, item := range items {
			// New manifests summarize when data was actually copied. Older ones
			// need their file timestamps read once to recover that time.
			var manifest struct {
				Version    int                 `json:"version"`
				UpdatedAt  string              `json:"updated_at"`
				LastDataAt string              `json:"last_data_at"`
				Source     captureSourceInfo   `json:"source"`
				LastRun    *captureRunRecord   `json:"last_run"`
				Snapshots  []*capturedSnapshot `json:"snapshots"`
			}
			data, err := os.ReadFile(filepath.Join(dir, item.Name(), captureManifestName))
			if !item.IsDir() || !validCaptureName(item.Name()) || err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Version == 0 {
				continue
			}
			lastDataAt := manifest.LastDataAt
			if lastDataAt == "" {
				var old captureManifest
				if json.Unmarshal(data, &old) == nil {
					lastDataAt = old.lastCapturedDataAt()
				}
			}
			rows, err := queryMaps(c.DB, "SELECT coverage,last_attempt_at,last_success_at,error,index_version FROM source_states WHERE host_id=? AND source_name=?", host.ID, item.Name())
			if err != nil {
				return hosts, err
			}
			state := map[string]any{}
			if len(rows) > 0 {
				state = rows[0]
			}
			finished := manifest.LastRun != nil && manifest.LastRun.FinishedAt != ""
			indexed, indexedOK := parseTime(firstString(state["last_success_at"]))
			captured, capturedOK := parseTime(lastDataAt)
			versionChanged := false
			adapter, adapterErr := MakeAdapter(SourceConfig{Name: item.Name(), Kind: manifest.Source.Kind, Path: manifest.Source.Path, Account: manifest.Source.Account})
			if adapterErr == nil {
				versionChanged = firstString(state["index_version"]) != captureSourceIndexVersion(adapter, host.ID)
			}
			pendingSnapshots := false
			if len(manifest.Snapshots) > 0 {
				marker := loadIndexMarker(filepath.Join(dir, item.Name()))
				for _, snapshot := range manifest.Snapshots {
					if !marker.saved[snapshot.Path+"\x1f"+snapshot.CapturedAt] {
						pendingSnapshots = true
						break
					}
					for _, generation := range snapshot.Generations {
						if !marker.saved[snapshot.Path+"\x1f"+generation.CapturedAt] {
							pendingSnapshots = true
							break
						}
					}
					if pendingSnapshots {
						break
					}
				}
			}
			needsIndex := !indexedOK || firstString(state["coverage"]) != "complete" || (capturedOK && captured.After(indexed)) || versionChanged || pendingSnapshots
			reason := ""
			switch {
			case !indexedOK:
				reason = "not indexed"
			case firstString(state["coverage"]) != "complete":
				reason = "previous index incomplete"
			case versionChanged:
				reason = "indexer updated"
			case capturedOK && captured.After(indexed):
				reason = "new capture data"
			case pendingSnapshots:
				reason = "snapshots awaiting index"
			}
			sources = append(sources, map[string]any{"name": item.Name(), "kind": manifest.Source.Kind, "account": manifest.Source.Account, "path": manifest.Source.Path,
				"captured_at": manifest.UpdatedAt, "last_data_at": lastDataAt, "capture_finished": finished,
				"indexed_at": state["last_success_at"], "last_attempt_at": state["last_attempt_at"], "coverage": defaultString(state["coverage"], "not-indexed"), "error": state["error"],
				"needs_index": needsIndex, "index_reason": reason})
		}
		record := map[string]any{"id": host.ID, "label": host.Label, "user": host.User, "current": host.ID == currentHost().ID, "sources": sources}
		hosts = append(hosts, record)
	}
	return hosts, nil
}

func captureSourceIndexVersion(adapter Adapter, hostID string) string {
	version := sourceIndexVersion(adapter)
	if _, partial := adapter.(partialAdapter); partial && hostID != currentHost().ID {
		version += "@offhost"
	}
	return version
}
