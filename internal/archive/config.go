package archive

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultCap = int64(100_000_000)

type SourceConfig struct {
	Name    string
	Kind    string
	Path    string
	Account string
	Enabled bool
	Options map[string]any
	// File is the configuration file that defines the source, where toggles
	// are written: library.toml, a library's hosts/<id>.toml, or archive.toml.
	File string
}

type Config struct {
	Path              string
	Library           bool
	Executable        string
	CatalogPath       string
	ArchiveRoot       string
	StagingRoot       string
	VolumeID          string
	APIToken          string
	Host              string
	Port              int
	PackageCapBytes   int64
	UpcomingDays      int
	EligibleDays      int
	SnoozeDays        int
	StagingCapBytes   int64
	EnableReclamation bool
	ReleaseHookProven bool
	TL1URL            string
	TL1Token          string
	GitHubToken       string
	CPUIDLECeiling    float64
	IOMBPSCeiling     float64
	YieldPollSeconds  float64
	Sources           []SourceConfig

	// CaptureRoot holds raw captures of each host's sources; see capture.go.
	CaptureRoot        string
	CaptureGenerations int
}

func defaultConfig(path string) Config {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, "Library", "Application Support", "AI Work Archive")
	return Config{
		Path: path, CatalogPath: filepath.Join(root, "catalog.sqlite3"),
		ArchiveRoot: filepath.Join(root, "preserved"), StagingRoot: filepath.Join(root, "staging"),
		Host: "127.0.0.1", Port: 8765, PackageCapBytes: defaultCap,
		UpcomingDays: 23, EligibleDays: 30, SnoozeDays: 14, StagingCapBytes: 2_000_000_000,
		CPUIDLECeiling: .35, IOMBPSCeiling: 25, YieldPollSeconds: 2,
		CaptureRoot: filepath.Join(root, "captures"), CaptureGenerations: 1,
	}
}

func LoadConfig(path string) (Config, error) { return loadConfig(path, true) }

// loadConfig without hostSources leaves out the current host's source file,
// which needs this Mac's host ID (see detectHost): the MCP server has no use
// for sources and must not wait for ioreg.
func loadConfig(path string, hostSources bool) (Config, error) {
	if path == "" {
		path = os.Getenv("AIWA_CONFIG")
	}
	if path == "" {
		path = libraryConfigBeside(runningExecutable())
	}
	if path == "" {
		path = "archive.toml"
	}
	path = expandPath(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	// Relative paths resolve against the configuration's own directory, never
	// the process CWD, so a library can move with the drive that holds it.
	base := filepath.Dir(path)
	config := defaultConfig(path)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, fmt.Errorf("configuration is unavailable: %s", path)
		}
		return config, err
	}
	defer file.Close()
	root := map[string]any{}
	var source map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := stripTOMLComment(strings.TrimSpace(scanner.Text()))
		if line == "" {
			continue
		}
		if line == "[[sources]]" {
			if source != nil {
				config.Sources = append(config.Sources, sourceFromMap(source, base))
			}
			source = map[string]any{}
			continue
		}
		if strings.HasPrefix(line, "[") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value := parseTOMLScalar(strings.TrimSpace(raw))
		if source != nil {
			source[key] = value
		} else {
			root[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return config, err
	}
	if source != nil {
		config.Sources = append(config.Sources, sourceFromMap(source, base))
	}
	dataDir := resolvePath(base, stringValue(root, "data_dir", filepath.Dir(config.CatalogPath)))
	config.CatalogPath = resolvePath(base, stringValue(root, "catalog_path", filepath.Join(dataDir, "catalog.sqlite3")))
	config.ArchiveRoot = resolvePath(base, stringValue(root, "archive_root", filepath.Join(dataDir, "preserved")))
	config.StagingRoot = resolvePath(base, stringValue(root, "staging_root", filepath.Join(dataDir, "staging")))
	config.Library = boolValue(root, "library", false)
	// Captures belong with the library they feed. Outside a library they sit
	// beside the catalog rather than under archive_root, whose drive may be
	// absent (a missing mount point must never be recreated on the boot disk).
	captureRoot := filepath.Join(filepath.Dir(config.CatalogPath), "captures")
	if config.Library {
		captureRoot = filepath.Join(base, "captures")
	}
	config.CaptureRoot = resolvePath(base, stringValue(root, "capture_root", captureRoot))
	config.CaptureGenerations = intValue(root, "capture_snapshot_generations", config.CaptureGenerations)
	config.VolumeID = stringValue(root, "volume_id", "")
	config.APIToken = stringValue(root, "api_token", os.Getenv("AIWA_API_TOKEN"))
	config.Executable = resolvePath(base, stringValue(root, "executable", ""))
	config.Host = stringValue(root, "host", config.Host)
	config.Port = intValue(root, "port", config.Port)
	config.PackageCapBytes = int64Value(root, "package_cap_bytes", config.PackageCapBytes)
	config.UpcomingDays = intValue(root, "upcoming_days", config.UpcomingDays)
	config.EligibleDays = intValue(root, "eligible_days", config.EligibleDays)
	config.SnoozeDays = intValue(root, "snooze_days", config.SnoozeDays)
	config.StagingCapBytes = int64Value(root, "staging_cap_bytes", config.StagingCapBytes)
	config.EnableReclamation = boolValue(root, "enable_reclamation", false)
	config.ReleaseHookProven = boolValue(root, "release_hook_proven", false)
	config.TL1URL = stringValue(root, "tl1_url", "")
	config.TL1Token = stringValue(root, "tl1_token", os.Getenv("AIWA_TL1_TOKEN"))
	config.GitHubToken = stringValue(root, "github_token", os.Getenv("GITHUB_TOKEN"))
	config.CPUIDLECeiling = floatValue(root, "cpu_idle_ceiling", config.CPUIDLECeiling)
	config.IOMBPSCeiling = floatValue(root, "io_mbps_ceiling", config.IOMBPSCeiling)
	config.YieldPollSeconds = floatValue(root, "yield_poll_seconds", config.YieldPollSeconds)
	for index := range config.Sources {
		config.Sources[index].File = path
	}
	if config.Library && hostSources {
		if err := mergeHostSources(&config); err != nil {
			return config, err
		}
	}
	if config.APIToken == "" {
		config.APIToken = randomToken()
	}
	return config, nil
}

func (c Config) EnsureDirs() error {
	if err := os.MkdirAll(filepath.Dir(c.CatalogPath), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(c.StagingRoot, 0o755)
}

func sourceFromMap(value map[string]any, base string) SourceConfig {
	options := map[string]any{}
	for key, item := range value {
		switch key {
		case "name", "kind", "path", "account", "enabled":
		default:
			options[key] = item
		}
	}
	return SourceConfig{Name: stringValue(value, "name", ""), Kind: stringValue(value, "kind", ""),
		Path: resolvePath(base, stringValue(value, "path", "")), Account: stringValue(value, "account", "local"),
		Enabled: boolValue(value, "enabled", true), Options: options}
}

// resolvePath expands ~ and anchors a relative path at base. Empty stays empty
// so an unset path remains detectably unset.
func resolvePath(base, value string) string {
	value = expandPath(value)
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(base, value)
}

func parseTOMLScalar(value string) any {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if value[0] == '"' {
			if parsed, err := strconv.Unquote(value); err == nil {
				return parsed
			}
		}
		return value[1 : len(value)-1]
	}
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseInt(strings.ReplaceAll(value, "_", ""), 10, 64); err == nil {
		return parsed
	}
	if parsed, err := strconv.ParseFloat(strings.ReplaceAll(value, "_", ""), 64); err == nil {
		return parsed
	}
	return value
}

func stripTOMLComment(value string) string {
	quoted := byte(0)
	escaped := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quoted == '"' {
			escaped = true
			continue
		}
		if character == '"' || character == '\'' {
			if quoted == 0 {
				quoted = character
			} else if quoted == character {
				quoted = 0
			}
			continue
		}
		if character == '#' && quoted == 0 {
			return strings.TrimSpace(value[:index])
		}
	}
	return value
}

func stringValue(values map[string]any, key, fallback string) string {
	if value, ok := values[key]; ok {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return fallback
}

func boolValue(values map[string]any, key string, fallback bool) bool {
	if value, ok := values[key].(bool); ok {
		return value
	}
	return fallback
}

func intValue(values map[string]any, key string, fallback int) int {
	return int(int64Value(values, key, int64(fallback)))
}

func int64Value(values map[string]any, key string, fallback int64) int64 {
	switch value := values[key].(type) {
	case int64:
		return value
	case float64:
		return int64(value)
	}
	return fallback
}

func floatValue(values map[string]any, key string, fallback float64) float64 {
	switch value := values[key].(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	}
	return fallback
}

func randomToken() string {
	value := make([]byte, 24)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func SetSourceEnabled(path, name string, enabled bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(data), "\n")
	start, end := -1, len(lines)
	for index, line := range lines {
		trimmed := stripTOMLComment(strings.TrimSpace(line))
		if trimmed == "[[sources]]" {
			if start >= 0 {
				end = index
				break
			}
			candidateEnd := len(lines)
			for check := index + 1; check < len(lines); check++ {
				if strings.HasPrefix(strings.TrimSpace(lines[check]), "[") {
					candidateEnd = check
					break
				}
			}
			block := strings.Join(lines[index:candidateEnd], "")
			if regexpAssignment(block, "name") == name {
				start, end = index, candidateEnd
			}
		}
	}
	if start < 0 {
		return fmt.Errorf("configured source not found: %s", name)
	}
	replacement := fmt.Sprintf("enabled = %t\n", enabled)
	found := false
	for index := start + 1; index < end; index++ {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "enabled") && strings.Contains(trimmed, "=") {
			indent := lines[index][:len(lines[index])-len(strings.TrimLeft(lines[index], " \t"))]
			lines[index] = indent + replacement
			found = true
			break
		}
	}
	if !found {
		lines = append(lines[:end], append([]string{replacement}, lines[end:]...)...)
	}
	temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".alexandria.tmp")
	if err := os.WriteFile(temporary, []byte(strings.Join(lines, "")), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// SetRootString updates one top-level string without rewriting comments or
// source tables. It is used by the launcher so Finder can start the embedded
// Go service without depending on a shell PATH.
func SetRootString(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(data), "\n")
	tableAt := len(lines)
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			tableAt = index
			break
		}
	}
	replacement := fmt.Sprintf("%s = %q\n", key, value)
	found := -1
	for index := 0; index < tableAt; index++ {
		name, _, ok := strings.Cut(stripTOMLComment(strings.TrimSpace(lines[index])), "=")
		if ok && strings.TrimSpace(name) == key {
			found = index
			break
		}
	}
	if found >= 0 {
		lines[found] = replacement
	} else {
		lines = append(lines[:tableAt], append([]string{replacement}, lines[tableAt:]...)...)
	}
	temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".alexandria.tmp")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(temporary, []byte(strings.Join(lines, "")), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func regexpAssignment(block, key string) string {
	for _, line := range strings.Split(block, "\n") {
		name, value, ok := strings.Cut(stripTOMLComment(strings.TrimSpace(line)), "=")
		if ok && strings.TrimSpace(name) == key {
			return asString(parseTOMLScalar(strings.TrimSpace(value)))
		}
	}
	return ""
}

const exampleConfig = `# Pharos — all locations are opt-in; no home-directory scan is performed.
data_dir = "~/Library/Application Support/AI Work Archive"
archive_root = "/Volumes/euclid/Alexandria"
# Set this from ` + "`alexandria volume-id /Volumes/euclid`" + ` to reject a wrong volume.
volume_id = ""
host = "127.0.0.1"
port = 8765
package_cap_bytes = 100000000

[[sources]]
name = "tl1"
kind = "tl1"
path = "~/.tl1/registry.json"
enabled = false

[[sources]]
name = "conductor"
kind = "conductor"
path = "~/Library/Application Support/com.conductor.app"
enabled = false

[[sources]]
name = "codex"
kind = "codex"
path = "~/.codex"
enabled = false

[[sources]]
name = "claude"
kind = "claude"
path = "~/.claude/projects"
enabled = false
`

func InitConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	value := strings.Replace(exampleConfig, "host =", fmt.Sprintf("api_token = %q\nhost =", randomToken()), 1)
	return os.WriteFile(path, []byte(value), 0o600)
}
