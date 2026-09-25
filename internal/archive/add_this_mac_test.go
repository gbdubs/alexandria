package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddThisMacAddsCapturesAndIndexesInOneStep(t *testing.T) {
	useHost(t, "host_new_mac")
	home := probeHome(t)
	session := "11111111-1111-4111-8111-111111111111"
	probePut(t, filepath.Join(home, ".claude", "projects", "-proj", session+".jsonl"), claudeLine(session, "u1", "hello from the new mac", "/proj", 1))
	config, err := InitLibrary(filepath.Join(t.TempDir(), "Pharos"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}

	// Without --yes and without a terminal to ask, nothing is added.
	var out bytes.Buffer
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := runAddThisMacCLI(config, nil, stdin, &out); err != nil {
		t.Fatalf("declined run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Nothing was changed.") {
		t.Fatalf("declined run output:\n%s", out.String())
	}
	if _, err := os.Stat(hostConfigPath(config)); !os.IsNotExist(err) {
		t.Fatalf("a declined run wrote the host file: %v", err)
	}

	out.Reset()
	if err := runAddThisMacCLI(config, []string{"--yes"}, stdin, &out); err != nil {
		t.Fatalf("add-this-mac: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Found on this Mac:", "claude", "Saved this Mac's sources", "Capturing 1 source", "You can eject", "Indexing 1 source", "Done."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if hosted, err := os.ReadFile(hostConfigPath(config)); err != nil || !strings.Contains(string(hosted), `kind = "claude"`) {
		t.Fatalf("host file: %v\n%s", err, hosted)
	}
	if _, err := os.Stat(filepath.Join(config.CaptureRoot, "host_new_mac", "claude", "manifest.json")); err != nil {
		t.Fatalf("capture: %v", err)
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	if err := catalog.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE text='hello from the new mac'").Scan(&found); err != nil || found != 1 {
		t.Fatalf("indexed messages: %d, %v", found, err)
	}
	catalog.Close()

	// Running it again (as the CLI does, with the configuration reloaded) adds
	// nothing new and indexes nothing unchanged.
	if config, err = LoadConfig(config.Path); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runAddThisMacCLI(config, nil, stdin, &out); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already added") || strings.Contains(out.String(), "Add 1 new source") || !strings.Contains(out.String(), "0 workspaces written") {
		t.Fatalf("rerun output:\n%s", out.String())
	}
}
