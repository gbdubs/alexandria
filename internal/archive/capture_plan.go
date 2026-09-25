package archive

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type captureItem struct {
	path string // original, absolute
	rel  string // slash-separated, relative to files/
}

type capturePlan struct {
	files, databases []captureItem
	// skipped exist but are not captured (databases without conversations),
	// so they are not missing either.
	skipped []string
	// complete is false when part of the source could not be listed; nothing
	// may then be marked missing.
	complete      bool
	warnings      []captureError
	installations []capturedInstallation
	pathMap       []capturePathMapping
}

// planCapture lists exactly what the source's adapter would read.
func planCapture(source SourceConfig) (capturePlan, error) {
	plan := capturePlan{complete: true}
	info, err := os.Stat(source.Path)
	if err != nil {
		return plan, fmt.Errorf("source path is unavailable: %w", err)
	}
	rootRel := captureFilesDir
	if !info.IsDir() {
		rootRel = path.Join(captureFilesDir, filepath.Base(source.Path))
	}
	plan.pathMap = []capturePathMapping{{Original: source.Path, Captured: rootRel}}
	jsonl := func(file string) bool { return strings.HasSuffix(strings.ToLower(file), ".jsonl") }
	switch strings.ToLower(source.Kind) {
	case "claude":
		plan.files = plan.walk(source.Path, info, "", jsonl)
	case "codex":
		adapter := jsonlAdapter{baseAdapter: baseAdapter{config: source}, provider: "codex"}
		plan.files = plan.walk(source.Path, info, "", func(file string) bool { return jsonl(file) && adapter.accept(file) })
	case "canonical", "tl1-export":
		plan.files = plan.walk(source.Path, info, "", func(file string) bool {
			return !info.IsDir() || strings.HasSuffix(strings.ToLower(file), ".json")
		})
	case "chatgpt", "chatgpt-export":
		file := (&chatGPTAdapter{baseAdapter{config: source}}).exportFile()
		if _, err := os.Stat(file); err != nil {
			return plan, fmt.Errorf("ChatGPT export is unavailable: %w", err)
		}
		rel, _ := filepath.Rel(source.Path, file)
		if !info.IsDir() {
			rel = filepath.Base(file)
		}
		plan.files = []captureItem{{file, filepath.ToSlash(rel)}}
	case "conductor":
		// The Conductor adapter's candidates: every SQLite file, of which it
		// reads those holding sessions and messages (not, say, cache.db).
		candidates := plan.walk(source.Path, info, "", func(file string) bool {
			if !info.IsDir() {
				return true
			}
			name := strings.ToLower(filepath.Base(file))
			return (strings.HasSuffix(name, ".db") || strings.Contains(name, ".sqlite")) &&
				!strings.HasSuffix(name, "-wal") && !strings.HasSuffix(name, "-shm") && !strings.HasSuffix(name, "-journal")
		})
		for _, candidate := range candidates {
			if ok, err := conductorConversationDatabase(candidate.path); err != nil {
				plan.warnings = append(plan.warnings, captureError{Path: candidate.path, Error: err.Error()})
				plan.skipped = append(plan.skipped, candidate.rel)
			} else if ok {
				plan.databases = append(plan.databases, candidate)
			} else {
				plan.skipped = append(plan.skipped, candidate.rel)
			}
		}
	case "tl1":
		return plan, plan.tl1(source)
	default:
		return plan, fmt.Errorf("unsupported source kind: %s", source.Kind)
	}
	return plan, nil
}

func (p *capturePlan) walk(root string, info fs.FileInfo, prefix string, include func(string) bool) []captureItem {
	if !info.IsDir() {
		if include(root) {
			return []captureItem{{root, path.Join(prefix, filepath.Base(root))}}
		}
		return nil
	}
	items := []captureItem{}
	_ = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			p.complete = false
			p.warnings = append(p.warnings, captureError{Path: file, Error: err.Error()})
			return nil
		}
		if entry.IsDir() || !include(file) {
			return nil
		}
		rel, _ := filepath.Rel(root, file)
		items = append(items, captureItem{file, path.Join(prefix, filepath.ToSlash(rel))})
		return nil
	})
	return items
}

func conductorConversationDatabase(file string) (bool, error) {
	db, err := openReadOnlySQLite(file)
	if err != nil {
		return false, err
	}
	defer db.Close()
	tables, err := tableNames(db)
	if err != nil {
		return false, err
	}
	return firstPresent(tables, "sessions", "workspaces", "threads") != "" && firstPresent(tables, "messages", "session_messages") != "", nil
}

// tl1 captures the registry, and for each installation the TL1 adapter would
// read, a snapshot of its database and its transcripts and script logs. The
// registry keeps absolute paths, so the manifest maps each to its capture.
func (p *capturePlan) tl1(source SourceConfig) error {
	registry := filepath.Base(source.Path)
	p.files = []captureItem{{source.Path, registry}}
	installations, err := (&tl1Adapter{baseAdapter: baseAdapter{config: source}}).installations()
	if err != nil {
		return err
	}
	databases, transcripts, keys := map[string]string{}, map[string]string{}, map[string]bool{}
	for _, installation := range installations {
		key := installation.ID
		if !validCaptureName(key) {
			key = "installation-" + hashBytes([]byte(installation.ID))[:16]
		}
		if keys[key] {
			key += "-" + hashBytes([]byte(installation.Database))[:8]
		}
		keys[key] = true
		record := capturedInstallation{ID: installation.ID, Project: installation.Project, DBPath: installation.Database, CodeRepo: installation.Repository, TranscriptsDir: installation.Transcripts, ConfigPath: installation.ConfigPath}
		rel, seen := databases[installation.Database]
		if !seen {
			rel = path.Join("installations", key, filepath.Base(installation.Database))
			databases[installation.Database] = rel
			p.databases = append(p.databases, captureItem{installation.Database, rel})
			p.pathMap = append(p.pathMap, capturePathMapping{Original: installation.Database, Captured: path.Join(captureFilesDir, rel)})
		}
		record.CapturedDB = path.Join(captureFilesDir, rel)
		if installation.Transcripts != "" {
			rel, seen := transcripts[installation.Transcripts]
			if !seen {
				rel = path.Join("installations", key, "transcripts")
				transcripts[installation.Transcripts] = rel
				if info, err := os.Stat(installation.Transcripts); err == nil && info.IsDir() {
					p.files = append(p.files, p.walk(installation.Transcripts, info, rel, func(file string) bool {
						return tl1CapturedFile(filepath.Base(file))
					})...)
				}
				p.pathMap = append(p.pathMap, capturePathMapping{Original: installation.Transcripts, Captured: path.Join(captureFilesDir, rel)})
			}
			record.CapturedTranscripts = path.Join(captureFilesDir, rel)
		}
		p.installations = append(p.installations, record)
	}
	return nil
}
