package archive

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConfigResolvesRelativePathsAgainstConfigDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(root, "library")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `data_dir = "data"
archive_root = "preserved"
staging_root = "../staging"
executable = "Pharos.app/Contents/MacOS/alexandria"

[[sources]]
name = "relative"
kind = "claude"
path = "captures/claude"

[[sources]]
name = "home"
kind = "codex"
path = "~/.codex"

[[sources]]
name = "absolute"
kind = "codex"
path = "/absolute/codex"

[[sources]]
name = "unset"
kind = "codex"
`
	if err := os.WriteFile(filepath.Join(library, "archive.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// Load through a relative config path from a CWD that is not the config's
	// directory, so any CWD-relative resolution would be visible.
	t.Chdir(root)
	loaded, err := LoadConfig(filepath.Join("library", "archive.toml"))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for name, pair := range map[string][2]string{
		"path":        {loaded.Path, filepath.Join(library, "archive.toml")},
		"catalog":     {loaded.CatalogPath, filepath.Join(library, "data", "catalog.sqlite3")},
		"archive":     {loaded.ArchiveRoot, filepath.Join(library, "preserved")},
		"staging":     {loaded.StagingRoot, filepath.Join(root, "staging")},
		"executable":  {loaded.Executable, filepath.Join(library, "Pharos.app", "Contents", "MacOS", "alexandria")},
		"source rel":  {loaded.Sources[0].Path, filepath.Join(library, "captures", "claude")},
		"source home": {loaded.Sources[1].Path, filepath.Join(home, ".codex")},
		"source abs":  {loaded.Sources[2].Path, "/absolute/codex"},
		"source none": {loaded.Sources[3].Path, ""},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s resolved to %q, want %q", name, pair[0], pair[1])
		}
	}
	if loaded.Library {
		t.Error("a config without library = true must not be treated as a library")
	}
}

func TestLibraryConfigIsDiscoveredBesideAppBundle(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "Pharos.app", "Contents", "MacOS", "alexandria")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := libraryConfigBeside(executable); got != "" {
		t.Fatalf("found %q before library.toml exists", got)
	}
	path := filepath.Join(root, "library.toml")
	if err := os.WriteFile(path, []byte("library = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := libraryConfigBeside(executable); got != path {
		t.Fatalf("discovered %q, want %q", got, path)
	}
	for _, other := range []string{
		"",
		filepath.Join(root, "alexandria"),
		filepath.Join(root, "Pharos.app", "Contents", "Resources", "alexandria"),
		filepath.Join(root, "Pharos", "Contents", "MacOS", "alexandria"),
	} {
		if got := libraryConfigBeside(other); got != "" {
			t.Errorf("executable %q outside an app bundle discovered %q", other, got)
		}
	}
}

func TestInitLibraryWritesRelativeConfigAndCatalog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Pharos")
	identity := func(string) string { return "uuid:EUCLID" }
	config, err := InitLibrary(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "library.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"library = true", `catalog_path = "catalog/catalog.sqlite3"`, `archive_root = "preserved"`,
		`staging_root = "staging"`, `volume_id = "uuid:EUCLID"`, `host = "127.0.0.1"`, "port = 8766", "enable_reclamation = false", "api_token = "} {
		if !strings.Contains(text, want) {
			t.Errorf("library.toml is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[[sources]]") || strings.Contains(text, dir) {
		t.Errorf("library.toml must hold no sources or absolute paths:\n%s", text)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("library.toml holds the API token and must be private: %v %v", info.Mode(), err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Library || loaded.Port != 8766 || loaded.VolumeID != "uuid:EUCLID" || loaded.EnableReclamation ||
		loaded.CatalogPath != filepath.Join(dir, "catalog", "catalog.sqlite3") || loaded.ArchiveRoot != filepath.Join(dir, "preserved") ||
		loaded.StagingRoot != filepath.Join(dir, "staging") || len(loaded.Sources) != 0 || loaded.APIToken != config.APIToken {
		t.Fatalf("unexpected library config: %+v", loaded)
	}
	for _, directory := range []string{loaded.ArchiveRoot, loaded.StagingRoot} {
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			t.Errorf("init-library did not create %s: %v", directory, err)
		}
	}
	if err := loaded.checkLibrary(identity); err != nil {
		t.Fatalf("a fresh library must pass its guards: %v", err)
	}
	catalog, err := OpenCatalog(loaded.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var version string
	err = catalog.DB.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	catalog.Close()
	if err != nil || version != strconv.Itoa(catalogSchemaVersion) {
		t.Fatalf("catalog was not initialized: version %q, %v", version, err)
	}
	if _, err := InitLibrary(dir, identity); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second init-library was not refused: %v", err)
	}
	if again, _ := os.ReadFile(path); string(again) != text {
		t.Fatal("a refused init-library changed library.toml")
	}
}

func TestInitLibraryPinsOnlyStableVolumeIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Pharos")
	config, err := InitLibrary(dir, func(string) string { return "device:16777232" })
	if err != nil {
		t.Fatal(err)
	}
	if config.VolumeID != "" {
		t.Fatalf("pinned an unstable identity %q", config.VolumeID)
	}
	if err := config.checkLibrary(func(string) string { return "device:1" }); err != nil {
		t.Fatalf("an unpinned library must open on any volume: %v", err)
	}
}

func TestLibraryRefusesMissingCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.toml")
	if err := os.WriteFile(path, []byte("library = true\ncatalog_path = \"catalog/catalog.sqlite3\"\nstaging_root = \"staging\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"--config", path, "health"})
	if err == nil || !strings.Contains(err.Error(), "catalog not found at "+filepath.Join(dir, "catalog", "catalog.sqlite3")) {
		t.Fatalf("missing library catalog was not refused: %v", err)
	}
	for _, created := range []string{"catalog", "staging"} {
		if _, err := os.Stat(filepath.Join(dir, created)); !os.IsNotExist(err) {
			t.Errorf("refused open still created %s: %v", created, err)
		}
	}
	// A legacy config keeps creating its catalog on first use.
	legacy := filepath.Join(dir, "archive.toml")
	if err := os.WriteFile(legacy, []byte("data_dir = \"legacy\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.checkLibrary(nil); err != nil {
		t.Fatalf("legacy config was guarded: %v", err)
	}
}

func TestLibraryRefusesWrongVolume(t *testing.T) {
	config, err := InitLibrary(filepath.Join(t.TempDir(), "Pharos"), func(string) string { return "uuid:EUCLID" })
	if err != nil {
		t.Fatal(err)
	}
	var probed string
	err = config.checkLibrary(func(path string) string { probed = path; return "uuid:CLONE" })
	if err == nil || !strings.Contains(err.Error(), "uuid:EUCLID") || !strings.Contains(err.Error(), "uuid:CLONE") {
		t.Fatalf("wrong volume was not refused with both identities: %v", err)
	}
	if probed != filepath.Dir(config.Path) {
		t.Fatalf("probed %q instead of the library directory", probed)
	}
	// Legacy configs keep volume_id as an archive_root health signal only.
	config.Library = false
	calls := 0
	if err := config.checkLibrary(func(string) string { calls++; return "uuid:CLONE" }); err != nil || calls != 0 {
		t.Fatalf("legacy config was volume-guarded: %v (probes %d)", err, calls)
	}
}

func TestCatalogRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.sqlite3")
	setVersion := func(value string) {
		t.Helper()
		catalog, err := OpenCatalog(path)
		if err != nil {
			t.Fatal(err)
		}
		defer catalog.Close()
		if _, err := catalog.DB.Exec("UPDATE meta SET value=? WHERE key='schema_version'", value); err != nil {
			t.Fatal(err)
		}
	}
	storedVersion := func() string {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var value string
		if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	setVersion("3")
	catalog, err := OpenCatalog(path)
	if err != nil {
		t.Fatalf("an older catalog must open: %v", err)
	}
	catalog.Close()
	if got := storedVersion(); got != strconv.Itoa(catalogSchemaVersion) {
		t.Fatalf("older catalog recorded version %q after migration", got)
	}
	newer := strconv.Itoa(catalogSchemaVersion + 1)
	setVersion(newer)
	if _, err := OpenCatalog(path); err == nil || !strings.Contains(err.Error(), "schema version "+newer) {
		t.Fatalf("a newer catalog was not refused: %v", err)
	}
	if got := storedVersion(); got != newer {
		t.Fatalf("refused open changed schema_version to %q", got)
	}
}
