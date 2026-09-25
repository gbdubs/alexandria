package archive

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Trimmed `diskutil info -plist` output: Euclid as found (external APFS, not
// encrypted), and an internal Apple silicon data volume without FileVault.
const euclidPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>APFSPhysicalStores</key>
	<array>
		<dict>
			<key>APFSPhysicalStore</key>
			<string>disk4s2</string>
		</dict>
	</array>
	<key>BusProtocol</key>
	<string>PCI-Express</string>
	<key>Encryption</key>
	<false/>
	<key>FileVault</key>
	<false/>
	<key>Internal</key>
	<false/>
	<key>ParentWholeDisk</key>
	<string>disk5</string>
	<key>TotalSize</key>
	<integer>4000577273856</integer>
	<key>VolumeName</key>
	<string>euclid</string>
	<key>VolumeUUID</key>
	<string>642C2C39-5926-4831-ABE1-34642F37B103</string>
</dict>
</plist>
`

// quietDriveCache waits out drive refreshes that /api/health calls in earlier
// tests started in the background, which read driveTool, and empties the
// cache.
func quietDriveCache(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		drives.mu.Lock()
		if !drives.refreshing {
			drives.key, drives.state = "", nil
			drives.mu.Unlock()
			return
		}
		drives.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("a drive refresh never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stubDriveTool(t *testing.T, outputs map[string]string) *[]string {
	t.Helper()
	quietDriveCache(t)
	calls := &[]string{}
	var mu sync.Mutex
	previous := driveTool
	driveTool = func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		output, ok := outputs[filepath.Base(name)]
		if !ok {
			return nil, fmt.Errorf("%s is not stubbed", name)
		}
		return []byte(output), nil
	}
	t.Cleanup(func() { quietDriveCache(t); driveTool = previous })
	return calls
}

func plistWith(base string, replacements ...string) string {
	return strings.NewReplacer(replacements...).Replace(base)
}

func TestParsePlist(t *testing.T) {
	value, err := parsePlist([]byte(euclidPlist))
	if err != nil {
		t.Fatal(err)
	}
	info := value.(map[string]any)
	stores := info["APFSPhysicalStores"].([]any)
	if plistString(info, "VolumeName") != "euclid" || info["TotalSize"] != int64(4000577273856) || plistBool(info, "Internal") ||
		plistString(stores[0].(map[string]any), "APFSPhysicalStore") != "disk4s2" {
		t.Fatalf("parsed %#v", info)
	}
	if wholeDisk("disk4s2") != "disk4" || wholeDisk("disk12s3s1") != "disk12" || wholeDisk("disk7") != "disk7" {
		t.Fatal("wholeDisk")
	}
}

func TestInspectVolumeReadsEncryptionSpotlightAndLocation(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, diskutil, mdutil, hdiutil string
		location, encryption, spotlight string
	}{
		{"euclid as found", euclidPlist, "/Volumes/euclid:\n\tIndexing enabled. \n", "", "external", "none", "enabled"},
		{"encrypted external", plistWith(euclidPlist, "<key>FileVault</key>\n\t<false/>", "<key>FileVault</key>\n\t<true/>"), "Indexing disabled.", "", "external", "filevault", "disabled"},
		{"internal without FileVault", plistWith(euclidPlist, "<key>Internal</key>\n\t<false/>", "<key>Internal</key>\n\t<true/>", "<key>Encryption</key>\n\t<false/>", "<key>Encryption</key>\n\t<true/>"),
			"Error: unknown indexing state.", "", "internal", "hardware-only", "unknown"},
		{"encrypted disk image", plistWith(euclidPlist, "PCI-Express", "Disk Image"), "Indexing and searching disabled.",
			`<plist><dict><key>images</key><array><dict><key>image-path</key><string>/tmp/x.dmg</string><key>image-encrypted</key><true/>
			<key>system-entities</key><array><dict><key>dev-entry</key><string>/dev/disk7s1</string><key>mount-point</key><string>MOUNT</string></dict></array></dict></array></dict></plist>`,
			"disk-image", "disk-image", "disabled"},
	}
	mount, _, err := volumeMount(dir)
	if err != nil {
		t.Skip(err)
	}
	for _, test := range cases {
		stubDriveTool(t, map[string]string{"diskutil": test.diskutil, "mdutil": test.mdutil, "hdiutil": strings.ReplaceAll(test.hdiutil, "MOUNT", mount)})
		facts := inspectVolume(context.Background(), filepath.Join(dir, "not", "yet", "created"))
		if facts.Location != test.location || facts.Encryption != test.encryption || facts.Spotlight != test.spotlight || facts.MountPoint != mount {
			t.Errorf("%s: %+v", test.name, facts)
		}
		if test.name == "euclid as found" && (facts.Name != "euclid" || facts.PhysicalDisk != "disk4" || facts.UUID != "642C2C39-5926-4831-ABE1-34642F37B103") {
			t.Errorf("euclid facts: %+v", facts)
		}
		if test.name == "encrypted disk image" && facts.ImagePath != "/tmp/x.dmg" {
			t.Errorf("image path: %+v", facts)
		}
	}
}

func checkNamed(t *testing.T, report map[string]any, id string) driveCheck {
	t.Helper()
	for _, check := range report["checks"].([]driveCheck) {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("no %s check in %v", id, report["checks"])
	return driveCheck{}
}

// The checks recommend commands for the user; Pharos itself only ever runs
// read-only inspection commands.
func TestDriveChecksRecommendWithoutRunningAnything(t *testing.T) {
	config, _ := backupFixture(t)
	config.VolumeID = ""
	calls := stubDriveTool(t, map[string]string{"diskutil": euclidPlist, "mdutil": "Indexing enabled."})
	state := collectDrive(context.Background(), config)
	state.facts.MountPoint, state.facts.Filesystem = "/Volumes/euclid", "apfs"
	report := driveReport(config, state)
	if report["status"] != "problem" || report["scope"] != "library" {
		t.Fatalf("report: %v", report)
	}
	encryption := checkNamed(t, report, "encryption")
	if encryption.Status != "problem" || encryption.Command != "diskutil apfs encryptVolume /Volumes/euclid -user disk" || !strings.Contains(encryption.Action, `Encrypt "euclid"`) {
		t.Errorf("encryption: %+v", encryption)
	}
	spotlight := checkNamed(t, report, "spotlight")
	if spotlight.Status != "warning" || spotlight.Command != "sudo mdutil -i off /Volumes/euclid" {
		t.Errorf("spotlight: %+v", spotlight)
	}
	if check := checkNamed(t, report, "filesystem"); check.Status != "ok" {
		t.Errorf("filesystem: %+v", check)
	}
	if check := checkNamed(t, report, "location"); check.Status != "ok" || !strings.Contains(check.Summary, "PCI-Express") {
		t.Errorf("location: %+v", check)
	}
	if check := checkNamed(t, report, "volume_pin"); check.Status != "warning" || !strings.Contains(check.Recommendation, `volume_id = "uuid:642C2C39-5926-4831-ABE1-34642F37B103"`) {
		t.Errorf("volume pin: %+v", check)
	}
	backup := checkNamed(t, report, "backup")
	if backup.Status != "warning" || !strings.Contains(backup.Command, " backup ") || !strings.Contains(backup.Summary, "only copy") {
		t.Errorf("backup: %+v", backup)
	}
	sizes := report["sizes"].(librarySizes)
	if sizes.CatalogBytes == 0 || sizes.CapturesBytes == 0 || sizes.LibraryBytes != sizes.CatalogBytes+sizes.CapturesBytes+sizes.PreservedBytes {
		t.Errorf("sizes: %+v", sizes)
	}
	if len(*calls) == 0 {
		t.Fatal("nothing was inspected")
	}
	for _, call := range *calls {
		if !strings.HasPrefix(call, "/usr/sbin/diskutil info -plist /") && !strings.HasPrefix(call, "/usr/bin/mdutil -s /") && call != "/usr/bin/hdiutil info -plist" {
			t.Errorf("ran %q", call)
		}
	}

	// Fixed: encrypted, not indexed, pinned, and backed up recently.
	config.VolumeID = "uuid:642C2C39-5926-4831-ABE1-34642F37B103"
	state.facts.Encryption, state.facts.Spotlight = "filevault", "disabled"
	if err := recordBackup(config, backupRecord{FinishedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano), Destination: "/Volumes/Other/Pharos Backup", BytesTotal: 5e9}); err != nil {
		t.Fatal(err)
	}
	report = driveReport(config, state)
	if report["status"] != "ok" {
		t.Fatalf("fixed drive: %v", report["checks"])
	}
	if check := checkNamed(t, report, "backup"); !strings.Contains(check.Summary, "2 hours ago") || check.Command != "" {
		t.Errorf("fresh backup: %+v", check)
	}
	// A stale backup suggests backing up to the same place again.
	stale := backupHistory{Backups: []backupRecord{{FinishedAt: time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano), Destination: "/Volumes/Other/Pharos Backup"}}}
	if check := backupCheck(config, stale, state.facts); check.Status != "warning" || !strings.Contains(check.Summary, "10 days ago") || !strings.HasSuffix(check.Command, "backup '/Volumes/Other/Pharos Backup'") {
		t.Errorf("stale backup: %+v", check)
	}
}

func TestDriveChecksForOtherSetups(t *testing.T) {
	internal := volumeFacts{MountPoint: "/System/Volumes/Data", Name: "Data", Filesystem: "apfs", Location: "internal", Encryption: "hardware-only", Spotlight: "enabled"}
	if check := encryptionCheck(internal); check.Status != "problem" || check.Command != "sudo fdesetup enable" || !strings.Contains(check.Action, "FileVault") {
		t.Errorf("internal encryption: %+v", check)
	}
	if check := spotlightCheck(internal, "/Users/me/Pharos"); check.Status != "info" || check.Command != "" || !strings.Contains(check.Action, "/Users/me/Pharos") {
		t.Errorf("internal spotlight: %+v", check)
	}
	if check := spotlightCheck(internal, "/Users/me/Pharos.noindex"); check.Status != "ok" {
		t.Errorf("noindex: %+v", check)
	}
	exfat := volumeFacts{MountPoint: "/Volumes/STICK", Name: "STICK", Filesystem: "exfat", Location: "external", Encryption: "none"}
	if check := filesystemCheck(exfat); check.Status != "problem" || !strings.Contains(check.Recommendation, "erases everything") {
		t.Errorf("exfat: %+v", check)
	}
	if check := encryptionCheck(exfat); check.Status != "problem" || check.Command != "" {
		t.Errorf("exfat encryption must not suggest encryptVolume: %+v", check)
	}
	odd := volumeFacts{MountPoint: "/Volumes/My Drive's", Name: "My Drive's", Filesystem: "apfs", Location: "external", Encryption: "none"}
	if check := encryptionCheck(odd); check.Command != `diskutil apfs encryptVolume '/Volumes/My Drive'\''s' -user disk` {
		t.Errorf("quoting: %q", check.Command)
	}
	facts := volumeFacts{Name: "euclid", Location: "external"}
	day := func(days int) string {
		return time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	}
	growing := backupHistory{Backups: []backupRecord{{FinishedAt: day(0), BytesTotal: 60e9}, {FinishedAt: day(5), BytesTotal: 55e9}, {FinishedAt: day(10), BytesTotal: 50e9}}}
	check := spaceCheck(facts, librarySizes{LibraryBytes: 60e9}, 80e9, growing)
	if check.Status != "warning" || !strings.Contains(check.Summary, "1.0 GB a day") || !strings.Contains(check.Summary, "lasts about 79 days") && !strings.Contains(check.Summary, "lasts about 80 days") {
		t.Errorf("growing library: %+v", check)
	}
	if check := spaceCheck(facts, librarySizes{LibraryBytes: 60e9}, 500e6, backupHistory{}); check.Status != "problem" {
		t.Errorf("nearly full: %+v", check)
	}
	if check := spaceCheck(facts, librarySizes{LibraryBytes: 60e9}, 4e12, backupHistory{}); check.Status != "ok" {
		t.Errorf("roomy: %+v", check)
	}
}

// Health answers from caches: diskutil runs once for the drive facts and once
// for the volume identity, however often it is polled, and a diskutil that
// hangs holds a poll (and so a release) up for at most identityTimeout.
func TestDriveHealthAnswersFromCacheAndRefreshesInBackground(t *testing.T) {
	config, _ := backupFixture(t)
	quietDriveCache(t)
	previousTimeout := identityTimeout
	identityTimeout = 200 * time.Millisecond
	var diskutil atomic.Int32
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	previous := driveTool
	driveTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "diskutil") {
			diskutil.Add(1)
			select {
			case <-release:
				return []byte(euclidPlist), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte("Indexing enabled."), nil
	}
	t.Cleanup(func() { unblock(); quietDriveCache(t); driveTool = previous; identityTimeout = previousTimeout })
	server := probeServer(t, config.Path)
	started := time.Now()
	for range 3 {
		_, health := probeCall(t, server, http.MethodGet, "/api/health", "")
		if drive := health["drive"].(map[string]any); drive["status"] != "checking" {
			t.Fatalf("drive before the first check finished: %v", drive)
		}
	}
	// The first poll waited out the identity lookup; the others did not.
	if elapsed := time.Since(started); elapsed > identityTimeout+time.Second {
		t.Fatalf("health waited for a hung diskutil: %v", elapsed)
	}
	unblock()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, health := probeCall(t, server, http.MethodGet, "/api/health", "")
		if drive := health["drive"].(map[string]any); drive["status"] != "checking" {
			if drive["scope"] != "library" || drive["checks"] == nil {
				t.Fatalf("drive: %v", drive)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drive checks never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := diskutil.Load()
	for range 10 {
		probeCall(t, server, http.MethodGet, "/api/health", "")
	}
	// One identity lookup and one drive inspection, nothing per poll.
	if calls := diskutil.Load(); calls != 2 || before != 2 {
		t.Fatalf("diskutil ran %d times (%d before the last 10 polls)", calls, before)
	}
	if _, err := os.Stat(backupRecordPath(config)); err == nil {
		t.Fatal("health wrote to the library")
	}
}
