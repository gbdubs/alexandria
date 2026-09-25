package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func libraryStatusCall(t *testing.T, server *Server, path string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
	}
	value := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func activityKinds(status map[string]any) []string {
	kinds := []string{}
	items, _ := status["activities"].([]any)
	for _, item := range items {
		kinds = append(kinds, firstString(item.(map[string]any)["kind"]))
	}
	return kinds
}

func TestLibraryStatusNamesTheDriveAndIsIdleWhenNothingRuns(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	config.Library = true
	server := NewServer(config, catalog)
	status := libraryStatusCall(t, server, "/api/library/status")
	drive, _ := status["drive"].(map[string]any)
	if status["library_dir"] != filepath.Dir(config.Path) || status["catalog_path"] != config.CatalogPath || status["catalog_open"] != true {
		t.Fatalf("library identity: %#v", status)
	}
	// The test library is on this Mac's internal disk: nothing to eject.
	if drive["mounted"] != true || drive["mount_point"] == "" || drive["location"] != "internal" || drive["ejectable"] != false {
		t.Fatalf("drive: %#v", drive)
	}
	if status["idle"] != true || status["safe_to_unplug"] != true || status["writing"] != false || len(activityKinds(status)) != 0 {
		t.Fatalf("idle status: %#v", status)
	}
	if unplug, _ := status["unplug"].(map[string]any); unplug["action"] != "none" {
		t.Fatalf("unplug on the internal disk: %#v", unplug)
	}
	if capture := libraryStatusCall(t, server, "/api/capture"); capture["safe_to_unplug"] != true {
		t.Fatalf("capture status: %#v", capture)
	}
}

func TestUnplugAdviceAlwaysSaysEject(t *testing.T) {
	drive := libraryDrive{Name: "euclid", MountPoint: "/Volumes/euclid", Location: "external", Mounted: true, Ejectable: true}
	idle := unplugAdvice(drive, nil)
	if idle["action"] != "eject" || idle["without_eject"] != "probably-fine" || !strings.Contains(idle["summary"].(string), "eject euclid before unplugging") {
		t.Fatalf("idle: %#v", idle)
	}
	reading := unplugAdvice(drive, []libraryActivity{{Kind: "backup", Label: "Backing up"}})
	if reading["action"] != "eject" || reading["without_eject"] != "unsafe" || !strings.Contains(reading["summary"].(string), "Backing up is reading the library") {
		t.Fatalf("reading: %#v", reading)
	}
	writing := unplugAdvice(drive, []libraryActivity{{Kind: "capture", Label: "Capturing Mac A", Writes: true}, {Kind: "backup", Label: "Backing up"}})
	if writing["without_eject"] != "unsafe" || !strings.HasPrefix(writing["summary"].(string), "Capturing Mac A: writing to euclid now") || strings.Contains(writing["summary"].(string), "cannot stop") {
		t.Fatalf("writing: %#v", writing)
	}
	other := unplugAdvice(drive, []libraryActivity{{Kind: "capture-other", Label: "Capture by another process", Writes: true}})
	if !strings.HasSuffix(other["summary"].(string), "Pharos cannot stop it.") {
		t.Fatalf("another process's capture: %#v", other)
	}
	drive.Ejectable, drive.Location, drive.Name = false, "internal", "this Mac's internal disk"
	if internal := unplugAdvice(drive, nil); internal["action"] != "none" || internal["without_eject"] != "not-applicable" {
		t.Fatalf("internal: %#v", internal)
	}
}

// Every kind of work that holds or writes the library shows up, and each
// makes /api/capture's safe_to_unplug false, not only captures and syncs.
func TestLibraryStatusListsEverythingHoldingTheLibrary(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "s.jsonl"), "{}\n")
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	config.Sources = []SourceConfig{{Name: "claude", Kind: "claude", Path: claude, Enabled: true}}
	server := NewServer(config, catalog)
	expect := func(want ...string) map[string]any {
		t.Helper()
		status := libraryStatusCall(t, server, "/api/library/status")
		if got := strings.Join(activityKinds(status), ","); got != strings.Join(want, ",") {
			t.Fatalf("activities %q, want %q: %#v", got, strings.Join(want, ","), status["activities"])
		}
		idle := len(want) == 0
		if status["idle"] != idle || status["safe_to_unplug"] != idle {
			t.Fatalf("idle/safe_to_unplug with %v: %#v", want, status)
		}
		if capture := libraryStatusCall(t, server, "/api/capture"); capture["safe_to_unplug"] != idle {
			t.Fatalf("/api/capture safe_to_unplug with %v: %#v", want, capture["safe_to_unplug"])
		}
		return status
	}
	expect()

	// A capture by the service, with its progress.
	release := make(chan struct{})
	captureFault = func(stage, rel string) error { <-release; return nil }
	t.Cleanup(func() { captureFault = nil })
	request := httptest.NewRequest(http.MethodPost, "/api/capture", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("capture: %d %s", response.Code, response.Body.String())
	}
	status := expect("capture")
	activity := status["activities"].([]any)[0].(map[string]any)
	if activity["writes"] != true || activity["label"] != "Capturing Mac host-a" || !strings.Contains(firstString(activity["detail"]), "0 of 1 sources") || activity["on_eject"] == "" {
		t.Fatalf("capture activity: %#v", activity)
	}
	if status["writing"] != true {
		t.Fatalf("a capture writes: %#v", status)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for server.captures.active() {
		if time.Now().After(deadline) {
			t.Fatal("capture did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	expect()

	// A capture by another process (the CLI) holding this Mac's capture lock.
	session, err := beginCapture(config)
	if err != nil {
		t.Fatal(err)
	}
	expect("capture-other")
	session.Close()

	// Syncs and indexes.
	sync := server.startRunNamed("source-sync", []string{"claude"})
	index := server.startRunNamed(indexRunKind, []string{"host-b/claude", "host-b/codex"})
	server.updateRun(index.ID, func(run *SyncRun) { run.CurrentSource, run.CompletedSources, run.Conversations = "host-b/codex", 1, 3 })
	status = expect("index", "sync")
	activity = status["activities"].([]any)[0].(map[string]any)
	if activity["label"] != "Indexing captures" || activity["detail"] != "host-b/codex · 1 of 2 sources · 3 conversations written" || activity["progress"] != 0.5 {
		t.Fatalf("index activity: %#v", activity)
	}
	if activity := status["activities"].([]any)[1].(map[string]any); activity["progress"] != nil {
		t.Fatalf("a one-source sync has no progress to report: %#v", activity)
	}
	for _, run := range []*SyncRun{sync, index} {
		server.updateRun(run.ID, func(run *SyncRun) { run.State = "complete" })
	}
	expect()

	// A backup reads the library; it does not write to it.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.backups.begin(BackupRun{ID: "b1", Kind: "backup", State: "running", Phase: "catalog", Destination: "/Volumes/Other/Pharos Backup"}, cancel); err != nil {
		t.Fatal(err)
	}
	status = expect("backup")
	if status["writing"] != false || !strings.Contains(firstString(status["activities"].([]any)[0].(map[string]any)["detail"]), "/Volumes/Other/Pharos Backup · catalog") {
		t.Fatalf("backup: %#v", status)
	}
	server.backups.update(BackupRun{ID: "b1", State: "complete"}, true)
	expect()

	// The Git merge lookup after a sync or index, and the Library view's
	// pending refreshes.
	server.tasks.addGit(1, "after the last sync or index")
	expect("git")
	server.tasks.addGit(-1, "")
	if _, err := catalog.DB.Exec("INSERT INTO workspace_library_dirty(workspace_id) VALUES('w1'),('w2')"); err != nil {
		t.Fatal(err)
	}
	status = expect("maintenance")
	if detail := firstString(status["activities"].([]any)[0].(map[string]any)["detail"]); detail != "2 workspaces to refresh" {
		t.Fatalf("maintenance detail %q", detail)
	}
	if _, err := catalog.RefreshLibrary(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	expect()
}

// The Git lookup is counted while it runs and not after, including when a
// stop keeps it from starting.
func TestGitRefreshIsTrackedWhileItRuns(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	server.refreshGitInBackground(false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running, _, _ := server.tasks.gitRunning(); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git refresh still counted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.beginStop()
	server.refreshGitInBackground(true)
	if running, _, _ := server.tasks.gitRunning(); running {
		t.Fatal("a refresh that never started is counted")
	}
}

func TestBackupStatusSuggestsADestination(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	// A per-user catalog on this Mac's disk: no suggestion.
	if status := libraryStatusCall(t, server, "/api/backup"); status["suggested_destination"] != "" {
		t.Fatalf("per-user suggestion: %#v", status["suggested_destination"])
	}
	if err := recordBackup(config, backupRecord{FinishedAt: now(), Destination: "/Volumes/Other/Pharos Backup"}); err != nil {
		t.Fatal(err)
	}
	if status := libraryStatusCall(t, server, "/api/backup"); status["suggested_destination"] != "/Volumes/Other/Pharos Backup" {
		t.Fatalf("suggestion after a backup: %#v", status["suggested_destination"])
	}
}

// Once the drive checks have run (GET /api/health/drive, cheap to poll), the
// status names the volume as Finder does, without running diskutil itself.
func TestLibraryStatusNamesTheVolumeFromTheDriveChecks(t *testing.T) {
	config, _ := backupFixture(t)
	calls := stubDriveTool(t, map[string]string{"diskutil": euclidPlist, "mdutil": "Indexing disabled."})
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { catalog.Close() })
	config.APIToken = "test-token"
	server := NewServer(config, catalog)
	before := libraryStatusCall(t, server, "/api/library/status")
	if drive := before["drive"].(map[string]any); drive["name"] != "this Mac's internal disk" || drive["volume_uuid"] != "TEST-VOLUME" {
		t.Fatalf("before the checks: %#v", drive)
	}
	if len(*calls) != 0 {
		t.Fatalf("the status ran %v", *calls)
	}
	deadline := time.Now().Add(10 * time.Second)
	for libraryStatusCall(t, server, "/api/health/drive")["status"] == "checking" {
		if time.Now().After(deadline) {
			t.Fatal("the drive checks never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	report := libraryStatusCall(t, server, "/api/health/drive")
	if checks, _ := report["checks"].([]any); len(checks) != 7 || report["scope"] != "library" {
		t.Fatalf("drive report: %#v", report)
	}
	drive := libraryStatusCall(t, server, "/api/library/status")["drive"].(map[string]any)
	// The test library is on the internal disk, which diskutil (stubbed)
	// describes as the external euclid: a name, but nothing to eject.
	if drive["name"] != "euclid" || drive["volume_uuid"] != "642C2C39-5926-4831-ABE1-34642F37B103" || drive["location"] != "external" || drive["ejectable"] != false {
		t.Fatalf("after the checks: %#v", drive)
	}
}
