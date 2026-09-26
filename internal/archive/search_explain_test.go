package archive

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestSemanticExplanationAccountsForCosine(t *testing.T) {
	fields := []semanticField{{"Title", "Fix authentication error on login"}, {"Purpose", ""},
		{"First ask", "The sqlite database crashed when the oauth token expired."}, {"Last reply", "Added tests for the token refresh"}}
	parts := []string{}
	for _, field := range fields {
		parts = append(parts, field.text)
	}
	stored := semanticEmbed(strings.Join(parts, "\n"))
	document := buildSemanticDocument(fields)
	if !document.matches(stored) {
		t.Fatal("rebuilt document does not embed to the stored vector")
	}
	for _, query := range []string{"auth bug", "fix flaky sqlite test", "vector search explainability", "the"} {
		explanation := explainSemantic(query, document)
		cosine := semanticCosine(semanticEmbed(query), stored)
		if math.Abs(explanation.Cosine-cosine) > 1e-12 || math.Abs(explanation.Signal+explanation.Noise-cosine) > 1e-12 {
			t.Fatalf("%q: explanation %+v does not sum to cosine %v", query, explanation, cosine)
		}
	}
	explanation := explainSemantic("auth bug", document)
	byQuery := map[string]relatedTerm{}
	for _, term := range explanation.Terms {
		byQuery[term.Query+"/"+term.Via] = term
	}
	if term := byQuery["bug/concept"]; term.Concept != "failure" || !slices.Contains(term.Matched, "error") || term.Fields[0] != "Title" {
		t.Fatalf("bug should match error through the failure concept in the title: %+v", explanation.Terms)
	}
	if term := byQuery["auth/concept"]; !slices.Contains(term.Matched, "authentication") {
		t.Fatalf("auth should match authentication: %+v", explanation.Terms)
	}
	if byQuery["auth bug/phrase"].Value <= 0 {
		t.Fatalf("the pair auth bug should match authentication error: %+v", explanation.Terms)
	}
	if typo := explainSemantic("refrresh", document).Terms; len(typo) == 0 || typo[0].Via != "letters" || !slices.Contains(typo[0].Matched, "refresh") {
		t.Fatalf("a misspelling should share letters with refresh: %+v", typo)
	}
	// Unrelated text scores from collisions and a few scattered letters.
	if unrelated := explainSemantic("vector search explainability", document); unrelated.Signal != unrelated.Scattered || unrelated.Signal > .01 || len(unrelated.Terms) != 0 {
		t.Fatalf("unrelated query should have no shared features: %+v", unrelated)
	}
}

func ingestRecords(t *testing.T, catalog *Catalog, records ...WorkspaceRecord) {
	t.Helper()
	for _, record := range records {
		tx, err := catalog.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ingestWorkspace(tx, record, false); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func searchFixture() []WorkspaceRecord {
	message := func(id, role, kind, text string) MessageRecord {
		return MessageRecord{NativeID: id, Role: role, Kind: kind, Text: text, Selected: true, CreatedAt: "2026-09-20T12:00:0" + id[len(id)-1:] + "Z"}
	}
	return []WorkspaceRecord{
		{SourceID: "query", SourceKind: "conductor", Account: "local", Title: "Speed up catalog queries", ActivityAt: "2026-09-20T12:00:00Z",
			Conversations: []ConversationRecord{{NativeID: "query-thread", Provider: "claude", Account: "local", Messages: []MessageRecord{
				message("q1", "user", "message", "Why is catalog_query.go slow when the Library filters by repository?"),
				message("q2", "tool", "tool_result", "zqxwvtoolonly appears only in tool output"),
				message("q3", "assistant", "message", "Added an index; the Library query now takes 40ms."),
			}}}},
		{SourceID: "auth", SourceKind: "conductor", Account: "local", Title: "Login flow", Outcome: "Merged the login fix", ActivityAt: "2026-09-19T12:00:00Z",
			Conversations: []ConversationRecord{{NativeID: "auth-thread", Provider: "codex", Account: "local", Messages: []MessageRecord{
				message("a1", "user", "message", "Users see an authentication error after the token expires."),
				message("a2", "assistant", "message", "Refreshed the token before it expires, which resolves the authentication error."),
			}}}},
	}
}

func queryLibrary(t *testing.T, server *Server, parameters string) []map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/query/library?"+parameters, strings.NewReader(`{"select":["title"],"limit":25,"offset":0}`))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", parameters, response.Code, response.Body.String())
	}
	var body struct{ Rows []map[string]any }
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Rows
}

func segmentsText(message any, matchesOnly bool) string {
	text := ""
	for _, segment := range message.(map[string]any)["segments"].([]any) {
		segment := segment.(map[string]any)
		if !matchesOnly || segment["match"] == true {
			text += segment["text"].(string)
			if matchesOnly {
				text += "|"
			}
		}
	}
	return text
}

func TestLibrarySubstringSearchAndExplanations(t *testing.T) {
	catalog, config := testCatalog(t)
	ingestRecords(t, catalog, searchFixture()...)
	server := NewServer(config, catalog)

	if rows := queryLibrary(t, server, "search=log_que"); len(rows) != 0 {
		t.Fatalf("word search should not match inside catalog_query: %v", rows)
	}
	rows := queryLibrary(t, server, "search=log_que&substring=1&explain=1")
	if len(rows) != 1 || rows[0]["title"] != "Speed up catalog queries" {
		t.Fatalf("substring search should find catalog_query: %v", rows)
	}
	why := rows[0]["why"].(map[string]any)
	substring := why["substring"].(map[string]any)
	messages := substring["messages"].([]any)
	if len(messages) != 1 || segmentsText(messages[0], true) != "log_que|" || !strings.Contains(segmentsText(messages[0], false), "catalog_query.go") {
		t.Fatalf("substring match should mark log_que in context: %#v", substring)
	}
	if !slices.Equal(rows[0]["match_types"].([]any), []any{"substring"}) {
		t.Fatalf("match types: %v", rows[0]["match_types"])
	}

	// Tool output is only in the word index.
	if rows := queryLibrary(t, server, "search=zqxwvtoolonly&substring=1"); len(rows) != 1 {
		t.Fatalf("word search should still find tool output with substring on: %v", rows)
	}
	var indexed int
	if err := catalog.DB.QueryRow(`SELECT COUNT(*) FROM messages_trigram WHERE messages_trigram MATCH '"zqxwvtoolonly"'`).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("tool output should not be in the substring index: %d, %v", indexed, err)
	}

	rows = queryLibrary(t, server, "search=authentication+bug&explain=1")
	var auth map[string]any
	for _, row := range rows {
		if row["title"] == "Login flow" {
			auth = row
		}
	}
	if auth == nil {
		t.Fatalf("concept search should find the login work: %v", rows)
	}
	why = auth["why"].(map[string]any)
	if why["text"] != nil {
		t.Fatalf("no message holds both words, so there is no text match: %v", why["text"])
	}
	related := why["related"].(map[string]any)
	if related["exact"] != true {
		t.Fatalf("the rebuilt document should embed to the stored vector: %v", related)
	}
	found := false
	for _, term := range related["terms"].([]any) {
		term := term.(map[string]any)
		if term["query"] == "bug" && term["via"] == "concept" && slices.Contains(term["matched"].([]any), any("error")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("bug should match error through a concept: %v", related["terms"])
	}
	if math.Abs(related["signal"].(float64)+related["noise"].(float64)-related["cosine"].(float64)) > 1e-9 {
		t.Fatalf("signal and noise should sum to the cosine: %v", related)
	}
	parts := why["parts"].([]any)
	if len(parts) != 1 || math.Abs(parts[0].(map[string]any)["value"].(float64)-why["score"].(float64)) > 1e-12 {
		t.Fatalf("score parts: %v", why)
	}

	rows = queryLibrary(t, server, "search=token+expires&explain=1")
	if len(rows) == 0 || rows[0]["title"] != "Login flow" {
		t.Fatalf("word search: %v", rows)
	}
	text := rows[0]["why"].(map[string]any)["text"].(map[string]any)
	if text["rank"] != 1.0 || text["count"] != 2.0 || segmentsText(text["messages"].([]any)[0], true) != "token|expires|" {
		t.Fatalf("word match should mark whole words: %v", text)
	}

	if rows := queryLibrary(t, server, "search=token+expires"); rows[0]["why"] != nil {
		t.Fatal("explanations are only added on request")
	}
}

func TestLibraryQuotedPhraseSearch(t *testing.T) {
	catalog, config := testCatalog(t)
	records := searchFixture()
	records[0].Conversations[0].Messages[1].Text += "; backup manifest validated"
	records = append(records,
		WorkspaceRecord{SourceID: "reverse", SourceKind: "conductor", Account: "local", Title: "Reverse words", ActivityAt: "2026-09-18T12:00:00Z",
			Conversations: []ConversationRecord{{NativeID: "reverse-thread", Provider: "codex", Account: "local", Messages: []MessageRecord{
				{NativeID: "r1", Role: "user", Kind: "message", Text: "The error followed authentication in this flow.", Selected: true},
			}}}},
		WorkspaceRecord{SourceID: "separate", SourceKind: "conductor", Account: "local", Title: "Separate words", ActivityAt: "2026-09-17T12:00:00Z",
			Conversations: []ConversationRecord{{NativeID: "separate-thread", Provider: "codex", Account: "local", Messages: []MessageRecord{
				{NativeID: "s1", Role: "user", Kind: "message", Text: "An authentication failure occurred. Later there was an error.", Selected: true},
			}}}},
	)
	ingestRecords(t, catalog, records...)
	server := NewServer(config, catalog)
	search := func(query string, substring bool) []map[string]any {
		t.Helper()
		parameters := "search=" + url.QueryEscape(query) + "&explain=1"
		if substring {
			parameters += "&substring=1"
		}
		return queryLibrary(t, server, parameters)
	}
	rows := search(`"authentication error"`, true)
	if len(rows) != 1 || rows[0]["title"] != "Login flow" {
		t.Fatalf("a quoted phrase must be in order within one message: %v", rows)
	}
	if !slices.Contains(rows[0]["match_types"].([]any), any("phrase")) {
		t.Fatalf("phrase match type missing: %v", rows[0])
	}
	phrase := rows[0]["why"].(map[string]any)["phrase"].(map[string]any)
	if phrase["count"] != float64(2) || len(phrase["messages"].([]any)) != 2 {
		t.Fatalf("phrase explanation should cite matching messages: %v", phrase)
	}
	if rows := search(`"error authentication"`, true); len(rows) != 0 {
		t.Fatalf("reverse order must not match: %v", rows)
	}
	if rows := search(`"authentication error" zqxwvtoolonly`, true); len(rows) != 0 {
		t.Fatalf("unquoted terms cannot introduce work without the phrase: %v", rows)
	}
	if rows := search(`"authentication error" "token expires"`, true); len(rows) != 1 {
		t.Fatalf("both phrases in the same message should match: %v", rows)
	}
	if rows := search(`"authentication error" "expired token"`, true); len(rows) != 0 {
		t.Fatalf("both phrases must match in a message: %v", rows)
	}
	if rows := search(`"authentication error" uthentication`, true); len(rows) != 1 || rows[0]["why"].(map[string]any)["substring"] == nil {
		t.Fatalf("unquoted substring search should still work with a phrase: %v", rows)
	}
	if rows := search(`"backup manifest"`, false); len(rows) != 1 || rows[0]["title"] != "Speed up catalog queries" {
		t.Fatalf("phrase search should include tool output through the word index: %v", rows)
	}
	if rows := search(`"authentication error`, true); len(rows) == 0 {
		t.Fatal("an unfinished quote should keep ordinary search behavior")
	}
}

func TestSubstringIndexBackfillAndReingest(t *testing.T) {
	catalog, _ := testCatalog(t)
	var ready string
	if err := catalog.DB.QueryRow("SELECT value FROM meta WHERE key='message_trigram_version'").Scan(&ready); err != nil || ready != "1" {
		t.Fatalf("an empty catalog has nothing to backfill: %q %v", ready, err)
	}
	records := searchFixture()
	ingestRecords(t, catalog, records...)
	count := func(term string) int {
		t.Helper()
		var n int
		if err := catalog.DB.QueryRow(`SELECT COUNT(*) FROM messages_trigram WHERE messages_trigram MATCH ?`, substringQuery([]string{term})).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("uthentication") != 2 {
		t.Fatalf("ingest should index prose: %d", count("uthentication"))
	}
	// A catalog from before the index: empty it and backfill, twice over.
	for run := 0; run < 2; run++ {
		if _, err := catalog.DB.Exec("DELETE FROM meta WHERE key IN ('message_trigram_version','message_trigram_cursor')"); err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			if _, err := catalog.DB.Exec("INSERT INTO messages_trigram(messages_trigram) VALUES('delete-all')"); err != nil {
				t.Fatal(err)
			}
		}
		progress, err := catalog.substringIndexProgress(context.Background())
		if err != nil || progress.Ready || progress.Done != 0 || progress.Total != 2 {
			t.Fatalf("progress before backfill: %+v %v", progress, err)
		}
		catalog.maintainSubstringIndex(context.Background())
		if progress, err := catalog.substringIndexProgress(context.Background()); err != nil || !progress.Ready {
			t.Fatalf("progress after backfill: %+v %v", progress, err)
		}
		if count("uthentication") != 2 {
			t.Fatalf("run %d: backfill should index each message once: %d", run, count("uthentication"))
		}
	}
	records[1].Conversations[0].Messages[0].Text = "Users see a session timeout."
	ingestRecords(t, catalog, records[1])
	if count("uthentication") != 1 || count("timeou") != 1 {
		t.Fatalf("reingest should replace changed text: %d %d", count("uthentication"), count("timeou"))
	}
}

func TestHighlightSegments(t *testing.T) {
	segments := highlightSegments("Parsers  parse\nthe PARSE tree", []string{"parse"}, true, true, false)
	got := ""
	for _, segment := range segments {
		if segment["match"] == true {
			got += "[" + segment["text"].(string) + "]"
		} else {
			got += segment["text"].(string)
		}
	}
	if got != "…Parsers [parse] the [PARSE] tree" {
		t.Fatalf("got %q", got)
	}
}

func TestSnippetSegmentsMergeWindows(t *testing.T) {
	text := []rune(strings.Repeat("x", 100) + " alpha " + strings.Repeat("y", 400) + " beta gamma " + strings.Repeat("z", 50))
	window := func(start, size int) snippetWindow {
		return snippetWindow{start, text[start-1 : min(len(text), start-1+size)]}
	}
	segments := snippetSegments([]snippetWindow{window(490, 40), window(95, 20), window(500, 40)}, len(text), []string{"alpha", "beta", "gamma"}, true)
	got := ""
	for _, segment := range segments {
		if segment["match"] == true {
			got += "[" + segment["text"].(string) + "]"
		} else {
			got += segment["text"].(string)
		}
	}
	if got != "…xxxxxx [alpha] yyyyyyy…"+strings.Repeat("y", 18)+" [beta] [gamma] "+strings.Repeat("z", 20)+"…" {
		t.Fatalf("got %q", got)
	}
}
