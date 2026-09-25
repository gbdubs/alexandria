package archive

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Source locations differ between Macs, so a library keeps each host's
// [[sources]] in <library>/hosts/<host-id>.toml. Sources in library.toml apply
// on every Mac; a host file's source replaces a library.toml source of the same
// name on that host only.
const hostsDirName = "hosts"

// hostFileMu serializes rewrites of host files by this process.
var hostFileMu sync.Mutex

var unsafeFileName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// hostConfigPath is the current host's source file, or "" for a configuration
// that is not a library, or while this Mac has only a fallback ID.
func hostConfigPath(config Config) string {
	if !config.Library || config.Path == "" || currentHost().Fallback {
		return ""
	}
	name := strings.Trim(unsafeFileName.ReplaceAllString(currentHost().ID, "_"), ".")
	if name == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(config.Path), hostsDirName, name+".toml")
}

// mergeHostSources adds the current host's sources to a library's config.
func mergeHostSources(config *Config) error {
	path := hostConfigPath(*config)
	if path == "" {
		return nil
	}
	hosted, err := readSourceBlocks(path, filepath.Dir(config.Path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read host sources %s: %w", path, err)
	}
	names := map[string]bool{}
	for _, source := range hosted {
		names[source.Name] = true
	}
	for _, source := range config.Sources {
		if !names[source.Name] {
			hosted = append(hosted, source)
		}
	}
	config.Sources = hosted
	return nil
}

// readSourceBlocks reads only the [[sources]] tables of a file in the
// configuration format; top-level keys in a host file are ignored.
func readSourceBlocks(path, base string) ([]SourceConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sources := []SourceConfig{}
	var block map[string]any
	flush := func() {
		if block != nil {
			source := sourceFromMap(block, base)
			source.File = path
			sources = append(sources, source)
		}
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := stripTOMLComment(strings.TrimSpace(scanner.Text()))
		switch {
		case line == "[[sources]]":
			flush()
			block = map[string]any{}
		case strings.HasPrefix(line, "["):
			flush()
			block = nil
		case block != nil:
			if key, raw, ok := strings.Cut(line, "="); ok {
				block[strings.TrimSpace(key)] = parseTOMLScalar(strings.TrimSpace(raw))
			}
		}
	}
	flush()
	return sources, scanner.Err()
}

func hostFileHeader(host Host) string {
	who := host.Label
	if host.User != "" {
		who += " (user " + host.User + ")"
	}
	return fmt.Sprintf(`# Pharos sources for %s.
# Host ID: %s
# These [[sources]] apply only on this Mac and replace any same-named source
# in library.toml. "~" is this user's home directory; relative paths resolve
# against the library directory. Safe to edit by hand.
`, strings.ReplaceAll(who, "\n", " "), host.ID)
}

func sourceBlock(source SourceConfig) string {
	return fmt.Sprintf("[[sources]]\nname = %q\nkind = %q\npath = %q\nenabled = %t\n", source.Name, source.Kind, tildePath(source.Path), source.Enabled)
}

// tildePath writes a home-relative path as ~/..., which keeps a host file
// valid if the user's home directory is renamed.
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if relative, err := filepath.Rel(home, path); err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
		if relative == "." {
			return "~"
		}
		return "~/" + filepath.ToSlash(relative)
	}
	return path
}

// appendHostSources creates the host file (header only when sources is empty)
// or appends blocks to it, preserving what the user wrote.
func appendHostSources(path string, host Host, sources []SourceConfig) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(hostFileHeader(host))
	} else if err != nil {
		return err
	} else if len(sources) == 0 {
		return nil
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	for _, source := range sources {
		text += "\n" + sourceBlock(source)
	}
	return writeFileAtomic(path, []byte(text), 0o600)
}

// writeFileAtomic replaces path in one rename, so a reader or an unplugged
// drive sees either the old file or the complete new one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
