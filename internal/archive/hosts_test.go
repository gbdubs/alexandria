package archive

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// useHost simulates the library being attached to another Mac.
func useHost(t *testing.T, id string) {
	t.Helper()
	previous := currentHost
	currentHost = func() Host { return Host{ID: id, Label: "Mac " + id, User: "tester"} }
	t.Cleanup(func() { currentHost = previous })
}

func TestHostIDSurvivesAnIoregFailure(t *testing.T) {
	t.Setenv("PHAROS_HOST_ID", "")
	support := t.TempDir()
	var waited []time.Duration
	hardware := func(uuid string) func([]time.Duration) (string, error) {
		return func(waits []time.Duration) (string, error) { waited = waits; return uuid, nil }
	}
	failing := func(waits []time.Duration) (string, error) { waited = waits; return "", errors.New("ioreg timed out") }
	long, short := []time.Duration{5 * time.Second, 15 * time.Second}, []time.Duration{2 * time.Second}
	first := detectHost(support, hardware("UUID-A"), false)
	var record hostRecord
	if data, err := os.ReadFile(filepath.Join(support, "host.json")); err != nil || json.Unmarshal(data, &record) != nil ||
		record != (hostRecord{HardwareUUID: "UUID-A", User: first.User, ID: first.ID}) || first.Fallback {
		t.Fatalf("host.json %+v %v", record, err)
	}
	if !slices.Equal(waited, long) {
		t.Fatalf("with nothing recorded, ioreg got %v, want %v", waited, long)
	}
	// With a record to stand in, a slow ioreg is not waited out.
	if again := detectHost(support, failing, false); again.ID != first.ID || again.Fallback || !slices.Equal(waited, short) {
		t.Fatalf("host ID when ioreg failed: %+v after %v, was %s", again, waited, first.ID)
	}
	// Migration Assistant copied host.json to a new Mac: its hardware wins.
	moved := detectHost(support, hardware("UUID-B"), false)
	if moved.ID == first.ID || detectHost(support, failing, false).ID != moved.ID {
		t.Fatalf("new hardware kept the copied host ID %s", moved.ID)
	}
	// Nothing recorded, or recorded for another user: the hostname, marked as
	// a fallback. The MCP server tries ioreg only briefly.
	hostname, _ := os.Hostname()
	byHostname := stableID("host", "hostname:"+hostname, first.User)
	if got := detectHost(t.TempDir(), failing, false); got.ID != byHostname || !got.Fallback || !slices.Equal(waited, long) {
		t.Fatalf("without host.json: %+v after %v, want the fallback %s", got, waited, byHostname)
	}
	if got := detectHost(t.TempDir(), failing, true); got.ID != byHostname || !got.Fallback || !slices.Equal(waited, short) {
		t.Fatalf("quick detection: %+v after %v", got, waited)
	}
	other, _ := json.Marshal(hostRecord{HardwareUUID: "UUID-B", User: first.User + "-other", ID: "host_other"})
	if err := os.WriteFile(filepath.Join(support, "host.json"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectHost(support, failing, false); got.ID != byHostname || !got.Fallback {
		t.Fatalf("another user's host.json was used: %+v", got)
	}
	t.Setenv("PHAROS_HOST_ID", "host_pinned")
	if got := detectHost(support, hardware("UUID-C"), false); got.ID != "host_pinned" || got.Fallback {
		t.Fatalf("PHAROS_HOST_ID ignored: %+v", got)
	}
	if data, _ := os.ReadFile(filepath.Join(support, "host.json")); !bytes.Equal(data, other) {
		t.Fatalf("PHAROS_HOST_ID rewrote host.json: %s", data)
	}
	if _, err := os.Stat("/usr/sbin/ioreg"); err == nil {
		if uuid, err := platformUUID(short); err != nil || len(uuid) != 36 {
			t.Fatalf("platformUUID: %q %v", uuid, err)
		}
	}
}

// useFallbackHost simulates a Mac that ioreg could not name.
func useFallbackHost(t *testing.T) {
	t.Helper()
	previous := currentHost
	currentHost = func() Host { return Host{ID: "host_by_hostname", Label: "Mac", User: "tester", Fallback: true} }
	t.Cleanup(func() { currentHost = previous })
}

func TestNothingIsRecordedUnderAFallbackHostID(t *testing.T) {
	useFallbackHost(t)
	catalog, config := testCatalog(t)
	meta := func(key string) string {
		var value string
		_ = catalog.DB.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&value)
		return value
	}
	count := func(table string) (rows int) {
		if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if meta(legacyHostKey) != "" || count("hosts") != 0 {
		t.Fatalf("a new catalog recorded the fallback host: legacy %q, %d hosts", meta(legacyHostKey), count("hosts"))
	}
	source := newCopyFixture(t, sessionCopy("/Users/ann/.claude/projects/x/session.jsonl", "m1"))
	if result := catalog.Ingest(source, nil); !strings.Contains(fmt.Sprint(result.Error), "could not identify this Mac") || count("source_states") != 0 || count("workspaces") != 0 {
		t.Fatalf("ingest under a fallback ID: %v, %d source states", result.Error, count("source_states"))
	}
	if result := catalog.IngestExisting(source, nil); !strings.Contains(fmt.Sprint(result.Error), "could not identify this Mac") {
		t.Fatalf("repair under a fallback ID: %v", result.Error)
	}
	config.CaptureRoot = filepath.Join(t.TempDir(), "captures")
	if _, err := beginCapture(config); !errors.Is(err, errHostUnknown) {
		t.Fatalf("capture under a fallback ID: %v", err)
	}
	config.Library, config.Path = true, filepath.Join(t.TempDir(), "library.toml")
	if hostConfigPath(config) != "" {
		t.Fatal("a fallback ID named a host source file")
	}
	if _, err := AcceptProbe(config, ProbeReport{}, ProbeSelection{All: true}); !errors.Is(err, errHostUnknown) {
		t.Fatalf("accepting probe results under a fallback ID: %v", err)
	}

	// Rows indexed before hosts are claimed only by a Mac that knows its ID.
	useHost(t, "host-a")
	if result := catalog.Ingest(source, nil); result.Error != nil {
		t.Fatal(result.Error)
	}
	if _, err := catalog.DB.Exec("DELETE FROM meta WHERE key=?", legacyHostKey); err != nil {
		t.Fatal(err)
	}
	useFallbackHost(t)
	if err := catalog.Initialize(); !errors.Is(err, errHostUnknown) {
		t.Fatalf("claiming older rows under a fallback ID: %v", err)
	}
	useHost(t, "host-a")
	if err := catalog.Initialize(); err != nil || meta(legacyHostKey) != "host-a" {
		t.Fatalf("claim by a known host: %v, legacy %q", err, meta(legacyHostKey))
	}
}

// copyFixture is an in-memory source.
type copyFixture struct {
	config      SourceConfig
	records     []WorkspaceRecord
	fingerprint string
	requested   []string
}

func newCopyFixture(t *testing.T, records ...WorkspaceRecord) *copyFixture {
	return &copyFixture{config: SourceConfig{Name: "claude", Kind: "claude", Path: t.TempDir(), Account: "local", Enabled: true}, records: records}
}
func (f *copyFixture) Config() SourceConfig { return f.config }
func (f *copyFixture) Capability() string   { return "retrieval-only" }
func (f *copyFixture) Fingerprint() (string, error) {
	return defaultString(f.fingerprint, hashBytes([]byte(jsonText(f.records)))), nil
}
func (f *copyFixture) Discover(emit func(WorkspaceRecord) error) error {
	for _, record := range f.records {
		if err := emit(record); err != nil {
			return err
		}
	}
	return nil
}
func (f *copyFixture) DiscoverSelected(selected map[string]bool, emit func(WorkspaceRecord) error) error {
	f.requested = nil
	for id := range selected {
		f.requested = append(f.requested, id)
	}
	sort.Strings(f.requested)
	for _, record := range f.records {
		if selected[record.SourceID] {
			if err := emit(record); err != nil {
				return err
			}
		}
	}
	return nil
}

// sessionCopy is one host's copy of the Claude session "session": each ID is a
// user message plus a 15-token usage event on its own day.
func sessionCopy(origin string, ids ...string) WorkspaceRecord {
	messages := []MessageRecord{}
	for index, id := range ids {
		at := fmt.Sprintf("2026-09-%02dT00:00:00Z", index+1)
		messages = append(messages,
			MessageRecord{NativeID: id, Role: "user", Kind: "message", Text: "message " + id, CreatedAt: at, Selected: true},
			MessageRecord{NativeID: id + ":usage", Role: "assistant", Kind: "metadata", CreatedAt: at, Selected: true,
				Text: `{"type":"assistant","message":{"id":"` + id + `","usage":{"input_tokens":10,"output_tokens":5}}}`})
	}
	ended := messages[len(messages)-1].CreatedAt
	return WorkspaceRecord{SourceID: "session", SourceKind: "claude", Account: "local", Title: "Session", Location: filepath.Dir(origin), ActivityAt: ended,
		Conversations: []ConversationRecord{{NativeID: "session", Provider: "claude", Account: "local", Origin: origin, Coverage: "complete",
			StartedAt: messages[0].CreatedAt, EndedAt: ended, Messages: messages}}}
}

func ingestAs(t *testing.T, catalog *Catalog, host string, adapter Adapter) IngestResult {
	t.Helper()
	useHost(t, host)
	result := catalog.Ingest(adapter, nil)
	if result.Error != nil {
		t.Fatalf("%s ingest: %v", host, result.Error)
	}
	return result
}

type sessionState struct {
	messages       int
	tokens         float64
	sessionTokens  int64
	ended, writer  string
	nativeMessages []string
}

func readSession(t *testing.T, catalog *Catalog) sessionState {
	t.Helper()
	var state sessionState
	var writer sql.NullString
	if err := catalog.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM messages m WHERE m.conversation_id=c.id),c.ended_at,c.origin_host_id,
		(SELECT MAX(value) FROM metrics x WHERE x.workspace_id=c.workspace_id AND x.name='total_tokens'),
		(SELECT SUM(total_tokens) FROM agent_sessions a WHERE a.conversation_id=c.id)
		FROM conversations c WHERE native_id='session'`).Scan(&state.messages, &state.ended, &writer, &state.tokens, &state.sessionTokens); err != nil {
		t.Fatal(err)
	}
	state.writer = writer.String
	rows, err := queryMaps(catalog.DB, "SELECT native_id FROM messages WHERE kind='message' ORDER BY native_id")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		state.nativeMessages = append(state.nativeMessages, firstString(row["native_id"]))
	}
	return state
}

func TestHostsKeepSeparateSourceState(t *testing.T) {
	useHost(t, "host-a")
	catalog, config := testCatalog(t)
	macA := newCopyFixture(t, sessionCopy("/Users/ann/.claude/projects/x/session.jsonl", "m1"))
	macB := newCopyFixture(t, sessionCopy("/Users/bob/.claude/projects/x/session.jsonl", "m1", "m2"))
	macA.fingerprint, macB.fingerprint = "same-everywhere", "same-everywhere"
	if result := ingestAs(t, catalog, "host-a", macA); result.Workspaces != 1 {
		t.Fatalf("host A: %+v", result)
	}
	var before string
	if err := catalog.DB.QueryRow("SELECT last_success_at||fingerprint FROM source_states WHERE host_id='host-a' AND source_name='claude'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Same source name and fingerprint on another Mac is a different source.
	if result := ingestAs(t, catalog, "host-b", macB); result.SkippedUnchanged || result.Workspaces != 1 {
		t.Fatalf("host B's first sync was skipped: %+v", result)
	}
	var after string
	if err := catalog.DB.QueryRow("SELECT last_success_at||fingerprint FROM source_states WHERE host_id='host-a' AND source_name='claude'").Scan(&after); err != nil || after != before {
		t.Fatalf("host B's sync changed host A's state: %q -> %q (%v)", before, after, err)
	}
	for _, table := range []string{"source_states", "source_record_states"} {
		var hosts int
		if err := catalog.DB.QueryRow("SELECT COUNT(DISTINCT host_id) FROM " + table).Scan(&hosts); err != nil || hosts != 2 {
			t.Fatalf("%s hosts = %d, %v", table, hosts, err)
		}
	}
	if result := ingestAs(t, catalog, "host-a", macA); !result.SkippedUnchanged {
		t.Fatalf("host A lost its own fingerprint: %+v", result)
	}
	if result := ingestAs(t, catalog, "host-b", macB); !result.SkippedUnchanged {
		t.Fatalf("host B lost its own fingerprint: %+v", result)
	}
	freshness := catalog.Freshness()
	sources := freshness["sources"].([]map[string]any)
	others := freshness["other_host_sources"].([]map[string]any)
	if len(sources) != 1 || sources[0]["host_id"] != "host-b" || len(others) != 1 || others[0]["host_label"] != "Mac host-a" {
		t.Fatalf("freshness is not split by host: %#v", freshness)
	}
	config.Sources = []SourceConfig{macB.config}
	inventory, err := NewServer(config, catalog).sourceInventory()
	if err != nil {
		t.Fatal(err)
	}
	item := inventory["items"].([]map[string]any)[0]
	if item["host_id"] != "host-b" || item["host_label"] != "Mac host-b" || item["last_success_at"] == nil || len(inventory["hosts"].([]map[string]any)) != 2 {
		t.Fatalf("source inventory lacks host provenance: %#v", inventory)
	}
}

func TestIdenticalCopyFromAnotherHostIsOnlySighted(t *testing.T) {
	catalog, _ := testCatalog(t)
	record := sessionCopy("/Users/ann/.claude/projects/x/session.jsonl", "m1", "m2")
	ingestAs(t, catalog, "host-a", newCopyFixture(t, record))
	// Migration Assistant copied the same file to the same path on Mac B.
	if result := ingestAs(t, catalog, "host-b", newCopyFixture(t, record)); result.Workspaces != 0 || result.SkippedCurrent != 1 {
		t.Fatalf("identical copy was reindexed: %+v", result)
	}
	var sightings, conversationSightings int
	if err := catalog.DB.QueryRow("SELECT (SELECT COUNT(*) FROM workspace_sightings),(SELECT COUNT(*) FROM conversation_sightings)").Scan(&sightings, &conversationSightings); err != nil || sightings != 2 || conversationSightings != 2 {
		t.Fatalf("sightings = %d/%d, %v", sightings, conversationSightings, err)
	}
	if state := readSession(t, catalog); state.writer != "host-a" || state.messages != 4 {
		t.Fatalf("identical copy changed the writer: %+v", state)
	}
}

func TestRepairExistingIsHostScoped(t *testing.T) {
	catalog, _ := testCatalog(t)
	record := func(id string) WorkspaceRecord {
		session := sessionCopy("/Users/ann/.claude/projects/x/"+id+".jsonl", "m1")
		session.SourceID, session.Conversations[0].NativeID = id, id
		return session
	}
	ingestAs(t, catalog, "host-a", newCopyFixture(t, record("seen-on-a")))
	ingestAs(t, catalog, "host-b", newCopyFixture(t, record("seen-on-b")))
	for host, want := range map[string]string{"host-a": "seen-on-a", "host-b": "seen-on-b"} {
		useHost(t, host)
		fixture := newCopyFixture(t, record("seen-on-a"), record("seen-on-b"))
		if result := catalog.IngestExisting(fixture, nil); result.Error != nil || result.Workspaces != 1 {
			t.Fatalf("%s repair: %+v", host, result)
		}
		if strings.Join(fixture.requested, ",") != want {
			t.Fatalf("%s repaired %v, want only %s", host, fixture.requested, want)
		}
	}
}

func TestCrossHostPrefixNeverShrinksConversation(t *testing.T) {
	catalog, _ := testCatalog(t)
	originA := "/Users/ann/.claude/projects/x/session.jsonl"
	ingestAs(t, catalog, "host-a", newCopyFixture(t, sessionCopy(originA, "m1", "m2", "m3", "m4", "m5")))
	full := readSession(t, catalog)
	if full.messages != 10 || full.tokens != 75 || full.sessionTokens != 75 || full.writer != "host-a" {
		t.Fatalf("initial ingest: %+v", full)
	}
	// Mac B still has the older, shorter copy of the session A later continued.
	if result := ingestAs(t, catalog, "host-b", newCopyFixture(t, sessionCopy("/Users/bob/.claude/projects/x/session.jsonl", "m1", "m2", "m3"))); result.Workspaces != 1 {
		t.Fatalf("host B's copy was not applied: %+v", result)
	}
	if got := readSession(t, catalog); got.messages != full.messages || got.tokens != full.tokens || got.sessionTokens != full.sessionTokens ||
		got.ended != full.ended || got.writer != "host-a" {
		t.Fatalf("an older copy from another host regressed the conversation: %+v, want %+v", got, full)
	}
	var searchable int
	if err := catalog.DB.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"message m5"'`).Scan(&searchable); err != nil || searchable != 1 {
		t.Fatalf("newest message left the search index: %d %v", searchable, err)
	}
	rows, err := queryMaps(catalog.DB, "SELECT host_id,origin,messages FROM conversation_sightings ORDER BY host_id")
	if err != nil || len(rows) != 2 || rows[0]["origin"] != originA || integer(rows[0]["messages"]) != 10 || integer(rows[1]["messages"]) != 6 {
		t.Fatalf("conversation sightings: %#v %v", rows, err)
	}
	var workspaceID string
	if err := catalog.DB.QueryRow("SELECT id FROM workspaces").Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	detail, err := catalog.WorkDetail(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if sightings := detail["sightings"].([]map[string]any); len(sightings) != 2 || sightings[0]["location"] == sightings[1]["location"] {
		t.Fatalf("work detail sightings: %#v", sightings)
	}
	// The writer is still authoritative for its own copy, including pruning.
	edited := sessionCopy(originA, "m1", "m2", "m4", "m5")
	edited.Conversations[0].Messages[2].Text = "edited m2"
	if result := ingestAs(t, catalog, "host-a", newCopyFixture(t, edited)); result.Workspaces != 1 {
		t.Fatalf("host A's edited copy was not applied: %+v", result)
	}
	got := readSession(t, catalog)
	if got.messages != 8 || got.tokens != 60 || strings.Join(got.nativeMessages, ",") != "m1,m2,m4,m5" {
		t.Fatalf("writer re-sync did not replace its copy: %+v", got)
	}
	var text string
	if err := catalog.DB.QueryRow("SELECT text FROM messages WHERE native_id='m2'").Scan(&text); err != nil || text != "edited m2" {
		t.Fatalf("edited text = %q, %v", text, err)
	}
}

func TestDivergedCopiesAreUnionedUntilOneHoldsEverything(t *testing.T) {
	catalog, _ := testCatalog(t)
	originA, originB := "/Users/ann/.claude/projects/x/session.jsonl", "/Users/bob/.claude/projects/x/session.jsonl"
	ingestAs(t, catalog, "host-a", newCopyFixture(t, sessionCopy(originA, "m1", "m2", "m3", "m4")))
	// The session was resumed on Mac B from an older copy and diverged.
	ingestAs(t, catalog, "host-b", newCopyFixture(t, sessionCopy(originB, "m1", "m2", "b3")))
	got := readSession(t, catalog)
	if strings.Join(got.nativeMessages, ",") != "b3,m1,m2,m3,m4" || got.tokens < 60 || got.ended != "2026-09-04T00:00:00.000Z" || got.writer != mergedWriter {
		t.Fatalf("diverged copy was not unioned: %+v", got)
	}
	// Neither copy holds every message now, so neither may prune the other's.
	if result := ingestAs(t, catalog, "host-a", newCopyFixture(t, sessionCopy(originA, "m1", "m2", "m3"))); result.Workspaces != 1 {
		t.Fatalf("host A's partial copy was not applied: %+v", result)
	}
	if got := readSession(t, catalog); strings.Join(got.nativeMessages, ",") != "b3,m1,m2,m3,m4" {
		t.Fatalf("a partial copy pruned a diverged conversation: %+v", got)
	}
	// A copy holding everything unchanged takes the conversation over.
	complete := sessionCopy(originA, "m1", "m2", "m3", "m4")
	complete.Conversations[0].Messages = append(complete.Conversations[0].Messages, sessionCopy(originB, "m1", "m2", "b3").Conversations[0].Messages[4:]...)
	ingestAs(t, catalog, "host-c", newCopyFixture(t, complete))
	if got := readSession(t, catalog); got.writer != "host-c" || got.messages != 10 {
		t.Fatalf("complete copy did not take over: %+v", got)
	}
}

func TestPreHostCatalogMigratesToLegacyHost(t *testing.T) {
	useHost(t, "host-a")
	catalog, _ := testCatalog(t)
	path := catalog.Path
	// Rewind the catalog to the shape it had before hosts existed.
	for _, statement := range []string{
		"DROP TABLE source_states", "DROP TABLE source_record_states", "DROP TABLE workspace_sightings",
		"DROP TABLE conversation_sightings", "DROP TABLE hosts", "ALTER TABLE conversations DROP COLUMN origin_host_id",
		"DELETE FROM meta WHERE key IN ('legacy_host_id','host_sightings_version')",
		`CREATE TABLE source_states (source_name TEXT PRIMARY KEY, kind TEXT NOT NULL, capability TEXT NOT NULL, cursor TEXT,
			fingerprint TEXT, coverage TEXT NOT NULL, last_attempt_at TEXT, last_success_at TEXT, error TEXT,
			pending_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`,
		`CREATE TABLE source_record_states (source_name TEXT NOT NULL, source_kind TEXT NOT NULL, source_account TEXT NOT NULL,
			source_id TEXT NOT NULL, digest TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY(source_name,source_kind,source_account,source_id))`,
		`INSERT INTO source_states VALUES('claude','claude','retrieval-only','fp','fp','complete','2026-09-01','2026-09-01',NULL,0,'2026-09-01')`,
		`INSERT INTO source_record_states VALUES('claude','claude','local','session','digest','2026-09-01')`,
		`INSERT INTO repositories(id,display_name,local_locations_json,created_at,updated_at) VALUES('repo','pharos','["/Users/ann/src/pharos"]','t','t')`,
		`INSERT INTO workspaces(id,source_kind,source_account,source_id,repository_id,title,location,indexed_at)
			VALUES('w-session','claude','local','session','repo','Session','/Users/ann/src/pharos','2026-09-01'),
			('w-old','codex','local','old','repo','Old','/Users/ann/src/old','2026-08-01')`,
		`INSERT INTO conversations(id,workspace_id,provider,account,native_id,origin,ended_at)
			VALUES('c-session','w-session','claude','local','session','/Users/ann/.claude/projects/x/session.jsonl','2026-09-05T00:00:00Z')`,
	} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for index, id := range []string{"m1", "m2", "m3"} {
		if _, err := catalog.DB.Exec(`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,source_order,content_hash) VALUES(?,?,?,?,?,?,?,?)`,
			stableID("message", "c-session", id), "c-session", id, "user", "message", "message "+id, index, hashBytes([]byte("message "+id))); err != nil {
			t.Fatal(err)
		}
	}
	catalog.Close()
	for range 2 { // the migration is idempotent
		reopened, err := OpenCatalog(path)
		if err != nil {
			t.Fatal(err)
		}
		catalog = reopened
		var states, digests, index int
		var legacy string
		if err := catalog.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM source_states WHERE host_id='host-a' AND fingerprint='fp'),
			(SELECT COUNT(*) FROM source_record_states WHERE host_id='host-a' AND digest='digest'),
			(SELECT COUNT(*) FROM sqlite_master WHERE name='source_record_states_record_idx'),
			(SELECT value FROM meta WHERE key='legacy_host_id')`).Scan(&states, &digests, &index, &legacy); err != nil {
			t.Fatal(err)
		}
		if states != 1 || digests != 1 || index != 1 || legacy != "host-a" {
			t.Fatalf("source state was not rehosted: states=%d digests=%d index=%d legacy=%q", states, digests, index, legacy)
		}
		rows, err := queryMaps(catalog.DB, "SELECT workspace_id,host_id,source_name,location,repository_locations_json FROM workspace_sightings ORDER BY workspace_id")
		if err != nil || len(rows) != 2 || rows[0]["source_name"] != "" || rows[1]["source_name"] != "claude" || rows[1]["host_id"] != "host-a" ||
			rows[1]["location"] != "/Users/ann/src/pharos" || rows[1]["repository_locations_json"] != `["/Users/ann/src/pharos"]` {
			t.Fatalf("workspace sightings were not backfilled: %#v %v", rows, err)
		}
		var origin string
		if err := catalog.DB.QueryRow("SELECT origin FROM conversation_sightings WHERE conversation_id='c-session' AND host_id='host-a'").Scan(&origin); err != nil || origin != "/Users/ann/.claude/projects/x/session.jsonl" {
			t.Fatalf("conversation sighting = %q, %v", origin, err)
		}
		catalog.Close()
	}
	// Legacy rows belong to host A: host B's shorter copy cannot prune them,
	// and repair on host B does not select host A's records.
	useHost(t, "host-b")
	catalog, err := OpenCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	prefix := sessionCopy("/Users/bob/.claude/projects/x/session.jsonl", "m1")
	prefix.Conversations[0].Messages = prefix.Conversations[0].Messages[:1]
	if result := catalog.Ingest(newCopyFixture(t, prefix), nil); result.Error != nil || result.Workspaces != 1 {
		t.Fatalf("host B's copy was not applied: %+v", result)
	}
	var messages int
	var ended string
	if err := catalog.DB.QueryRow("SELECT COUNT(*),MAX(c.ended_at) FROM messages m JOIN conversations c ON c.id=m.conversation_id").Scan(&messages, &ended); err != nil || messages != 3 || ended != "2026-09-05T00:00:00Z" {
		t.Fatalf("host B pruned legacy messages: %d %q %v", messages, ended, err)
	}
	fixture := newCopyFixture(t)
	fixture.config.Name, fixture.config.Kind = "codex", "codex"
	if result := catalog.IngestExisting(fixture, nil); len(fixture.requested) != 0 || result.Workspaces != 0 {
		t.Fatalf("host B repaired host A's legacy records: %v %+v", fixture.requested, result)
	}
}
