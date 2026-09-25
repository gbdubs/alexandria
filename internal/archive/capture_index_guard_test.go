package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Indexing a capture must never delete, overwrite or roll back what its host
// already wrote from a newer read of the source, whatever the source's state,
// part states, extractor version, or an interrupted sync. Several of these
// reproduce an independent review's findings.

func canonicalExport(t *testing.T, file, title string, messages ...string) {
	t.Helper()
	rows := []map[string]string{}
	for index, text := range messages {
		rows = append(rows, map[string]string{"id": string(rune('a' + index)), "role": "user", "text": text})
	}
	data, _ := json.Marshal(map[string]any{"workspaces": []any{map[string]any{"id": "work", "title": title,
		"conversations": []any{map[string]any{"id": "thread", "messages": rows}}}}})
	writeSourceFile(t, file, string(data))
}

func workspaceTitle(t *testing.T, catalog *Catalog, sourceID string) string {
	t.Helper()
	var title string
	_ = catalog.DB.QueryRow("SELECT title FROM workspaces WHERE source_id=?", sourceID).Scan(&title)
	return title
}

func TestOlderWholeSourceCaptureAfterAFailedOrInterruptedSync(t *testing.T) {
	for _, later := range []string{"failed", "removed", "interrupted"} {
		t.Run(later, func(t *testing.T) {
			useHost(t, "host-a")
			catalog, _ := testCatalog(t)
			export := filepath.Join(t.TempDir(), "export.json")
			source := SourceConfig{Name: "export", Kind: "canonical", Path: export, Account: "local", Enabled: true}
			config := captureTestConfig(t, source)
			canonicalExport(t, export, "Captured", "hello")
			runCapture(t, config)
			canonicalExport(t, export, "Synced later", "hello", "only in the catalog")
			ingestSource(t, catalog, source)
			adapter, _ := MakeAdapter(source)
			switch later {
			case "failed":
				writeSourceFile(t, export, `{"workspaces":[`)
				if result := catalog.Ingest(adapter, nil); result.Error == nil {
					t.Fatal("the broken export was ingested")
				}
			case "removed":
				os.Remove(export)
				_ = catalog.Ingest(adapter, nil)
			case "interrupted":
				canonicalExport(t, export, "Synced later", "hello", "only in the catalog", "newest")
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_ = catalog.IngestContext(ctx, adapter, nil)
			}
			result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]
			if result.Older != 1 || countRows(t, catalog, "SELECT COUNT(*) FROM messages WHERE text='only in the catalog'") != 1 ||
				workspaceTitle(t, catalog, "work") != "Synced later" {
				t.Fatalf("an older capture rolled the catalog back: %#v, title %q", result, workspaceTitle(t, catalog, "work"))
			}
		})
	}
}

func TestOlderTranscriptCaptureWithoutPartStateOrAfterAnExtractorChange(t *testing.T) {
	for _, change := range []string{"no part states", "extractor changed", "renamed source"} {
		t.Run(change, func(t *testing.T) {
			useHost(t, "host-a")
			catalog, _ := testCatalog(t)
			claude := filepath.Join(t.TempDir(), "claude")
			file := filepath.Join(claude, "-proj", "s1.jsonl")
			writeSourceFile(t, file, claudeLine("s1", "u1", "one", "/proj", 1))
			source := SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}
			config := captureTestConfig(t, source)
			runCapture(t, config)
			appendSourceFile(t, file, claudeLine("s1", "u2", "two", "/proj", 2))
			live := source
			if change == "renamed source" {
				// The capture's source name is "claude"; the live one differs.
				live.Name = "claude-renamed"
			}
			ingestSource(t, catalog, live)
			switch change {
			case "no part states":
				catalog.DB.Exec("DELETE FROM source_item_states")
			case "extractor changed":
				catalog.DB.Exec("UPDATE source_item_states SET extractor='claude:message-model-v6'")
			}
			if result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]; result.Older != 1 || conversationMessages(t, catalog, "s1") != 2 {
				t.Fatalf("an older capture rolled the session back: %#v, %d messages", result, conversationMessages(t, catalog, "s1"))
			}
		})
	}
}

func TestOlderConductorCaptureLeavesNewerSessionsAlone(t *testing.T) {
	for _, change := range []string{"renamed", "message finished in place", "message deleted and another added"} {
		t.Run(change, func(t *testing.T) {
			useHost(t, "host-a")
			catalog, _ := testCatalog(t)
			dir := filepath.Join(t.TempDir(), "conductor")
			exec := conductorFixture(t, dir)
			exec(addConductorSession("s1", 3)...)
			source := SourceConfig{Name: "conductor", Kind: "conductor", Path: dir, Account: "local", Enabled: true}
			config := captureTestConfig(t, source)
			runCapture(t, config)
			switch change {
			case "renamed":
				exec("UPDATE sessions SET title='Renamed later' WHERE id='s1'")
			case "message finished in place":
				exec("UPDATE session_messages SET content='final full answer' WHERE id='s1-m1'")
			case "message deleted and another added":
				exec("DELETE FROM session_messages WHERE id='s1-m1'",
					"INSERT INTO session_messages VALUES('s1-m9','s1','assistant','newest reply','2026-09-01T02:00:00Z','2026-09-01T02:00:00Z',NULL)")
			}
			ingestSource(t, catalog, source)
			want := map[string]string{"renamed": "SELECT COUNT(*) FROM workspaces WHERE title='Renamed later'",
				"message finished in place":         "SELECT COUNT(*) FROM messages WHERE text='final full answer'",
				"message deleted and another added": "SELECT COUNT(*) FROM messages WHERE text='newest reply'"}[change]
			if countRows(t, catalog, want) != 1 {
				t.Fatal("setup: the live sync missed the change")
			}
			// Part states alone would pass the snapshot over; without them the
			// record guard must hold on its own.
			catalog.DB.Exec("DELETE FROM source_item_states")
			result := indexCaptures(t, catalog, config.CaptureRoot, "host-a")[0]
			if countRows(t, catalog, want) != 1 || result.Older != 1 {
				t.Fatalf("an older snapshot rolled the session back: %#v", result)
			}
		})
	}
}

// A resumed sync used to skip a changed Conductor session whose activity time
// had not moved and record its part as current, so the change was never
// ingested.
func TestResumedSyncStillIngestsChangedSessions(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	dir := filepath.Join(t.TempDir(), "conductor")
	exec := conductorFixture(t, dir)
	exec(addConductorSession("s1", 2)...)
	exec(addConductorSession("s2", 2)...)
	source := SourceConfig{Name: "conductor", Kind: "conductor", Path: dir, Account: "local", Enabled: true}
	ingestSource(t, catalog, source)
	exec("INSERT INTO session_messages VALUES('s1-m9','s1','assistant','the reply','2026-09-01T01:00:00Z','2026-09-01T01:00:00Z',NULL)")
	if _, err := catalog.DB.Exec("UPDATE source_states SET coverage='indexing' WHERE source_name='conductor'"); err != nil {
		t.Fatal(err)
	}
	ingestSource(t, catalog, source)
	ingestSource(t, catalog, source)
	if countRows(t, catalog, "SELECT COUNT(*) FROM messages WHERE text='the reply'") != 1 {
		t.Fatal("the reply was never ingested")
	}
}

// Another Mac indexes a capture without the capturing Mac's Git checkouts; the
// capturing Mac's own sync must still fill in what only they know.
func TestOwnSyncEnrichesWhatAnotherMacIndexed(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:example/checkout.git"}} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, output)
		}
	}
	useHost(t, "host-a")
	claude := filepath.Join(root, "claude")
	writeSourceFile(t, filepath.Join(claude, "-p", "s.jsonl"), claudeLine("s", "u1", "hi", repo, 1))
	source := SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true}
	config := captureTestConfig(t, source)
	runCapture(t, config)
	catalog, _ := testCatalog(t)
	useHost(t, "host-b")
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	remote := func() string {
		var value string
		_ = catalog.DB.QueryRow("SELECT COALESCE(r.canonical_remote,'') FROM workspaces w JOIN repositories r ON r.id=w.repository_id").Scan(&value)
		return value
	}
	if remote() != "" {
		t.Fatal("setup: another Mac looked up the checkout")
	}
	useHost(t, "host-a")
	if result := ingestSource(t, catalog, source); result.SkippedUnchanged || result.Parsed != 1 || remote() != "git@github.com:example/checkout.git" {
		t.Fatalf("the host's own sync left what another Mac indexed as it was: %#v, remote %q", result, remote())
	}
	// Once enriched, another index of the same capture elsewhere keeps it.
	useHost(t, "host-b")
	indexCaptures(t, catalog, config.CaptureRoot, "host-a")
	if remote() != "git@github.com:example/checkout.git" {
		t.Fatal("re-indexing elsewhere dropped the enrichment")
	}
}

func TestFailedIndexLeavesTheHostsSourceStateAlone(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	export := filepath.Join(t.TempDir(), "export.json")
	source := SourceConfig{Name: "export", Kind: "canonical", Path: export, Account: "local", Enabled: true}
	config := captureTestConfig(t, source)
	canonicalExport(t, export, "Fine", "hello")
	ingestSource(t, catalog, source)
	writeSourceFile(t, export, `{"workspaces":[`)
	runCapture(t, config)
	useHost(t, "host-b")
	targets, _ := captureTargets(config.CaptureRoot, []string{"host-a"}, false, nil)
	if result := catalog.IndexCapture(context.Background(), targets[0], nil); result.Error == nil {
		t.Fatal("a broken capture was indexed")
	}
	var coverage string
	var failure *string
	if err := catalog.DB.QueryRow("SELECT coverage,error FROM source_states WHERE host_id='host-a' AND source_name='export'").Scan(&coverage, &failure); err != nil || coverage != "complete" || failure != nil {
		t.Fatalf("the host's source state changed: %s %v %v", coverage, failure, err)
	}
}

func TestOnlyOneIndexOfACaptureRunsAtATime(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	claude := filepath.Join(t.TempDir(), "claude")
	writeSourceFile(t, filepath.Join(claude, "p", "s.jsonl"), claudeLine("s", "u1", "hi", "/p", 1))
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true})
	runCapture(t, config)
	targets, _ := captureTargets(config.CaptureRoot, nil, false, nil)
	lock, err := os.OpenFile(filepath.Join(targets[0].Dir, indexLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if result := catalog.IndexCapture(context.Background(), targets[0], nil); !strings.Contains(fmt.Sprint(result.Error), "already being indexed") {
		t.Fatalf("a second index ran: %#v", result)
	}
	lock.Close()
	if result := catalog.IndexCapture(context.Background(), targets[0], nil); result.Error != nil || result.Workspaces != 1 {
		t.Fatalf("index after the other finished: %#v", result)
	}
}

func TestManifestPathsCannotLeaveTheCapture(t *testing.T) {
	useHost(t, "host-a")
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	file := filepath.Join(claude, "p", "s.jsonl")
	writeSourceFile(t, file, claudeLine("s", "u1", "hi", "/p", 1))
	config := captureTestConfig(t, SourceConfig{Name: "claude", Kind: "claude", Path: claude, Account: "local", Enabled: true})
	runCapture(t, config)
	victim := filepath.Join(root, "victim.txt")
	writeSourceFile(t, victim, "keep me")
	dir := filepath.Join(config.CaptureRoot, "host-a", "claude")
	manifest := readCaptureManifest(t, config, "claude")
	// A rewrite of the source would move the "previous capture" into history.
	manifest.Files[0].Captured = "files/../../../../victim.txt"
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, captureManifestName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, file, claudeLine("s", "u9", "rewritten", "/p", 2))
	if summary := runCapture(t, config); summary.OK {
		t.Fatal("a capture used a manifest naming a path outside it")
	}
	catalog, _ := testCatalog(t)
	targets, _ := captureTargets(config.CaptureRoot, nil, false, nil)
	if result := catalog.IndexCapture(context.Background(), targets[0], nil); result.Error == nil {
		t.Fatal("an index used a manifest naming a path outside it")
	}
	if readText(t, victim) != "keep me" {
		t.Fatal("a file outside the capture was touched")
	}
	for _, bad := range []string{"history/../../x", "files/../files/../../x", "/etc/passwd", "files\\..\\x", "x/files/y"} {
		manifest := &captureManifest{Snapshots: []*capturedSnapshot{{Captured: "files/a.db", Generations: []capturedVersion{{Captured: bad}}}}}
		if manifest.unsafePath() == "" {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestIndexMarkerSkipsSnapshotsReplacedWhileIndexing(t *testing.T) {
	marker := &indexMarker{saved: map[string]bool{"/db\x1fold": true}, Snapshots: []indexedSnapshot{{"/db", "old"}}}
	before := &captureManifest{Snapshots: []*capturedSnapshot{{Path: "/db", CapturedAt: "one", Generations: []capturedVersion{{CapturedAt: "old"}}},
		{Path: "/other", CapturedAt: "x"}}}
	marker.add("/db", "one")
	marker.add("/other", "x")
	current := &captureManifest{Snapshots: []*capturedSnapshot{{Path: "/db", CapturedAt: "two", Generations: []capturedVersion{{CapturedAt: "one"}, {CapturedAt: "old"}}},
		{Path: "/other", CapturedAt: "x"}}}
	marker.keepUnchanged(before, current)
	if marker.has("/db", "one") || !marker.has("/db", "old") || !marker.has("/other", "x") {
		t.Fatalf("marker after a concurrent capture: %#v", marker.Snapshots)
	}
}

// guardCapture decides per conversation: older ones are left alone, newer
// ones written, and the workspace's own fields only by a copy no older anywhere.
func TestCaptureGuardWritesOnlyWhatIsNewer(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	record := func(title string, observed int64, first, second []string, firstAt, secondAt int64) WorkspaceRecord {
		conversation := func(id string, ids []string, at int64) ConversationRecord {
			messages := []MessageRecord{}
			for _, message := range ids {
				messages = append(messages, MessageRecord{NativeID: message, Role: "user", Kind: "message", Text: "text " + message, Selected: true})
			}
			return ConversationRecord{NativeID: id, Provider: "claude", Account: "local", Origin: "/src/" + id + ".jsonl", Messages: messages, Observed: at}
		}
		return WorkspaceRecord{SourceID: "w", SourceKind: "claude", Account: "local", Title: title, Observed: observed,
			Conversations: []ConversationRecord{conversation("c1", first, firstAt), conversation("c2", second, secondAt)}}
	}
	apply := func(value WorkspaceRecord, fromCapture bool) error {
		t.Helper()
		tx, err := catalog.beginWrite(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = ingestCopy(tx, value, false, "host-a", "claude", fromCapture)
		if err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	messages := func(id string) int { return conversationMessages(t, catalog, id) }
	if err := apply(record("Live", 10, []string{"a", "b"}, []string{"x"}, 10, 10), false); err != nil {
		t.Fatal(err)
	}
	if err := apply(record("Older", 5, []string{"a"}, []string{}, 5, 5), true); !errors.Is(err, errOlderCopy) {
		t.Fatalf("an older capture was applied: %v", err)
	}
	// c1 older, c2 newer: only c2 is written, and not the workspace's title.
	if err := apply(record("Mixed", 20, []string{"a"}, []string{"x", "y"}, 5, 20), true); err != nil {
		t.Fatal(err)
	}
	if messages("c1") != 2 || messages("c2") != 2 || workspaceTitle(t, catalog, "w") != "Live" {
		t.Fatalf("mixed capture: c1 %d, c2 %d, title %q", messages("c1"), messages("c2"), workspaceTitle(t, catalog, "w"))
	}
	// Newer in every part, a capture replaces and prunes like a live sync.
	if err := apply(record("Newer", 30, []string{"a", "c"}, []string{"y"}, 30, 30), true); err != nil {
		t.Fatal(err)
	}
	if messages("c1") != 2 || messages("c2") != 1 || workspaceTitle(t, catalog, "w") != "Newer" {
		t.Fatalf("newer capture: c1 %d, c2 %d, title %q", messages("c1"), messages("c2"), workspaceTitle(t, catalog, "w"))
	}
	// Rows from before versions were recorded fall back to their sightings.
	catalog.DB.Exec("DELETE FROM copy_versions")
	if err := apply(record("Stale", 40, []string{"a"}, []string{"y"}, 40, 40), true); !errors.Is(err, errOlderCopy) {
		t.Fatalf("a capture older than an unversioned write was applied: %v", err)
	}
}
