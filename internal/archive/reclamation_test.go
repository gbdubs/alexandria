//go:build reclamation

package archive

import "testing"

func TestProtectStoresCanonicalTimestamp(t *testing.T) {
	catalog, _ := testCatalog(t)
	if err := catalog.Protect("workspace", "w", "snooze", "2026-10-01", ""); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := catalog.DB.QueryRow("SELECT until_at FROM protections").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-01T00:00:00.000Z"; got != want {
		t.Errorf("until_at = %q; want %q", got, want)
	}
}
