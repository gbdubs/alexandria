package archive

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The library drive holds the only copy of every archived conversation, which
// can include pasted secrets, and travels between Macs. doctor and /api/health
// report how that drive is set up; every problem comes with a recommendation
// and the command or Finder action for the user. Pharos never runs them.
//
// For a per-user configuration the checks apply to archive_root's drive.

const (
	// Health is polled, so the slow facts (diskutil, mdutil, hdiutil, and
	// walking the library for its size) are cached.
	driveFactsTTL    = 5 * time.Minute
	driveToolTimeout = 5 * time.Second
	backupFreshFor   = 7 * 24 * time.Hour
)

// driveTool runs a macOS tool; tests replace it.
var driveTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, driveToolTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// volumeFacts describes the volume holding a path.
type volumeFacts struct {
	MountPoint string `json:"mount_point"`
	Name       string `json:"name"`
	Filesystem string `json:"filesystem"`
	UUID       string `json:"volume_uuid,omitempty"`
	// Location is internal, external, disk-image or unknown.
	Location string `json:"location"`
	Protocol string `json:"protocol,omitempty"`
	// Encryption is filevault (a password unlocks it), disk-image (on an
	// encrypted image), hardware-only (an internal disk that unlocks without
	// a password), none, or unknown.
	Encryption string `json:"encryption"`
	// Spotlight is enabled, disabled, or unknown.
	Spotlight    string `json:"spotlight"`
	ImagePath    string `json:"image_path,omitempty"`
	PhysicalDisk string `json:"physical_disk,omitempty"`
	Error        string `json:"error,omitempty"`
}

// displayName names the volume the way Finder does.
func (f volumeFacts) displayName() string {
	switch {
	case f.Location == "internal":
		return "this Mac's internal disk"
	case f.Name != "":
		return f.Name
	case f.MountPoint != "":
		return filepath.Base(f.MountPoint)
	}
	return "the library's drive"
}

func inspectVolume(ctx context.Context, path string) volumeFacts {
	facts := volumeFacts{Location: "unknown", Encryption: "unknown", Spotlight: "unknown"}
	mount, filesystem, err := volumeMount(nearestExisting(path))
	if err != nil {
		facts.Error = err.Error()
		return facts
	}
	facts.MountPoint, facts.Filesystem = mount, filesystem
	facts.Name = filepath.Base(mount)
	output, err := driveTool(ctx, "/usr/sbin/diskutil", "info", "-plist", mount)
	var info map[string]any
	if err == nil {
		value, parseErr := parsePlist(output)
		info, _ = value.(map[string]any)
		err = parseErr
	}
	if info == nil {
		facts.Error = fmt.Sprintf("diskutil info %s: %v", mount, err)
	} else {
		facts.UUID = plistString(info, "VolumeUUID")
		facts.Name = defaultString(plistString(info, "VolumeName"), facts.Name)
		facts.Protocol = plistString(info, "BusProtocol")
		internal := plistBool(info, "Internal")
		switch {
		case facts.Protocol == "Disk Image":
			facts.Location = "disk-image"
		case internal:
			facts.Location = "internal"
		default:
			facts.Location = "external"
		}
		// An internal Apple silicon or T2 data volume is always encrypted by
		// hardware, but only FileVault makes a password necessary to read it.
		switch {
		case plistBool(info, "FileVault"):
			facts.Encryption = "filevault"
		case plistBool(info, "Encryption") && internal:
			facts.Encryption = "hardware-only"
		case plistBool(info, "Encryption"):
			facts.Encryption = "filevault"
		default:
			facts.Encryption = "none"
		}
		facts.PhysicalDisk = plistString(info, "ParentWholeDisk")
		if stores, ok := info["APFSPhysicalStores"].([]any); ok && len(stores) > 0 {
			if store, ok := stores[0].(map[string]any); ok {
				facts.PhysicalDisk = wholeDisk(plistString(store, "APFSPhysicalStore"))
			}
		}
	}
	// An image's own encryption is invisible to diskutil.
	if facts.Location == "disk-image" {
		path, encryption := diskImage(ctx, mount)
		facts.ImagePath = path
		if facts.Encryption == "none" {
			facts.Encryption = encryption
		}
	}
	facts.Spotlight = spotlightStatus(ctx, mount)
	return facts
}

// wholeDisk turns disk4s2 into disk4.
func wholeDisk(device string) string {
	if at := strings.Index(strings.TrimPrefix(device, "disk"), "s"); strings.HasPrefix(device, "disk") && at > 0 {
		return device[:len("disk")+at]
	}
	return device
}

func diskImage(ctx context.Context, mount string) (string, string) {
	output, err := driveTool(ctx, "/usr/bin/hdiutil", "info", "-plist")
	if err != nil {
		return "", "unknown"
	}
	value, _ := parsePlist(output)
	info, _ := value.(map[string]any)
	images, _ := info["images"].([]any)
	for _, item := range images {
		image, _ := item.(map[string]any)
		entities, _ := image["system-entities"].([]any)
		for _, entity := range entities {
			if values, _ := entity.(map[string]any); values != nil && plistString(values, "mount-point") == mount {
				if plistBool(image, "image-encrypted") {
					return plistString(image, "image-path"), "disk-image"
				}
				return plistString(image, "image-path"), "none"
			}
		}
	}
	return "", "unknown"
}

func spotlightStatus(ctx context.Context, mount string) string {
	output, err := driveTool(ctx, "/usr/bin/mdutil", "-s", mount)
	text := strings.ToLower(string(output))
	switch {
	case err != nil:
		return "unknown"
	case strings.Contains(text, "indexing enabled"):
		return "enabled"
	case strings.Contains(text, "disabled"):
		return "disabled"
	}
	return "unknown"
}

func nearestExisting(path string) string {
	path = filepath.Clean(path)
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func deviceOf(path string) (uint64, bool) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Dev), true
}

// librarySizes counts what the library keeps on the checked volume.
type librarySizes struct {
	CatalogBytes   int64 `json:"catalog_bytes"`
	CapturesBytes  int64 `json:"captures_bytes"`
	PreservedBytes int64 `json:"preserved_bytes"`
	LibraryBytes   int64 `json:"library_bytes"`
}

func measureLibrary(ctx context.Context, config Config, device uint64) librarySizes {
	on := func(path string) bool { value, ok := deviceOf(path); return ok && value == device }
	sizes := librarySizes{}
	if on(config.CatalogPath) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(config.CatalogPath + suffix); err == nil {
				sizes.CatalogBytes += info.Size()
			}
		}
	}
	if on(config.CaptureRoot) {
		sizes.CapturesBytes = treeSizeContext(ctx, config.CaptureRoot)
	}
	if on(config.ArchiveRoot) {
		sizes.PreservedBytes = treeSizeContext(ctx, config.ArchiveRoot)
	}
	sizes.LibraryBytes = sizes.CatalogBytes + sizes.CapturesBytes + sizes.PreservedBytes
	return sizes
}

func treeSizeContext(ctx context.Context, root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// driveScope is the directory whose drive the checks describe.
func driveScope(config Config) (string, string) {
	if config.Library {
		return "library", filepath.Dir(config.Path)
	}
	return "archive_root", config.ArchiveRoot
}

type driveState struct {
	facts   volumeFacts
	sizes   librarySizes
	checked time.Time
}

func collectDrive(ctx context.Context, config Config) *driveState {
	_, path := driveScope(config)
	state := &driveState{facts: inspectVolume(ctx, path), checked: time.Now()}
	if device, ok := deviceOf(path); ok {
		state.sizes = measureLibrary(ctx, config, device)
	}
	return state
}

// driveCache keeps the last drive state per checked directory and device,
// so a remount or another drive at the same path is checked afresh.
type driveCache struct {
	mu         sync.Mutex
	key        string
	state      *driveState
	refreshing bool
}

var drives driveCache

func driveKey(config Config) string {
	_, path := driveScope(config)
	device, _ := deviceOf(path)
	return fmt.Sprintf("%s\x00%d", path, device)
}

// cached returns the state for config, if any, and whether the caller should
// refresh it; only one caller at a time is told to.
func (c *driveCache) cached(config Config) (*driveState, bool) {
	key := driveKey(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != key {
		c.key, c.state = key, nil
	}
	stale := c.state == nil || time.Since(c.state.checked) > driveFactsTTL
	if !stale || c.refreshing {
		return c.state, false
	}
	c.refreshing = true
	return c.state, true
}

// peek returns the state for config, if any, without taking on a refresh.
func (c *driveCache) peek(config Config) *driveState {
	key := driveKey(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != key {
		return nil
	}
	return c.state
}

func (c *driveCache) store(config Config, state *driveState) {
	key := driveKey(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if state != nil && c.key == key {
		c.state = state
	}
}

// driveHealth answers from the cache at once and refreshes it in the
// background, so a slow diskutil never holds up a poll.
func (s *Server) driveHealth() map[string]any {
	config := s.Config()
	state, refresh := drives.cached(config)
	if refresh {
		s.spawn(func(ctx context.Context) {
			var fresh *driveState
			defer func() { drives.store(config, fresh) }()
			if state := collectDrive(ctx, config); ctx.Err() == nil {
				fresh = state
			}
		})
	}
	if state == nil {
		scope, path := driveScope(config)
		return map[string]any{"status": "checking", "scope": scope, "path": path, "checks": []driveCheck{}}
	}
	return driveReport(config, state)
}

// Health polls need the volume identity (for storage() and the pin check),
// which comes from diskutil. It is cached per path and device; a poll only
// waits for the first lookup, bounded by identityTimeout, and afterwards gets
// the cached value while at most one refresh runs in the background.
var (
	identityTTL     = time.Minute
	identityTimeout = 2 * time.Second
)

var volumeIdentities = struct {
	sync.Mutex
	values map[string]*cachedIdentity
}{values: map[string]*cachedIdentity{}}

type cachedIdentity struct {
	value   string
	expires time.Time
	pending chan struct{} // closed when the lookup in flight finishes
}

func cachedVolumeIdentity(path string) string {
	device, _ := deviceOf(path)
	key := fmt.Sprintf("%s\x00%d", path, device)
	volumeIdentities.Lock()
	entry := volumeIdentities.values[key]
	if entry == nil {
		entry = &cachedIdentity{}
		volumeIdentities.values[key] = entry
	}
	if entry.value != "" && time.Now().Before(entry.expires) {
		defer volumeIdentities.Unlock()
		return entry.value
	}
	pending := entry.pending
	if pending == nil {
		pending = make(chan struct{})
		entry.pending = pending
		go func() {
			value, ok := lookupVolumeIdentity(path)
			ttl := identityTTL
			if !ok {
				ttl = min(ttl, 10*time.Second) // retry a failed lookup soon
			}
			volumeIdentities.Lock()
			entry.value, entry.expires, entry.pending = value, time.Now().Add(ttl), nil
			volumeIdentities.Unlock()
			close(pending)
		}()
	}
	stale := entry.value
	volumeIdentities.Unlock()
	if stale != "" {
		return stale
	}
	<-pending
	volumeIdentities.Lock()
	defer volumeIdentities.Unlock()
	return entry.value
}

// lookupVolumeIdentity is volumeIdentity with one bounded diskutil call on the
// mount point, rather than one unbounded call per path component. It reports
// whether diskutil answered.
func lookupVolumeIdentity(path string) (string, bool) {
	if mount, _, err := volumeMount(nearestExisting(path)); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), identityTimeout)
		defer cancel()
		if output, err := driveTool(ctx, "/usr/sbin/diskutil", "info", "-plist", mount); err == nil {
			value, _ := parsePlist(output)
			info, _ := value.(map[string]any)
			for _, key := range []string{"VolumeUUID", "DiskUUID"} {
				if uuid := plistString(info, key); uuid != "" {
					return "uuid:" + uuid, true
				}
			}
		}
	}
	if device, ok := deviceOf(path); ok {
		return fmt.Sprintf("device:%d", device), false
	}
	return "unavailable", false
}

type driveCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // ok, info, unknown, warning, problem
	Summary string `json:"summary"`
	// Recommendation, Command and Action are for the user; Pharos never
	// runs them.
	Recommendation string `json:"recommendation,omitempty"`
	Command        string `json:"command,omitempty"`
	Action         string `json:"action,omitempty"`
}

var checkRank = map[string]int{"ok": 0, "info": 1, "unknown": 2, "warning": 3, "problem": 4}

func driveReport(config Config, state *driveState) map[string]any {
	scope, path := driveScope(config)
	history := loadBackupHistory(config)
	var last any
	if len(history.Backups) > 0 {
		last = history.Backups[0]
	}
	report := map[string]any{"scope": scope, "path": path, "last_backup": last}
	checks := []driveCheck{}
	if _, err := os.Stat(path); err != nil {
		report["status"] = "disconnected"
		checks = append(checks, backupCheck(config, history, volumeFacts{}))
		report["checks"] = checks
		return report
	}
	facts := state.facts
	total, free := diskUsage(path)
	report["volume"] = facts
	report["sizes"] = state.sizes
	report["total_bytes"], report["free_bytes"] = total, free
	report["checked_at"] = state.checked.UTC().Format(time.RFC3339)
	checks = append(checks, encryptionCheck(facts), spotlightCheck(facts, path), filesystemCheck(facts), locationCheck(facts, scope),
		spaceCheck(facts, state.sizes, int64(free), history), pinCheck(config, facts, path), backupCheck(config, history, facts))
	status := "ok"
	for _, check := range checks {
		if checkRank[check.Status] > checkRank[status] {
			status = check.Status
		}
	}
	report["status"] = status
	report["checks"] = checks
	return report
}

func unknownCheck(id string, facts volumeFacts) driveCheck {
	return driveCheck{ID: id, Status: "unknown", Summary: "Could not inspect the volume: " + defaultString(facts.Error, "no details")}
}

func encryptionCheck(facts volumeFacts) driveCheck {
	check := driveCheck{ID: "encryption", Status: "ok"}
	name := facts.displayName()
	switch {
	case facts.Encryption == "unknown":
		return unknownCheck("encryption", facts)
	case facts.Encryption == "filevault" && facts.Location == "internal":
		check.Summary = "This Mac's internal disk is protected by FileVault."
	case facts.Encryption == "filevault":
		check.Summary = fmt.Sprintf("%s is encrypted; it can be read only after its password is entered.", name)
	case facts.Encryption == "disk-image":
		check.Summary = fmt.Sprintf("The library is on an encrypted disk image (%s).", facts.ImagePath)
	case facts.Location == "internal":
		check.Status = "problem"
		check.Summary = "FileVault is off, so this Mac's internal disk unlocks without a password and anyone with the Mac can read the library."
		check.Recommendation = "Turn on FileVault. It encrypts in the background and keeps your data."
		check.Action = "System Settings → Privacy & Security → FileVault → Turn On…"
		check.Command = "sudo fdesetup enable"
	case facts.Location == "disk-image":
		check.Status = "problem"
		check.Summary = fmt.Sprintf("The library is on a disk image that is not encrypted (%s).", defaultString(facts.ImagePath, facts.MountPoint))
		check.Recommendation = "Create an encrypted image and copy the library into it, then retire this one."
		check.Action = "Disk Utility → File → New Image → Blank Image…, with Encryption set to 256-bit AES"
	default:
		check.Status = "problem"
		check.Summary = fmt.Sprintf("%s is not encrypted: anyone who picks up the drive can read every archived conversation, including any secrets pasted into them.", name)
		if facts.Filesystem != "apfs" {
			check.Recommendation = "macOS encrypts only APFS volumes; see the file system check first."
			return check
		}
		check.Recommendation = "Encrypt the volume. It encrypts in the background, keeps everything on the drive, and the drive stays usable meanwhile. Keep the password in a password manager: without it the library cannot be read, on any Mac."
		check.Action = fmt.Sprintf("In Finder, Control-click %q in the sidebar and choose Encrypt %q…", name, name)
		check.Command = "diskutil apfs encryptVolume " + shellWord(facts.MountPoint) + " -user disk"
	}
	return check
}

func spotlightCheck(facts volumeFacts, path string) driveCheck {
	check := driveCheck{ID: "spotlight", Status: "ok"}
	name := facts.displayName()
	switch {
	case facts.Spotlight == "unknown":
		check.Status = "unknown"
		check.Summary = "Could not read Spotlight's indexing status for " + name + "."
	case facts.Spotlight == "disabled":
		check.Summary = fmt.Sprintf("Spotlight does not index %s.", name)
	case strings.Contains(path, ".noindex"):
		check.Summary = "Spotlight skips the library: it is inside a folder named *.noindex."
	case facts.Location == "internal":
		// Turning indexing off for the startup disk would disable Spotlight
		// for the whole Mac, and folder exclusions are not readable here.
		check.Status = "info"
		check.Summary = "Spotlight indexes this Mac's internal disk. Unless you have excluded the library's folder, its file names and any plain-text files are in the Mac's Spotlight index."
		check.Recommendation = "Exclude the library's folder from Spotlight."
		check.Action = fmt.Sprintf("System Settings → Spotlight → Search Privacy… (Spotlight Privacy… on older macOS), click +, and add %s", path)
	default:
		check.Status = "warning"
		check.Summary = fmt.Sprintf("Spotlight indexes %s. It keeps an index of the drive's file names and any plain-text files on the drive, searchable from any Mac it is plugged into, and its indexer can keep the drive busy when you eject it.", name)
		check.Recommendation = "Turn off Spotlight indexing for this drive. If another Mac starts indexing it again, doctor reports it there."
		check.Command = "sudo mdutil -i off " + shellWord(facts.MountPoint)
		check.Action = fmt.Sprintf("Or System Settings → Spotlight → Search Privacy… (Spotlight Privacy… on older macOS), click +, and choose %s", name)
	}
	return check
}

func filesystemCheck(facts volumeFacts) driveCheck {
	check := driveCheck{ID: "filesystem", Status: "ok"}
	name := facts.displayName()
	switch facts.Filesystem {
	case "apfs":
		check.Summary = name + " uses APFS."
	case "hfs":
		check.Status = "warning"
		check.Summary = name + " uses Mac OS Extended (HFS+), which current macOS cannot encrypt."
		check.Recommendation = "Back up the library, then convert the volume to APFS; the conversion keeps the data."
		check.Action = "Disk Utility → select the volume → Edit → Convert to APFS…"
	case "exfat", "msdos":
		check.Status = "problem"
		check.Summary = name + " uses exFAT or FAT, which macOS cannot encrypt, which has no journal to survive an unplug mid-write, and which ignores file permissions."
		check.Recommendation = "Move the library to an APFS volume. Reformatting this drive erases everything on it: copy the library off first, erase the drive as APFS (Encrypted) in Disk Utility, and copy it back."
	case "smbfs", "afpfs", "nfs", "webdav":
		check.Status = "problem"
		check.Summary = fmt.Sprintf("The library is on a network volume (%s); SQLite databases are not safe on network file systems.", facts.Filesystem)
		check.Recommendation = "Keep the library on a local APFS drive."
	case "":
		return unknownCheck("filesystem", facts)
	default:
		check.Status = "info"
		check.Summary = fmt.Sprintf("%s uses %s.", name, facts.Filesystem)
	}
	return check
}

func locationCheck(facts volumeFacts, scope string) driveCheck {
	check := driveCheck{ID: "location", Status: "ok"}
	switch facts.Location {
	case "external":
		check.Summary = fmt.Sprintf("%s is an external drive (%s).", facts.displayName(), defaultString(facts.Protocol, "unknown bus"))
	case "disk-image":
		check.Status = "info"
		check.Summary = fmt.Sprintf("%s is a disk image (%s); it travels only with the drive holding the image.", facts.displayName(), defaultString(facts.ImagePath, "path unknown"))
	case "internal":
		check.Status = "info"
		check.Summary = "The library is on this Mac's internal disk, so it does not travel with you."
		if scope == "archive_root" {
			check.Summary = "archive_root is on this Mac's internal disk."
		}
	default:
		return unknownCheck("location", facts)
	}
	return check
}

func spaceCheck(facts volumeFacts, sizes librarySizes, free int64, history backupHistory) driveCheck {
	check := driveCheck{ID: "free_space", Status: "ok"}
	check.Summary = fmt.Sprintf("%s free on %s; the library uses %s there (catalog %s, captures %s, preserved %s).",
		humanBytes(free), facts.displayName(), humanBytes(sizes.LibraryBytes), humanBytes(sizes.CatalogBytes), humanBytes(sizes.CapturesBytes), humanBytes(sizes.PreservedBytes))
	rate, days := history.growth()
	if rate > 0 {
		left := float64(free) / rate
		check.Summary += fmt.Sprintf(" It grew about %s a day over the last %.0f days of backups; at that rate the free space lasts about %s.", humanBytes(int64(rate)), days, humanDays(left))
		switch {
		case left < 30:
			check.Status = "problem"
		case left < 90:
			check.Status = "warning"
		}
	} else if len(history.Backups) < 2 {
		check.Summary += " Its growth rate appears once backups have run on at least two days."
	}
	switch {
	case free < captureFreeMargin:
		check.Status = "problem"
		check.Summary += " Captures stop while less than 1 GB is free."
	case free < sizes.LibraryBytes && check.Status == "ok":
		// Captures keep previous snapshots and a sync can grow the catalog
		// by gigabytes before it is compacted.
		check.Status = "warning"
		check.Summary += " There is less free space than the library already uses."
	}
	if check.Status != "ok" {
		check.Recommendation = "Free up space on the drive, or move the library to a larger one."
	}
	return check
}

func pinCheck(config Config, facts volumeFacts, path string) driveCheck {
	check := driveCheck{ID: "volume_pin", Status: "ok"}
	actual := "uuid:" + facts.UUID
	if facts.UUID == "" {
		actual = cachedVolumeIdentity(path)
	}
	switch {
	case config.VolumeID == "" && config.Library:
		check.Status = "warning"
		check.Summary = "library.toml does not pin volume_id, so a copy of the library on another drive (a clone, or a restored backup) would be opened as if it were this one and could silently diverge."
		check.Recommendation = fmt.Sprintf("Pin it: set volume_id = %q in %s (the command prints the value).", actual, config.Path)
		check.Command = shellWord(executablePath()) + " volume-id " + shellWord(path)
	case config.VolumeID == "":
		check.Status = "info"
		check.Summary = "volume_id is not set, so a different drive mounted at archive_root would be accepted."
		check.Recommendation = fmt.Sprintf("Set volume_id = %q in %s.", actual, config.Path)
		check.Command = shellWord(executablePath()) + " volume-id " + shellWord(path)
	case config.VolumeID != actual:
		check.Status = "problem"
		check.Summary = fmt.Sprintf("volume_id pins %s, but %s is on %s.", config.VolumeID, path, actual)
		check.Recommendation = fmt.Sprintf("If this drive is the one you meant (for example after moving or restoring the library deliberately), set volume_id = %q in %s.", actual, config.Path)
	default:
		check.Summary = "Pinned to this volume (" + actual + ")."
	}
	return check
}

func backupCheck(config Config, history backupHistory, facts volumeFacts) driveCheck {
	check := driveCheck{ID: "backup", Status: "ok"}
	destination := filepath.Join(os.Getenv("HOME"), "Pharos Backup")
	if !config.Library {
		destination = "/Volumes/BACKUP-DRIVE/Pharos Backup"
	}
	if len(history.Backups) > 0 {
		destination = history.Backups[0].Destination
	}
	command := shellWord(executablePath()) + " --config " + shellWord(config.Path) + " backup " + shellWord(destination)
	if len(history.Backups) == 0 {
		check.Status = "warning"
		check.Summary = "No backup recorded of the catalog, captures and archive_root."
		if config.Library {
			check.Summary = fmt.Sprintf("No backup recorded: %s holds the only copy of the library.", facts.displayName())
		}
		check.Recommendation = "Back up to another drive, or to a folder on a Mac you trust that has FileVault on. Later backups copy only what changed."
		check.Command = command
		return check
	}
	last := history.Backups[0]
	finished, _ := time.Parse(time.RFC3339Nano, last.FinishedAt)
	age := time.Since(finished)
	check.Summary = fmt.Sprintf("Last backup %s ago to %s (%s).", humanDuration(age), last.Destination, humanBytes(last.BytesTotal))
	if last.Partial {
		check.Status = "warning"
		check.Summary = fmt.Sprintf("The last backup, %s ago to %s, was partial: %s could not be found, so the backup has only an earlier copy of it, if any.", humanDuration(age), last.Destination, strings.Join(last.Missing, ", "))
		check.Recommendation = "Connect the drive holding the missing part (or fix its path in the configuration) and back up again."
		check.Command = command
		return check
	}
	if age > backupFreshFor {
		check.Status = "warning"
		check.Recommendation = "Back up again; it copies only what changed since the last backup."
		check.Command = command
	}
	return check
}

// runDoctor never opens the catalog, so it also diagnoses a library that its
// guards refuse to open, such as one whose drive is not mounted.
func runDoctor(config Config) error {
	checks := map[string]any{"loopback_bind": config.Host == "127.0.0.1" || config.Host == "::1" || config.Host == "localhost", "api_token_configured": config.APIToken != "", "archive_storage": storage(config), "tl1_hook_configured": config.TL1URL != ""}
	sources := []map[string]any{}
	for _, source := range config.Sources {
		if !source.Enabled {
			continue
		}
		_, err := os.Stat(source.Path)
		sources = append(sources, map[string]any{"name": source.Name, "kind": source.Kind, "path": source.Path, "exists": err == nil})
	}
	checks["sources"] = sources
	checks["library_guard"] = "ok"
	if err := config.checkLibrary(volumeIdentity); err != nil {
		checks["library_guard"] = err.Error()
	}
	checks["drive"] = driveReport(config, collectDrive(context.Background(), config))
	return printJSON(checks)
}

func humanBytes(value int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	amount, unit := float64(value), 0
	for amount >= 1000 && unit < len(units)-1 {
		amount /= 1000
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", value)
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}

func humanDuration(value time.Duration) string {
	switch {
	case value < time.Minute:
		return "less than a minute"
	case value < time.Hour:
		return plural(int(value.Minutes()), "minute")
	case value < 48*time.Hour:
		return plural(int(value.Hours()), "hour")
	}
	return plural(int(value.Hours()/24), "day")
}

func humanDays(days float64) string {
	if days > 3650 || math.IsInf(days, 0) {
		return "more than ten years"
	}
	if days >= 730 {
		return fmt.Sprintf("%.0f years", days/365)
	}
	return plural(int(days), "day")
}

func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", count, unit)
}

// shellWord quotes value for a POSIX shell only when it needs quoting, so a
// command shown to the user stays readable.
func shellWord(value string) string {
	if value != "" && !strings.ContainsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%", r))
	}) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
