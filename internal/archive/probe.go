package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The probe looks only in the fixed places where supported agents keep their
// conversations. It never walks the home directory, never enters a folder that
// macOS guards with a privacy prompt, and indexes nothing: a location becomes a
// source only when the user accepts it into this host's file.

const (
	probeWalkLimit      = 250_000 // directory entries walked per location
	probeWalkBudget     = 3 * time.Second
	probeSampleSize     = 50
	claudeCleanupDays   = 30 // Claude Code's cleanupPeriodDays default
	probeSmallFileLimit = 32 << 20
)

var probeEnvNames = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"}

// tccFolders are home-relative folders whose contents macOS guards with a
// privacy prompt (Files and Folders, App Data, iCloud, Mail, and the like).
var tccFolders = []string{"Desktop", "Documents", "Downloads", "Movies", "Music", "Pictures", ".Trash",
	"Library/Containers", "Library/Group Containers", "Library/Mobile Documents", "Library/CloudStorage",
	"Library/Mail", "Library/Messages", "Library/Safari", "Library/Calendars", "Library/Reminders",
	"Library/Cookies", "Library/HomeKit", "Library/IdentityServices", "Library/Suggestions",
	"Library/Metadata/CoreSpotlight", "Library/Application Support/AddressBook",
	"Library/Application Support/CallHistoryDB", "Library/Application Support/com.apple.TCC",
	"Library/Application Support/MobileSync"}

var (
	errProtected    = errors.New("inside a folder macOS protects with a permission prompt")
	sessionUUID     = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	sourceNameValid = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ProbeStatus is what the UI needs to decide whether to offer onboarding.
type ProbeStatus struct {
	Library         bool   `json:"library"`
	Host            Host   `json:"host"`
	KnownHost       bool   `json:"known_host"`
	HostFile        string `json:"host_file,omitempty"`
	HostFileDisplay string `json:"host_file_display,omitempty"`
	HostFileExists  bool   `json:"host_file_exists"`
	NeedsOnboarding bool   `json:"needs_onboarding"`
	HostSources     int    `json:"host_sources"`
	SharedSources   int    `json:"shared_sources"`
}

type ProbeReport struct {
	ProbeStatus
	Home        string           `json:"home"`
	ShellEnv    string           `json:"shell_env"`
	Environment []ProbeVariable  `json:"environment"`
	Candidates  []ProbeCandidate `json:"candidates"`
	Checked     []ProbeLocation  `json:"checked"`
	ElapsedMS   int64            `json:"elapsed_ms"`
	TimingsMS   map[string]int64 `json:"timings_ms"`
}

type ProbeVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	From  string `json:"from"`
}

type ProbeLocation struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	FoundBy string `json:"found_by"`
	Status  string `json:"status"`
}

type ProbeCandidate struct {
	Name          string           `json:"name"`
	Kind          string           `json:"kind"`
	Path          string           `json:"path,omitempty"`
	DisplayPath   string           `json:"display_path,omitempty"`
	FoundBy       string           `json:"found_by,omitempty"`
	Status        string           `json:"status"` // found, empty, protected, unreadable, manual
	Exists        bool             `json:"exists"`
	Readable      bool             `json:"readable"`
	Count         int              `json:"count"`
	Unit          string           `json:"unit,omitempty"`
	Files         int              `json:"files"`
	Bytes         int64            `json:"bytes"`
	Oldest        string           `json:"oldest,omitempty"`
	Newest        string           `json:"newest,omitempty"`
	Truncated     bool             `json:"truncated,omitempty"`
	RetentionDays int              `json:"retention_days,omitempty"`
	Retention     string           `json:"retention,omitempty"`
	Detail        string           `json:"detail,omitempty"`
	Configured    *ProbeConfigured `json:"configured,omitempty"`
	Overlap       *ProbeOverlap    `json:"overlap,omitempty"`
	Acceptable    bool             `json:"acceptable"`
	Recommended   bool             `json:"recommended"`

	resolved string
	baseName string
	samples  []probeSession
}

type ProbeConfigured struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope"` // host, library, or config
	File    string `json:"file"`
}

type ProbeOverlap struct {
	Sampled   int                `json:"sampled"`
	Total     int                `json:"total"`
	InLibrary int                `json:"in_library"`
	Estimated int                `json:"estimated_in_library"`
	Hosts     []ProbeOverlapHost `json:"hosts"`
	Summary   string             `json:"summary"`
}

type ProbeOverlapHost struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Current  bool   `json:"current"`
	Sessions int    `json:"sessions"`
}

type probeSession struct {
	id       string
	modified time.Time
}

type probeRun struct {
	home       string
	config     Config
	report     *ProbeReport
	candidates []*ProbeCandidate
	seen       map[string]*ProbeCandidate
	deadline   time.Time
}

// probeShellEnv reads variables from the login shell; tests replace it.
var probeShellEnv = cachedLoginShellEnv

// probeTempDirs are where ephemeral TL1 installations live; tests replace it.
var probeTempDirs = func() []string {
	temporary := filepath.Clean(os.TempDir())
	dirs := []string{temporary}
	if resolved, err := filepath.EvalSymlinks(temporary); err == nil && resolved != temporary {
		dirs = append(dirs, resolved)
	}
	return dirs
}

func probeStatus(config Config, catalog *Catalog) ProbeStatus {
	host := currentHost()
	status := ProbeStatus{Library: config.Library, Host: host, HostFile: hostConfigPath(config)}
	if status.HostFile != "" {
		status.HostFileDisplay = filepath.ToSlash(filepath.Join(hostsDirName, filepath.Base(status.HostFile)))
		_, err := os.Stat(status.HostFile)
		status.HostFileExists = err == nil
		status.NeedsOnboarding = !status.HostFileExists
	}
	for _, source := range config.Sources {
		if status.HostFile != "" && source.File == status.HostFile {
			status.HostSources++
		} else {
			status.SharedSources++
		}
	}
	if catalog != nil {
		_ = catalog.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM source_states WHERE host_id=?1)
			OR EXISTS(SELECT 1 FROM workspace_sightings WHERE host_id=?1)`, host.ID).Scan(&status.KnownHost)
	}
	return status
}

// ProbeSources reports candidate sources on this Mac for config's host.
func ProbeSources(config Config, catalog *Catalog) ProbeReport {
	home, _ := os.UserHomeDir()
	return probeSources(home, config, catalog)
}

func probeSources(home string, config Config, catalog *Catalog) ProbeReport {
	started := time.Now()
	report := ProbeReport{ProbeStatus: probeStatus(config, catalog), Home: home, Environment: []ProbeVariable{},
		Candidates: []ProbeCandidate{}, Checked: []ProbeLocation{}, TimingsMS: map[string]int64{}}
	run := &probeRun{home: home, config: config, report: &report, seen: map[string]*ProbeCandidate{}, deadline: started.Add(probeWalkBudget)}
	type shellResult struct {
		values map[string]string
		err    error
	}
	// The login shell takes about half a second to start; read it while the
	// default locations are walked.
	shell := make(chan shellResult, 1)
	go func() {
		values, err := probeShellEnv(probeEnvNames)
		shell <- shellResult{values, err}
	}()
	timed := func(name string, probe func()) {
		begin := time.Now()
		probe()
		report.TimingsMS[name] += time.Since(begin).Milliseconds()
	}
	timed("claude", func() {
		run.claude(filepath.Join(home, ".claude"), "default location", false)
		run.claude(filepath.Join(home, ".config", "claude"), "default location", false)
		// Alternate profiles such as ~/.claude-work are usually selected with
		// CLAUDE_CONFIG_DIR from a shell alias, which no environment reveals.
		if entries, err := os.ReadDir(home); err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".claude") && entry.Name() != ".claude" && (entry.IsDir() || entry.Type()&fs.ModeSymlink != 0) {
					run.claude(filepath.Join(home, entry.Name()), "~/.claude* directory", true)
				}
			}
		}
	})
	timed("codex", func() { run.codex(filepath.Join(home, ".codex"), "default location") })
	timed("conductor", func() {
		run.conductor(filepath.Join(home, "Library", "Application Support", "com.conductor.app"), "default location")
	})
	timed("tl1", func() { run.tl1(filepath.Join(home, ".tl1", "registry.json"), "default location") })
	environment := func(values map[string]string, from string) {
		for _, name := range probeEnvNames {
			value := values[name]
			if value == "" {
				continue
			}
			report.Environment = append(report.Environment, ProbeVariable{Name: name, Value: value, From: from})
			foundBy := name + " (" + from + ")"
			path := resolvePath(home, value)
			if name == "CLAUDE_CONFIG_DIR" {
				timed("claude", func() { run.claude(path, foundBy, false) })
			} else {
				timed("codex", func() { run.codex(path, foundBy) })
			}
		}
	}
	process := map[string]string{}
	for _, name := range probeEnvNames {
		process[name] = os.Getenv(name)
	}
	environment(process, "service environment")
	begin := time.Now()
	result := <-shell
	report.TimingsMS["shell_env_wait"] = time.Since(begin).Milliseconds()
	switch {
	case result.err != nil:
		report.ShellEnv = "unavailable: " + result.err.Error()
	case result.values == nil:
		report.ShellEnv = "disabled"
	default:
		report.ShellEnv = "read"
		environment(result.values, "login shell")
	}
	run.name()
	timed("overlap", func() { run.overlap(catalog) })
	for _, candidate := range run.candidates {
		candidate.Acceptable = candidate.Path != ""
		candidate.Recommended = candidate.Acceptable && candidate.Status == "found" && candidate.Configured == nil
		report.Candidates = append(report.Candidates, *candidate)
	}
	report.Candidates = append(report.Candidates, ProbeCandidate{Name: "chatgpt", Kind: "chatgpt", Status: "manual",
		Detail: "ChatGPT keeps conversations in the cloud, so there is nothing to find on this Mac. Export them (ChatGPT → Settings → Data controls → Export data), unzip the archive, and add a [[sources]] block with kind = \"chatgpt\" whose path is the folder holding conversations.json. Inside the library a relative path works on every Mac."})
	report.ElapsedMS = time.Since(started).Milliseconds()
	return report
}

// locate resolves a location without entering protected folders, records it
// as checked, and returns a new candidate, or nil if it is missing or was
// already found another way.
func (r *probeRun) locate(kind, path, foundBy string) (*ProbeCandidate, string) {
	path = filepath.Clean(path)
	display := tildeWithin(r.home, path)
	resolved, err := resolveUnprotected(r.home, path)
	status := "found"
	switch {
	case errors.Is(err, errProtected):
		status = "protected"
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		r.report.Checked = append(r.report.Checked, ProbeLocation{kind, display, foundBy, "missing"})
		return nil, ""
	case err != nil:
		status = "unreadable"
	}
	key := kind + "\x00" + resolved
	if existing := r.seen[key]; existing != nil {
		if !strings.Contains(existing.FoundBy, foundBy) {
			existing.FoundBy += "; " + foundBy
		}
		r.report.Checked = append(r.report.Checked, ProbeLocation{kind, display, foundBy, "same as " + existing.DisplayPath})
		return nil, ""
	}
	candidate := &ProbeCandidate{Kind: kind, Path: path, DisplayPath: display, FoundBy: foundBy, Status: status, Exists: status != "protected", Readable: status == "found", resolved: resolved}
	// Adapters walk without following a symbolic link at the root itself.
	if info, err := os.Lstat(path); status == "found" && err == nil && info.Mode()&fs.ModeSymlink != 0 {
		candidate.Path = resolved
	}
	switch status {
	case "protected":
		candidate.Detail = "Not scanned: " + resolved + " is " + errProtected.Error() + ". Adding it may make macOS ask for access when Pharos syncs."
	case "unreadable":
		candidate.Detail = "Could not be read: " + err.Error()
	}
	r.seen[key] = candidate
	return candidate, resolved
}

func (r *probeRun) keep(candidate *ProbeCandidate) {
	r.candidates = append(r.candidates, candidate)
	r.report.Checked = append(r.report.Checked, ProbeLocation{candidate.Kind, candidate.DisplayPath, candidate.FoundBy, candidate.Status})
}

func (r *probeRun) claude(configDir, foundBy string, requireSessions bool) {
	candidate, resolved := r.locate("claude", filepath.Join(configDir, "projects"), foundBy)
	if candidate == nil {
		return
	}
	candidate.Unit, candidate.baseName = "session", filepath.Base(configDir)
	if candidate.Status == "found" {
		// Top-level transcripts are sessions; subagents/ holds their helpers.
		stats, err := walkTranscripts(resolved, r.deadline, func(relative string) (string, bool) {
			if parts := strings.Split(relative, "/"); len(parts) == 2 {
				return strings.TrimSuffix(parts[1], filepath.Ext(parts[1])), true
			}
			return "", false
		})
		stats.apply(candidate, err)
		days, where := claudeCleanupPeriod(r.home, configDir)
		candidate.RetentionDays = days
		candidate.Retention = fmt.Sprintf("Claude Code deletes transcripts %d days after their last activity (%s).", days, where)
	}
	if requireSessions && candidate.Status == "empty" {
		// Forgotten, so CLAUDE_CONFIG_DIR naming it still reports it.
		delete(r.seen, "claude\x00"+resolved)
		r.report.Checked = append(r.report.Checked, ProbeLocation{"claude", candidate.DisplayPath, foundBy, "empty"})
		return
	}
	r.keep(candidate)
}

func claudeCleanupPeriod(home, configDir string) (int, string) {
	path := filepath.Join(configDir, "settings.json")
	if resolved, err := resolveUnprotected(home, path); err == nil {
		if data, err := readSmallFile(resolved); err == nil {
			var settings map[string]any
			if json.Unmarshal(data, &settings) == nil {
				if days, ok := settings["cleanupPeriodDays"].(float64); ok && days >= 0 {
					return int(days), "cleanupPeriodDays in " + tildeWithin(home, path)
				}
			}
		}
	}
	return claudeCleanupDays, "the default; " + tildeWithin(home, path) + " does not set cleanupPeriodDays"
}

func (r *probeRun) codex(dir, foundBy string) {
	candidate, resolved := r.locate("codex", dir, foundBy)
	if candidate == nil {
		return
	}
	candidate.Unit, candidate.baseName = "session", filepath.Base(dir)
	if candidate.Status == "found" {
		var total probeStats
		var firstErr error
		present := false
		for _, name := range []string{"sessions", "archived_sessions"} {
			root, err := resolveUnprotected(r.home, filepath.Join(resolved, name))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			present = true
			if err == nil {
				var stats probeStats
				stats, err = walkTranscripts(root, r.deadline, func(relative string) (string, bool) {
					ids := sessionUUID.FindAllString(filepath.Base(relative), -1)
					if len(ids) == 0 {
						return "", true
					}
					return strings.ToLower(ids[len(ids)-1]), true
				})
				total.add(stats)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if !present {
			r.report.Checked = append(r.report.Checked, ProbeLocation{"codex", candidate.DisplayPath, foundBy, "no sessions or archived_sessions folder"})
			return
		}
		total.apply(candidate, firstErr)
		if firstErr != nil && total.files > 0 {
			candidate.Status, candidate.Readable, candidate.Detail = "found", true, "Partly readable: "+firstErr.Error()
		}
	}
	r.keep(candidate)
}

func (r *probeRun) conductor(dir, foundBy string) {
	candidate, resolved := r.locate("conductor", dir, foundBy)
	if candidate == nil {
		return
	}
	candidate.Unit, candidate.baseName = "database", "conductor"
	if candidate.Status == "found" {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			candidate.Status, candidate.Readable, candidate.Detail = "unreadable", false, "Could not be read: "+err.Error()
			r.keep(candidate)
			return
		}
		// Only databases at the top of the folder are checked, by table
		// names alone, so the probe stays fast next to multi-GB databases.
		details := []string{}
		var newest time.Time
		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if entry.IsDir() || !(strings.HasSuffix(name, ".db") || strings.Contains(name, ".sqlite")) || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal") {
				continue
			}
			path := filepath.Join(resolved, entry.Name())
			tables, err := sqliteTables(path)
			if err != nil {
				details = append(details, entry.Name()+" unreadable ("+err.Error()+")")
				continue
			}
			sessions, messages := firstPresent(tables, "sessions", "workspaces", "threads"), firstPresent(tables, "messages", "session_messages")
			if sessions == "" || messages == "" {
				continue
			}
			candidate.Count++
			for _, file := range []string{path, path + "-wal"} {
				if info, err := os.Stat(file); err == nil {
					candidate.Files++
					candidate.Bytes += info.Size()
					if info.ModTime().After(newest) {
						newest = info.ModTime()
					}
				}
			}
			details = append(details, fmt.Sprintf("%s (%s, %s)", entry.Name(), sessions, messages))
		}
		candidate.Detail = strings.Join(details, "; ")
		if candidate.Count == 0 {
			candidate.Status = "empty"
		} else {
			candidate.Newest = newest.UTC().Format(time.RFC3339)
		}
	}
	r.keep(candidate)
}

func sqliteTables(path string) (map[string]bool, error) {
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := queryMapsContext(ctx, db, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	tables := map[string]bool{}
	for _, row := range rows {
		tables[firstString(row["name"])] = true
	}
	return tables, nil
}

func (r *probeRun) tl1(registry, foundBy string) {
	candidate, resolved := r.locate("tl1", registry, foundBy)
	if candidate == nil {
		return
	}
	candidate.Unit, candidate.baseName = "installation", "tl1"
	if candidate.Status == "found" {
		r.inspectTL1(candidate, resolved)
	}
	r.keep(candidate)
}

// inspectTL1 applies the TL1 adapter's installation rules without entering
// protected folders: installations whose repository is in a temporary
// directory (test runs) are skipped, as are those with missing files.
func (r *probeRun) inspectTL1(candidate *ProbeCandidate, registry string) {
	data, err := readSmallFile(registry)
	var parsed struct {
		Installations []map[string]any `json:"installations"`
	}
	if err == nil {
		err = json.Unmarshal(data, &parsed)
	}
	if err != nil {
		candidate.Status, candidate.Readable, candidate.Detail = "unreadable", false, "Could not read the TL1 registry: "+err.Error()
		return
	}
	temporary := probeTempDirs()
	within := func(path string, roots []string) bool {
		for _, root := range roots {
			if pathWithin(root, path) {
				return true
			}
		}
		return false
	}
	registryTemporary := within(registry, temporary)
	projects := []string{}
	named := map[string]bool{}
	seen := map[string]bool{}
	skipped, protected := 0, 0
	var newest time.Time
	for _, row := range parsed.Installations {
		database, configPath, repository := expandPath(firstString(row["db_path"])), expandPath(firstString(row["config_path"])), expandPath(firstString(row["code_repo"]))
		key := defaultString(row["installation_id"], database) + "\x1f" + database
		if database == "" || configPath == "" || repository == "" || seen[key] || (!registryTemporary && within(filepath.Clean(repository), temporary)) {
			skipped++
			continue
		}
		if protectedLocation(r.home, database) || protectedLocation(r.home, configPath) || protectedLocation(r.home, repository) {
			protected++
			continue
		}
		dbInfo, dbErr := os.Stat(database)
		configInfo, configErr := os.Stat(configPath)
		repoInfo, repoErr := os.Stat(repository)
		if dbErr != nil || configErr != nil || repoErr != nil || !dbInfo.Mode().IsRegular() || !configInfo.Mode().IsRegular() || !repoInfo.IsDir() {
			skipped++
			continue
		}
		seen[key] = true
		candidate.Count++
		candidate.Files++
		candidate.Bytes += dbInfo.Size()
		if dbInfo.ModTime().After(newest) {
			newest = dbInfo.ModTime()
		}
		if project := firstString(row["project_name"]); project != "" && !named[project] {
			named[project] = true
			projects = append(projects, project)
		}
	}
	parts := []string{}
	if len(projects) > 0 {
		parts = append(parts, "Projects: "+strings.Join(projects, ", "))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d temporary or missing installations skipped", skipped))
	}
	if protected > 0 {
		parts = append(parts, fmt.Sprintf("%d installations in protected folders not checked", protected))
	}
	candidate.Detail = strings.Join(parts, " · ")
	if candidate.Count == 0 {
		candidate.Status = "empty"
	} else {
		candidate.Newest = newest.UTC().Format(time.RFC3339)
	}
}

type probeStats struct {
	files, sessions int
	bytes           int64
	oldest, newest  time.Time
	truncated       bool
	samples         []probeSession
}

func (s *probeStats) add(other probeStats) {
	s.files += other.files
	s.sessions += other.sessions
	s.bytes += other.bytes
	s.truncated = s.truncated || other.truncated
	s.samples = append(s.samples, other.samples...)
	if !other.oldest.IsZero() && (s.oldest.IsZero() || other.oldest.Before(s.oldest)) {
		s.oldest = other.oldest
	}
	if other.newest.After(s.newest) {
		s.newest = other.newest
	}
}

func (s probeStats) apply(candidate *ProbeCandidate, err error) {
	if err != nil && s.files == 0 {
		candidate.Status, candidate.Readable, candidate.Detail = "unreadable", false, "Could not be read: "+err.Error()
		return
	}
	candidate.Count, candidate.Files, candidate.Bytes, candidate.Truncated, candidate.samples = s.sessions, s.files, s.bytes, s.truncated, s.samples
	if s.files == 0 {
		candidate.Status = "empty"
		return
	}
	candidate.Oldest, candidate.Newest = s.oldest.UTC().Format(time.RFC3339), s.newest.UTC().Format(time.RFC3339)
	if s.truncated {
		candidate.Detail = "Counting stopped early; totals are lower bounds."
	}
}

// walkTranscripts measures the *.jsonl files under root. classify maps a
// root-relative slash path to a session ID and whether the file is a session.
func walkTranscripts(root string, deadline time.Time, classify func(string) (string, bool)) (probeStats, error) {
	var stats probeStats
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if visited++; visited > probeWalkLimit || (visited%512 == 0 && time.Now().After(deadline)) {
			stats.truncated = true
			return filepath.SkipAll
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		stats.files++
		stats.bytes += info.Size()
		if modified := info.ModTime(); stats.oldest.IsZero() || modified.Before(stats.oldest) {
			stats.oldest = modified
		}
		if info.ModTime().After(stats.newest) {
			stats.newest = info.ModTime()
		}
		relative, _ := filepath.Rel(root, path)
		if id, session := classify(filepath.ToSlash(relative)); session {
			stats.sessions++
			if id != "" {
				stats.samples = append(stats.samples, probeSession{id, info.ModTime()})
			}
		}
		return nil
	})
	return stats, err
}

// name gives every candidate a source name that is stable on this host and
// unique among configured sources: a configured location keeps its name.
func (r *probeRun) name() {
	taken := map[string]bool{}
	for _, source := range r.config.Sources {
		taken[source.Name] = true
	}
	hostFile := r.report.HostFile
	for _, candidate := range r.candidates {
		for _, source := range r.config.Sources {
			if !strings.EqualFold(source.Kind, candidate.Kind) || source.Path == "" {
				continue
			}
			path := filepath.Clean(source.Path)
			if resolved, err := resolveUnprotected(r.home, path); err == nil || errors.Is(err, errProtected) {
				path = resolved
			}
			if pathWithin(path, candidate.resolved) || pathWithin(candidate.resolved, path) {
				scope := "config"
				if hostFile != "" && source.File == hostFile {
					scope = "host"
				} else if r.config.Library {
					scope = "library"
				}
				candidate.Configured = &ProbeConfigured{Name: source.Name, Enabled: source.Enabled, Scope: scope, File: source.File}
				candidate.Name = source.Name
				break
			}
		}
	}
	for _, candidate := range r.candidates {
		if candidate.Configured != nil {
			continue
		}
		base := suggestedSourceName(candidate.Kind, candidate.baseName)
		name := base
		for index := 2; taken[name]; index++ {
			name = base + "-" + strconv.Itoa(index)
		}
		taken[name] = true
		candidate.Name = name
	}
}

// suggestedSourceName derives "claude-work" from ~/.claude-work and "claude"
// from ~/.claude or ~/.config/claude.
func suggestedSourceName(kind, base string) string {
	base = strings.Trim(strings.ToLower(unsafeFileName.ReplaceAllString(strings.TrimLeft(base, "."), "-")), "-.")
	switch {
	case base == "" || base == kind:
		return kind
	case strings.HasPrefix(base, kind+"-"):
		return base
	}
	return kind + "-" + base
}

func (r *probeRun) overlap(catalog *Catalog) {
	if catalog == nil {
		return
	}
	for _, candidate := range r.candidates {
		if len(candidate.samples) == 0 {
			continue
		}
		overlap, err := catalog.sampleOverlap(candidate.Kind, sampleSessions(candidate.samples, probeSampleSize), candidate.Count)
		if err == nil {
			candidate.Overlap = overlap
		}
	}
}

// sampleSessions picks up to n sessions spread evenly from oldest to newest.
func sampleSessions(sessions []probeSession, n int) []string {
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].modified.Equal(sessions[j].modified) {
			return sessions[i].modified.Before(sessions[j].modified)
		}
		return sessions[i].id < sessions[j].id
	})
	ids := []string{}
	seen := map[string]bool{}
	for index := 0; index < n && index < len(sessions); index++ {
		id := sessions[index*len(sessions)/min(n, len(sessions))].id
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// sampleOverlap reports how many sampled sessions the library already holds
// and which hosts indexed them.
func (c *Catalog) sampleOverlap(kind string, ids []string, total int) (*ProbeOverlap, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	arguments := []any{kind}
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	rows, err := queryMaps(c.DB, `SELECT w.source_id id,COALESCE(s.host_id,'') host_id,COALESCE(h.label,'') label
		FROM workspaces w LEFT JOIN workspace_sightings s ON s.workspace_id=w.id LEFT JOIN hosts h ON h.id=s.host_id
		WHERE w.source_kind=? AND w.source_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, arguments...)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	byHost := map[string]map[string]bool{}
	labels := map[string]string{}
	for _, row := range rows {
		id, host := firstString(row["id"]), firstString(row["host_id"])
		found[id] = true
		if host == "" {
			continue
		}
		if byHost[host] == nil {
			byHost[host] = map[string]bool{}
		}
		byHost[host][id] = true
		labels[host] = defaultString(row["label"], host)
	}
	overlap := &ProbeOverlap{Sampled: len(ids), Total: max(total, len(ids)), InLibrary: len(found), Hosts: []ProbeOverlapHost{}}
	overlap.Estimated = int(float64(overlap.Total)*float64(overlap.InLibrary)/float64(overlap.Sampled) + .5)
	for host, sessions := range byHost {
		overlap.Hosts = append(overlap.Hosts, ProbeOverlapHost{ID: host, Label: labels[host], Current: host == currentHost().ID, Sessions: len(sessions)})
	}
	sort.Slice(overlap.Hosts, func(i, j int) bool {
		if overlap.Hosts[i].Sessions != overlap.Hosts[j].Sessions {
			return overlap.Hosts[i].Sessions > overlap.Hosts[j].Sessions
		}
		return overlap.Hosts[i].Label < overlap.Hosts[j].Label
	})
	overlap.Summary = overlapSummary(*overlap)
	return overlap, nil
}

func overlapSummary(overlap ProbeOverlap) string {
	sample := fmt.Sprintf("%s sampled sessions", groupDigits(overlap.Sampled))
	if overlap.Sampled == overlap.Total {
		sample = fmt.Sprintf("its %s sessions", groupDigits(overlap.Total))
	}
	if overlap.InLibrary == 0 {
		if overlap.Sampled == 1 && overlap.Total == 1 {
			return "Its session is not in the library yet."
		}
		return "None of " + sample + " are in the library yet."
	}
	verb := "are"
	if overlap.InLibrary == 1 {
		verb = "is"
	}
	text := fmt.Sprintf("%s of %s %s already in the library", groupDigits(overlap.InLibrary), sample, verb)
	names := []string{}
	for _, host := range overlap.Hosts {
		name := host.Label
		if host.Current {
			name = "this Mac"
		}
		names = append(names, name)
	}
	switch len(names) {
	case 0:
	case 1:
		text += ", indexed from " + names[0]
	default:
		text += ", indexed from " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	return text + "."
}

func groupDigits(value int) string {
	text := strconv.Itoa(value)
	for index := len(text) - 3; index > 0 && text[index-1] != '-'; index -= 3 {
		text = text[:index] + "," + text[index:]
	}
	return text
}

// protectedLocation reports whether touching path could raise a macOS
// privacy prompt. Other volumes count too: removable and network volumes
// have their own prompts.
func protectedLocation(home, path string) bool {
	path = filepath.Clean(path)
	if home != "" && pathWithin(home, path) {
		relative, _ := filepath.Rel(home, path)
		relative = strings.ToLower(filepath.ToSlash(relative))
		for _, folder := range tccFolders {
			folder = strings.ToLower(folder)
			if relative == folder || strings.HasPrefix(relative, folder+"/") {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(path, "/Volumes/")
}

// resolveUnprotected follows symbolic links like filepath.EvalSymlinks, but
// checks each location before touching it, so a link into ~/Documents is
// reported with errProtected instead of being followed into a prompt.
func resolveUnprotected(home, path string) (string, error) {
	pending := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	current, links := "/", 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, part)
		if protectedLocation(home, next) {
			return filepath.Join(append([]string{next}, pending...)...), errProtected
		}
		info, err := os.Lstat(next)
		if err != nil {
			return filepath.Join(append([]string{next}, pending...)...), err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			current = next
			continue
		}
		if links++; links > 40 {
			return path, fmt.Errorf("too many symbolic links: %s", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return next, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(current, target)
		}
		pending = append(strings.Split(strings.TrimPrefix(filepath.Clean(target), "/"), "/"), pending...)
		current = "/"
	}
	return current, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, "../")
}

func tildeWithin(home, path string) string {
	if home != "" && pathWithin(home, path) {
		if relative, _ := filepath.Rel(home, path); relative != "." {
			return "~/" + filepath.ToSlash(relative)
		}
		return "~"
	}
	return path
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, probeSmallFileLimit+1))
	if err == nil && len(data) > probeSmallFileLimit {
		err = fmt.Errorf("%s is larger than %d bytes", path, probeSmallFileLimit)
	}
	return data, err
}
