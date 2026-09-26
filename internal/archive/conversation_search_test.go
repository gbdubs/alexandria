package archive

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestLibraryFindSearchesConversationsAndEvidence(t *testing.T) {
	catalog, _ := testCatalog(t)
	for _, statement := range []string{
		`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES('w','codex','local','w','Search work','2026-09-26')`,
		`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES('c1','w','codex','local','c1'),('c2','w','codex','local','c2')`,
		`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash,selected) VALUES
			('m1','c1','m1','user','message','We decieved the parser.','one',1),
			('m2','c1','m2','assistant','message','Fixed the Tokenizer, parser.','two',1),
			('m3','c2','m3','assistant','thinking','Tokenizer: parser may fail.','three',1),
			('m4','c2','m4','assistant','delegation','{"tool":"Task","input":"secret heliotrope"}','four',1)`,
		`INSERT INTO messages_fts(message_id,text) SELECT id,text FROM messages`,
		`INSERT INTO change_sets(id,workspace_id,classification,created_at) VALUES('set','w','attempted','2026-09-26')`,
		`INSERT INTO change_files(change_set_id,path,status,tracked) VALUES('set','src/parser.go','modified',1)`,
		`INSERT INTO tool_calls(id,workspace_id,conversation_id,sequence,provider,kind,tool_name,tool_category,status,call_message_id) VALUES('tool','w','c2',1,'codex','tool_call','web.open','web','ok','m3')`,
		`INSERT INTO tool_urls(tool_call_id,position,url,host,source) VALUES('tool',0,'https://example.com/docs','example.com','result')`,
		`INSERT INTO tool_urls(tool_call_id,position,url,host,source) VALUES('tool',1,'https://example.com/unopened','example.com','search_result')`,
	} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	find := func(o LibraryFindOptions) []libraryHit {
		t.Helper()
		o.Limit = 50
		result, err := catalog.LibraryFind(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		return result["items"].([]libraryHit)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: "Deception", Fuzzy: true}); len(hits) != 1 || hits[0].MessageID != "m1" || hits[0].ConversationID != "c1" {
		t.Fatalf("fuzzy hit: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: "Deception"}); len(hits) != 0 {
		t.Fatalf("exact word unexpectedly matched: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: "heliotrope", Fuzzy: true}); len(hits) != 0 {
		t.Fatalf("delegation input appeared as prose: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: `"tokenizer parser"`}); len(hits) != 2 {
		t.Fatalf("case-insensitive phrase: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: `"tokenizer parser"`, Separators: true}); len(hits) != 0 {
		t.Fatalf("separator-sensitive phrase: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: `"Tokenizer, parser"`, Separators: true, CaseSensitive: true}); len(hits) != 1 || hits[0].ConversationID != "c1" {
		t.Fatalf("literal phrase: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "text", Query: `"tokenizer, parser"`, Separators: true, CaseSensitive: true}); len(hits) != 0 {
		t.Fatalf("case-sensitive phrase: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "file", Query: "parser.go"}); len(hits) != 1 || hits[0].Attribution != "workspace" {
		t.Fatalf("ambiguous file attribution: %#v", hits)
	}
	if hits := find(LibraryFindOptions{Kind: "url", Query: "example.com"}); len(hits) != 1 || hits[0].ConversationID != "c2" || !strings.Contains(hits[0].URL, "/docs") {
		t.Fatalf("tool URL: %#v", hits)
	}
}

func TestLibraryFindFuzzyBeyondTenThousandVocabularyTerms(t *testing.T) {
	catalog, _ := testCatalog(t)
	for _, statement := range []string{
		`INSERT INTO workspaces(id,source_kind,source_account,source_id,title,indexed_at) VALUES('w','codex','local','w','Retrieval','2026-09-26')`,
		`INSERT INTO conversations(id,workspace_id,provider,account,native_id) VALUES('c','w','codex','local','c')`,
		`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m','c','m','assistant','message','We can retrieve the note.','hash')`,
		`INSERT INTO messages_fts(message_id,text) VALUES('m','We can retrieve the note.')`,
	} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var vocabulary strings.Builder
	for i := 0; i < 10050; i++ {
		fmt.Fprintf(&vocabulary, "re%06d ", i)
	}
	if _, err := catalog.DB.Exec(`INSERT INTO messages_fts(message_id,text) VALUES('vocabulary-only',?)`, vocabulary.String()); err != nil {
		t.Fatal(err)
	}
	result, err := catalog.LibraryFind(context.Background(), LibraryFindOptions{Kind: "text", Query: "retreive", Fuzzy: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	hits := result["items"].([]libraryHit)
	if len(hits) != 1 || hits[0].MessageID != "m" {
		t.Fatalf("fuzzy match beyond vocabulary cutoff: %#v", hits)
	}
}
