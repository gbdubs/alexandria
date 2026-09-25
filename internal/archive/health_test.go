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
