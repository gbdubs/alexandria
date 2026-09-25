package archive

import (
	"context"
	"reflect"
	"testing"
)

func TestCommandURLs(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{"curl", `curl -sS "https://api.example.com/v1/items?limit=5" | jq .`, []string{"https://api.example.com/v1/items?limit=5"}},
		{"continued line", "curl -X POST \\\n  https://hooks.example.com/run \\\n  -d '{}'", []string{"https://hooks.example.com/run"}},
		{"variable", "BASE=\"https://pool.example.dev\"\nKEY=abc\ncurl -s \"$BASE/tasks\" -H \"X-Key: $KEY\"", []string{"https://pool.example.dev"}},
		{"chained variable", "HOST=https://svc.example.org\nURL=\"$HOST/api\"\nwget -qO- \"$URL\"", []string{"https://svc.example.org"}},
		{"python", "python3 - <<'PY'\nimport urllib.request\nurl = 'https://data.example.net/x.json'\nprint(urllib.request.urlopen(url).read())\nPY", []string{"https://data.example.net/x.json"}},
		{"git clone", "git clone https://github.com/owner/repo.git /tmp/repo", []string{"https://github.com/owner/repo.git"}},
		{"pr body", "gh pr create --title T --body \"$(cat <<'EOF'\nSee https://docs.example.com/page for details\nEOF\n)\"", nil},
		{"echo", `echo "open https://example.com in a browser"`, nil},
		{"unrelated assignment", "DOCS=https://docs.example.com\ncurl -s https://api.example.com/health", []string{"https://api.example.com/health"}},
	}
	for _, test := range cases {
		if got := commandURLs(test.text); !reflect.DeepEqual(got, test.want) && !(len(got) == 0 && len(test.want) == 0) {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}

func TestCleanToolURL(t *testing.T) {
	for raw, want := range map[string][2]string{
		"https://Example.com/a).":          {"https://Example.com/a", "example.com"},
		"http://localhost:3009/dev?x=1":    {"http://localhost:3009/dev?x=1", "localhost:3009"},
		"file:///tmp/report.pdf":           {"", ""},
		"https://${HOST}/api":              {"", ""},
		"chrome://newtab/":                 {"", ""},
		"wss://stream.example.com/socket,": {"wss://stream.example.com/socket", "stream.example.com"},
	} {
		address, host := cleanToolURL(raw)
		if address != want[0] || host != want[1] {
			t.Errorf("%q: got %q %q, want %q", raw, address, host, want)
		}
	}
}

func TestToolURLLedger(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	writeFixture(t, root, "project/claude-web.jsonl", []string{
		`{"type":"user","uuid":"u1","sessionId":"claude-web","cwd":"/tmp/repo","timestamp":"2026-09-20T10:00:00.000Z","message":{"role":"user","content":"research it"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"claude-web","timestamp":"2026-09-20T10:00:05.000Z","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{"query":"cranelift wasm32"}},{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://cranelift.dev/docs","prompt":"Summarize https://ignored.example.com"}},{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"API=https://api.github.com\ncurl -s \"$API/repos/a/b\" | jq .stargazers_count"}},{"type":"tool_use","id":"toolu_4","name":"mcp__claude-in-chrome__navigate","input":{"tabId":1,"url":"http://localhost:3009/dev"}}],"usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0,"output_tokens":50}}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"claude-web","timestamp":"2026-09-20T10:00:07.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Web search results for query: \"cranelift wasm32\"\n\nLinks: [{\"title\":\"Cranelift\",\"url\":\"https://cranelift.dev/\"},{\"title\":\"Repo\",\"url\":\"https://github.com/bytecodealliance/wasmtime\"}]"}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"u2","sessionId":"claude-web","timestamp":"2026-09-20T10:00:08.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"Docs summary"}]}}`,
		`{"type":"user","uuid":"u4","parentUuid":"u3","sessionId":"claude-web","timestamp":"2026-09-20T10:00:09.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"42"}]}}`,
		`{"type":"user","uuid":"u5","parentUuid":"u4","sessionId":"claude-web","timestamp":"2026-09-20T10:00:10.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_4","content":"Navigated to http://localhost:3009/dev"}]}}`,
	})
	ingestFixture(t, catalog, "claude", root)
	calls := map[string]map[string]any{}
	rows, err := queryMaps(catalog.DB, "SELECT * FROM tool_calls")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		calls[firstString(row["tool_name"])] = row
	}
	search, fetch, shell, browser := calls["WebSearch"], calls["WebFetch"], calls["Bash"], calls["mcp__claude-in-chrome__navigate"]
	if search["search_query"] != "cranelift wasm32" || search["host"] != nil || integer(search["url_count"]) != 0 {
		t.Fatalf("search call: %#v", search)
	}
	if fetch["url"] != "https://cranelift.dev/docs" || fetch["host"] != "cranelift.dev" || integer(fetch["url_count"]) != 1 {
		t.Fatalf("fetch call: %#v", fetch)
	}
	if shell["url"] != "https://api.github.com" || shell["hosts"] != "api.github.com" {
		t.Fatalf("shell call: %#v", shell)
	}
	if browser["host"] != "localhost:3009" {
		t.Fatalf("browser call: %#v", browser)
	}
	detail, err := catalog.toolCallDetail(context.Background(), firstString(search["id"]))
	if err != nil {
		t.Fatal(err)
	}
	urls, _ := detail["urls"].([]map[string]any)
	if len(urls) != 2 || urls[0]["source"] != "search_result" || urls[1]["host"] != "github.com" {
		t.Fatalf("search result urls: %#v", detail["urls"])
	}
}

func TestCodexWebSearchCalls(t *testing.T) {
	catalog, _ := testCatalog(t)
	root := t.TempDir()
	writeFixture(t, root, "sessions/2026/09/20/rollout-codex-web.jsonl", []string{
		`{"type":"session_meta","timestamp":"2026-09-20T10:00:00Z","payload":{"id":"codex-web","cwd":"/tmp/repo","model":"gpt-5.5"}}`,
		`{"type":"response_item","timestamp":"2026-09-20T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"look it up"}]}}`,
		`{"type":"response_item","timestamp":"2026-09-20T10:00:02Z","payload":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"excel accrint source","queries":["excel accrint source","accrint algorithm"]}}}`,
		`{"type":"response_item","timestamp":"2026-09-20T10:00:03Z","payload":{"type":"web_search_call","status":"completed","action":{"type":"open_page","url":"https://stackoverflow.com/questions/14427610"}}}`,
		`{"type":"response_item","timestamp":"2026-09-20T10:00:04Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found it."}]}}`,
	})
	ingestFixture(t, catalog, "codex", root)
	rows, err := queryMaps(catalog.DB, "SELECT * FROM tool_calls ORDER BY sequence")
	if err != nil || len(rows) != 2 {
		t.Fatalf("calls: %#v %v", rows, err)
	}
	if rows[0]["tool_name"] != "web_search" || rows[0]["tool_category"] != "web" || rows[0]["status"] != "ok" || rows[0]["search_query"] != "excel accrint source" {
		t.Fatalf("search: %#v", rows[0])
	}
	if rows[1]["url"] != "https://stackoverflow.com/questions/14427610" || rows[1]["host"] != "stackoverflow.com" || rows[1]["status"] != "ok" {
		t.Fatalf("open page: %#v", rows[1])
	}
}
