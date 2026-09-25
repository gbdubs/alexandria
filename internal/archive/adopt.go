package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	sqlite "modernc.org/sqlite"
)

// Adopting turns an existing per-user install into a new portable library
// without re-indexing. The catalog is copied as one consistent snapshot while
// the old service may keep writing, only the copy is migrated, and the old
// install's sources become this Mac's host file. The old install is only read,
// so it remains a working fallback.
//
// library.toml is written last: until then the directory holds no library the
// guards or the app would accept, whatever point an attempt stops at.

// adoptPartialSuffix names the catalog copy until it is complete. The next
// attempt deletes it.
const adoptPartialSuffix = ".adopt-partial"

// adoptCopyChunk is the size of one backup step, and so of one progress
// report. Tests lower it.
var adoptCopyChunk = int64(64 << 20)

// adoptMinMargin is the least free space left beside the copy for its
// migrations and WAL. Tests lower it.
var adoptMinMargin = int64(2 << 30)

// adoptFault injects failures in tests at a named stage.
var adoptFault func(stage string) error

type AdoptOptions struct {
	Identity  func(string) string // volumeIdentity outside tests
	Progress  io.Writer           // phases and copy progress; nil discards them
	FullCheck bool                // integrity_check instead of quick_check
}

type AdoptResult struct {
	Config       Config // the new library
	Legacy       Config
	CatalogBytes int64
	CopyTime     time.Duration
	Check        string // the SQLite check the migrated copy passed
	Host         Host
	LegacyHost   string // the host that owns rows indexed before hosts existed
	HostFile     string
	Sources      []string
	Preserved    preservedCopy
	Notes        []string // legacy settings deliberately not carried over
}

func adoptFaultAt(stage string) error {
	if adoptFault == nil {
		return nil
	}
	return adoptFault(stage)
}

// AdoptLibrary creates the library dir from the install configured by
// legacyPath. It checks everything it can before writing anything, and on
// failure removes everything it wrote.
func AdoptLibrary(ctx context.Context, dir, legacyPath string, options AdoptOptions) (AdoptResult, error) {
	result := AdoptResult{Host: currentHost()}
	progress := options.Progress
	if progress == nil {
		progress = io.Discard
	}
	if options.Identity == nil {
		options.Identity = volumeIdentity
	}
	if legacyPath == "" {
		return result, errors.New("name the configuration of the install to adopt")
	}
	legacy, err := LoadConfig(expandPath(legacyPath))
	if err != nil {
		return result, err
	}
	result.Legacy = legacy
	if legacy.Library {
		return result, fmt.Errorf("%s is already a portable library; copy its directory instead", legacy.Path)
	}
	if info, err := os.Stat(legacy.CatalogPath); err != nil || !info.Mode().IsRegular() {
		return result, fmt.Errorf("%s has no catalog at %s", legacy.Path, legacy.CatalogPath)
	}
	top, blocks, err := readLegacyConfig(legacy.Path)
	if err != nil {
		return result, err
	}
	dir, err = filepath.Abs(expandPath(dir))
	if err != nil {
		return result, err
	}
	// A missing parent is usually an unmounted drive, whose mount point must
	// not be recreated on the startup disk.
	if info, err := os.Stat(filepath.Dir(dir)); err != nil || !info.IsDir() {
		return result, fmt.Errorf("%s does not exist; is its drive mounted?", filepath.Dir(dir))
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return result, fmt.Errorf("%s is not a directory", dir)
	}
	libraryPath := filepath.Join(dir, libraryConfigName)
	catalogPath := filepath.Join(dir, "catalog", "catalog.sqlite3")
	result.HostFile = hostConfigPath(Config{Library: true, Path: libraryPath})
	// A leftover -wal would be replayed onto the new catalog when it opens.
	occupied := append(catalogFiles(catalogPath), libraryPath, result.HostFile)
	if err := refuseExisting(dir, occupied); err != nil {
		return result, err
	}
	if err := adoptHostCheck(result.Host); err != nil {
		return result, err
	}
	preservedDir := filepath.Join(dir, "preserved")
	if result.Preserved, err = planPreserved(legacy.ArchiveRoot, dir); err != nil {
		return result, err
	}

	var created []string
	mkdir := func(path string) error {
		if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		} else if err == nil {
			created = append(created, path)
		}
		return nil
	}
	partial := catalogPath + adoptPartialSuffix
	fail := func(err error) (AdoptResult, error) {
		for _, path := range catalogFiles(partial) {
			os.Remove(path)
		}
		// Opening the copy creates the initialize lock beside it. Adopt refuses
		// an existing catalog, so no other process can be holding this one.
		os.Remove(filepath.Join(filepath.Dir(catalogPath), initializeLockName))
		// Directories go only once empty; what others put there stays.
		for index := len(created) - 1; index >= 0; index-- {
			os.Remove(created[index])
		}
		return result, err
	}
	// The copy calls this once it knows the catalog's size, before it writes.
	prepare := func(bytes int64) error {
		if err := checkAdoptSpace(dir, bytes, result.Preserved.Bytes); err != nil {
			return err
		}
		for _, path := range []string{dir, filepath.Dir(catalogPath), filepath.Join(dir, "staging"), preservedDir, filepath.Dir(result.HostFile)} {
			if err := mkdir(path); err != nil {
				return err
			}
		}
		// Left by an attempt that was killed; the name is adopt's own.
		for _, path := range catalogFiles(partial) {
			os.Remove(path)
		}
		return nil
	}
	fmt.Fprintf(progress, "Copying the catalog %s\n", legacy.CatalogPath)
	meter := newCopyMeter(progress)
	started := time.Now()
	result.CatalogBytes, err = copyCatalogSnapshot(ctx, legacy.CatalogPath, partial, prepare, meter.report)
	meter.end()
	if err != nil {
		return fail(err)
	}
	result.CopyTime = time.Since(started)
	if err := syncFile(partial); err != nil {
		return fail(err)
	}
	if err := adoptFaultAt("migrate"); err != nil {
		return fail(err)
	}
	if result.LegacyHost, err = pinLegacyHost(partial, result.Host.ID); err != nil {
		return fail(err)
	}
	if result.LegacyHost == result.Host.ID {
		fmt.Fprintf(progress, "Migrating the copy; rows indexed before hosts existed belong to this Mac (%s)\n", result.Host.Label)
	} else {
		result.Notes = append(result.Notes, "rows indexed before hosts existed already belonged to host "+result.LegacyHost+", not to this Mac; that was kept")
		fmt.Fprintf(progress, "Migrating the copy; rows indexed before hosts existed already belong to host %s\n", result.LegacyHost)
	}
	if result.Check, err = migrateAdoptedCatalog(ctx, partial, result.LegacyHost, options.FullCheck, progress); err != nil {
		return fail(err)
	}

	if err := copyPreserved(ctx, &result.Preserved, preservedDir, &created); err != nil {
		return fail(fmt.Errorf("copy preserved packages: %w", err))
	}
	var hostText string
	hostText, result.Sources = adoptedHostFile(result.Host, filepath.Dir(legacy.Path), blocks)
	if err := writeFileAtomic(result.HostFile, []byte(hostText), 0o600); err != nil {
		return fail(err)
	}
	created = append(created, result.HostFile)
	if err := adoptFaultAt("rename"); err != nil {
		return fail(err)
	}
	if err := refuseExisting(dir, catalogFiles(catalogPath)); err != nil {
		return fail(fmt.Errorf("while adopting: %w", err))
	}
	if err := os.Rename(partial, catalogPath); err != nil {
		return fail(err)
	}
	created = append(created, catalogPath)
	syncDir(filepath.Dir(catalogPath))
	if err := adoptFaultAt("commit"); err != nil {
		return fail(err)
	}
	settings, notes := adoptedSettings(legacy, top)
	result.Notes = append(notes, result.Notes...)
	if _, err := writeLibraryConfig(dir, options.Identity, settings); err != nil {
		return fail(err)
	}
	created = append(created, libraryPath)
	syncDir(dir)

	config, err := LoadConfig(libraryPath)
	if err == nil {
		err = config.checkLibrary(options.Identity)
	}
	if err == nil && (config.CatalogPath != catalogPath || len(config.Sources) != len(blocks)) {
		err = fmt.Errorf("%s does not load as written (catalog %s, %d sources)", libraryPath, config.CatalogPath, len(config.Sources))
	}
	if err == nil {
		err = adoptFaultAt("verify")
	}
	if err != nil {
		return fail(fmt.Errorf("the new library does not pass its guards: %w", err))
	}
	result.Config = config
	return result, nil
}

// catalogFiles are a SQLite database and the files beside it that SQLite
// would read with it.
func catalogFiles(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

func refuseExisting(dir string, paths []string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to adopt into %s: %s already exists", dir, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// adoptHostCheck refuses a fallback host ID, which adopting would make
// permanent: it becomes the owner of every legacy row and names the host file.
// Tests replace it.
var adoptHostCheck = func(host Host) error {
	// Adopting is rare and deliberate, so give ioreg the patient waits.
	platform := func() (string, error) { return platformUUID([]time.Duration{5 * time.Second, 15 * time.Second}) }
	return checkAdoptHost(host, os.Getenv("PHAROS_HOST_ID"), platform)
}

// checkAdoptHost accepts host when PHAROS_HOST_ID chose it explicitly, or when
// it is the ID this Mac's hardware UUID gives right now; not one recorded in
// host.json or derived from the hostname because ioreg did not answer.
func checkAdoptHost(host Host, explicit string, platform func() (string, error)) error {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		if host.ID != explicit {
			return fmt.Errorf("this process identifies the Mac as %s, not as PHAROS_HOST_ID %s", host.ID, explicit)
		}
		return nil
	}
	hardware, err := platform()
	if err != nil {
		return fmt.Errorf("cannot read this Mac's hardware UUID (%v), and adopting would make a fallback host ID permanent; try again, or set PHAROS_HOST_ID to the host ID to use", err)
	}
	if want := stableID("host", hardware, host.User); host.ID != want {
		return fmt.Errorf("this Mac's host ID %s is a fallback, not %s from its hardware UUID, and adopting would make it permanent; try again, or set PHAROS_HOST_ID to the host ID to use", host.ID, want)
	}
	return nil
}

// copyCatalogSnapshot copies the catalog src to dst with SQLite's online
// backup API from a read-only, query-only connection, and returns the bytes
// copied. It steps in chunks inside one read transaction held on the source
// connection. That pins a single snapshot, including whatever is still in the
// source's WAL: commits by a running service, which WAL mode does not block,
// neither restart the backup nor reach the copy. (Without the transaction
// every such commit restarts a chunked backup, and one unbounded step cannot
// report progress.) prepare runs with the exact size before anything is
// written.
func copyCatalogSnapshot(ctx context.Context, src, dst string, prepare func(int64) error, report func(done, total int64)) (int64, error) {
	db, err := openReadOnlySQLite(src)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return 0, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// The first read starts the snapshot.
	var version sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT (SELECT value FROM meta WHERE key='schema_version')").Scan(&version); err != nil {
		return 0, fmt.Errorf("%s is not a readable Pharos catalog: %w", src, err)
	}
	if stored, err := strconv.Atoi(strings.TrimSpace(version.String)); version.Valid && (err != nil || stored > catalogSchemaVersion) {
		return 0, fmt.Errorf("catalog %s has schema version %s, but this build of Pharos supports up to %d; adopt it with a newer build", src, version.String, catalogSchemaVersion)
	}
	var pages, pageSize int64
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	total := pages * pageSize
	if err := prepare(total); err != nil {
		return 0, err
	}
	target := (&url.URL{Scheme: "file", Path: dst}).String() + "?_pragma=journal_mode(off)&_pragma=synchronous(off)"
	step := int32(max(1, adoptCopyChunk/pageSize))
	withCaptureIOPolicy(func() {
		err = conn.Raw(func(driverConn any) error {
			source, ok := driverConn.(interface {
				NewBackup(string) (*sqlite.Backup, error)
			})
			if !ok {
				return errors.New("the SQLite driver cannot make online backups")
			}
			backup, err := source.NewBackup(target)
			if err != nil {
				return err
			}
			for done := int64(0); ; {
				if err := ctx.Err(); err != nil {
					backup.Finish()
					return err
				}
				more, err := backup.Step(step)
				if err == nil && more {
					err = adoptFaultAt("copy")
				}
				if err != nil {
					backup.Finish()
					return err
				}
				done = min(done+int64(step), pages)
				report(done*pageSize, total)
				if !more {
					return backup.Finish()
				}
			}
		})
	})
	if err != nil {
		return 0, fmt.Errorf("copy the catalog: %w", err)
	}
	if info, err := os.Stat(dst); err != nil || info.Size() != total {
		return 0, fmt.Errorf("the catalog copy %s is incomplete (%v)", dst, err)
	}
	return total, nil
}

// pinLegacyHost records host as the owner of the copy's rows indexed before
// hosts existed, unless a host-aware build already recorded one, and returns
// the owner. Otherwise the first process to open the library with host-aware
// code would decide, and that may be an MCP call or a probe on another Mac.
func pinLegacyHost(path, host string) (string, error) {
	db, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var owner string
	if _, err = db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)", legacyHostKey, host); err == nil {
		err = db.QueryRow("SELECT value FROM meta WHERE key=?", legacyHostKey).Scan(&owner)
	}
	if err != nil {
		return "", fmt.Errorf("record this Mac as the owner of the copy's rows: %w", err)
	}
	return owner, nil
}

// migrateAdoptedCatalog runs every migration on the copy by opening it as the
// service would, checks it, and leaves it one self-contained, flushed file.
func migrateAdoptedCatalog(ctx context.Context, path, legacyHost string, full bool, progress io.Writer) (string, error) {
	started := time.Now()
	catalog, err := OpenCatalog(path)
	if err != nil {
		return "", fmt.Errorf("migrate the copied catalog: %w", err)
	}
	var owner string
	err = catalog.DB.QueryRow("SELECT value FROM meta WHERE key=?", legacyHostKey).Scan(&owner)
	if closeErr := catalog.Close(); err == nil {
		err = closeErr
	}
	if err != nil || owner != legacyHost {
		return "", fmt.Errorf("the migrated copy attributes its rows to host %q, not %q (%v)", owner, legacyHost, err)
	}
	// Close empties the WAL; what is left must be the whole catalog.
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return "", fmt.Errorf("the copied catalog kept a %d-byte WAL after closing", info.Size())
	}
	// quick_check reads every page of every table and index, which finds what
	// a bad copy could break, in 55-60% of integrity_check's time (measured on
	// a 7.8 GB catalog); the latter also matches index entries to rows, which
	// a page-for-page copy cannot change.
	check := "quick_check"
	if full {
		check = "integrity_check"
	}
	fmt.Fprintf(progress, "Migrated in %s; running %s\n", time.Since(started).Round(time.Millisecond), check)
	if err := adoptFaultAt("check"); err != nil {
		return "", err
	}
	started = time.Now()
	// Being the last to close, this connection removes the -wal and -shm
	// files it opens.
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=query_only(1)")
	if err != nil {
		return "", err
	}
	rows, err := queryMapsContext(ctx, db, "PRAGMA "+check+"(20)")
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("%s of the copied catalog: %w", check, err)
	}
	if len(rows) != 1 || firstString(rows[0][check]) != "ok" {
		problems := []string{}
		for _, row := range rows {
			problems = append(problems, firstString(row[check]))
		}
		return "", fmt.Errorf("the copied catalog failed %s: %s", check, strings.Join(problems, "; "))
	}
	fmt.Fprintf(progress, "%s ok in %s\n", check, time.Since(started).Round(time.Millisecond))
	return check, syncFile(path)
}

func checkAdoptSpace(dir string, catalog, preserved int64) error {
	var stat syscall.Statfs_t
	// Before anything is created dir may not exist; its parent does.
	if err := syscall.Statfs(dir, &stat); err != nil {
		if err := syscall.Statfs(filepath.Dir(dir), &stat); err != nil {
			return err
		}
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	margin := max(adoptMinMargin, catalog/20)
	if need := catalog + preserved + margin; free < need {
		return fmt.Errorf("%s has %s free, but adopting needs %s: %s for the catalog, %s for preserved packages, and %s for migrations",
			dir, byteSize(free), byteSize(need), byteSize(catalog), byteSize(preserved), byteSize(margin))
	}
	return nil
}

type copyMeter struct {
	w           io.Writer
	start, last time.Time
	tty, open   bool // open: a terminal line awaits its newline
}

func newCopyMeter(w io.Writer) *copyMeter {
	meter := &copyMeter{w: w, start: time.Now()}
	if file, ok := w.(*os.File); ok {
		if info, err := file.Stat(); err == nil {
			meter.tty = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return meter
}

// report rewrites one line on a terminal and logs a line every 5 s elsewhere.
func (m *copyMeter) report(done, total int64) {
	now := time.Now()
	interval := 5 * time.Second
	if m.tty {
		interval = 250 * time.Millisecond
	}
	if done < total && now.Sub(m.last) < interval {
		return
	}
	m.last = now
	rate := float64(done) / max(now.Sub(m.start).Seconds(), 1e-3)
	line := fmt.Sprintf("Copying the catalog: %s of %s (%d%%), %s/s", byteSize(done), byteSize(total), done*100/max(total, 1), byteSize(int64(rate)))
	if done < total && rate > 0 {
		line += ", about " + time.Duration(float64(total-done)/rate*float64(time.Second)).Round(time.Second).String() + " left"
	}
	if m.tty {
		fmt.Fprintf(m.w, "\r%s\033[K", line)
		m.open = true
		if done >= total {
			m.end()
		}
		return
	}
	fmt.Fprintln(m.w, line)
}

func (m *copyMeter) end() {
	if m.open {
		fmt.Fprintln(m.w)
		m.open = false
	}
}

func byteSize(bytes int64) string {
	switch {
	case bytes >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(bytes)/1e9)
	case bytes >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(bytes)/1e6)
	case bytes >= 1e3:
		return fmt.Sprintf("%.1f kB", float64(bytes)/1e3)
	}
	return fmt.Sprintf("%d bytes", bytes)
}

type tomlAssignment struct{ key, value string }

// readLegacyConfig returns a configuration's top-level assignments and its
// [[sources]] blocks, with values as written, divided up as LoadConfig does.
func readLegacyConfig(path string) (map[string]string, [][]tomlAssignment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	top := map[string]string{}
	var blocks [][]tomlAssignment
	for _, line := range strings.Split(string(data), "\n") {
		line = stripTOMLComment(strings.TrimSpace(line))
		if line == "[[sources]]" {
			blocks = append(blocks, nil)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "[") {
			continue
		}
		assignment := tomlAssignment{strings.TrimSpace(key), strings.TrimSpace(value)}
		if len(blocks) == 0 {
			top[assignment.key] = assignment.value
		} else {
			blocks[len(blocks)-1] = append(blocks[len(blocks)-1], assignment)
		}
	}
	return top, blocks, nil
}

// adoptedHostFile writes the legacy [[sources]] as this Mac's host file with
// every assignment as written, except that a path relative to the legacy
// configuration becomes absolute (~/... under the home directory), since
// relative paths in a host file resolve against the library.
func adoptedHostFile(host Host, legacyDir string, blocks [][]tomlAssignment) (string, []string) {
	var text strings.Builder
	text.WriteString(hostFileHeader(host))
	names := []string{}
	for _, block := range blocks {
		text.WriteString("\n[[sources]]\n")
		for _, assignment := range block {
			value := assignment.value
			switch assignment.key {
			case "name":
				names = append(names, asString(parseTOMLScalar(value)))
			case "path":
				if path := expandPath(asString(parseTOMLScalar(value))); path != "" && !filepath.IsAbs(path) {
					value = strconv.Quote(tildePath(filepath.Join(legacyDir, path)))
				}
			}
			fmt.Fprintf(&text, "%s = %s\n", assignment.key, value)
		}
	}
	return text.String(), names
}

// adoptedSettings carries over the install's retention and pacing settings.
// Everything else stays as init-library writes it: locations, port, and token
// describe the library rather than the install; reclamation has to be proven
// again against the library's archive; and secrets and the TL1 hook URL name
// this Mac's services and do not belong on a drive that travels.
func adoptedSettings(legacy Config, top map[string]string) (string, []string) {
	lines := []string{
		fmt.Sprintf("package_cap_bytes = %d", legacy.PackageCapBytes),
		fmt.Sprintf("upcoming_days = %d", legacy.UpcomingDays),
		fmt.Sprintf("eligible_days = %d", legacy.EligibleDays),
	}
	float := func(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
	for _, setting := range []tomlAssignment{
		{"snooze_days", strconv.Itoa(legacy.SnoozeDays)},
		{"staging_cap_bytes", strconv.FormatInt(legacy.StagingCapBytes, 10)},
		{"cpu_idle_ceiling", float(legacy.CPUIDLECeiling)},
		{"io_mbps_ceiling", float(legacy.IOMBPSCeiling)},
		{"yield_poll_seconds", float(legacy.YieldPollSeconds)},
		{"capture_snapshot_generations", strconv.Itoa(legacy.CaptureGenerations)},
	} {
		if _, ok := top[setting.key]; ok {
			lines = append(lines, setting.key+" = "+setting.value)
		}
	}
	notes := []string{}
	for _, key := range []string{"enable_reclamation", "release_hook_proven"} {
		if parseTOMLScalar(top[key]) == true {
			notes = append(notes, key+" = true: reclamation stays disabled in the library until it is proven again against the library's archive")
		}
	}
	for _, secret := range []tomlAssignment{
		{"tl1_url", "it names a hook on this Mac and serves only reclamation"},
		{"tl1_token", "set AIWA_TL1_TOKEN for the service instead of storing it on the drive"},
		{"github_token", "set GITHUB_TOKEN for the service instead of storing it on the drive"},
	} {
		if _, ok := top[secret.key]; ok {
			notes = append(notes, secret.key+": "+secret.value)
		}
	}
	if info, err := os.Stat(legacy.CaptureRoot); err == nil && info.IsDir() {
		notes = append(notes, "captures in "+legacy.CaptureRoot+" stay there; the library captures into its own captures/")
	}
	return strings.Join(lines, "\n"), notes
}

type preservedCopy struct {
	Root       string // archive_root as configured
	Real       string // with symlinks resolved; "" when nothing is copied
	Files      int    // regular files under Root
	Bytes      int64  // their total size
	Copied     int    // files written by this attempt; the rest were already there
	SameVolume bool
	Note       string
}

// planPreserved resolves the legacy archive_root and estimates what copying it
// writes. It refuses an archive_root that contains the library or lies inside
// it, since copying either into the other never ends, except the library's own
// preserved/, which needs no copy.
func planPreserved(root, dir string) (preservedCopy, error) {
	plan := preservedCopy{Root: root}
	real, err := filepath.EvalSymlinks(root)
	info, statErr := os.Stat(real)
	switch {
	case err != nil || statErr != nil:
		plan.Note = root + " is not available (is its drive mounted?); nothing was copied from it"
		return plan, nil
	case !info.IsDir():
		plan.Note = root + " is not a directory; nothing was copied from it"
		return plan, nil
	}
	if target, err := os.Stat(filepath.Join(dir, "preserved")); err == nil && os.SameFile(info, target) {
		plan.Note = root + " already is the library's preserved/"
		return plan, nil
	}
	if insideByInode(dir, real) || insideByInode(real, dir) {
		return plan, fmt.Errorf("archive_root %s (%s) and the library %s contain one another, so its packages cannot be copied into the library; choose a library directory outside archive_root, or move archive_root", root, real, dir)
	}
	plan.Real, plan.Bytes = real, treeSize(real)
	plan.SameVolume = sameDevice(real, filepath.Dir(dir))
	return plan, nil
}

// insideByInode reports whether path, or the part of it that exists, is dir
// or lies inside it. It compares devices and inodes, so neither symlinks,
// case, nor firmlinks hide a match.
func insideByInode(path, dir string) bool {
	top, err := os.Stat(dir)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(nearestExisting(path))
	if err != nil {
		return false
	}
	for {
		if info, err := os.Stat(real); err == nil && os.SameFile(info, top) {
			return true
		}
		parent := filepath.Dir(real)
		if parent == real {
			return false
		}
		real = parent
	}
}

// copyPreserved copies the planned archive into the library's preserved/ and
// verifies each file's size. A file already there with the same size is kept;
// hard links (packages link their blobs) stay links, also between files kept
// and files copied. What it creates is appended to created.
func copyPreserved(ctx context.Context, plan *preservedCopy, to string, created *[]string) error {
	if plan.Real == "" {
		return nil
	}
	plan.Bytes = 0
	library, err := os.Stat(filepath.Dir(to))
	if err != nil {
		return err
	}
	links := map[[2]uint64]string{}
	return filepath.WalkDir(plan.Real, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(plan.Real, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if os.SameFile(info, library) {
				return fmt.Errorf("%s holds the library %s", plan.Root, filepath.Dir(to))
			}
			if err := os.Mkdir(dest, 0o755); err == nil {
				*created = append(*created, dest)
			} else if !errors.Is(err, fs.ErrExist) {
				return err
			}
			return nil
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if _, statErr := os.Lstat(dest); err == nil && errors.Is(statErr, fs.ErrNotExist) {
				if err = os.Symlink(link, dest); err == nil {
					*created = append(*created, dest)
				}
			}
			return err
		case !info.Mode().IsRegular():
			return nil
		}
		plan.Files++
		plan.Bytes += info.Size()
		first := ""
		var inode [2]uint64
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
			inode = [2]uint64{uint64(stat.Dev), stat.Ino}
			first = links[inode]
		}
		if existing, err := os.Lstat(dest); err == nil && existing.Mode().IsRegular() && existing.Size() == info.Size() {
			if first == "" {
				if inode != ([2]uint64{}) {
					links[inode] = dest
				}
				return nil
			}
			if linked, err := os.Stat(first); err == nil && os.SameFile(existing, linked) {
				return nil
			}
			// A separate copy of what the archive links, e.g. from an
			// interrupted attempt: link it again below.
		}
		os.Remove(dest)
		if first == "" || os.Link(first, dest) != nil {
			if err := copyFileSynced(path, dest, info); err != nil {
				return err
			}
			if inode != ([2]uint64{}) {
				links[inode] = dest
			}
		}
		*created = append(*created, dest)
		plan.Copied++
		if copied, err := os.Lstat(dest); err != nil || copied.Size() != info.Size() {
			return fmt.Errorf("%s was not copied completely", dest)
		}
		return nil
	})
}

func copyFileSynced(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary := dst + adoptPartialSuffix
	out, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		// Written through to the drive; library.toml's full flush comes last.
		err = syscall.Fsync(int(out.Fd()))
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chtimes(temporary, info.ModTime(), info.ModTime())
	}
	if err == nil {
		err = os.Rename(temporary, dst)
	}
	if err != nil {
		os.Remove(temporary)
	}
	return err
}

func sameDevice(a, b string) bool {
	var statA, statB syscall.Stat_t
	return syscall.Stat(a, &statA) == nil && syscall.Stat(b, &statB) == nil && statA.Dev == statB.Dev
}

// syncFile flushes a file through the drive's own cache: os.File.Sync is
// F_FULLFSYNC on macOS.
func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func syncDir(path string) {
	if dir, err := os.Open(path); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
}

// runAdoptCLI is `init-library DIR --adopt CONFIG [--full-check]`. Adoption
// is init-library with an existing catalog: the same library.toml, the same
// refusal to overwrite one, and install-library.sh runs either.
func runAdoptCLI(args []string) error {
	usage := errors.New("usage: alexandria init-library DIR --adopt CONFIG [--full-check]")
	var dir, legacy string
	full := false
	for index := 0; index < len(args); index++ {
		switch arg := args[index]; {
		case arg == "--adopt" && index+1 < len(args):
			legacy = args[index+1]
			index++
		case strings.HasPrefix(arg, "--adopt="):
			legacy = strings.TrimPrefix(arg, "--adopt=")
		case arg == "--full-check":
			full = true
		case dir == "" && !strings.HasPrefix(arg, "-"):
			dir = arg
		default:
			return usage
		}
	}
	if dir == "" || legacy == "" {
		return usage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The copy stops at the first interrupt. Migrations cannot stop midway, so
	// a second interrupt kills the process, which leaves only the partial copy.
	go func() { <-ctx.Done(); stop() }()
	result, err := AdoptLibrary(ctx, dir, legacy, AdoptOptions{Identity: volumeIdentity, Progress: os.Stderr, FullCheck: full})
	if err != nil {
		return fmt.Errorf("%w\nNothing was adopted: %s holds no library, and the install at %s was only read", err, dir, legacy)
	}
	printAdoptResult(os.Stdout, result)
	return nil
}

func printAdoptResult(w io.Writer, result AdoptResult) {
	config, legacy := result.Config, result.Legacy
	dir := filepath.Dir(config.Path)
	rate := float64(result.CatalogBytes) / max(result.CopyTime.Seconds(), 1e-3)
	fmt.Fprintf(w, "Adopted %s as the library %s.\n", legacy.Path, dir)
	fmt.Fprintf(w, "  Catalog: %s copied in %s (%s/s) from %s, migrated, %s ok.\n",
		byteSize(result.CatalogBytes), result.CopyTime.Round(time.Second), byteSize(int64(rate)), legacy.CatalogPath, result.Check)
	if result.LegacyHost == result.Host.ID {
		fmt.Fprintf(w, "  Rows indexed before hosts existed belong to this Mac, %s (%s).\n", result.Host.Label, result.Host.ID)
	} else {
		fmt.Fprintf(w, "  Rows indexed before hosts existed belong to host %s, as the catalog already recorded.\n", result.LegacyHost)
	}
	fmt.Fprintf(w, "  Sources (%s) are this Mac's, in %s.\n", strings.Join(result.Sources, ", "), result.HostFile)
	preserved := result.Preserved
	switch {
	case preserved.Note != "":
		fmt.Fprintf(w, "  Preserved packages: %s.\n", preserved.Note)
	case preserved.Files == 0:
		fmt.Fprintf(w, "  Preserved packages: none in %s.\n", preserved.Root)
	default:
		fmt.Fprintf(w, "  Preserved packages: %d files (%s) from %s, %d copied now, sizes verified.\n", preserved.Files, byteSize(preserved.Bytes), preserved.Root, preserved.Copied)
	}
	if preserved.Real != "" && preserved.Real != preserved.Root {
		fmt.Fprintf(w, "  %s resolves to %s, which was copied.\n", preserved.Root, preserved.Real)
	}
	if preserved.SameVolume {
		fmt.Fprintf(w, "  %s is already on the library's volume", preserved.Root)
		if preserved.Files > 0 {
			fmt.Fprint(w, "; once satisfied, delete it to reclaim the space its copy in preserved/ now also takes")
		}
		fmt.Fprintln(w, ".")
	}
	if config.VolumeID != "" {
		fmt.Fprintf(w, "  The library is pinned to volume %s.\n", config.VolumeID)
	} else {
		fmt.Fprintf(w, "  The library is not pinned to a volume: none with a stable identity holds %s.\n", dir)
	}
	if len(result.Notes) > 0 {
		fmt.Fprintln(w, "Not carried over from", legacy.Path+":")
		for _, note := range result.Notes {
			fmt.Fprintln(w, "  -", note)
		}
	}
	app := filepath.Join(dir, "Pharos.app")
	build := fmt.Sprintf("Build the app into the library: macos/install-library.sh %s (it keeps this library.toml).", shellQuote(dir))
	if _, err := os.Stat(app); err == nil {
		build = fmt.Sprintf("%s is installed; rerunning macos/install-library.sh %s updates it and keeps library.toml.", app, shellQuote(dir))
	}
	fmt.Fprintf(w, `Next steps:
  1. %s
  2. Quit the old Pharos, which serves %s on port %d. Both apps share a bundle ID, so macOS may bring the running one forward instead of opening the library's.
  3. Open %s. It serves the library on port %d and resumes indexing this Mac's sources from where the copy left off.
  4. Point MCP clients at %s instead of "--config %s mcp". Opening the app writes it, as does `+"`alexandria install-mcp`"+`.
  5. Keep the old install as a fallback until satisfied; it no longer receives anything new. To remove it later, delete %s and its -wal/-shm files.
`, build, legacy.Path, legacy.Port, app, config.Port, mcpLauncherPath(pharosSupportDir()), legacy.Path, legacy.CatalogPath)
}
