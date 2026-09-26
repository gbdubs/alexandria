package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A portable library is one self-contained directory, typically on an
// external drive, holding Pharos.app beside library.toml, the catalog, and the
// preserved archive. Its relative paths resolve against that directory, so the
// library works wherever the drive is mounted and on whichever Mac mounts it.
const libraryConfigName = "library.toml"

// libraryPort leaves the default 8765 to an internal (~/Library) install, so a
// Mac can run both while its library is being set up without the app's health
// probe reaching the other service.
const libraryPort = 8766

const libraryConfigTemplate = `# Pharos portable library. Relative paths resolve against the directory
# containing this file, so the library works wherever its drive is mounted.
library = true
%s
catalog_path = "catalog/catalog.sqlite3"
archive_root = "preserved"
staging_root = "staging"
api_token = %q
host = "127.0.0.1"
# 8765 is left to an internal (~/Library) install on the same Mac.
port = %d
%s
enable_reclamation = false
release_hook_proven = false

# Source locations differ per Mac and are configured per host, not here.
`

// defaultLibrarySettings are the retention settings of a new library; adopting
// an install carries over its own.
const defaultLibrarySettings = "package_cap_bytes = 100000000\nupcoming_days = 23\neligible_days = 30"

func runningExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	// A symlink on PATH should still find the library beside the real bundle.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// libraryConfigBeside returns the library.toml beside the app bundle whose
// Contents/MacOS holds executable, or "" when there is none.
func libraryConfigBeside(executable string) string {
	macOS := filepath.Dir(executable)
	contents := filepath.Dir(macOS)
	bundle := filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(bundle, ".app") {
		return ""
	}
	path := filepath.Join(filepath.Dir(bundle), libraryConfigName)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

// checkLibrary runs before a library's catalog is opened. A copy of the
// library on another volume (a clone or backup) must not diverge silently from
// the pinned one, and a missing catalog usually means the drive holding it is
// not mounted, so neither may be papered over with a fresh empty catalog.
// identity is volumeIdentity outside tests; it shells out, so call this once.
func (c Config) checkLibrary(identity func(string) string) error {
	if !c.Library {
		return nil
	}
	dir := filepath.Dir(c.Path)
	if c.VolumeID != "" {
		if actual := identity(dir); actual != c.VolumeID {
			return fmt.Errorf("library %s pins volume_id %q but is on volume %q; refusing to open what may be a different copy of it. If it was moved or restored deliberately, set volume_id = %q in %s", dir, c.VolumeID, actual, actual, c.Path)
		}
	}
	if _, err := os.Stat(c.CatalogPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("catalog not found at %s; is the drive mounted? Pharos never creates an empty catalog for a library implicitly; run `pharos init-library DIR` to create a new library", c.CatalogPath)
	} else if err != nil {
		return err
	}
	return nil
}

// InitLibrary writes dir/library.toml and creates its catalog. It never
// overwrites a configuration, and removes the one it wrote if the catalog
// cannot be created, so a failed attempt can simply be retried.
func InitLibrary(dir string, identity func(string) string) (Config, error) {
	dir, err := filepath.Abs(expandPath(dir))
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Config{}, err
	}
	path, err := writeLibraryConfig(dir, identity, defaultLibrarySettings)
	if err != nil {
		return Config{}, err
	}
	config, err := createLibraryCatalog(path)
	if err != nil {
		_ = os.Remove(path)
		return Config{}, err
	}
	return config, nil
}

// writeLibraryConfig creates dir/library.toml, never replacing one, and
// flushes it to the drive.
func writeLibraryConfig(dir string, identity func(string) string, settings string) (string, error) {
	path := filepath.Join(dir, libraryConfigName)
	// Device numbers change between mounts; pinning one would lock the library
	// out the next time the drive is attached.
	pin := "# No stable volume identity was found. Set this from `pharos volume-id` to\n# refuse copies of this library on other volumes.\nvolume_id = \"\""
	if volume := identity(dir); strings.HasPrefix(volume, "uuid:") {
		pin = fmt.Sprintf("# Pharos refuses to open this library from any other volume. After moving it\n# deliberately, set this from `pharos volume-id` on its new location.\nvolume_id = %q", volume)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("refusing to overwrite %s", path)
	} else if err != nil {
		return "", err
	}
	_, err = fmt.Fprintf(file, libraryConfigTemplate, pin, randomToken(), libraryPort, settings)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func createLibraryCatalog(path string) (Config, error) {
	config, err := LoadConfig(path)
	if err != nil {
		return config, err
	}
	if err := config.EnsureDirs(); err != nil {
		return config, err
	}
	if err := os.MkdirAll(config.ArchiveRoot, 0o755); err != nil {
		return config, err
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		return config, err
	}
	return config, catalog.Close()
}
