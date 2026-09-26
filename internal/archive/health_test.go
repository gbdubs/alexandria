package archive

import (
	"context"
	"testing"
)

func TestHealthCountsStayCachedUntilACommit(t *testing.T) {
	catalog, _ := libraryFixture(t)
	other := otherProcess(t, catalog)
	ctx := context.Background()
	first, err := catalog.HealthCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := catalog.HealthCounts(ctx)
	if !sameRow(first, second) {
		t.Fatal("an unchanged catalog recounted")
	}
	messages := integer(first["messages"])
	if _, err := other.DB.Exec(`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m-health','cc','health','user','message','Again','h')`); err != nil {
		t.Fatal(err)
	}
	third, _ := catalog.HealthCounts(ctx)
	if integer(third["messages"]) != messages+1 {
		t.Fatalf("messages after another connection's insert = %v, want %d", third["messages"], messages+1)
	}
	// Sections are cached independently.
	pricing, _ := catalog.PricingHealth(ctx)
	again, _ := catalog.PricingHealth(ctx)
	if !sameRow(pricing, again) {
		t.Fatal("an unchanged catalog repriced")
	}
	if counts, _ := catalog.HealthCounts(ctx); !sameRow(counts, third) {
		t.Fatal("computing pricing recounted")
	}
}

func TestMessageCountFollowsEveryWrite(t *testing.T) {
	catalog, _ := libraryFixture(t)
	other := otherProcess(t, catalog)
	check := func(step string) {
		t.Helper()
		var kept, counted int64
		if err := catalog.DB.QueryRow("SELECT (SELECT value FROM catalog_counts WHERE name='messages'),(SELECT COUNT(*) FROM messages)").Scan(&kept, &counted); err != nil {
			t.Fatal(err)
		}
		if kept != counted || counted == 0 {
			t.Fatalf("%s: kept count %d, messages %d", step, kept, counted)
		}
	}
	check("fixture")
	// Another process, of any build, writes through the same triggers.
	for _, statement := range []string{
		`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m-new','cc','new','user','message','Hi','h')`,
		`INSERT INTO messages(id,conversation_id,native_id,role,kind,text,content_hash) VALUES('m-new','cc','new','user','message','Again','h')
			ON CONFLICT(conversation_id,native_id) DO UPDATE SET text=excluded.text`,
		`DELETE FROM messages WHERE id='m1'`,
		`DELETE FROM conversations WHERE id='ca'`,
	} {
		if _, err := other.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
		check(statement)
	}
	// A catalog from before the count gets one on its next open.
	for _, statement := range []string{"DROP TRIGGER catalog_counts_messages_insert", "DROP TRIGGER catalog_counts_messages_delete", "DROP TABLE catalog_counts"} {
		if _, err := catalog.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Initialize(); err != nil {
		t.Fatal(err)
	}
	check("upgrade")
	counts, err := catalog.HealthCounts(context.Background())
	if err != nil || integer(counts["messages"]) == 0 {
		t.Fatalf("health counts: %v %v", counts, err)
	}
}
