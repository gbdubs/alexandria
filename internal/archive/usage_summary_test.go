package archive

import (
	"context"
	"testing"
	"time"
)

func TestUsageSummaryWindows(t *testing.T) {
	catalog, _ := testCatalog(t)
	// Usage lands in the 23:00 hour (1020 tokens) and the 01:00 hour (2030).
	ingestUsageFixture(t, catalog)
	now := time.Date(2026, 9, 21, 2, 15, 0, 0, time.UTC)
	catalog.now = func() time.Time { return now }
	// Keep the authorship rebuild from replacing the rows below.
	catalog.background = func(func(context.Context)) bool { return false }
	for _, message := range []struct {
		id     string
		sentAt time.Time
		typed  int64
	}{
		{"recent", now.Add(-30 * time.Minute), 12},
		{"earlier", now.Add(-5 * time.Hour), 30},
		{"old", now.Add(-40 * 24 * time.Hour), 100},
		{"machine", now.Add(-10 * time.Minute), 0},
	} {
		if _, err := catalog.DB.Exec(`INSERT INTO message_authorship(message_id,conversation_id,workspace_id,sent_at,day,total_chars,typed_chars,typed_words,spans_json)
			VALUES(?,?,?,?,?,?,?,?,'[]')`, message.id, "c", "w", message.sentAt.Format(time.RFC3339), message.sentAt.Format("2006-01-02"), 100, message.typed*5, message.typed); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := catalog.UsageSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	windows := map[string]*usageSummaryWindow{}
	for _, window := range summary["windows"].([]*usageSummaryWindow) {
		windows[window.Key] = window
	}
	for key, want := range map[string]struct{ words, messages, tokens int64 }{
		// 01:15 starts the window a quarter into the 01:00 hour: 3/4 of 2030.
		"1h":  {12, 1, 1523},
		"6h":  {42, 2, 3050},
		"30d": {42, 2, 3050},
		"all": {142, 3, 3050},
	} {
		got := windows[key]
		if got == nil || got.HumanWords != want.words || got.Messages != want.messages || got.Tokens != want.tokens {
			t.Fatalf("%s: got %+v, want %+v", key, got, want)
		}
	}
	if len(windows) != 6 {
		t.Fatalf("windows: %#v", windows)
	}
}
