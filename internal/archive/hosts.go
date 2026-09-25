package archive

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Host is one Mac and OS user. The library travels between hosts, so source
// paths and sync state are recorded per host while identities are not.
type Host struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	User  string `json:"user"`
	// Fallback marks an ID derived from the hostname, because neither ioreg
	// nor an earlier run could name this Mac's hardware. Such an ID is not
	// this Mac's lasting identity, so nothing is recorded under it (see
	// errHostUnknown).
	Fallback bool `json:"fallback,omitempty"`
}

// errHostUnknown refuses to record anything under a fallback host ID, which
// would make this Mac look like a new host once ioreg answers again.
var errHostUnknown = errors.New("Pharos could not identify this Mac: ioreg did not report its hardware UUID and no earlier run recorded it. Quit and reopen Pharos to try again, or set PHAROS_HOST_ID")

// currentHost is computed once per process. Tests replace it to simulate a
// library carried between Macs.
var currentHost = sync.OnceValue(func() Host { return detectHost(pharosSupportDir(), platformUUID, quickHostDetection) })

// quickHostDetection gives ioreg one short try, for the MCP server, which
// must answer agents promptly.
var quickHostDetection bool

// legacyHostKey names the host that owned the catalog before hosts were
// recorded: conversations without origin_host_id were written from it.
const legacyHostKey = "legacy_host_id"

var platformUUIDPattern = regexp.MustCompile(`"IOPlatformUUID" = "([^"]+)"`)

// hostRecord is <support>/host.json, the host ID last derived from this Mac's
// hardware UUID.
type hostRecord struct {
	HardwareUUID string `json:"hardware_uuid"`
	User         string `json:"user"`
	ID           string `json:"id"`
}

func detectHost(support string, platform func(waits []time.Duration) (string, error), quick bool) Host {
	name := os.Getenv("USER")
	if account, err := user.Current(); err == nil && account.Username != "" {
		name = account.Username
	}
	hostname, _ := os.Hostname()
	label := defaultString(os.Getenv("PHAROS_HOST_LABEL"), defaultString(commandOutput("/usr/sbin/scutil", "--get", "ComputerName"), hostname))
	if id := strings.TrimSpace(os.Getenv("PHAROS_HOST_ID")); id != "" {
		return Host{ID: id, Label: label, User: name}
	}
	// The hardware UUID survives renames and reinstalls; the user keeps two
	// accounts on one Mac, with different home directories, apart.
	path := filepath.Join(support, "host.json")
	var record hostRecord
	data, err := os.ReadFile(path)
	recorded := err == nil && json.Unmarshal(data, &record) == nil && record.ID != "" && record.User == name
	// ioreg answers in well under a second unless the Mac is very busy. Wait
	// long only when the hostname would stand in for it.
	waits := []time.Duration{5 * time.Second, 15 * time.Second}
	if recorded || quick {
		waits = []time.Duration{2 * time.Second}
	}
	hardware, err := platform(waits)
	if err == nil {
		host := Host{ID: stableID("host", hardware, name), Label: label, User: name}
		recordHost(path, hostRecord{HardwareUUID: hardware, User: name, ID: host.ID})
		return host
	}
	// Another ID would make this Mac a new host, with its own onboarding,
	// captures and sync state, so keep the one last detected here. Migration
	// Assistant copies host.json to a new Mac, but only a failed ioreg falls
	// back to it: on the new Mac ioreg names the new hardware and rewrites it.
	if recorded {
		fmt.Fprintf(os.Stderr, "Pharos: %v; using this Mac's host ID recorded in %s\n", err, path)
		return Host{ID: record.ID, Label: label, User: name}
	}
	fmt.Fprintf(os.Stderr, "Pharos: %v; identifying this Mac by its hostname %q for now\n", err, hostname)
	return Host{ID: stableID("host", "hostname:"+hostname, name), Label: label, User: name, Fallback: true}
}

// platformUUID asks ioreg, by absolute path whatever PATH the process has,
// for the hardware UUID, once per wait.
func platformUUID(waits []time.Duration) (string, error) {
	err := errors.New("ioreg did not report IOPlatformUUID")
	for _, wait := range waits {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		output, runErr := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		cancel()
		if match := platformUUIDPattern.FindSubmatch(output); match != nil {
			return string(match[1]), nil
		}
		if runErr != nil {
			err = fmt.Errorf("ioreg did not report IOPlatformUUID: %w", runErr)
		}
	}
	return "", err
}

// recordHost writes host.json when it changed. Failing to is harmless.
func recordHost(path string, record hostRecord) {
	data, _ := json.MarshalIndent(record, "", "  ")
	data = append(data, '\n')
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil || os.WriteFile(temporary, data, 0o644) != nil || os.Rename(temporary, path) != nil {
		os.Remove(temporary)
	}
}

func commandOutput(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// registerHost records a host whose data is being ingested, seen at seen
// (default now): for another Mac's capture, when it was captured.
func (c *Catalog) registerHost(host Host, seen string) error {
	seen = defaultString(seen, now())
	_, err := c.DB.Exec(`INSERT INTO hosts(id,label,user_name,first_seen_at,last_seen_at) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET label=excluded.label,user_name=COALESCE(excluded.user_name,hosts.user_name),
		last_seen_at=MAX(excluded.last_seen_at,hosts.last_seen_at)`,
		host.ID, defaultString(host.Label, host.ID), nilIfEmpty(host.User), seen, seen)
	return err
}

func (c *Catalog) Hosts() ([]map[string]any, error) {
	rows, err := queryMaps(c.DB, "SELECT id,label,user_name,first_seen_at,last_seen_at FROM hosts ORDER BY last_seen_at DESC,id")
	for _, row := range rows {
		row["current"] = firstString(row["id"]) == currentHost().ID
	}
	return rows, err
}

// migrateHosts makes a pre-host catalog host-aware. Everything indexed before
// belongs to the Mac that created the catalog, which is the one first opening
// it with this code. It never rewrites conversations or messages.
func (c *Catalog) migrateHosts() error {
	has, err := c.hasColumn("conversations", "origin_host_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := c.DB.Exec("ALTER TABLE conversations ADD COLUMN origin_host_id TEXT"); err != nil {
			return err
		}
	}
	if host := currentHost(); !host.Fallback {
		if err := c.registerHost(host, ""); err != nil {
			return err
		}
		if _, err := c.DB.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)", legacyHostKey, host.ID); err != nil {
			return err
		}
	}
	var legacy string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key=?", legacyHostKey).Scan(&legacy); err == sql.ErrNoRows {
		// Unclaimed, under a fallback ID: fine while there is nothing to claim.
		var indexed bool
		if err := c.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM workspaces)").Scan(&indexed); err != nil || indexed {
			return cmp.Or(err, errHostUnknown)
		}
		return nil
	} else if err != nil {
		return err
	}
	if err := c.rehostSourceStates(legacy); err != nil {
		return err
	}
	return c.backfillSightings(legacy)
}

// rehostSourceStates moves pre-host sync state under the legacy host. SQLite
// cannot change a primary key in place, but both tables are small. Each step
// is restartable: a leftover *_legacy table is finished on the next open.
func (c *Catalog) rehostSourceStates(legacy string) error {
	for _, table := range []string{"source_states", "source_record_states"} {
		hosted, err := c.hasColumn(table, "host_id")
		if err != nil {
			return err
		}
		if !hosted {
			if _, err := c.DB.Exec("ALTER TABLE " + table + " RENAME TO " + table + "_legacy"); err != nil {
				return err
			}
		}
	}
	const legacyTables = "('source_states_legacy','source_record_states_legacy')"
	tables, err := queryMaps(c.DB, "SELECT name FROM sqlite_master WHERE type='table' AND name IN "+legacyTables)
	if err != nil || len(tables) == 0 {
		return err
	}
	// Indexes follow a renamed table; drop them so the schema recreates them.
	indexes, err := queryMaps(c.DB, "SELECT name FROM sqlite_master WHERE type='index' AND sql IS NOT NULL AND tbl_name IN "+legacyTables)
	if err != nil {
		return err
	}
	for _, index := range indexes {
		if _, err := c.DB.Exec(`DROP INDEX IF EXISTS "` + firstString(index["name"]) + `"`); err != nil {
			return err
		}
	}
	if _, err := c.DB.Exec(schemaSQL()); err != nil {
		return err
	}
	tx, err := c.beginWrite(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range tables {
		old := firstString(row["name"])
		table := strings.TrimSuffix(old, "_legacy")
		from, err := tableColumns(tx, old)
		if err != nil {
			return err
		}
		to, err := tableColumns(tx, table)
		if err != nil {
			return err
		}
		columns := []string{}
		for column := range from {
			if to[column] && column != "host_id" {
				columns = append(columns, column)
			}
		}
		sort.Strings(columns)
		list := strings.Join(columns, ",")
		if _, err := tx.Exec("INSERT OR IGNORE INTO "+table+"(host_id,"+list+") SELECT ?,"+list+" FROM "+old, legacy); err != nil {
			return err
		}
		if _, err := tx.Exec("DROP TABLE " + old); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// backfillSightings attributes rows indexed before sightings existed to the
// legacy host, once; later rows are sighted as they are ingested. Records
// ingested before per-record digests get an empty, unknown source_name.
func (c *Catalog) backfillSightings(legacy string) error {
	var done string
	if err := c.DB.QueryRow("SELECT value FROM meta WHERE key='host_sightings_version'").Scan(&done); err == nil {
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	tx, err := c.beginWrite(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`INSERT OR IGNORE INTO workspace_sightings(workspace_id,host_id,source_name,location,repository_locations_json,first_seen_at,last_seen_at)
			SELECT w.id,s.host_id,s.source_name,w.location,COALESCE(r.local_locations_json,'[]'),w.indexed_at,w.indexed_at
			FROM source_record_states s JOIN workspaces w ON w.source_kind=s.source_kind AND w.source_account=s.source_account AND w.source_id=s.source_id
			LEFT JOIN repositories r ON r.id=w.repository_id WHERE s.host_id=?1`,
		`INSERT OR IGNORE INTO workspace_sightings(workspace_id,host_id,source_name,location,repository_locations_json,first_seen_at,last_seen_at)
			SELECT w.id,?1,'',w.location,COALESCE(r.local_locations_json,'[]'),w.indexed_at,w.indexed_at
			FROM workspaces w LEFT JOIN repositories r ON r.id=w.repository_id
			WHERE NOT EXISTS(SELECT 1 FROM workspace_sightings s WHERE s.workspace_id=w.id)`,
		`INSERT OR IGNORE INTO conversation_sightings(conversation_id,host_id,origin,source_name,messages,ended_at,first_seen_at,last_seen_at)
			SELECT c.id,s.host_id,COALESCE(c.origin,''),s.source_name,NULL,c.ended_at,s.first_seen_at,s.last_seen_at
			FROM conversations c JOIN workspace_sightings s ON s.workspace_id=c.workspace_id WHERE s.host_id=?1`,
	} {
		if _, err := tx.Exec(statement, legacy); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES('host_sightings_version','1')"); err != nil {
		return err
	}
	return tx.Commit()
}
