package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// A backup copies the library to a folder on another drive, so the library
// drive is not the only copy of the archive:
//
//	DEST/pharos-backup.json       what was backed up, from where, and whether the run finished
//	DEST/library.toml
//	DEST/catalog/catalog.sqlite3  a consistent snapshot (SQLite's online backup API)
//	DEST/captures/ hosts/ preserved/
//
// The catalog is read through its own read-only connection, so a backup runs
// while the service does and never needs the catalog to be writable. Other
// files are copied when their size or mtime differs, each to a temporary name
// that is fsynced and renamed, so an interrupted backup leaves every file
// either as it was or complete. Nothing is deleted from DEST without --prune.
// Each finished backup is recorded in the library's backups.json, which also
// holds the random library id that ties a destination to one library.
const (
	backupRecordName      = "backups.json"
	backupRecordLockName  = ".backups.json.lock"
	backupManifestName    = "pharos-backup.json"
	backupManifestVersion = 1
	backupTempSuffix      = ".pharos-backup-tmp"
	backupLockName        = ".pharos-backup.lock"
	backupHistoryLimit    = 50
)

type backupRecord struct {
	StartedAt       string  `json:"started_at"`
	FinishedAt      string  `json:"finished_at"`
	Destination     string  `json:"destination"`
	Host            Host    `json:"host"`
	FilesTotal      int     `json:"files_total"`
	FilesCopied     int     `json:"files_copied"`
	BytesTotal      int64   `json:"bytes_total"`
	BytesCopied     int64   `json:"bytes_copied"`
	DurationSeconds float64 `json:"duration_seconds"`
	// Partial backups finished without parts of the library that were
	// missing, such as an archive_root whose drive was not mounted.
	Partial bool     `json:"partial,omitempty"`
	Missing []string `json:"missing,omitempty"`
}

// backupHistory is backups.json: the library's id and its finished backups,
// newest first.
type backupHistory struct {
	Version   int            `json:"version"`
	LibraryID string         `json:"library_id,omitempty"`
	Backups   []backupRecord `json:"backups"`
}

func backupRecordPath(config Config) string {
	if config.Library {
		return filepath.Join(filepath.Dir(config.Path), backupRecordName)
	}
	return filepath.Join(filepath.Dir(config.CatalogPath), backupRecordName)
}

func loadBackupHistory(config Config) backupHistory {
	history := backupHistory{}
	if data, err := os.ReadFile(backupRecordPath(config)); err == nil {
		_ = json.Unmarshal(data, &history)
	}
	if history.Backups == nil {
		history.Backups = []backupRecord{}
	}
	return history
}

// growth estimates the library's growth in bytes a day from the full backups
// of the last 90 days, and the span in days it is based on.
func (h backupHistory) growth() (float64, float64) {
	backups := slices.DeleteFunc(slices.Clone(h.Backups), func(record backupRecord) bool { return record.Partial })
	if len(backups) < 2 {
		return 0, 0
	}
	newest, _ := time.Parse(time.RFC3339Nano, backups[0].FinishedAt)
	oldest := backups[0]
	for _, record := range backups[1:] {
		if finished, err := time.Parse(time.RFC3339Nano, record.FinishedAt); err == nil && newest.Sub(finished) <= 90*24*time.Hour {
			oldest = record
		}
	}
	started, _ := time.Parse(time.RFC3339Nano, oldest.FinishedAt)
	days := newest.Sub(started).Hours() / 24
	if days < 1 {
		return 0, days
	}
	return float64(backups[0].BytesTotal-oldest.BytesTotal) / days, days
}

// updateBackupHistory changes backups.json under a lock shared by every
// process, replacing it atomically when update reports a change.
func updateBackupHistory(config Config, update func(*backupHistory) bool) (backupHistory, error) {
	path := backupRecordPath(config)
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), backupRecordLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return backupHistory{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return backupHistory{}, err
	}
	history := loadBackupHistory(config)
	if !update(&history) {
		return history, nil
	}
	history.Version = 1
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return history, err
	}
	return history, writeFileAtomic(path, append(data, '\n'), 0o600)
}

func recordBackup(config Config, record backupRecord) error {
	_, err := updateBackupHistory(config, func(history *backupHistory) bool {
		history.Backups = append([]backupRecord{record}, history.Backups...)
		if len(history.Backups) > backupHistoryLimit {
			history.Backups = history.Backups[:backupHistoryLimit]
		}
		return true
	})
	return err
}

// libraryID returns the library's id, creating it on first use.
func libraryID(config Config) (string, error) {
	history, err := updateBackupHistory(config, func(history *backupHistory) bool {
		if history.LibraryID != "" {
			return false
		}
		history.LibraryID = randomToken()
		return true
	})
	return history.LibraryID, err
}

// backupItem is one thing backed up; Path is relative to DEST.
type backupItem struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Path   string `json:"path"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

type backupCatalog struct {
	Path    string            `json:"path"`
	Size    int64             `json:"size"`
	MTimeNS int64             `json:"mtime_ns"`
	Method  string            `json:"method"`
	TakenAt string            `json:"taken_at"`
	Source  sqliteSourceState `json:"source"`
}

// backupLibrary identifies the library a destination belongs to, so two
// libraries never share, overwrite, or prune one destination. A library
// travels between Macs and keeps its id; a per-user install's catalog lives
// on one Mac, and a copy of it on another Mac (Migration Assistant copies
// backups.json too) is a different install, so its host is part of it.
type backupLibrary struct {
	ID           string `json:"id"`
	HostID       string `json:"host_id,omitempty"`
	Path         string `json:"path"`
	VolumeID     string `json:"volume_id"`
	PathInVolume string `json:"path_in_volume"`
}

func (l backupLibrary) same(other backupLibrary) bool {
	return l.ID != "" && l.ID == other.ID && l.HostID == other.HostID
}

type backupManifest struct {
	Version    int            `json:"version"`
	Library    backupLibrary  `json:"library"`
	Host       Host           `json:"host"`
	StartedAt  string         `json:"started_at"`
	FinishedAt string         `json:"finished_at,omitempty"`
	Complete   bool           `json:"complete"`
	Missing    []string       `json:"missing,omitempty"`
	Catalog    *backupCatalog `json:"catalog,omitempty"`
	Items      []backupItem   `json:"items"`
}

type backupPlan struct {
	library backupLibrary
	catalog backupItem
	trees   []backupItem
	files   []backupItem
	// exclude holds the source paths no tree may copy, whatever the layout:
	// the live catalog and its journals (the snapshot is the copy), the
	// backup record, staging, and every other item's source.
	exclude map[string]bool
}

// layoutOverlap reports whether two slash paths in DEST nest or coincide.
func layoutOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(right, left+"/") || strings.HasPrefix(left, right+"/")
}

func planBackup(config Config) (backupPlan, error) {
	root := filepath.Dir(config.CatalogPath)
	if config.Library {
		root = filepath.Dir(config.Path)
	}
	relative := func(path, fallback string) string {
		if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
			return filepath.ToSlash(rel)
		}
		return fallback
	}
	history := loadBackupHistory(config)
	plan := backupPlan{library: backupLibrary{ID: history.LibraryID, Path: root, VolumeID: config.VolumeID},
		catalog: backupItem{Name: "catalog", Source: config.CatalogPath}, exclude: map[string]bool{}}
	if !config.Library {
		plan.library.HostID = currentHost().ID
	}
	if mount, _, err := volumeMount(root); err == nil {
		if rel, err := filepath.Rel(mount, root); err == nil {
			plan.library.PathInVolume = filepath.ToSlash(rel)
		}
	}
	// Items never nest in DEST, so none overwrites or prunes another, the
	// snapshot, or the manifest: an item whose place in the library would
	// (archive_root being the catalog's directory, say) gets its own name.
	used := []string{backupManifestName, backupLockName}
	place := func(item *backupItem, preferred, fallback string) error {
		for _, candidate := range []string{preferred, fallback} {
			if candidate != "" && candidate != "." && !slices.ContainsFunc(used, func(other string) bool { return layoutOverlap(candidate, other) }) {
				item.Path = candidate
				used = append(used, candidate)
				return nil
			}
		}
		return fmt.Errorf("cannot back up %s: its place in the backup overlaps another part of the library", item.Source)
	}
	if err := place(&plan.catalog, relative(config.CatalogPath, ""), "catalog/catalog.sqlite3"); err != nil {
		return plan, err
	}
	file := backupItem{Name: "config", Source: config.Path}
	if err := place(&file, filepath.Base(config.Path), "config.toml"); err != nil {
		return plan, err
	}
	plan.files = append(plan.files, file)
	for _, tree := range []backupItem{{Name: "captures", Source: config.CaptureRoot}, {Name: "preserved", Source: config.ArchiveRoot}, {Name: "hosts", Source: filepath.Join(root, hostsDirName)}} {
		if tree.Source == "" || tree.Name == "hosts" && !config.Library {
			continue
		}
		preferred := relative(tree.Source, "")
		// A symlinked root is backed up as the directory it points to.
		tree.Source = canonical(tree.Source)
		if err := place(&tree, preferred, tree.Name); err != nil {
			return plan, err
		}
		plan.trees = append(plan.trees, tree)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		plan.exclude[canonical(config.CatalogPath+suffix)] = true
	}
	record := backupRecordPath(config)
	plan.exclude[canonical(record)] = true
	plan.exclude[canonical(filepath.Join(filepath.Dir(record), backupRecordLockName))] = true
	if config.StagingRoot != "" {
		plan.exclude[canonical(config.StagingRoot)] = true
	}
	for _, item := range append(slices.Clone(plan.trees), plan.files...) {
		plan.exclude[canonical(item.Source)] = true
	}
	return plan, nil
}

func (p backupPlan) sources() []string {
	sources := []string{p.library.Path, p.catalog.Source}
	for _, item := range append(slices.Clone(p.trees), p.files...) {
		sources = append(sources, item.Source)
	}
	return sources
}

// canonical resolves symlinks in the existing part of path (/tmp is
// /private/tmp), so containment checks compare like with like.
func canonical(path string) string {
	path = filepath.Clean(path)
	existing := nearestExisting(path)
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return path
	}
	rest, _ := filepath.Rel(existing, path)
	return filepath.Join(resolved, rest)
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

// backupVolume reports the device number and facts of the volume holding
// path; tests replace it.
var backupVolume = func(path string) (uint64, volumeFacts) {
	device, _ := deviceOf(nearestExisting(path))
	return device, inspectVolume(context.Background(), path)
}

// driveLink is one volume a path depends on: its own or, for a volume on a
// disk image, the volume holding the image file, and so on.
type driveLink struct {
	device uint64
	disk   string
	image  string // the disk image through which the path depends on it
}

func driveChain(path string) ([]driveLink, volumeFacts) {
	chain := []driveLink{}
	var first volumeFacts
	image := ""
	for depth := 0; depth < 4; depth++ {
		device, facts := backupVolume(path)
		if depth == 0 {
			first = facts
		}
		if slices.ContainsFunc(chain, func(other driveLink) bool { return other.device == device }) {
			break
		}
		chain = append(chain, driveLink{device: device, disk: facts.PhysicalDisk, image: image})
		if facts.ImagePath == "" {
			break
		}
		image, path = facts.ImagePath, facts.ImagePath
	}
	return chain, first
}

// checkBackupDestination refuses a destination on the library's own volume or
// physical drive, including through a disk image stored on either: losing
// the drive would lose the backup with it.
func checkBackupDestination(plan backupPlan, dest string) (volumeFacts, error) {
	destChain, destFacts := driveChain(dest)
	checked := map[uint64]bool{}
	for _, source := range plan.sources() {
		source = canonical(source)
		if within(dest, source) || within(source, dest) {
			return destFacts, fmt.Errorf("%s overlaps the library's %s", dest, source)
		}
		if _, err := os.Stat(source); err != nil {
			continue
		}
		device, _ := deviceOf(source)
		if checked[device] {
			continue
		}
		checked[device] = true
		sourceChain, _ := driveChain(source)
		for _, destLink := range destChain {
			for _, sourceLink := range sourceChain {
				sameVolume := destLink.device == sourceLink.device
				sameDisk := destLink.disk != "" && destLink.disk == sourceLink.disk
				if !sameVolume && !sameDisk {
					continue
				}
				switch {
				case destLink.image != "":
					return destFacts, fmt.Errorf("%s is on a disk image (%s) stored on the same drive as %s; a backup there would be lost with it. Choose a folder on another drive", dest, destLink.image, source)
				case sourceLink.image != "":
					return destFacts, fmt.Errorf("the library's %s is on a disk image (%s) stored on the same drive as %s; a backup there would be lost with it. Choose a folder on another drive", source, sourceLink.image, dest)
				case sameVolume:
					return destFacts, fmt.Errorf("%s is on the same volume as %s; a backup there would be lost with the library's drive. Choose a folder on another drive", dest, source)
				default:
					return destFacts, fmt.Errorf("%s is on the same physical drive (%s) as %s; a backup there would be lost with it. Choose a folder on another drive", dest, sourceLink.disk, source)
				}
			}
		}
	}
	return destFacts, nil
}

// BackupRun is a backup's progress and outcome.
type BackupRun struct {
	ID              string         `json:"id"`
	Kind            string         `json:"kind"`
	State           string         `json:"state"`
	Phase           string         `json:"phase"`
	Destination     string         `json:"destination"`
	Prune           bool           `json:"prune"`
	CatalogBytes    int64          `json:"catalog_bytes"`
	CatalogCopied   bool           `json:"catalog_copied"`
	CatalogWALPeak  int64          `json:"catalog_wal_peak_bytes"`
	FilesTotal      int            `json:"files_total"`
	FilesCopied     int            `json:"files_copied"`
	FilesUnchanged  int            `json:"files_unchanged"`
	FilesPruned     int            `json:"files_pruned"`
	BytesTotal      int64          `json:"bytes_total"`
	BytesCopied     int64          `json:"bytes_copied"`
	Errors          []captureError `json:"errors"`
	Warnings        []string       `json:"warnings"`
	Missing         []string       `json:"missing"`
	StartedAt       string         `json:"started_at"`
	UpdatedAt       string         `json:"updated_at"`
	CompletedAt     any            `json:"completed_at"`
	DurationSeconds float64        `json:"duration_seconds"`
	BytesPerSecond  float64        `json:"bytes_per_second"`
}

func (r BackupRun) clone() BackupRun {
	r.Errors, r.Warnings, r.Missing = slices.Clone(r.Errors), slices.Clone(r.Warnings), slices.Clone(r.Missing)
	return r
}

// backupDestinationError is a failure writing DEST, which most likely affects
// every later file too, so the backup stops there.
type backupDestinationError struct{ err error }

func (e backupDestinationError) Error() string { return "backup destination: " + e.err.Error() }
func (e backupDestinationError) Unwrap() error { return e.err }

func toBackupDestination(err error) error {
	if err == nil || errors.As(err, new(backupDestinationError)) {
		return err
	}
	return backupDestinationError{err}
}

type backupJob struct {
	ctx      context.Context
	config   Config
	plan     backupPlan
	dest     string
	prune    bool
	previous *backupManifest
	run      BackupRun
	progress func(BackupRun)
	buffer   []byte
	started  time.Time
	// keep holds the DEST files (by device and inode, so names that the
	// destination stores differently, as HFS+ does accented ones, still
	// match) that the item being backed up accounts for; prune keeps them.
	keep     map[fileKey]bool
	symlinks int
}

type fileKey struct{ device, inode uint64 }

func keyOf(info fs.FileInfo) (fileKey, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{uint64(stat.Dev), uint64(stat.Ino)}, true
}

func (j *backupJob) keepPath(path string) {
	if info, err := os.Lstat(path); err == nil {
		if key, ok := keyOf(info); ok {
			j.keep[key] = true
		}
	}
}

// prepareBackup checks a backup before it starts and writes nothing.
func prepareBackup(config Config, dest string, prune bool) (*backupJob, error) {
	if strings.TrimSpace(dest) == "" {
		return nil, errors.New("a backup destination is required")
	}
	// The service's working directory means nothing to its caller.
	if dest = expandPath(dest); !filepath.IsAbs(dest) {
		return nil, fmt.Errorf("the backup destination must be an absolute path, not %q", dest)
	}
	dest = canonical(dest)
	plan, err := planBackup(config)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(plan.catalog.Source); err != nil {
		return nil, fmt.Errorf("cannot back up the catalog: %w", err)
	}
	// Never create a missing mount point: under /Volumes that would put the
	// backup on the startup disk while its drive is away.
	if _, err := os.Stat(filepath.Dir(dest)); err != nil {
		return nil, fmt.Errorf("%s is unavailable; is its drive mounted? %w", filepath.Dir(dest), err)
	}
	facts, err := checkBackupDestination(plan, dest)
	if err != nil {
		return nil, err
	}
	previous, err := readBackupDestination(dest, plan.library)
	if err != nil {
		return nil, err
	}
	job := &backupJob{config: config, plan: plan, dest: dest, prune: prune, previous: previous, buffer: make([]byte, 1<<20),
		run: BackupRun{ID: randomToken(), Kind: "backup", State: "pending", Destination: dest, Prune: prune, Errors: []captureError{}, Warnings: []string{}, Missing: []string{}}}
	name := facts.displayName()
	switch facts.Encryption {
	case "none":
		job.run.Warnings = append(job.run.Warnings, fmt.Sprintf("%s is not encrypted, and the backup holds every archived conversation.", name))
	case "hardware-only":
		job.run.Warnings = append(job.run.Warnings, "FileVault is off, so this Mac's internal disk, and the backup on it, unlock without a password.")
	}
	if facts.Spotlight == "enabled" && !strings.Contains(dest, ".noindex") {
		if facts.Location == "internal" {
			job.run.Warnings = append(job.run.Warnings, fmt.Sprintf("Spotlight indexes this Mac's internal disk; exclude %s under System Settings → Spotlight → Search Privacy….", dest))
		} else {
			job.run.Warnings = append(job.run.Warnings, fmt.Sprintf("Spotlight indexes %s; turn it off with: sudo mdutil -i off %s", name, shellWord(facts.MountPoint)))
		}
	}
	return job, nil
}

// readBackupDestination accepts a missing or empty directory, or an earlier
// backup of the same library, whose manifest it returns.
func readBackupDestination(dest string, library backupLibrary) (*backupManifest, error) {
	entries, err := os.ReadDir(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dest, backupManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				return nil, fmt.Errorf("%s is neither empty nor a Pharos backup; choose an empty or new folder", dest)
			}
		}
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	manifest := &backupManifest{}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, fmt.Errorf("backup manifest %s is unreadable: %w", filepath.Join(dest, backupManifestName), err)
	}
	if manifest.Version > backupManifestVersion {
		return nil, fmt.Errorf("%s was written by a newer Pharos (manifest version %d)", dest, manifest.Version)
	}
	if !manifest.Library.same(library) {
		from := manifest.Library.Path
		if manifest.Host.Label != "" {
			from += " on " + manifest.Host.Label
		}
		return nil, fmt.Errorf("%s is a backup of another library (%s); choose another folder", dest, from)
	}
	return manifest, nil
}

// Run backs up the library; its disk I/O runs in the utility tier, like a
// capture's. It returns early, between files or catalog pages, once ctx ends.
func (j *backupJob) Run(ctx context.Context, progress func(BackupRun)) BackupRun {
	j.ctx, j.progress, j.started = ctx, progress, time.Now()
	j.run.State, j.run.StartedAt = "running", now()
	var err error
	withCaptureIOPolicy(func() { err = j.execute() })
	if err != nil {
		j.fail("", err)
	}
	elapsed := time.Since(j.started).Seconds()
	j.run.DurationSeconds = elapsed
	if elapsed > 0 {
		j.run.BytesPerSecond = float64(j.run.BytesCopied) / elapsed
	}
	if j.symlinks > 0 {
		j.run.Warnings = append(j.run.Warnings, fmt.Sprintf("%d symbolic links inside the library were not followed; what they point to is not in the backup", j.symlinks))
	}
	switch {
	case ctx.Err() != nil:
		j.run.State = "cancelled"
	case len(j.run.Errors) > 0:
		j.run.State = "failed"
	case len(j.run.Missing) > 0:
		j.run.State = "partial"
	default:
		j.run.State = "complete"
	}
	j.run.Phase = j.run.State
	j.run.CompletedAt = now()
	j.report()
	return j.run.clone()
}

var errBackupDestinationBusy = errors.New("another backup to this destination is running")

func (j *backupJob) execute() error {
	if err := os.MkdirAll(j.dest, 0o700); err != nil {
		return toBackupDestination(err)
	}
	// Two backups writing one destination would share temporary names.
	lock, err := os.OpenFile(filepath.Join(j.dest, backupLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return toBackupDestination(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errBackupDestinationBusy
		}
		return toBackupDestination(err)
	}
	// The library's first backup gives it its id, before DEST names it.
	if j.plan.library.ID == "" {
		id, err := libraryID(j.config)
		if err != nil {
			return fmt.Errorf("cannot record the library's id in %s: %w", backupRecordPath(j.config), err)
		}
		j.plan.library.ID = id
	}
	// Checked again under the lock: DEST may have changed since prepareBackup.
	previous, err := readBackupDestination(j.dest, j.plan.library)
	if err != nil {
		return err
	}
	j.previous = previous
	manifest := backupManifest{Version: backupManifestVersion, Library: j.plan.library, Host: currentHost(), StartedAt: j.run.StartedAt, Items: []backupItem{}}
	if j.previous != nil {
		manifest.Catalog = j.previous.Catalog
	}
	// Until the run finishes, the manifest says the backup is incomplete.
	if err := j.writeManifest(manifest); err != nil {
		return err
	}
	j.phase("catalog")
	catalog, err := j.backupCatalog(manifest.Catalog)
	if err != nil {
		return err
	}
	manifest.Catalog = catalog
	for _, item := range append(slices.Clone(j.plan.trees), j.plan.files...) {
		if err := j.ctx.Err(); err != nil {
			return err
		}
		j.phase(item.Name)
		files, bytes := j.run.FilesTotal, j.run.BytesTotal
		if err := j.item(item); err != nil {
			return err
		}
		item.Files, item.Bytes = j.run.FilesTotal-files, j.run.BytesTotal-bytes
		manifest.Items = append(manifest.Items, item)
	}
	j.run.BytesTotal += j.run.CatalogBytes
	if len(j.run.Errors) > 0 {
		return nil
	}
	j.phase("finishing")
	manifest.FinishedAt, manifest.Complete, manifest.Missing = now(), true, j.run.Missing
	// The manifest's full device flush also makes every file fsynced before
	// it durable.
	if err := j.writeManifest(manifest); err != nil {
		return err
	}
	record := backupRecord{StartedAt: j.run.StartedAt, FinishedAt: manifest.FinishedAt, Destination: j.dest, Host: manifest.Host,
		FilesTotal: j.run.FilesTotal, FilesCopied: j.run.FilesCopied, BytesTotal: j.run.BytesTotal, BytesCopied: j.run.BytesCopied,
		DurationSeconds: time.Since(j.started).Seconds(), Partial: len(j.run.Missing) > 0, Missing: j.run.Missing}
	if err := recordBackup(j.config, record); err != nil {
		j.run.Warnings = append(j.run.Warnings, fmt.Sprintf("the backup is complete, but recording it in %s failed: %v", backupRecordPath(j.config), err))
	}
	return nil
}

func (j *backupJob) writeManifest(manifest backupManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return toBackupDestination(writeFileDurably(filepath.Join(j.dest, backupManifestName), append(data, '\n')))
}

func (j *backupJob) phase(name string) {
	j.run.Phase = name
	j.report()
}

func (j *backupJob) report() {
	j.run.UpdatedAt = now()
	if j.progress != nil {
		j.progress(j.run.clone())
	}
}

func (j *backupJob) fail(path string, err error) {
	j.run.Errors = append(j.run.Errors, captureError{Path: path, Error: err.Error()})
}

// backupCatalog snapshots the catalog unless it is unchanged since the
// snapshot already in DEST.
func (j *backupJob) backupCatalog(previous *backupCatalog) (*backupCatalog, error) {
	source := j.plan.catalog.Source
	state, err := sqliteState(source)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", source, err)
	}
	// Reading the catalog, as the snapshot itself does, can leave an empty
	// WAL behind; an empty WAL's mtime says nothing about the content.
	if state.WALSize == 0 {
		state.WALMTimeNS = 0
	}
	destination := filepath.Join(j.dest, filepath.FromSlash(j.plan.catalog.Path))
	if previous != nil && previous.Path == j.plan.catalog.Path && previous.Source == state && current(destination, previous.Size, previous.MTimeNS) {
		j.run.CatalogBytes = previous.Size
		return previous, nil
	}
	if err := ensureFree(j.dest, state.Size+state.WALSize); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, toBackupDestination(err)
	}
	temporary := destination + backupTempSuffix
	os.Remove(temporary)
	ctx, stop := j.boundWAL(source, state.WALSize)
	err = snapshotSQLite(ctx, source, temporary)
	stop()
	if cause := context.Cause(ctx); err != nil && errors.Is(cause, errBackupWALGrew) {
		err = cause
	}
	if err != nil {
		os.Remove(temporary)
		return nil, fmt.Errorf("catalog snapshot: %w", err)
	}
	info, err := syncPath(temporary)
	if err == nil {
		err = os.Chmod(temporary, 0o600)
	}
	if err == nil {
		err = os.Rename(temporary, destination)
	}
	if err != nil {
		os.Remove(temporary)
		return nil, toBackupDestination(err)
	}
	j.run.CatalogBytes, j.run.CatalogCopied = info.Size(), true
	j.run.BytesCopied += info.Size()
	j.report()
	return &backupCatalog{Path: j.plan.catalog.Path, Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), Method: "sqlite-backup", TakenAt: now(), Source: state}, nil
}

// backupWALGrowth bounds how far the catalog's WAL may grow while a snapshot
// holds its read transaction, during which no checkpoint can reuse it. A sync
// running meanwhile is the usual cause; the backup then fails and can simply
// be run again afterwards.
var (
	backupWALGrowth int64 = 1 << 30
	backupWALPoll         = 100 * time.Millisecond
)

var errBackupWALGrew = errors.New("the catalog's WAL grew by more than the backup allows while it was copied (indexing is probably writing); back up again once it finishes")

// boundWAL returns a context for the snapshot that ends once the source's WAL
// grows by more than backupWALGrowth, recording the peak it saw.
func (j *backupJob) boundWAL(source string, start int64) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(j.ctx)
	done := make(chan struct{})
	var peak atomic.Int64
	peak.Store(start)
	go func() {
		ticker := time.NewTicker(backupWALPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if info, err := os.Stat(source + "-wal"); err == nil {
				peak.Store(max(peak.Load(), info.Size()))
				if info.Size()-start > backupWALGrowth {
					cancel(errBackupWALGrew)
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(done)
		cancel(nil)
		j.run.CatalogWALPeak = peak.Load()
	}
}

// current reports whether target is a complete copy of a source file of this
// size and mtime. File systems with coarse timestamps store the copied mtime
// rounded (exFAT to 10 ms, HFS+ to 1 s, FAT to 2 s), so a rounded mtime within
// 2 s matches too.
func current(target string, size, mtimeNS int64) bool {
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}
	actual := info.ModTime().UnixNano()
	coarse := actual%int64(10*time.Millisecond) == 0 && max(actual-mtimeNS, mtimeNS-actual) < int64(2*time.Second)
	return actual == mtimeNS || coarse
}

func ensureFree(dir string, bytes int64) error {
	if bytes == 0 {
		return nil
	}
	_, free := diskUsage(dir)
	if int64(free)-bytes < captureFreeMargin {
		return backupDestinationError{fmt.Errorf("needs %s but only %s is free", humanBytes(bytes+captureFreeMargin), humanBytes(int64(free)))}
	}
	return nil
}

// item backs up one directory or file.
func (j *backupJob) item(item backupItem) error {
	info, err := os.Stat(item.Source)
	if errors.Is(err, fs.ErrNotExist) {
		// Missing archive_root usually means its drive is not mounted; any
		// other part the last backup had is missing too. Its earlier backup
		// is kept, and the backup is partial.
		if item.Name == "preserved" || j.previouslyHad(item.Name) {
			j.run.Missing = append(j.run.Missing, item.Source)
		}
		return nil
	} else if err != nil {
		j.fail(item.Source, err)
		return nil
	}
	destination := filepath.Join(j.dest, filepath.FromSlash(item.Path))
	j.keep = map[fileKey]bool{}
	errorsBefore := len(j.run.Errors)
	listed := 0
	switch {
	case item.Name == "config":
		err = j.copyConfig(item.Source, destination)
	case !info.IsDir():
		_, err = j.copyAll([]string{item.Source}, item.Source, destination)
	case item.Name == "captures":
		listed, err = j.captures(item.Source, destination)
	default:
		listed, err = j.tree(item.Source, destination)
	}
	if err != nil || !j.prune || !info.IsDir() {
		return err
	}
	switch {
	case len(j.run.Errors) > errorsBefore:
		// An incomplete listing must not prune what it failed to list.
	case listed == 0:
		if entries, err := os.ReadDir(destination); err == nil && len(entries) > 0 {
			j.run.Warnings = append(j.run.Warnings, fmt.Sprintf("%s has no files, so its earlier backup in %s was not pruned", item.Source, destination))
		}
	default:
		j.pruneTree(destination, item.Source)
	}
	return nil
}

func (j *backupJob) previouslyHad(name string) bool {
	return j.previous != nil && slices.ContainsFunc(j.previous.Items, func(item backupItem) bool { return item.Name == name && item.Files > 0 })
}

// captures copies each host's captures while holding its capture lock
// shared, which keeps captures (which take it exclusively) from changing
// files and manifests mid-copy.
func (j *backupJob) captures(source, destination string) (int, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		j.fail(source, err)
		return 0, nil
	}
	listed := 0
	for _, entry := range entries {
		path := filepath.Join(source, entry.Name())
		if j.plan.exclude[path] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || skipBackupName(entry.Name()) {
			continue
		}
		if !info.IsDir() {
			if info.Mode().IsRegular() {
				copied, err := j.copyAll([]string{path}, path, filepath.Join(destination, entry.Name()))
				listed += copied
				if err != nil {
					return listed, err
				}
			}
			continue
		}
		// A host directory may be a symlink; back up what it points to.
		host := canonical(path)
		unlock, err := j.lockCaptures(host)
		if err != nil {
			if j.ctx.Err() != nil {
				return listed, j.ctx.Err()
			}
			j.fail(path, err)
			continue
		}
		count, err := j.tree(host, filepath.Join(destination, entry.Name()))
		unlock()
		listed += count
		if err != nil {
			return listed, err
		}
	}
	return listed, nil
}

func (j *backupJob) lockCaptures(hostDir string) (func(), error) {
	lock, err := os.Open(filepath.Join(hostDir, captureLockName))
	if errors.Is(err, fs.ErrNotExist) {
		return func() {}, nil
	} else if err != nil {
		return nil, err
	}
	for waiting := false; ; {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return func() { lock.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			lock.Close()
			return nil, err
		}
		if !waiting {
			waiting = true
			j.phase("waiting for a capture to finish")
		}
		select {
		case <-j.ctx.Done():
			lock.Close()
			return nil, j.ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// skipBackupName leaves out locks and the temporaries of writes in progress.
func skipBackupName(name string) bool {
	return name == captureLockName || strings.HasSuffix(name, captureTempSuffix) || strings.HasSuffix(name, backupTempSuffix) ||
		strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

// tree copies the regular files under source, which must not be a symlink,
// and returns how many it listed. Symlinks inside are not followed, and
// prune keeps whatever an earlier backup copied where they are.
func (j *backupJob) tree(source, destination string) (int, error) {
	files := []string{}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := j.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			j.fail(path, err)
			if entry != nil && entry.IsDir() && path != source {
				return fs.SkipDir
			}
			return nil
		}
		if path != source && j.plan.exclude[path] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(source, path)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			j.symlinks++
			j.keepPath(filepath.Join(destination, rel))
		case entry.Type().IsRegular() && !skipBackupName(entry.Name()):
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return len(files), err
	}
	return j.copyAll(files, source, destination)
}

// copyAll copies what changed among files (all under root, or root itself)
// to the same relative paths under destination, and returns how many it
// accounted for.
func (j *backupJob) copyAll(files []string, root, destination string) (int, error) {
	type copyJob struct {
		source, target string
		info           fs.FileInfo
	}
	jobs := []copyJob{}
	planned := int64(0)
	listed := 0
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				j.fail(file, err)
			}
			continue
		}
		listed++
		target := destination
		if file != root {
			rel, _ := filepath.Rel(root, file)
			target = filepath.Join(destination, rel)
		}
		j.run.FilesTotal++
		j.run.BytesTotal += info.Size()
		if current(target, info.Size(), info.ModTime().UnixNano()) {
			j.run.FilesUnchanged++
			j.keepPath(target)
			continue
		}
		jobs = append(jobs, copyJob{file, target, info})
		planned += info.Size()
	}
	if err := ensureFree(nearestExisting(destination), planned); err != nil {
		return listed, err
	}
	for _, job := range jobs {
		if err := j.ctx.Err(); err != nil {
			return listed, err
		}
		if err := j.copyFile(job.source, job.target, job.info); err != nil {
			if errors.As(err, new(backupDestinationError)) || j.ctx.Err() != nil {
				return listed, err
			}
			j.fail(job.source, err)
			continue
		}
		j.keepPath(job.target)
		j.run.FilesCopied++
		j.run.BytesCopied += job.info.Size()
		j.report()
	}
	return listed, nil
}

// backupSecretKeys are credentials for other services, which a backup of the
// configuration leaves out. api_token only guards the local service, whose
// data the backup holds anyway, so it is kept and a restore works as is.
var backupSecretKeys = []string{"github_token", "tl1_token"}

func redactConfig(data []byte) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		key, value, ok := strings.Cut(stripTOMLComment(strings.TrimSpace(line)), "=")
		if ok && slices.Contains(backupSecretKeys, strings.TrimSpace(key)) && strings.Trim(strings.TrimSpace(value), `"'`) != "" {
			lines[index] = fmt.Sprintf("# %s was left out of this backup; set it again after restoring.\n", strings.TrimSpace(key))
		}
	}
	return []byte(strings.Join(lines, ""))
}

// copyConfig copies the configuration without credentials for other services.
func (j *backupJob) copyConfig(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		j.fail(source, err)
		return nil
	}
	data = redactConfig(data)
	j.run.FilesTotal++
	j.run.BytesTotal += int64(len(data))
	if existing, err := os.ReadFile(target); err == nil && string(existing) == string(data) {
		j.run.FilesUnchanged++
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return toBackupDestination(err)
	}
	if err := toBackupDestination(writeFileDurably(target, data)); err != nil {
		return err
	}
	j.run.FilesCopied++
	j.run.BytesCopied += int64(len(data))
	return nil
}

var errBackupSourceShrank = errors.New("file shrank while it was being copied")

func (j *backupJob) copyFile(source, target string, info fs.FileInfo) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return toBackupDestination(err)
	}
	temporary := target + backupTempSuffix
	out, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return toBackupDestination(err)
	}
	abandon := func(err error) error {
		out.Close()
		os.Remove(temporary)
		return err
	}
	// Exactly the size seen is copied, as a capture does: an appended log
	// yields a consistent prefix and the next backup sees the new size.
	for copied := int64(0); copied < info.Size(); {
		if err := j.ctx.Err(); err != nil {
			return abandon(err)
		}
		chunk := j.buffer[:min(int64(len(j.buffer)), info.Size()-copied)]
		read, err := io.ReadFull(in, chunk)
		if read > 0 {
			if _, err := out.Write(chunk[:read]); err != nil {
				return abandon(toBackupDestination(err))
			}
			copied += int64(read)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if copied < info.Size() {
				return abandon(errBackupSourceShrank)
			}
		} else if err != nil {
			return abandon(err)
		}
	}
	if err := os.Chtimes(temporary, info.ModTime(), info.ModTime()); err != nil {
		return abandon(toBackupDestination(err))
	}
	if err := syscall.Fsync(int(out.Fd())); err != nil {
		return abandon(toBackupDestination(err))
	}
	if err := out.Close(); err != nil {
		os.Remove(temporary)
		return toBackupDestination(err)
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return toBackupDestination(err)
	}
	return nil
}

// pruneTree deletes files under destination that the library no longer has,
// and backup temporaries an interrupted run left behind. It never touches the
// manifest, the lock, or another item, whatever the layout. A file is kept if
// it is one this run accounted for or, by the name the destination lists it
// under, the library still has it: either suffices, so neither a destination
// that stores names differently (HFS+, exFAT) nor one whose inode numbers
// are made up (exFAT) loses files.
func (j *backupJob) pruneTree(destination, source string) {
	protected := map[string]bool{filepath.Join(j.dest, backupManifestName): true, filepath.Join(j.dest, backupLockName): true,
		filepath.Join(j.dest, filepath.FromSlash(j.plan.catalog.Path)): true}
	for _, item := range append(slices.Clone(j.plan.trees), j.plan.files...) {
		if other := filepath.Join(j.dest, filepath.FromSlash(item.Path)); other != destination {
			protected[other] = true
		}
	}
	dirs := []string{}
	_ = filepath.WalkDir(destination, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == destination {
			return nil
		}
		info, infoErr := entry.Info()
		key, ok := keyOf(info)
		kept := infoErr != nil || !ok || j.keep[key] || protected[path] || appleDouble(path)
		if !kept && !entry.IsDir() {
			rel, _ := filepath.Rel(destination, path)
			original := filepath.Join(source, rel)
			if info, err := os.Lstat(original); err == nil && !info.IsDir() && !j.plan.exclude[original] && !skipBackupName(entry.Name()) {
				kept = true
			}
		}
		switch {
		case entry.IsDir() && kept:
			return fs.SkipDir
		case entry.IsDir():
			dirs = append(dirs, path)
		case !kept && removeListed(path) == nil:
			j.run.FilesPruned++
		}
		return nil
	})
	for index := len(dirs) - 1; index >= 0; index-- {
		if entries, err := os.ReadDir(dirs[index]); err == nil && len(entries) == 0 {
			removeListed(dirs[index])
		}
	}
}

// removeListed deletes a file, or a directory known to be empty, by the name
// its directory listing gave. macOS's exFAT driver lists names decomposed but
// unlinks only the form it stored, so a name it refuses is renamed first.
func removeListed(path string) error {
	err := os.Remove(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".pharos-prune-"+randomToken()[:12]+backupTempSuffix)
	if err := os.Rename(path, temporary); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		os.Rename(temporary, path)
		return err
	}
	return nil
}

// appleDouble reports whether path is the "._" file in which macOS keeps
// the extended attributes of a file beside it on exFAT, FAT and network
// volumes. It goes with that file, not with the library.
func appleDouble(path string) bool {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "._") {
		return false
	}
	_, err := os.Lstat(filepath.Join(filepath.Dir(path), strings.TrimPrefix(name, "._")))
	return err == nil
}

func runBackupCLI(config Config, args []string) error {
	prune, positional := false, []string{}
	for _, arg := range args {
		switch arg {
		case "--prune", "-prune":
			prune = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown flag %s; usage: alexandria backup [--prune] DEST", arg)
			}
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 {
		return errors.New("usage: alexandria backup [--prune] DEST")
	}
	// On the command line a relative DEST means the working directory.
	dest := expandPath(positional[0])
	if !filepath.IsAbs(dest) {
		absolute, err := filepath.Abs(dest)
		if err != nil {
			return err
		}
		dest = absolute
	}
	job, err := prepareBackup(config, dest, prune)
	if err != nil {
		return err
	}
	for _, warning := range job.run.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	phase := ""
	run := job.Run(ctx, func(run BackupRun) {
		if run.Phase != phase {
			phase = run.Phase
			fmt.Fprintf(os.Stderr, "backup: %s\n", phase)
		}
	})
	if err := printJSON(run); err != nil {
		return err
	}
	if run.State == "partial" {
		return fmt.Errorf("backup partial: %s missing", strings.Join(run.Missing, ", "))
	}
	if run.State != "complete" {
		return fmt.Errorf("backup %s", run.State)
	}
	return nil
}
