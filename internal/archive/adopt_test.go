package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const adoptVolume = "uuid:ADOPT-TEST"

func adoptIdentity(string) string { return adoptVolume }

// legacyInstall is a per-user install as it was before hosts: archive.toml
// with sources, a schema-4 catalog under data_dir, and preserved packages.
type legacyInstall struct {
	root, config, catalog, archive string
}

func newLegacyInstall(t *testing.T) legacyInstall {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyInstall{root: root, config: filepath.Join(root, "legacy", "archive.toml"),
		catalog: filepath.Join(root, "legacy", "data", "catalog.sqlite3"), archive: filepath.Join(root, "legacy", "archive")}
	projects := filepath.Join(root, "home", ".claude", "projects", "-Users-test-pharos")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	for session := range 12 {
		lines := []string{}
		for message := range 8 {
			lines = append(lines, fmt.Sprintf(`{"sessionId":"s%d","uuid":"s%d-m%d","type":"user","cwd":"/Users/test/pharos","timestamp":"2026-09-%02dT00:%02d:00Z","message":{"content":"session%d message %d %s"}}`,
				session, session, message, session+1, message, session, message, strings.Repeat("padding ", 50)))
		}
		if err := os.WriteFile(filepath.Join(projects, fmt.Sprintf("s%d.jsonl", session)), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf(`data_dir = "data"
archive_root = "archive"
api_token = "legacy-token"
port = 18793
package_cap_bytes = 50000000
upcoming_days = 20
snooze_days = 7
enable_reclamation = true
github_token = "ghp_test"

[[sources]]
name = "claude"
kind = "claude"
path = %q
account = "work"

[[sources]]
name = "codex"   # never read by these tests
kind = "codex"
path = "~/.codex-adopt-test"
enabled = false

[[sources]]
name = "export"
kind = "canonical"
path = "exports/canonical.json"
enabled = false
include_archived = true
`, filepath.Dir(projects))
	if err := os.MkdirAll(filepath.Dir(legacy.config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy.config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// Preserved packages hard-link their blobs.
	blob := filepath.Join(legacy.archive, "blobs", "sha256", "ab", "abcdef")
	pkg := filepath.Join(legacy.archive, "packages", "w1", "op1", "conversation.json")
	for _, dir := range []string{filepath.Dir(blob), filepath.Dir(pkg)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(blob, []byte("preserved evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, pkg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy.archive, "packages", "w1", "op1", "receipt.json"), []byte(`{"receipt_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	useHost(t, "mac-studio")
	loaded, err := LoadConfig(legacy.config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(loaded.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := MakeAdapter(loaded.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	if result := catalog.Ingest(adapter, nil); result.Error != nil || result.Conversations != 12 {
		t.Fatalf("legacy ingest: %+v", result)
	}
	rewindToSchema4(t, catalog.DB)
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}
	return legacy
}

// rewindToSchema4 gives a catalog the shape it had before hosts, keeping its
// source state.
func rewindToSchema4(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE source_states_old (source_name TEXT PRIMARY KEY, kind TEXT NOT NULL, capability TEXT NOT NULL, cursor TEXT,
			fingerprint TEXT, coverage TEXT NOT NULL, last_attempt_at TEXT, last_success_at TEXT, error TEXT,
			pending_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`,
		`INSERT INTO source_states_old SELECT source_name,kind,capability,cursor,fingerprint,coverage,last_attempt_at,last_success_at,error,pending_count,updated_at FROM source_states`,
		`CREATE TABLE source_record_states_old (source_name TEXT NOT NULL, source_kind TEXT NOT NULL, source_account TEXT NOT NULL,
			source_id TEXT NOT NULL, digest TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY(source_name,source_kind,source_account,source_id))`,
		`INSERT INTO source_record_states_old SELECT source_name,source_kind,source_account,source_id,digest,updated_at FROM source_record_states`,
		"DROP TABLE source_states", "DROP TABLE source_record_states", "DROP TABLE workspace_sightings",
		"DROP TABLE conversation_sightings", "DROP TABLE hosts", "ALTER TABLE conversations DROP COLUMN origin_host_id",
		"ALTER TABLE source_states_old RENAME TO source_states", "ALTER TABLE source_record_states_old RENAME TO source_record_states",
		"DELETE FROM meta WHERE key IN ('legacy_host_id','host_sightings_version','initialized_build')",
		"UPDATE meta SET value='4' WHERE key='schema_version'",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// openService opens the catalog the way a running legacy service holds it:
// one pooled connection that never checkpoints, so commits stay in the WAL.
func openService(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// commitRecord commits one conversation with its messages and FTS rows in a
// single transaction, as an ingest does.
func commitRecord(db *sql.DB, tag string, messages int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	conversation := "conv-" + tag
	if _, err := tx.Exec(`INSERT INTO conversations(id,workspace_id,provider,account,native_id,origin)
		SELECT ?,id,'claude','work',?,'/late/'||? FROM workspaces LIMIT 1`, conversation, tag, tag); err != nil {
		return err
	}
	for index := range messages {
		native := fmt.Sprintf("msg-%s-%d", tag, index)
		id := stableID("message", conversation, native)
		text := fmt.Sprintf("word%s number %d", tag, index)
		if _, err := tx.Exec(`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES(?,?,?,?,?,?,?)`,
			id, conversation, native, "user", "message", text, hashBytes([]byte(text))); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO messages_fts(message_id,text) VALUES(?,?)", id, text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type catalogCounts struct{ conversations, messages, fts int }

func countCatalog(t *testing.T, db *sql.DB) catalogCounts {
	t.Helper()
	var counts catalogCounts
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM conversations),(SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM messages_fts)").
		Scan(&counts.conversations, &counts.messages, &counts.fts); err != nil {
		t.Fatal(err)
	}
	return counts
}

type fileState struct {
	info   os.FileInfo
	digest [32]byte
}

func stateOf(t *testing.T, path string) fileState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileState{info, sha256.Sum256(data)}
}

func sourceSchema(t *testing.T, path string) (string, int) {
	t.Helper()
	db, err := openReadOnlySQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	var hosts int
	if err := db.QueryRow("SELECT (SELECT value FROM meta WHERE key='schema_version'),(SELECT COUNT(*) FROM sqlite_master WHERE name='hosts')").Scan(&version, &hosts); err != nil {
		t.Fatal(err)
	}
	return version, hosts
}

func withAdoptTuning(t *testing.T, chunk int64) {
	t.Helper()
	previousChunk, previousMargin, previousCheck := adoptCopyChunk, adoptMinMargin, adoptHostCheck
	adoptCopyChunk, adoptMinMargin = chunk, 1<<20
	// useHost stands in for this Mac's hardware-derived ID.
	adoptHostCheck = func(Host) error { return nil }
	t.Cleanup(func() {
		adoptCopyChunk, adoptMinMargin, adoptFault, adoptHostCheck = previousChunk, previousMargin, nil, previousCheck
	})
}

// treeListing lists every path under root, to show that a refused or failed
// adoption created nothing.
func treeListing(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestAdoptCopiesARunningInstallAsOneSnapshot(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 16<<10)
	service := openService(t, legacy.catalog)
	// Committed but not checkpointed: only in the WAL.
	if err := commitRecord(service, "walonly", 3); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(legacy.catalog + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the legacy WAL holds nothing: %v", err)
	}
	expected := countCatalog(t, service)
	before := stateOf(t, legacy.catalog)
	steps := 0
	adoptFault = func(stage string) error {
		if stage == "copy" {
			if steps++; steps == 1 {
				// The running service commits while the copy is under way.
				return commitRecord(service, "duringcopy", 3)
			}
		}
		return nil
	}
	dir := filepath.Join(legacy.root, "Euclid", "Pharos")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	useHost(t, "mac-studio")
	result, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity, Progress: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if steps < 10 {
		t.Fatalf("the copy took %d steps; the test needs a chunked copy", steps)
	}

	// The source: same file, still schema 4, never migrated; the service's
	// commit during the copy reached it.
	if after, err := os.Stat(legacy.catalog); err != nil || !os.SameFile(before.info, after) {
		t.Fatalf("the legacy catalog was replaced: %v", err)
	}
	if version, hosts := sourceSchema(t, legacy.catalog); version != "4" || hosts != 0 {
		t.Fatalf("the legacy catalog was migrated: schema %s, hosts table %d", version, hosts)
	}
	if got := countCatalog(t, service); got.conversations != expected.conversations+1 {
		t.Fatalf("the service's commit is missing from the source: %+v", got)
	}

	// The copy: exactly the snapshot, migrated, searchable.
	config := result.Config
	if !config.Library || config.VolumeID != adoptVolume || config.Port != libraryPort || config.EnableReclamation ||
		config.CatalogPath != filepath.Join(dir, "catalog", "catalog.sqlite3") || config.ArchiveRoot != filepath.Join(dir, "preserved") {
		t.Fatalf("unexpected library config: %+v", config)
	}
	if err := config.checkLibrary(adoptIdentity); err != nil {
		t.Fatalf("the adopted library fails its guards: %v", err)
	}
	if err := config.checkLibrary(func(string) string { return "uuid:CLONE" }); err == nil {
		t.Fatal("the adopted library is not pinned to its volume")
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := countCatalog(t, catalog.DB); got != expected {
		t.Fatalf("copy counts %+v, want the snapshot's %+v", got, expected)
	}
	var version, legacyHost string
	var rehosted, sightings, during int
	if err := catalog.DB.QueryRow(`SELECT (SELECT value FROM meta WHERE key='schema_version'),(SELECT value FROM meta WHERE key='legacy_host_id'),
		(SELECT COUNT(*) FROM source_states WHERE host_id='mac-studio' AND source_name='claude'),(SELECT COUNT(*) FROM conversation_sightings WHERE host_id='mac-studio'),
		(SELECT COUNT(*) FROM messages WHERE native_id LIKE 'msg-duringcopy-%')`).Scan(&version, &legacyHost, &rehosted, &sightings, &during); err != nil {
		t.Fatal(err)
	}
	if version != strconv.Itoa(catalogSchemaVersion) || legacyHost != "mac-studio" || rehosted != 1 || sightings != expected.conversations || during != 0 {
		t.Fatalf("copy not migrated as expected: schema %s, legacy host %q, rehosted %d, sightings %d, during-copy messages %d", version, legacyHost, rehosted, sightings, during)
	}
	for query, want := range map[string]int{"wordwalonly": 1, "session7": 1, "wordduringcopy": 0} {
		found, err := catalog.Search(SearchOptions{Query: query, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if items := found["items"].([]map[string]any); len(items) != want {
			t.Errorf("search %q found %d workspaces in the copy, want %d", query, len(items), want)
		}
	}
	for _, leftover := range []string{"-wal", "-shm", "-journal"} {
		if info, err := os.Stat(config.CatalogPath + adoptPartialSuffix + leftover); err == nil {
			t.Errorf("left %s behind (%d bytes)", info.Name(), info.Size())
		}
	}
	if _, err := os.Stat(config.CatalogPath + adoptPartialSuffix); err == nil {
		t.Error("left the partial copy behind")
	}

	// library.toml: init-library's, with the install's retention settings.
	text, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"library = true", `volume_id = "uuid:ADOPT-TEST"`, "port = 8766", "package_cap_bytes = 50000000",
		"upcoming_days = 20", "eligible_days = 30", "snooze_days = 7", "enable_reclamation = false", "release_hook_proven = false"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("library.toml lacks %q", want)
		}
	}
	for _, unwanted := range []string{"legacy-token", "ghp_test", "18793", "[[sources]]", legacy.root} {
		if strings.Contains(string(text), unwanted) {
			t.Errorf("library.toml carries over %q", unwanted)
		}
	}
	if len(result.Notes) != 2 || !strings.HasPrefix(result.Notes[0], "enable_reclamation") || !strings.HasPrefix(result.Notes[1], "github_token") {
		t.Errorf("notes: %q", result.Notes)
	}

	// The host file keeps the sources as written; only the relative path moves.
	hostText, err := os.ReadFile(result.HostFile)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`
[[sources]]
name = "claude"
kind = "claude"
path = %q
account = "work"

[[sources]]
name = "codex"
kind = "codex"
path = "~/.codex-adopt-test"
enabled = false

[[sources]]
name = "export"
kind = "canonical"
path = %q
enabled = false
include_archived = true
`, filepath.Join(legacy.root, "home", ".claude", "projects"), filepath.Join(legacy.root, "legacy", "exports", "canonical.json"))
	if !strings.HasSuffix(string(hostText), want) || !strings.Contains(string(hostText), "# Host ID: mac-studio") {
		t.Fatalf("host file:\n%s\nwant it to end with:\n%s", hostText, want)
	}
	if result.HostFile != filepath.Join(dir, "hosts", "mac-studio.toml") || strings.Join(result.Sources, ",") != "claude,codex,export" {
		t.Fatalf("host file %s, sources %q", result.HostFile, result.Sources)
	}
	sources := map[string]SourceConfig{}
	for _, source := range config.Sources {
		sources[source.Name] = source
	}
	if claude := sources["claude"]; claude.Account != "work" || !claude.Enabled || claude.File != result.HostFile {
		t.Errorf("claude source: %+v", claude)
	}
	home, _ := os.UserHomeDir()
	if codex := sources["codex"]; codex.Enabled || codex.Path != filepath.Join(home, ".codex-adopt-test") {
		t.Errorf("codex source: %+v", codex)
	}
	if export := sources["export"]; export.Options["include_archived"] != true {
		t.Errorf("export source: %+v", export)
	}

	// Preserved packages: copied, hard links kept.
	blob, err := os.Stat(filepath.Join(dir, "preserved", "blobs", "sha256", "ab", "abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := os.Stat(filepath.Join(dir, "preserved", "packages", "w1", "op1", "conversation.json"))
	if err != nil || !os.SameFile(blob, pkg) || blob.Size() != int64(len("preserved evidence")) {
		t.Fatalf("preserved package not copied as a link to its blob: %v", err)
	}
	if preserved := result.Preserved; preserved.Files != 3 || preserved.Copied != 3 || !preserved.SameVolume {
		t.Fatalf("preserved: %+v", preserved)
	}
	if _, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second adoption into the library was not refused: %v", err)
	}
	catalog.Close()

	// The owner of the legacy rows is settled: another Mac opening the library
	// first cannot claim them.
	if result.LegacyHost != "mac-studio" {
		t.Fatalf("legacy host %q", result.LegacyHost)
	}
	useHost(t, "macbook")
	other, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var owner string
	var foreign int
	if err := other.DB.QueryRow("SELECT (SELECT value FROM meta WHERE key='legacy_host_id'),(SELECT COUNT(*) FROM source_states WHERE host_id<>'mac-studio')").Scan(&owner, &foreign); err != nil || owner != "mac-studio" || foreign != 0 {
		t.Fatalf("another Mac's open changed ownership: owner %q, foreign states %d, %v", owner, foreign, err)
	}
}

// A per-user catalog that host-aware code already opened keeps the owner it
// recorded.
func TestAdoptKeepsARecordedLegacyHost(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	useHost(t, "old-mac")
	catalog, err := OpenCatalog(legacy.catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalog.Close()
	useHost(t, "mac-studio")
	result, err := AdoptLibrary(context.Background(), filepath.Join(legacy.root, "Pharos"), legacy.config, AdoptOptions{Identity: adoptIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if result.LegacyHost != "old-mac" || !strings.Contains(strings.Join(result.Notes, "\n"), "already belonged to host old-mac") {
		t.Fatalf("legacy host %q, notes %q", result.LegacyHost, result.Notes)
	}
}

// A service that died left its last commits in the WAL, and nothing holds the
// catalog open: the copy must still include them, and reading must not
// checkpoint them into the source.
func TestAdoptIncludesTheWALOfAStoppedServiceAndLeavesItAlone(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	service := openService(t, legacy.catalog)
	if err := commitRecord(service, "crash", 4); err != nil {
		t.Fatal(err)
	}
	// A file copy of an idle database with its WAL is what a crash leaves.
	crashed := filepath.Join(legacy.root, "crashed")
	if err := os.MkdirAll(filepath.Join(crashed, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(legacy.catalog + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(crashed, "data", "catalog.sqlite3"+suffix), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected := countCatalog(t, service)
	config := filepath.Join(crashed, "archive.toml")
	if err := os.WriteFile(config, []byte("data_dir = \"data\"\narchive_root = \"missing\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(crashed, "data", "catalog.sqlite3")
	before, wal := stateOf(t, source), stateOf(t, source+"-wal")
	useHost(t, "mac-studio")
	result, err := AdoptLibrary(context.Background(), filepath.Join(legacy.root, "Pharos"), config, AdoptOptions{Identity: adoptIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if after, afterWAL := stateOf(t, source), stateOf(t, source+"-wal"); after.digest != before.digest || afterWAL.digest != wal.digest ||
		!os.SameFile(before.info, after.info) || !after.info.ModTime().Equal(before.info.ModTime()) {
		t.Fatal("reading the stopped service's catalog changed its database or WAL")
	}
	catalog, err := OpenCatalog(result.Config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if got := countCatalog(t, catalog.DB); got != expected {
		t.Fatalf("copy counts %+v, want %+v including the WAL", got, expected)
	}
	if !strings.Contains(result.Preserved.Note, "not available") || len(result.Sources) != 0 {
		t.Fatalf("preserved %+v, sources %q", result.Preserved, result.Sources)
	}
}

// Every record the service commits while the copy runs is either wholly in
// the copy or wholly absent.
func TestAdoptCopyIsTransactionallyConsistentUnderConcurrentCommits(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 8<<10)
	service := openService(t, legacy.catalog)
	var stop atomic.Bool
	var committed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for index := 0; !stop.Load(); index++ {
			if err := commitRecord(service, fmt.Sprintf("c%d", index), 5); err != nil {
				t.Error(err)
				return
			}
			committed.Add(1)
		}
	}()
	for committed.Load() < 3 {
		runtime.Gosched()
	}
	var during int64
	adoptFault = func(stage string) error {
		if stage == "copy" && during == 0 {
			// Hold the copy until the service has committed more records.
			during = committed.Load() + 3
			for committed.Load() < during {
				runtime.Gosched()
			}
		}
		return nil
	}
	useHost(t, "mac-studio")
	result, err := AdoptLibrary(context.Background(), filepath.Join(legacy.root, "Pharos"), legacy.config, AdoptOptions{Identity: adoptIdentity})
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(result.Config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	rows, err := queryMaps(catalog.DB, `SELECT c.id,(SELECT COUNT(*) FROM messages m WHERE m.conversation_id=c.id) messages,
		(SELECT COUNT(*) FROM messages_fts f WHERE f.message_id IN (SELECT id FROM messages m WHERE m.conversation_id=c.id)) fts
		FROM conversations c WHERE c.id LIKE 'conv-c%'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if integer(row["messages"]) != 5 || integer(row["fts"]) != 5 {
			t.Fatalf("a torn record reached the copy: %v", row)
		}
	}
	if len(rows) == 0 || int64(len(rows)) >= committed.Load() {
		t.Fatalf("copy has %d of %d records committed during the adoption; want a proper snapshot", len(rows), committed.Load())
	}
	t.Logf("%d of %d concurrent records are in the copy", len(rows), committed.Load())
}

func TestAdoptFailuresLeaveNoLibrary(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 16<<10)
	useHost(t, "mac-studio")
	before := stateOf(t, legacy.catalog)
	dir := filepath.Join(legacy.root, "Pharos")
	cancelled, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, stage := range []string{"space", "copy", "cancel", "migrate", "check", "rename", "commit", "verify"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			adoptFault = func(at string) error {
				if at == stage {
					return errors.New("injected " + stage)
				}
				return nil
			}
			switch stage {
			case "space":
				adoptMinMargin = 1 << 60
				defer func() { adoptMinMargin = 1 << 20 }()
			case "cancel":
				ctx = cancelled
				adoptFault = func(at string) error {
					cancel()
					return nil
				}
			}
			_, err := AdoptLibrary(ctx, dir, legacy.config, AdoptOptions{Identity: adoptIdentity})
			if err == nil {
				t.Fatal("adoption did not fail")
			}
			// Everything it created is gone, down to the directory itself.
			if left := treeListing(t, dir); len(left) != 0 {
				t.Errorf("after %v these were left behind: %q", err, left)
			}
			if stage == "space" && !strings.Contains(err.Error(), "adopting needs") {
				t.Errorf("space error: %v", err)
			}
		})
	}
	adoptFault = nil
	after := stateOf(t, legacy.catalog)
	if after.digest != before.digest || !os.SameFile(before.info, after.info) {
		t.Fatal("a failed adoption changed the legacy catalog")
	}
	if _, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity}); err != nil {
		t.Fatalf("retrying after the failures: %v", err)
	}
}

func TestAdoptRefusesUnsuitableInputs(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	useHost(t, "mac-studio")
	adopt := func(dir, config string) error {
		_, err := AdoptLibrary(context.Background(), dir, config, AdoptOptions{Identity: adoptIdentity})
		return err
	}
	existing := filepath.Join(legacy.root, "existing")
	if _, err := InitLibrary(existing, adoptIdentity); err != nil {
		t.Fatal(err)
	}
	libraryText, _ := os.ReadFile(filepath.Join(existing, "library.toml"))
	if err := adopt(existing, legacy.config); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("adopting into an existing library: %v", err)
	}
	if again, _ := os.ReadFile(filepath.Join(existing, "library.toml")); string(again) != string(libraryText) {
		t.Error("a refused adoption changed library.toml")
	}
	if err := adopt(filepath.Join(legacy.root, "Pharos"), filepath.Join(existing, "library.toml")); err == nil || !strings.Contains(err.Error(), "already a portable library") {
		t.Errorf("adopting a library: %v", err)
	}
	if err := adopt(filepath.Join(legacy.root, "unmounted", "Pharos"), legacy.config); err == nil || !strings.Contains(err.Error(), "is its drive mounted") {
		t.Errorf("adopting under a missing mount point: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy.root, "unmounted")); err == nil {
		t.Error("a missing mount point was created")
	}
	empty := filepath.Join(legacy.root, "empty.toml")
	if err := os.WriteFile(empty, []byte("data_dir = \"nothing\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := adopt(filepath.Join(legacy.root, "Pharos"), empty); err == nil || !strings.Contains(err.Error(), "has no catalog") {
		t.Errorf("adopting an install without a catalog: %v", err)
	}
	// A catalog from a newer build is refused before anything is copied.
	db := openService(t, legacy.catalog)
	if _, err := db.Exec("UPDATE meta SET value='99' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	if err := adopt(filepath.Join(legacy.root, "Pharos"), legacy.config); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Errorf("adopting a newer catalog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy.root, "Pharos", "library.toml")); err == nil {
		t.Error("a refused adoption wrote library.toml")
	}
}

func TestInitLibraryAdoptCommandLine(t *testing.T) {
	for _, args := range [][]string{{"--adopt"}, {"dir"}, {"--adopt", "x"}, {"dir", "--adopt", "x", "extra"}, {"dir", "--adopt", "x", "--bogus"}} {
		if !strings.Contains(fmt.Sprint(runAdoptCLI(args)), "usage:") {
			t.Errorf("%q was not rejected with usage", args)
		}
	}
}

// A leftover -wal (or -shm, -journal) beside the catalog's final name would be
// replayed onto the new copy, so adopting refuses it, before and at the
// rename, and leaves it alone.
func TestAdoptRefusesLeftoverCatalogFiles(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	useHost(t, "mac-studio")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		dir := filepath.Join(legacy.root, "Pharos"+suffix)
		leftover := filepath.Join(dir, "catalog", "catalog.sqlite3"+suffix)
		if err := os.MkdirAll(filepath.Dir(leftover), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(leftover, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := treeListing(t, dir)
		_, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity})
		if err == nil || !strings.Contains(err.Error(), leftover+" already exists") {
			t.Fatalf("a leftover %s was not refused: %v", suffix, err)
		}
		if after := treeListing(t, dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Fatalf("a refused adoption changed %s: %q", dir, after)
		}
		if data, _ := os.ReadFile(leftover); string(data) != "stale" {
			t.Fatal("the leftover was changed")
		}
	}
	// One that appears while adopting is refused at the rename.
	dir := filepath.Join(legacy.root, "Pharos")
	planted := filepath.Join(dir, "catalog", "catalog.sqlite3-wal")
	adoptFault = func(stage string) error {
		if stage == "rename" {
			return os.WriteFile(planted, []byte("stale"), 0o644)
		}
		return nil
	}
	_, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity})
	if err == nil || !strings.Contains(err.Error(), planted+" already exists") {
		t.Fatalf("a -wal that appeared while adopting was not refused: %v", err)
	}
	if left := treeListing(t, dir); strings.Join(left, "\n") != strings.Join([]string{dir, filepath.Dir(planted), planted}, "\n") {
		t.Fatalf("left behind: %q", left)
	}
}

// A copy that fails quick_check (or integrity_check) is never adopted. Here a
// stale WAL from another database is replayed onto it, as a leftover -wal
// beside the final catalog would be.
func TestAdoptFailsWhenTheCopyFailsItsCheck(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	other := newLegacyInstall(t)
	service := openService(t, other.catalog)
	for index := range 20 {
		if err := commitRecord(service, fmt.Sprintf("stale%d", index), 20); err != nil {
			t.Fatal(err)
		}
	}
	stale, err := os.ReadFile(other.catalog + "-wal")
	if err != nil || len(stale) == 0 {
		t.Fatalf("no stale WAL: %v", err)
	}
	useHost(t, "mac-studio")
	dir := filepath.Join(legacy.root, "Pharos")
	for _, full := range []bool{false, true} {
		adoptFault = func(stage string) error {
			if stage == "check" {
				return os.WriteFile(filepath.Join(dir, "catalog", "catalog.sqlite3"+adoptPartialSuffix+"-wal"), stale, 0o644)
			}
			return nil
		}
		_, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity, FullCheck: full})
		check := map[bool]string{false: "quick_check", true: "integrity_check"}[full]
		if err == nil || !strings.Contains(err.Error(), check) {
			t.Fatalf("a copy failing %s was adopted: %v", check, err)
		}
		t.Logf("%s: %v", check, err)
		if left := treeListing(t, dir); len(left) != 0 {
			t.Fatalf("left behind: %q", left)
		}
	}
}

// archive_root must neither contain the library nor lie inside it, however the
// two paths are spelled; otherwise the copy recurses into itself.
func TestAdoptRefusesNestedArchiveRoots(t *testing.T) {
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	useHost(t, "mac-studio")
	root := legacy.root
	link := filepath.Join(root, "archive-link")
	if err := os.Symlink(legacy.archive, link); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(filepath.Dir(legacy.archive), strings.ToUpper(filepath.Base(legacy.archive)))
	caseInsensitive := false
	if info, err := os.Stat(upper); err == nil {
		original, _ := os.Stat(legacy.archive)
		caseInsensitive = os.SameFile(info, original)
	}
	withArchive := func(t *testing.T, archive string) string {
		t.Helper()
		text, err := os.ReadFile(legacy.config)
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(root, "legacy", "nested-"+strings.ReplaceAll(t.Name(), "/", "_")+".toml")
		replaced := strings.Replace(string(text), `archive_root = "archive"`, fmt.Sprintf("archive_root = %q", archive), 1)
		if err := os.WriteFile(config, []byte(replaced), 0o600); err != nil {
			t.Fatal(err)
		}
		return config
	}
	cases := map[string][2]string{
		"library is archive_root":        {legacy.archive, legacy.archive},
		"library inside archive_root":    {legacy.archive, filepath.Join(legacy.archive, "Pharos")},
		"through a symlinked segment":    {legacy.archive, filepath.Join(link, "Pharos")},
		"symlinked archive_root":         {link, filepath.Join(legacy.archive, "packages", "Pharos")},
		"with a trailing slash":          {legacy.archive + "/", filepath.Join(legacy.archive, "blobs") + "/"},
		"archive_root inside library":    {filepath.Join(root, "Lib", "old"), filepath.Join(root, "Lib")},
		"archive_root inside preserved/": {filepath.Join(root, "Lib2", "preserved", "x"), filepath.Join(root, "Lib2")},
	}
	if caseInsensitive {
		cases["in another case"] = [2]string{legacy.archive, filepath.Join(upper, "Pharos")}
	}
	for _, dir := range []string{filepath.Join(root, "Lib", "old"), filepath.Join(root, "Lib2", "preserved", "x")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			before := treeListing(t, root)
			_, err := AdoptLibrary(context.Background(), paths[1], withArchive(t, paths[0]), AdoptOptions{Identity: adoptIdentity})
			if err == nil || !strings.Contains(err.Error(), "contain one another") {
				t.Fatalf("adopting %s with archive_root %s: %v", paths[1], paths[0], err)
			}
			after := treeListing(t, root)
			if len(after) != len(before)+1 { // only the test's config
				t.Fatalf("a refused adoption created %d paths", len(after)-len(before)-1)
			}
		})
	}
	// The library's own preserved/ needs no copy, and a symlinked archive_root
	// is copied from where it points.
	inPlace := filepath.Join(root, "Lib3")
	if err := os.MkdirAll(filepath.Join(inPlace, "preserved"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := AdoptLibrary(context.Background(), inPlace, withArchive(t, filepath.Join(inPlace, "preserved")), AdoptOptions{Identity: adoptIdentity})
	if err != nil || !strings.Contains(result.Preserved.Note, "already is the library's preserved/") {
		t.Fatalf("archive_root in place: %+v %v", result.Preserved, err)
	}
	result, err = AdoptLibrary(context.Background(), filepath.Join(root, "Lib4"), withArchive(t, link), AdoptOptions{Identity: adoptIdentity})
	if err != nil || result.Preserved.Files != 3 || result.Preserved.Real != legacy.archive || result.Preserved.Root != link {
		t.Fatalf("symlinked archive_root: %+v %v", result.Preserved, err)
	}
	var summary strings.Builder
	printAdoptResult(&summary, result)
	if !strings.Contains(summary.String(), "3 files") || !strings.Contains(summary.String(), link+" resolves to "+legacy.archive) {
		t.Fatalf("summary:\n%s", summary.String())
	}
}

// Files kept from an interrupted attempt are linked like the rest: a retry
// neither copies a linked file separately nor leaves separate copies apart.
func TestCopyPreservedRestoresHardLinksOnRetry(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "archive"), filepath.Join(root, "Pharos", "preserved")
	names := []string{"blobs/ab/abcdef", "packages/a/conversation.json", "packages/b/conversation.json", "packages/c/conversation.json"}
	for index, name := range names {
		path := filepath.Join(from, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Link(filepath.Join(from, names[0]), path); err != nil {
			t.Fatal(err)
		}
	}
	// An interrupted attempt copied the blob and one package separately.
	for _, name := range names[:2] {
		path := filepath.Join(to, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := planPreserved(from, filepath.Dir(to))
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	if err := copyPreserved(context.Background(), &plan, to, &created); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(filepath.Join(to, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names[1:] {
		if info, err := os.Stat(filepath.Join(to, name)); err != nil || !os.SameFile(first, info) {
			t.Errorf("%s is not linked to the blob after the retry: %v", name, err)
		}
	}
	if plan.Files != 4 || plan.Copied != 3 {
		t.Fatalf("plan after retry: %+v", plan)
	}
}

func TestAdoptRefusesAFallbackHostID(t *testing.T) {
	hardware := func() (string, error) { return "HW-UUID", nil }
	broken := func() (string, error) { return "", errors.New("ioreg timed out") }
	real := Host{ID: stableID("host", "HW-UUID", "gbw"), User: "gbw"}
	fallback := Host{ID: stableID("host", "hostname:mac.local", "gbw"), User: "gbw"}
	for name, test := range map[string]struct {
		host     Host
		explicit string
		platform func() (string, error)
		refused  bool
	}{
		"hardware ID":             {real, "", hardware, false},
		"hostname fallback":       {fallback, "", hardware, true},
		"ioreg not answering":     {real, "", broken, true},
		"explicit PHAROS_HOST_ID": {Host{ID: "chosen", User: "gbw"}, " chosen ", broken, false},
		"explicit, but not used":  {fallback, "chosen", broken, true},
	} {
		if err := checkAdoptHost(test.host, test.explicit, test.platform); (err != nil) != test.refused {
			t.Errorf("%s: %v", name, err)
		}
	}
	legacy := newLegacyInstall(t)
	withAdoptTuning(t, 64<<20)
	adoptHostCheck = func(host Host) error { return checkAdoptHost(host, "", broken) }
	dir := filepath.Join(legacy.root, "Pharos")
	if _, err := AdoptLibrary(context.Background(), dir, legacy.config, AdoptOptions{Identity: adoptIdentity}); err == nil || !strings.Contains(err.Error(), "PHAROS_HOST_ID") {
		t.Fatalf("a fallback host ID was not refused: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a refused adoption created the library directory")
	}
}
