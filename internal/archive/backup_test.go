package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// backupFixture is a library with a catalog, captures, a host file and
// preserved data; tests treat dest as another drive.
func backupFixture(t *testing.T) (Config, string) {
	t.Helper()
	useHost(t, "host-a")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config, err := InitLibrary(filepath.Join(root, "Pharos"), func(string) string { return "uuid:TEST-VOLUME" })
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("INSERT INTO hosts(id,label,first_seen_at,last_seen_at) VALUES('host-z','Z','2026-01-01','2026-01-01')"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(config.CaptureRoot, "host-a")
	writeSourceFile(t, filepath.Join(host, "claude", "files", "p", "a.jsonl"), "one\n")
	writeSourceFile(t, filepath.Join(host, "claude", "files", "p", "b.jsonl"), strings.Repeat("two\n", 1000))
	writeSourceFile(t, filepath.Join(host, "claude", "manifest.json"), "{}\n")
	writeSourceFile(t, filepath.Join(host, captureLockName), "")
	writeSourceFile(t, filepath.Join(host, "claude", "files", "p", "c.jsonl"+captureTempSuffix), "half-written")
	writeSourceFile(t, filepath.Join(filepath.Dir(config.Path), "hosts", "host-a.toml"), "[[sources]]\nname = \"claude\"\n")
	writeSourceFile(t, filepath.Join(config.ArchiveRoot, "package", "x.json"), "{}")
	dest := filepath.Join(root, "Other Drive", "Pharos Backup")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	stubBackupDrives(t, filepath.Dir(dest), nil)
	return config, dest
}

// stubBackupDrives puts everything under other on another volume and drive;
// facts, if given, adjusts what either side reports.
func stubBackupDrives(t *testing.T, other string, facts func(path string, facts *volumeFacts)) {
	t.Helper()
	previous := backupVolume
	backupVolume = func(path string) (uint64, volumeFacts) {
		device, result := uint64(1), volumeFacts{PhysicalDisk: "disk4", Encryption: "filevault", Spotlight: "disabled", Location: "external"}
		if within(canonical(path), other) {
			device, result.PhysicalDisk = 2, "disk9"
		}
		if facts != nil {
			facts(canonical(path), &result)
		}
		return device, result
	}
	t.Cleanup(func() { backupVolume = previous })
}

// legacyBackupFixture is a per-user install: archive.toml and the catalog in
// a data directory, archive_root wherever archiveRoot puts it.
func legacyBackupFixture(t *testing.T, archiveRoot func(data string) string) (Config, string) {
	t.Helper()
	useHost(t, "mac-a")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "AI Work Archive")
	config := defaultConfig(filepath.Join(data, "archive.toml"))
	config.CatalogPath = filepath.Join(data, "catalog.sqlite3")
	config.ArchiveRoot = archiveRoot(data)
	config.StagingRoot = filepath.Join(data, "staging")
	config.CaptureRoot = filepath.Join(data, "captures")
	writeSourceFile(t, config.Path, "archive_root = \"elsewhere\"\n")
	if err := os.MkdirAll(config.ArchiveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "Other Drive", "Pharos Backup")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	stubBackupDrives(t, filepath.Dir(dest), nil)
	return config, dest
}

func backupFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

func runBackup(t *testing.T, config Config, dest string, prune bool) BackupRun {
	t.Helper()
	job, err := prepareBackup(config, dest, prune)
	if err != nil {
		t.Fatal(err)
	}
	run := job.Run(context.Background(), nil)
	if run.State != "complete" {
		t.Fatalf("backup %s: %+v", run.State, run.Errors)
	}
	return run
}

func TestBackupCopiesTheLibraryIncrementally(t *testing.T) {
	config, dest := backupFixture(t)
	first := runBackup(t, config, dest, false)
	// library.toml, 2 captures + manifest + host.json-less host dir, hosts/host-a.toml, preserved/x.json.
	if first.FilesCopied != 6 || !first.CatalogCopied || first.FilesUnchanged != 0 {
		t.Fatalf("first backup: %+v", first)
	}
	source := filepath.Join(config.CaptureRoot, "host-a", "claude", "files", "p", "b.jsonl")
	copied := filepath.Join(dest, "captures", "host-a", "claude", "files", "p", "b.jsonl")
	sourceInfo, _ := os.Stat(source)
	copiedInfo, err := os.Stat(copied)
	if err != nil || readText(t, copied) != readText(t, source) || !copiedInfo.ModTime().Equal(sourceInfo.ModTime()) || copiedInfo.Mode().Perm() != 0o600 {
		t.Fatalf("copy of b.jsonl: %v %v", copiedInfo, err)
	}
	for _, skipped := range []string{"captures/host-a/" + captureLockName, "captures/host-a/claude/files/p/c.jsonl" + captureTempSuffix, "staging"} {
		if _, err := os.Stat(filepath.Join(dest, skipped)); err == nil {
			t.Errorf("%s was backed up", skipped)
		}
	}
	for _, kept := range []string{"library.toml", "hosts/host-a.toml", "preserved/package/x.json", "captures/host-a/claude/manifest.json"} {
		if _, err := os.Stat(filepath.Join(dest, kept)); err != nil {
			t.Errorf("%s missing from the backup: %v", kept, err)
		}
	}
	snapshot, err := openReadOnlySQLite(filepath.Join(dest, "catalog", "catalog.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	var label, mode string
	if err := snapshot.QueryRow("SELECT label FROM hosts WHERE id='host-z'").Scan(&label); err != nil || label != "Z" {
		t.Fatalf("catalog snapshot: %q %v", label, err)
	}
	if err := snapshot.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("snapshot journal mode %q %v", mode, err)
	}
	snapshot.Close()
	var manifest backupManifest
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(dest, backupManifestName))), &manifest); err != nil || !manifest.Complete || manifest.Catalog == nil || manifest.Library.VolumeID != "uuid:TEST-VOLUME" {
		t.Fatalf("manifest: %+v %v", manifest, err)
	}
	history := loadBackupHistory(config)
	if len(history.Backups) != 1 || history.Backups[0].Destination != dest || history.Backups[0].BytesTotal != first.BytesTotal {
		t.Fatalf("backups.json: %+v", history)
	}

	// Nothing changed: nothing is copied, not even the catalog.
	second := runBackup(t, config, dest, false)
	if second.FilesCopied != 0 || second.CatalogCopied || second.FilesUnchanged != first.FilesCopied || second.BytesTotal != first.BytesTotal {
		t.Fatalf("unchanged backup: %+v", second)
	}

	// Changes are copied; a file gone from the library stays in the backup.
	writeSourceFile(t, source, strings.Repeat("two\n", 1001))
	writeSourceFile(t, filepath.Join(config.CaptureRoot, "host-a", "claude", "files", "p", "d.jsonl"), "new\n")
	if err := os.Remove(filepath.Join(config.CaptureRoot, "host-a", "claude", "files", "p", "a.jsonl")); err != nil {
		t.Fatal(err)
	}
	third := runBackup(t, config, dest, false)
	if third.FilesCopied != 2 || third.FilesPruned != 0 || readText(t, copied) != readText(t, source) {
		t.Fatalf("changed backup: %+v", third)
	}
	gone := filepath.Join(dest, "captures", "host-a", "claude", "files", "p", "a.jsonl")
	if _, err := os.Stat(gone); err != nil {
		t.Fatal("a file deleted from the library was deleted from the backup without --prune")
	}
	// A leftover temporary is ours to prune too.
	writeSourceFile(t, filepath.Join(dest, "preserved", "stale"+backupTempSuffix), "x")
	fourth := runBackup(t, config, dest, true)
	if fourth.FilesPruned != 2 || fourth.FilesCopied != 0 {
		t.Fatalf("pruned backup: %+v", fourth)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("--prune kept a file deleted from the library")
	}
	if len(loadBackupHistory(config).Backups) != 4 {
		t.Fatal("not every backup was recorded")
	}
}

func TestBackupRefusesUnsafeDestinations(t *testing.T) {
	config, dest := backupFixture(t)
	library := filepath.Dir(config.Path)
	cases := map[string]string{
		filepath.Join(library, "backup"): "overlaps",
		library:                          "overlaps",
		filepath.Join(filepath.Dir(dest), "missing", "deeper"): "unavailable",
		"": "required",
	}
	for destination, problem := range cases {
		if _, err := prepareBackup(config, destination, false); err == nil || !strings.Contains(err.Error(), problem) {
			t.Errorf("backup to %q: %v, want %q", destination, err, problem)
		}
	}
	writeSourceFile(t, filepath.Join(dest, "notes.txt"), "mine")
	if _, err := prepareBackup(config, dest, true); err == nil || !strings.Contains(err.Error(), "neither empty nor a Pharos backup") {
		t.Fatalf("non-empty destination: %v", err)
	}
	os.Remove(filepath.Join(dest, "notes.txt"))
	writeSourceFile(t, filepath.Join(dest, ".DS_Store"), "finder")
	runBackup(t, config, dest, false)
	// Another library may not take over this backup.
	record := backupRecordPath(config)
	saved := readText(t, record)
	writeSourceFile(t, record, strings.Replace(saved, `"library_id": "`, `"library_id": "other-`, 1))
	if _, err := prepareBackup(config, dest, true); err == nil || !strings.Contains(err.Error(), "another library") {
		t.Fatalf("foreign library: %v", err)
	}
	writeSourceFile(t, record, saved)
	for _, relative := range []string{"relative/path", "./x", "x"} {
		if _, err := prepareBackup(config, relative, false); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Errorf("relative destination %q: %v", relative, err)
		}
	}

	// Without the stand-in drives, a folder beside the library is refused.
	backupVolume = func(path string) (uint64, volumeFacts) {
		device, _ := deviceOf(nearestExisting(path))
		return device, volumeFacts{}
	}
	if _, err := prepareBackup(config, filepath.Join(filepath.Dir(dest), "Second"), false); err == nil || !strings.Contains(err.Error(), "same volume") {
		t.Fatalf("same-volume destination: %v", err)
	}
	backupVolume = func(path string) (uint64, volumeFacts) {
		device, _ := deviceOf(nearestExisting(path))
		if within(canonical(path), filepath.Dir(dest)) {
			device++
		}
		return device, volumeFacts{PhysicalDisk: "disk4"}
	}
	if _, err := prepareBackup(config, filepath.Join(filepath.Dir(dest), "Second"), false); err == nil || !strings.Contains(err.Error(), "same physical drive (disk4)") {
		t.Fatalf("same-drive destination: %v", err)
	}
}

func TestBackupWaitsForARunningCapture(t *testing.T) {
	config, dest := backupFixture(t)
	lock, err := os.OpenFile(filepath.Join(config.CaptureRoot, "host-a", captureLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	job, err := prepareBackup(config, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once atomic.Bool
	done := make(chan BackupRun)
	go func() {
		done <- job.Run(context.Background(), func(run BackupRun) {
			if run.Phase == "waiting for a capture to finish" && once.CompareAndSwap(false, true) {
				close(waiting)
			}
		})
	}()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the backup did not wait for the capture lock")
	}
	if _, err := os.Stat(filepath.Join(dest, "captures", "host-a")); err == nil {
		t.Fatal("captures were copied while a capture held the lock")
	}
	lock.Close()
	if run := <-done; run.State != "complete" {
		t.Fatalf("backup after the capture: %+v", run)
	}
	if _, err := os.Stat(filepath.Join(dest, "captures", "host-a", "claude", "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRefusesADestinationInUse(t *testing.T) {
	config, dest := backupFixture(t)
	runBackup(t, config, dest, false)
	lock, err := os.OpenFile(filepath.Join(dest, backupLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	job, err := prepareBackup(config, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if run := job.Run(context.Background(), nil); run.State != "failed" || len(run.Errors) != 1 || run.Errors[0].Error != errBackupDestinationBusy.Error() {
		t.Fatalf("second backup to a destination in use: %+v", run)
	}
}

func TestBackupCancelledRecordsNothing(t *testing.T) {
	config, dest := backupFixture(t)
	job, err := prepareBackup(config, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if run := job.Run(ctx, nil); run.State != "cancelled" {
		t.Fatalf("cancelled backup: %+v", run)
	}
	if len(loadBackupHistory(config).Backups) != 0 {
		t.Fatal("a cancelled backup was recorded")
	}
}

func TestBackupAPIRunsInBackground(t *testing.T) {
	config, dest := backupFixture(t)
	server := probeServer(t, config.Path)
	if code, body := probeCall(t, server, http.MethodPost, "/api/backup", `{"destination":"relative/path"}`); code != http.StatusBadRequest {
		t.Fatalf("relative destination: %d %v", code, body)
	}
	code, body := probeCall(t, server, http.MethodPost, "/api/backup", fmt.Sprintf(`{"destination":%q}`, dest))
	if code != http.StatusAccepted || body["ok"] != true {
		t.Fatalf("start: %d %v", code, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, status := probeCall(t, server, http.MethodGet, "/api/backup", "")
		if status["active"] == false {
			run, _ := status["run"].(map[string]any)
			if run["state"] != "complete" || status["last_success"] == nil {
				t.Fatalf("backup status: %v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup still running: %v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, body := probeCall(t, server, http.MethodPost, "/api/backup/cancel", ""); body["cancelled"] != false {
		t.Fatalf("cancel with nothing running: %v", body)
	}
	_, health := probeCall(t, server, http.MethodGet, "/api/health", "")
	if drive, ok := health["drive"].(map[string]any); !ok || drive["status"] == nil {
		t.Fatalf("health has no drive report: %v", health["drive"])
	}
}

// Item 1: a symlinked archive_root or hosts/ is backed up as what it points
// to, and a listing that comes back empty never prunes an earlier backup.
func TestBackupFollowsSymlinkedRootsAndNeverPrunesAnEmptyListing(t *testing.T) {
	config, dest := backupFixture(t)
	library := filepath.Dir(config.Path)
	elsewhere := filepath.Join(filepath.Dir(library), "Elsewhere")
	for _, name := range []string{"preserved", "hosts"} {
		if err := os.MkdirAll(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(library, name), filepath.Join(elsewhere, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(elsewhere, name), filepath.Join(library, name)); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink inside a tree is not followed, and its earlier copy is kept.
	writeSourceFile(t, filepath.Join(elsewhere, "linked.json"), "{}")
	if err := os.Symlink(filepath.Join(elsewhere, "linked.json"), filepath.Join(elsewhere, "preserved", "link.json")); err != nil {
		t.Fatal(err)
	}
	first := runBackup(t, config, dest, true)
	for _, kept := range []string{"preserved/package/x.json", "hosts/host-a.toml"} {
		if !backupFileExists(t, filepath.Join(dest, kept)) {
			t.Fatalf("%s is missing from the backup: %+v", kept, first)
		}
	}
	if first.FilesPruned != 0 || !strings.Contains(strings.Join(first.Warnings, " "), "symbolic links") {
		t.Fatalf("first backup: %+v", first)
	}
	writeSourceFile(t, filepath.Join(dest, "preserved", "link.json"), "an earlier copy")
	second := runBackup(t, config, dest, true)
	if second.FilesPruned != 0 || second.FilesCopied != 0 || !backupFileExists(t, filepath.Join(dest, "preserved", "link.json")) {
		t.Fatalf("second backup: %+v", second)
	}
	// archive_root now lists nothing (an unmounted drive's empty mount
	// point, say): its backup is kept, with a warning.
	if err := os.RemoveAll(filepath.Join(elsewhere, "preserved")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(elsewhere, "preserved"), 0o755); err != nil {
		t.Fatal(err)
	}
	third := runBackup(t, config, dest, true)
	if third.FilesPruned != 0 || !backupFileExists(t, filepath.Join(dest, "preserved", "package", "x.json")) || !strings.Contains(strings.Join(third.Warnings, " "), "was not pruned") {
		t.Fatalf("empty listing: %+v", third)
	}
}

// Item 2: HFS+ stores names decomposed. A copy whose name the destination
// stores differently from the library's is still the same file to --prune.
func TestBackupPruneKeepsFilesTheDestinationNamesDifferently(t *testing.T) {
	config, dest := backupFixture(t)
	composed := []string{"café.json", "한국어.json"}
	decomposed := []string{"café.json", "한국어.json"}
	for _, name := range composed {
		writeSourceFile(t, filepath.Join(config.ArchiveRoot, name), name)
	}
	runBackup(t, config, dest, false)
	// What an HFS+ destination would have stored.
	for index := range composed {
		temporary := filepath.Join(dest, "preserved", "renaming")
		if err := os.Rename(filepath.Join(dest, "preserved", composed[index]), temporary); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(dest, "preserved", decomposed[index])); err != nil {
			t.Fatal(err)
		}
	}
	run := runBackup(t, config, dest, true)
	if run.FilesPruned != 0 || run.FilesCopied != 0 {
		t.Fatalf("prune removed or recopied files it had just accounted for: %+v", run)
	}
	for _, name := range composed {
		if readText(t, filepath.Join(dest, "preserved", name)) != name {
			t.Fatalf("%s is gone from the backup", name)
		}
	}
}

// Item 3: a destination belongs to one library. A per-user install on
// another Mac is another library, even with the same catalog path and a copy
// of backups.json; a portable library stays itself on every Mac.
func TestBackupIdentityTiesADestinationToOneLibrary(t *testing.T) {
	config, dest := legacyBackupFixture(t, func(data string) string { return filepath.Join(data, "preserved") })
	runBackup(t, config, dest, false)
	id := loadBackupHistory(config).LibraryID
	if id == "" {
		t.Fatal("the first backup did not give the library an id")
	}
	var manifest backupManifest
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(dest, backupManifestName))), &manifest); err != nil || manifest.Library.ID != id || manifest.Library.HostID != "mac-a" {
		t.Fatalf("manifest library: %+v %v", manifest.Library, err)
	}
	useHost(t, "mac-b")
	if _, err := prepareBackup(config, dest, true); err == nil || !strings.Contains(err.Error(), "another library") {
		t.Fatalf("another Mac's install with a copied backups.json: %v", err)
	}
	os.Remove(backupRecordPath(config))
	if _, err := prepareBackup(config, dest, true); err == nil || !strings.Contains(err.Error(), "another library") {
		t.Fatalf("another Mac's install: %v", err)
	}

	library, libraryDest := backupFixture(t)
	runBackup(t, library, libraryDest, false)
	useHost(t, "host-b")
	if run := runBackup(t, library, libraryDest, true); run.State != "complete" {
		t.Fatalf("the same library on another Mac: %+v", run)
	}
}

// Item 4: with archive_root being the catalog's own directory, the tree
// copies neither the live catalog nor backup internals, and prune spares
// the snapshot, the manifest and the lock.
func TestBackupKeepsTheLiveCatalogOutOfTrees(t *testing.T) {
	config, dest := legacyBackupFixture(t, func(data string) string { return data })
	writeSourceFile(t, filepath.Join(config.ArchiveRoot, "package", "a.json"), "{}")
	writeSourceFile(t, filepath.Join(config.CaptureRoot, "host-a", "claude", "files", "x.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(config.StagingRoot, "partial.zip"), "zip")
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if _, err := catalog.DB.Exec("INSERT INTO hosts(id,label,first_seen_at,last_seen_at) VALUES('live','Live','2026-01-01','2026-01-01')"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		runBackup(t, config, dest, true)
	}
	preserved := filepath.Join(dest, "preserved")
	if !backupFileExists(t, filepath.Join(preserved, "package", "a.json")) || !backupFileExists(t, filepath.Join(dest, "captures", "host-a", "claude", "files", "x.jsonl")) {
		t.Fatal("the archive or captures are missing from the backup")
	}
	for _, name := range []string{"catalog.sqlite3", "catalog.sqlite3-wal", "catalog.sqlite3-shm", "backups.json", backupRecordLockName, "captures", "staging", "archive.toml"} {
		if backupFileExists(t, filepath.Join(preserved, name)) {
			t.Errorf("preserved/%s was copied", name)
		}
	}
	for _, name := range []string{backupManifestName, backupLockName, "catalog.sqlite3", "archive.toml"} {
		if !backupFileExists(t, filepath.Join(dest, name)) {
			t.Errorf("%s is missing after a pruned backup", name)
		}
	}
	snapshot, err := openReadOnlySQLite(filepath.Join(dest, "catalog.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var label, mode string
	if err := snapshot.QueryRow("SELECT label FROM hosts WHERE id='live'").Scan(&label); err != nil || label != "Live" {
		t.Fatalf("snapshot: %q %v", label, err)
	}
	if err := snapshot.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("snapshot journal mode %q %v", mode, err)
	}
}

// Item 5: a destination on a disk image stored on the library's drive is on
// that drive, and so is a library on an image stored on the destination's.
func TestBackupRefusesDiskImagesOnTheSameDrive(t *testing.T) {
	config, dest := backupFixture(t)
	library := filepath.Dir(config.Path)
	image := filepath.Join(filepath.Dir(library), "Images", "Backup.sparsebundle")
	stubBackupDrives(t, filepath.Dir(dest), func(path string, facts *volumeFacts) {
		if within(path, filepath.Dir(dest)) {
			facts.Location, facts.ImagePath, facts.PhysicalDisk = "disk-image", image, "disk12"
		}
	})
	if _, err := prepareBackup(config, dest, false); err == nil || !strings.Contains(err.Error(), "disk image ("+image+") stored on the same drive") {
		t.Fatalf("backup onto an image on the library drive: %v", err)
	}
	other := filepath.Join(filepath.Dir(dest), "library.sparsebundle")
	stubBackupDrives(t, filepath.Dir(dest), func(path string, facts *volumeFacts) {
		if within(path, library) || path == library {
			facts.Location, facts.ImagePath, facts.PhysicalDisk = "disk-image", other, "disk13"
		}
	})
	if _, err := prepareBackup(config, dest, false); err == nil || !strings.Contains(err.Error(), "is on a disk image ("+other+")") {
		t.Fatalf("library on an image on the destination drive: %v", err)
	}
}

// Item 6: exFAT, HFS+ and FAT store mtimes coarsely; the backup stays
// incremental there, and still copies a real change.
func TestBackupIsIncrementalOnCoarseTimestamps(t *testing.T) {
	config, dest := backupFixture(t)
	runBackup(t, config, dest, false)
	for _, granularity := range []time.Duration{10 * time.Millisecond, time.Second, 2 * time.Second} {
		_ = filepath.WalkDir(filepath.Join(dest, "captures"), func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() {
				info, _ := entry.Info()
				rounded := info.ModTime().Truncate(granularity)
				os.Chtimes(path, rounded, rounded)
			}
			return nil
		})
		if run := runBackup(t, config, dest, true); run.FilesCopied != 0 || run.FilesPruned != 0 {
			t.Fatalf("%v timestamps: %+v", granularity, run)
		}
	}
	source := filepath.Join(config.CaptureRoot, "host-a", "claude", "files", "p", "a.jsonl")
	writeSourceFile(t, source, "ONE\n") // same size, a later mtime
	if run := runBackup(t, config, dest, false); run.FilesCopied != 1 {
		t.Fatalf("changed file: %+v", run)
	}
}

// Item 6: a missing part makes the backup partial, and says so.
func TestBackupWithAMissingPartIsPartial(t *testing.T) {
	config, dest := backupFixture(t)
	runBackup(t, config, dest, false)
	if err := os.RemoveAll(config.ArchiveRoot); err != nil {
		t.Fatal(err)
	}
	job, err := prepareBackup(config, dest, true)
	if err != nil {
		t.Fatal(err)
	}
	run := job.Run(context.Background(), nil)
	if run.State != "partial" || len(run.Missing) != 1 || run.Missing[0] != config.ArchiveRoot {
		t.Fatalf("backup without archive_root: %+v", run)
	}
	if !backupFileExists(t, filepath.Join(dest, "preserved", "package", "x.json")) {
		t.Fatal("the missing part's earlier backup was deleted")
	}
	history := loadBackupHistory(config)
	if !history.Backups[0].Partial {
		t.Fatalf("history: %+v", history.Backups[0])
	}
	if check := backupCheck(config, history, volumeFacts{}); check.Status != "warning" || !strings.Contains(check.Summary, "partial") {
		t.Fatalf("backup check after a partial backup: %+v", check)
	}
	if rate, _ := (backupHistory{Backups: []backupRecord{{FinishedAt: now(), Partial: true, BytesTotal: 1}, {FinishedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano), BytesTotal: 5e9}}}).growth(); rate != 0 {
		t.Fatalf("growth counted a partial backup: %v", rate)
	}
}

// Item 6: concurrent records all land in backups.json.
func TestBackupRecordsAreNotLost(t *testing.T) {
	config, _ := backupFixture(t)
	var wait sync.WaitGroup
	for index := range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := recordBackup(config, backupRecord{Destination: fmt.Sprint(index), FinishedAt: now()}); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if history := loadBackupHistory(config); len(history.Backups) != 20 {
		t.Fatalf("%d of 20 records kept", len(history.Backups))
	}
}

// Item 6: credentials for other services stay out of the backup.
func TestBackupLeavesOtherServicesCredentialsOut(t *testing.T) {
	config, dest := legacyBackupFixture(t, func(data string) string { return filepath.Join(data, "preserved") })
	writeSourceFile(t, config.Path, "api_token = \"local-token\"\ngithub_token = \"ghp_secret\" # mine\ntl1_token = 'tl1-secret'\narchive_root = \"x\"\n\n[[sources]]\nname = \"github_token\"\n")
	runBackup(t, config, dest, false)
	copied := readText(t, filepath.Join(dest, "archive.toml"))
	if strings.Contains(copied, "ghp_secret") || strings.Contains(copied, "tl1-secret") || !strings.Contains(copied, `api_token = "local-token"`) ||
		!strings.Contains(copied, "# github_token was left out of this backup") || !strings.Contains(copied, "name = \"github_token\"") {
		t.Fatalf("backed-up configuration:\n%s", copied)
	}
	if run := runBackup(t, config, dest, false); run.FilesCopied != 0 {
		t.Fatalf("an unchanged configuration was copied again: %+v", run)
	}
}

// Item 6: while a writer keeps committing, a snapshot stops once the WAL it
// pins grows past the bound, rather than letting it grow without limit.
func TestBackupBoundsTheCatalogWAL(t *testing.T) {
	config, dest := backupFixture(t)
	previousGrowth, previousStep, previousPoll := backupWALGrowth, backupStepPages, backupWALPoll
	backupWALGrowth, backupStepPages, backupWALPoll = 64<<10, 1, 5*time.Millisecond
	t.Cleanup(func() { backupWALGrowth, backupStepPages, backupWALPoll = previousGrowth, previousStep, previousPoll })
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if _, err := catalog.DB.Exec("CREATE TABLE filler(body BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DB.Exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<4000) INSERT INTO filler SELECT randomblob(8000) FROM n"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var commits atomic.Int64
	var writeErr atomic.Value
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := catalog.DB.Exec("INSERT INTO filler VALUES(randomblob(8000))"); err != nil {
				writeErr.Store(err)
			} else {
				commits.Add(1)
			}
		}
	}()
	job, err := prepareBackup(config, dest, false)
	if err != nil {
		t.Fatal(err)
	}
	run := job.Run(context.Background(), nil)
	close(stop)
	<-done
	if run.State != "failed" || len(run.Errors) == 0 || !strings.Contains(run.Errors[0].Error, "WAL grew") || run.CatalogWALPeak <= backupWALGrowth {
		t.Fatalf("backup under a writer (%d commits, error %v): %+v", commits.Load(), writeErr.Load(), run)
	}
	if backupFileExists(t, filepath.Join(dest, "catalog", "catalog.sqlite3"+backupTempSuffix)) {
		t.Fatal("the abandoned snapshot was left behind")
	}
}
