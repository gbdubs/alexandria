# Configuration

The default file is `archive.toml`; override it with `--config` or `AIWA_CONFIG`. Paths may use `~`.
Relative paths (`data_dir`, `catalog_path`, `archive_root`, `staging_root`,
`executable`, and source `path`s) resolve against the directory containing the
configuration file, not the working directory. `~` and absolute paths are used
as written.

```toml
data_dir = "~/Library/Application Support/AI Work Archive"
catalog_path = "~/Library/Application Support/AI Work Archive/catalog.sqlite3"
staging_root = "~/Library/Application Support/AI Work Archive/staging"
staging_cap_bytes = 2000000000
archive_root = "/Volumes/euclid/Alexandria"
volume_id = "uuid:PASTE-THE-REPORTED-UUID"
api_token = "a-long-random-local-token"
# Used only by the optional Finder-launched macOS wrapper.
# `launch.sh` maintains this automatically for the packaged Go service.
# executable = "/absolute/path/to/Pharos.app/Contents/MacOS/alexandria"
host = "127.0.0.1"
port = 8765

package_cap_bytes = 100000000

cpu_idle_ceiling = 0.35
io_mbps_ceiling = 25.0
yield_poll_seconds = 2.0

# tl1_url = "http://127.0.0.1:8781/archive/v1"
# Prefer AIWA_TL1_TOKEN rather than storing this value.
# tl1_token = "..."
# Prefer GITHUB_TOKEN. GitHub enrichment sends only repo/PR identifiers.
# github_token = "..."

[[sources]]
name = "tl1"
kind = "tl1"
path = "~/.tl1/registry.json"
account = "local"

[[sources]]
name = "conductor"
kind = "conductor"
path = "~/Library/Application Support/com.conductor.app"
account = "local"

[[sources]]
name = "codex"
kind = "codex"
path = "~/.codex"
account = "local"
```

Source paths are explicit opt-ins. A missing or unreadable source is recorded as failed/stale; the service never claims freshness after a failed read. The internal catalog contains searchable cache, protection state, cursors, queues, and receipts. The authoritative retained evidence and its primary package manifest live under `archive_root`.

## Portable library

A portable library keeps Pharos and everything it has indexed in one directory,
typically on an external drive that moves between Macs:

```text
/Volumes/euclid/Pharos/
  Pharos.app
  library.toml             # relative paths resolve against this directory
  catalog/catalog.sqlite3
  preserved/               # archive_root
  staging/
```

Create or upgrade one with:

```sh
macos/install-library.sh /Volumes/euclid/Pharos
```

The script builds `Pharos.app` into the directory and runs
`alexandria init-library DIR` there unless `library.toml` already exists. A
rerun replaces only the app; the configuration and catalog are left alone. It
refuses while any process from that app bundle is running (the app, its service,
or an MCP server an agent client started), because replacing a running
executable can kill it mid-write, and when the directory's parent does not
exist, which usually means its drive is not mounted. The app itself normally runs from a copy on
each Mac (see [Running from an external drive](#running-from-an-external-drive)),
which a reinstall does not disturb; quit it to pick up the new build.

`init-library` never overwrites an existing `library.toml`. It creates the
catalog, `preserved/`, and `staging/`, and writes a configuration with:

- `library = true`, which enables the guards below;
- `catalog_path = "catalog/catalog.sqlite3"`, `archive_root = "preserved"`, and
  `staging_root = "staging"`, all relative;
- `volume_id` pinned to the volume holding the directory (see
  `alexandria volume-id DIR`);
- a new random `api_token`, `host = "127.0.0.1"`, and `port = 8766`, which
  leaves the default 8765 to a per-user install so both can run on one Mac;
- reclamation disabled, and no sources.

**Discovery.** The app uses `library.toml` beside `Pharos.app` when it exists
(running from a copy on the Mac, see below) and otherwise the per-user `~/Library/Application Support/AI Work
Archive/archive.toml`. The bundled CLI does the same when neither `--config` nor
`AIWA_CONFIG` is given (also through a symlink to it), so
`/Volumes/euclid/Pharos/Pharos.app/Contents/MacOS/alexandria ingest` needs no
flags. Any other executable still defaults to `./archive.toml`.

**Guards.** Before a library's catalog is opened, every command (`serve`,
`ingest`, `health`, and the rest; `mcp` before each request it answers from the
catalog itself, see [MCP access](#mcp-access)) checks that:

- the library directory is on the volume that `volume_id` pins, so a clone or
  backup of the drive is not mistaken for the original and silently diverges.
  After deliberately moving or restoring a library, set `volume_id` to the value
  the error reports, or to `""` to stop pinning;
- the catalog exists. A missing catalog usually means its drive is not mounted,
  so it is an error rather than an invitation to start an empty one. To start
  over deliberately, move `library.toml` aside and run `init-library` again.

Configurations without `library = true` are unaffected: there `volume_id` only
marks `archive_root` as the wrong volume in health, and a missing catalog is
created on first use.

**Catalog schema version.** Every catalog records the schema version it was last
opened with. A build refuses to open a catalog newer than it supports and leaves
it unchanged, so an older Pharos (for example a per-user install on another Mac
pointed at the library) cannot rewrite what a newer one wrote. Open it with the
app from the library, or upgrade the other build.

**Sources.** Source locations differ per Mac, so `init-library` configures none;
each Mac keeps its own in `hosts/<host-id>.toml` inside the library (see
[Adding a Mac](#adding-a-mac)). `[[sources]]` in `library.toml` apply on every
Mac, which suits sources stored with the library, such as a ChatGPT export at a
relative path. `~` paths resolve in the home directory of whichever Mac runs the
service, and a source whose path is missing on that Mac is recorded as failed
rather than treated as empty.

## Moving an existing install onto a drive

A per-user install (`archive.toml` with its catalog beside it, normally in
`~/Library/Application Support/AI Work Archive`) becomes a portable library
without re-indexing:

```sh
macos/install-library.sh /Volumes/euclid/Pharos \
    --adopt "$HOME/Library/Application Support/AI Work Archive/archive.toml"
```

The script builds `Pharos.app` into the directory and then runs
`alexandria init-library DIR --adopt CONFIG` where it would otherwise run
`init-library DIR`. The command also runs on its own; `install-library.sh DIR`
later adds the app and keeps the adopted `library.toml`. Adopting is
`init-library` with an existing catalog, hence the same command: the same
`library.toml`, the same refusal to replace one. It must run on the Mac that
built the catalog, since that Mac becomes the owner of everything indexed so
far. The old install is only read, so its app can keep running meanwhile and
remains a fallback afterwards.

What it does, in order:

1. **Checks** before creating anything: `DIR` holds no `library.toml`, no
   `catalog/catalog.sqlite3` and none of its `-wal`, `-shm` or `-journal` files
   (SQLite would replay a leftover `-wal` onto the new catalog), and no host
   file for this Mac; `DIR`'s parent exists (a missing `/Volumes/euclid` means
   the drive is not mounted, and it is never created on the startup disk);
   `CONFIG` is not already a library, and its catalog exists and is not newer
   than this build; `archive_root` neither contains `DIR` nor lies inside it,
   compared by inode so that symlinks and case do not hide it; this Mac's host
   ID comes from its hardware UUID right now, not from a fallback (see below);
   and `DIR`'s volume has room for the catalog, the preserved packages, and a
   margin of 5% of the catalog or 2 GB, whichever is larger.
2. **Copies the catalog** with SQLite's online backup API from a read-only,
   query-only connection, as one snapshot: the backup runs inside a single read
   transaction on the old catalog, so what the old service commits meanwhile
   neither restarts the copy nor appears in it, while everything it had
   committed, including what is still only in `catalog.sqlite3-wal`, is
   included. Progress (bytes, rate, time left) goes to stderr, and the copy's
   disk I/O runs in the utility tier, as captures do. The copy is written to
   `catalog/catalog.sqlite3.adopt-partial` and flushed with `F_FULLFSYNC`.
3. **Migrates the copy.** It records this Mac as the owner of the rows indexed
   before hosts existed (a catalog that host-aware code has already opened
   keeps the owner it has), rather than leave that to whichever process opens
   the library first, perhaps an MCP call on another Mac. Then it opens the copy
   as the service would, so the migrations (hosts, schema 5, and the rest) run on
   the copy only, and runs `PRAGMA quick_check` on it; anything but `ok`
   fails the adoption. `--full-check` runs
   `integrity_check` instead, which takes 1.7–1.9 times as long; what it adds,
   matching index entries to rows, is something a page-for-page copy cannot
   break.
4. **Copies `archive_root`** into `preserved/`, from where it points if it is
   a symlink, keeping hard links (packages link their blobs) and verifying
   each file's size. Files already there with the same size are kept and
   linked like the rest. If `archive_root` is missing (its drive not mounted),
   nothing is copied and the summary says so; if it already is the library's
   `preserved/`, nothing needs copying; if it is on the library's volume, the
   summary says that too.
5. **Writes this Mac's `hosts/<host-id>.toml`** with the install's `[[sources]]`
   exactly as written: names, kinds, `~` paths, `enabled`, `account`, and other
   options. Only a relative path changes, to the absolute path it meant (or
   `~/...`), since relative paths in a host file resolve against the library.
   With the host file in place the Mac is not offered onboarding.
6. **Commits.** It renames the copy to `catalog/catalog.sqlite3`, writes
   `library.toml` last, loads the library back, and runs its guards.

`library.toml` is the one `init-library` writes: relative locations, a
`volume_id` pinned to the drive, port 8766, a new `api_token`, reclamation off,
and no sources. From the install it carries over `package_cap_bytes`,
`upcoming_days`, and `eligible_days`, and, when set, `snooze_days`,
`staging_cap_bytes`, `cpu_idle_ceiling`, `io_mbps_ceiling`,
`yield_poll_seconds`, and `capture_snapshot_generations`. The rest does not
carry over, and the summary lists what was set:

- locations (`data_dir`, `catalog_path`, `archive_root`, `staging_root`,
  `capture_root`), `volume_id`, `executable`, `host`, `port`, and `api_token`
  describe the install rather than the library;
- `enable_reclamation` and `release_hook_proven` stay `false`: reclamation has
  to be proven again against the library's archive;
- `tl1_url` names a hook on this Mac, and `tl1_token` and `github_token` are
  secrets that should not travel on a drive; set `AIWA_TL1_TOKEN` and
  `GITHUB_TOKEN` for the service instead.

**If it fails.** An error or Ctrl-C removes everything the attempt created:
the partial copy, preserved packages it copied, the host file, the catalog,
`library.toml`, and the directories it made, down to `DIR` itself if it made
that. `install-library.sh --adopt` also removes the `Pharos.app` it just built.
`DIR` then holds no library that the guards or the app would accept. Ctrl-C
stops the copy at once, but migrations cannot stop midway, so there it takes
effect after them; a second Ctrl-C kills the process, which, like a crash,
leaves at most `catalog.sqlite3.adopt-partial` (with `-wal`/`-shm`) for the
next attempt to delete. The old catalog file is never written: it keeps its
inode, contents, and schema version. Like any SQLite reader, the copy updates
the old catalog's `-shm` index, and it leaves an empty `-wal` and a `-shm`
beside a catalog whose service is not running; that service uses them the next
time it starts.

**Host ID.** Adopting makes this Mac the owner of everything indexed so far and
names its host file after its host ID, so it refuses to proceed when the ID is
not the one this Mac's hardware UUID gives, for example when `ioreg` did not
answer and Pharos fell back to `host.json` or the hostname. Try again, or set
`PHAROS_HOST_ID` explicitly to the ID to use.

**Timing.** Measured with a 7.8 GB synthetic catalog on a Mac Studio (M3 Ultra)
while other benchmarks kept its internal SSD busy: copying between folders on
the internal SSD ran at 0.3–0.9 GB/s, also with 1.8 GB of the catalog still in a
live WAL, and onto an APFS disk image stored on that SSD at 0.03–0.45 GB/s.
Migrations took 1–9 s. `quick_check` took 73–88 s right after the copy (180 s
under the heaviest load, 29–31 s when repeated with the file cached), and
`integrity_check` 162 s. For a 43 GB catalog, expect 1–3 minutes of copying to
a fast drive and 7–8 minutes of checking (about twice that with
`--full-check`), longer while the Mac is busy. While the copy runs, the old
service's checkpoints cannot get past its snapshot, so the old catalog's WAL
grows by whatever that service indexes meanwhile; quitting the old app before
adopting avoids that.

**For the per-user install on this Mac and the drive `euclid`:**

```sh
cd path/to/alexandria   # this repository
# 1. Adopt. The old Pharos may keep running meanwhile.
macos/install-library.sh /Volumes/euclid/Pharos \
    --adopt "$HOME/Library/Application Support/AI Work Archive/archive.toml"
# 2. Quit the old Pharos (both apps share a bundle ID, so macOS may otherwise
#    bring it forward instead). Its service, and MCP servers agent clients
#    started with its configuration, show up here until they stop:
pgrep -fl 'AI Work Archive/archive.toml|dist/Pharos.app' || echo "old Pharos stopped"
# 3. Open the library's app. It runs from a local copy, serves the library on
#    port 8766, and resumes indexing this Mac's sources where the copy left off.
open /Volumes/euclid/Pharos/Pharos.app
```

4. In each agent client's MCP settings, replace the command of the Pharos
   server (`…/alexandria --config …/archive.toml mcp`) with
   `~/Library/Application Support/Pharos/bin/pharos-mcp`, without arguments.
   The app writes that launcher when it starts, and `alexandria install-mcp`
   writes it on demand (see [MCP access](#mcp-access)).

**The old install afterwards.** Keep it as a fallback until the library has
served you for a while; it receives nothing new once its app is quit. Do not run
both apps: they share a bundle ID, and both would index the same sources into
separate catalogs. To fall back, quit the library's app and start the old one
(for example with `launch.sh`); it catches up from the sources on its next sync,
for whatever they still hold (Claude Code deletes transcripts after 30 days by
default). To remove it, delete `catalog.sqlite3` and its `-wal`/`-shm` in
`~/Library/Application Support/AI Work Archive`, which is nearly all its space,
then the rest of that folder, `archive_root`'s original if it was copied into
`preserved/`, and any LaunchAgents installed from `macos/launchd` with its
configuration.

## Adding a Mac

Plug the drive in and double-click **Add This Mac.command** beside `Pharos.app`
(it opens in Terminal; macOS may first ask to let Terminal use files on a
removable volume). The same step from Terminal is:

```sh
/Volumes/euclid/Pharos/Pharos.app/Contents/MacOS/alexandria add-this-mac
```

It lists the conversation sources it finds on this Mac (below), asks before
adding the new ones to this Mac's `hosts/<host-id>.toml`, captures them onto the
drive, says when the drive is safe to eject, indexes them, and installs the MCP
launcher for this Mac's agent clients. `--yes` adds without asking;
`--capture-only` stops after the capture so indexing can run later on any Mac.
Ctrl-C stops safely, and running it again resumes. Run it again whenever you
want the library brought up to date: it adds sources that appeared since, and
captures and indexes only what changed.

The app offers the same steps. The first time a library is opened on a Mac, Pharos shows **New Mac detected:
<name>** with the conversation sources it found there. (A Mac that has already
indexed into the library, such as the one that created it, sees **Set up sources
for <name>** instead.) Pharos looks only in these places. It never searches the
rest of the home folder and never enters a folder that macOS guards with a
permission prompt: Desktop, Documents, Downloads, iCloud Drive and other cloud
storage, other apps' containers, Mail, Messages, and other volumes.

| Kind | Where Pharos looks |
| --- | --- |
| Claude Code | `$CLAUDE_CONFIG_DIR/projects`, `~/.claude/projects`, `~/.config/claude/projects`, and any `~/.claude*/projects` (for example `~/.claude-work`) that holds transcripts |
| Codex | `$CODEX_HOME` and `~/.codex`, when they have `sessions/` or `archived_sessions/` |
| Conductor | `~/Library/Application Support/com.conductor.app`, when a database at its top level has session and message tables (only table names are read) |
| TL1 | `~/.tl1/registry.json`, or for TL1 releases before it `~/.tl1/workspaces.json`; installations in a temporary directory or with missing files are skipped, as the TL1 adapter does |
| ChatGPT | nothing on disk: export your data from ChatGPT and add a source for the folder holding `conversations.json` |

The app and its service do not inherit the shell's environment, so Pharos also
reads `CLAUDE_CONFIG_DIR` and `CODEX_HOME` from the login shell (`$SHELL -l -i -c`,
two-second limit, cached for ten minutes). This runs the shell's startup files;
set `PHAROS_PROBE_SHELL=0` in the service's environment to skip it.

For each location the panel shows how many sessions it holds (databases for
Conductor, installations for TL1), their size and date range, and:

- **Retention.** Claude Code deletes transcripts `cleanupPeriodDays` after their
  last activity (30 unless that profile's `settings.json` says otherwise), so
  sync before then.
- **Overlap.** Up to 50 sessions spread from oldest to newest are looked up in
  the library, for example "45 of 50 sampled sessions are already in the library,
  indexed from Mac Studio". A copied `~/.claude` or a Mac the library has seen
  before shows up here.
- Whether it is already configured, and as which source.

Checked locations become enabled sources and unchecked ones paused sources, so
the Mac is not asked again; enable those later in Settings → Sources. The choice
is written, atomically, to `hosts/<host-id>.toml` in the library:

```toml
# Pharos sources for MacBook Air (user gbw).
# Host ID: host_…
# These [[sources]] apply only on this Mac and replace any same-named source
# in library.toml. "~" is this user's home directory; relative paths resolve
# against the library directory. Safe to edit by hand.

[[sources]]
name = "claude"
kind = "claude"
path = "~/.claude/projects"
enabled = true
```

The host ID comes from the Mac's hardware UUID and the user name, so two accounts
on one Mac have separate files; `/api/sources` and `/api/probe/status` report it.
Each run records it in `~/Library/Application Support/Pharos/host.json`; if
`ioreg` cannot report the hardware UUID (Pharos waits 2 seconds when a recorded
ID can stand in, otherwise up to 20), Pharos uses the recorded ID rather than
becoming a new host. A successful `ioreg` always wins, so a `host.json` that
Migration Assistant copied to a new Mac is replaced there. With neither, the ID
is derived from the hostname and reported with `"fallback": true`; nothing is
recorded under it (syncs, captures, host files, and claiming a pre-host
catalog fail with "Pharos could not identify this Mac") until a later run
identifies the Mac. The MCP server never waits for this before answering.
`PHAROS_HOST_ID` overrides all of it.
On that Mac, a host file source replaces a `library.toml` source of the same
name; every other `library.toml` source still applies. Toggling a source in
Settings writes whichever file defines it. Hand edits take effect when the
service restarts; accepting in the panel reloads sources immediately.

**Not now** writes nothing, and the panel returns the next time Pharos starts.
Settings → Sources names the Mac its sources belong to and offers **Find
sources on this Mac…** to look again, for example after installing another
agent.

After adding sources the panel captures them onto the library straight away
(see [Capture](#capture)), showing each source's files and bytes as they are
copied, then says how much it captured, for example "Captured 4.3 GB from
MacBook Air. You can eject euclid now; indexing can finish now or later on any
Mac." **Index now** indexes those captures (see [Indexing
captures](#indexing-captures)) with per-source progress; **Later** leaves them
to be indexed from Settings → Sources (see below), on this Mac or any other. Either
step can be left to run in the background: the header shows its progress, and
a message says when it finishes.

Probing from the command line:

```sh
alexandria probe                              # JSON report; changes nothing
alexandria probe --accept claude,codex        # add candidates by suggested name
alexandria probe --accept claude=claude-mbp   # ...under another source name
alexandria probe --accept-all                 # add every recommended candidate
```

The API equivalents are `GET /api/probe/status` (cheap; includes
`needs_onboarding`), `GET /api/probe`, and `POST /api/probe/accept` with
`{"accept": ["claude"], "decline": ["codex"], "names": {"claude": "claude-mbp"}}`.

A per-user `archive.toml` has no per-Mac file, so there `probe` only reports:
`--accept` and `POST /api/probe/accept` refuse, and the `[[sources]]` wanted are
copied into `archive.toml` by hand.

**Macs and captures** (Settings → Sources, below this Mac's source cards) shows
this Mac and every captured Mac with, per source, when it was last captured,
when it was last indexed (by an index of its captures or, on this Mac, a live
sync), and whether it needs indexing: never indexed, indexed only partly, or
captured again since. **Capture <this Mac>** captures this Mac's enabled
sources; **Index <Mac>** indexes one Mac's captures and **Index all Macs** every
Mac's (`POST /api/index` with `{"host": …}` or `{"all_hosts": true}`). Each of
this Mac's source cards also says when it was captured. The live sync buttons
(**Sync one**, **Sync all enabled**) and the enable switches work as before.

## Running from an external drive

A library's `Pharos.app` never runs from its drive. Code running from a drive
keeps it busy, so Finder refuses to eject it, and pulling the drive kills that
code the next time it pages.

**The local copy.** Opening `/Volumes/euclid/Pharos/Pharos.app` copies the app
to `~/Library/Application Support/Pharos/runtime/<build>/Pharos.app`, verifies
the copy's code signature (strict validation, and it must be exactly the running
build and satisfy its designated requirement), points
`runtime/current` at it, records the library in
`~/Library/Application Support/Pharos/library.json`, opens the copy with
`--library /Volumes/euclid/Pharos`, and quits. `<build>` is the bundle's cdhash,
which changes whenever either executable does, so a build is copied once and
later launches reuse its verified copy; copies that are neither current nor
running (the app, its service, or an MCP server) are deleted. The copy has the
same signature as the original, so privacy permissions carry over; with a
stable signing identity (see the README's Code signing) they also survive
rebuilds. The window shows "Opening Pharos from euclid…" while this happens.

- `library.json` holds `library_dir` (the directory with `library.toml`),
  `volume_uuid` (without the `uuid:` prefix), `volume_name`, and `updated_at`
  (RFC 3339). Other tools, such as an MCP launcher, run
  `runtime/current/Contents/MacOS/alexandria` against that library.
- Opening the local copy directly (from the Dock, say) opens the library in
  `library.json`, waiting for its drive if it is not connected.
- If a local copy of another build is already running (the drive's app was
  updated), it is asked to quit and the new build opens once it has; the new
  copy waits for the old service to stop. A copy that will not quit within
  15 seconds is reported instead. A running copy of the same build is simply
  brought forward.
- `PHAROS_RUN_IN_PLACE=1` runs the app from the drive as before, for debugging;
  the drive then cannot be ejected while Pharos runs. `PHAROS_SUPPORT_DIR`
  replaces `~/Library/Application Support/Pharos`.
- An app without `library.toml` beside it, such as `dist/Pharos.app`, is
  unaffected.

**Ejecting.** Pharos watches the library's volume, by UUID, through
DiskArbitration. When Finder or `diskutil eject` asks to unmount it, the app
first asks the service to release the library (`POST /api/release`): the
service refuses new requests, stops a running sync between workspaces (the next
sync resumes it), lets requests in flight finish, closes the catalog
(checkpointing its WAL), answers, and exits. The eject then goes ahead and the
window shows "Library disconnected — Reconnect euclid to continue." If the
release cannot finish within 7 seconds (DiskArbitration waits about 10 for an
answer), Pharos refuses the eject with "Pharos is finishing a write; try
ejecting again in a moment.", and the window says so until the service has
stopped, which it still does, so the next attempt succeeds. A release the
service refuses outright leaves Pharos running as it was. If something else
keeps the drive busy after Pharos let go, the window says the library was
released and offers Reopen Library.

**Unplugging without ejecting.** The service notices within about a second that
the catalog's directory is gone, or that SQLite's memory-mapped index has
faulted, and exits with "the library's drive disappeared … Reconnect the drive
to continue" without touching the catalog again. Every commit is atomic and
flushed to the drive, so at most the workspace being indexed is lost, and the
next sync indexes it again. The window shows the disconnected state; when the
same volume mounts again, even at another mount point such as
`/Volumes/euclid 1`, the app restarts the service and reloads.

**Stopping the service.** SIGTERM and SIGINT stop `serve` the same way as a
release, within 7 seconds (a second signal exits at once); the app sends
SIGTERM when it quits.

**Two services on one Mac.** Cookies ignore ports, so each service names its
login cookie `aiwa_token_<port>`; a per-user install on 8765 and a library on
8766 no longer sign each other out. Port 8765 also accepts the old `aiwa_token`.

**Tests.** `macos/test/eject-release.sh` exercises the service (release during a
sync, eject, a forced detach while syncing, catalog integrity afterwards), and
`macos/test/swift-harness.sh` the app's runtime cache, DiskArbitration
handling, and the Eject button's release-then-eject. Both use a throwaway disk
image and never launch Pharos.

### Library status and Eject

While Pharos runs it has the catalog open, whatever else it is doing, so the
way to disconnect the library's drive is always to eject it: Pharos then
releases the library first (above), or refuses the eject and says why.
Unplugging without ejecting is only "probably fine" when nothing is running:
every commit is atomic and flushed, so nothing committed is lost, but work in
progress is, and the next run redoes it.

`GET /api/library/status` says what holds or writes the library right now:

- `drive`: `name`, `mount_point`, `volume_uuid`, `location` (`external`,
  `disk-image`, `internal`, `unknown`), `mounted`, `ejectable` (under
  `/Volumes` and not the internal disk), and `pinned` (`volume_id` is set);
  also `library_dir`, `config_path`, `catalog_path`, `capture_root`, `host`,
  and `catalog_open` (always true while the service answers).
- `activities[]`: `kind`, `label`, `detail`, `progress` (0–1, or null when it
  cannot be estimated), `started_at`, `writes` (whether it writes to the drive),
  and `on_eject` (what a release does to it). Kinds: `sync` and `index` (runs),
  `capture` (by the service), `capture-other` (another process on this Mac,
  such as `alexandria capture`, holds the capture lock; Pharos cannot stop it,
  and it keeps the drive busy), `backup` (reads the library, writes elsewhere),
  `git` (the main-branch merge lookup that follows a sync or an index, or the
  first scan of a catalog), and `maintenance` (Library view rows still to be
  recomputed).
- `idle` (nothing running), `writing` (something writes to the drive), and
  `safe_to_unplug`, which equals `idle`, as does `GET /api/capture`'s field of
  that name.
- `unplug`: `action` (`eject`, or `none` for a library on the internal disk),
  `without_eject` (`probably-fine` when idle, `unsafe` otherwise,
  `not-applicable`), and a `summary` for people.

The header shows the drive's name and the first activity with its progress;
its panel lists every activity, the advice above, and **Eject <drive>**. In the
app, Eject asks the app (script message handler `pharosLibrary`, action
`eject`) to release the library exactly as for an eject from Finder, then to
run `diskutil eject` on the drive, which unmounts all of its volumes and ejects
it. If anything is running, the panel first says what Eject will stop (each
resumes on its next run) and asks to confirm. What happens next:

- The release is refused, or runs out of time while the service finishes a
  write: the page shows the reason and Pharos keeps running (or, while
  finishing, shows "Finishing a write" and then the released library).
- The library is released and the drive ejects: the window says "<drive>
  ejected" and that it can be unplugged. The same window follows an eject from
  Finder, since Pharos released the library first.
- The library is released but the drive does not eject, because something
  else has files open on it: the window says "Library released", names the
  process diskutil reported ("Spotlight is using euclid…", or "Terminal
  (process 812) is using euclid…"), and offers **Eject <drive>** again and
  **Reopen Library**.

In a plain browser there is no app to ask, so the panel says to eject the drive
in Finder (⏏ beside it in the sidebar) or with `diskutil eject`, with the
command to copy. The Pharos app releases the library for either; a service run
without the app (`alexandria serve`) has to be stopped first.

## Capture

Indexing parses every source into the catalog and is slow; copying the raw
source files is fast. `capture` copies this Mac's sources onto the capture root,
normally on the library drive. Once it finishes the drive can be unplugged and
the captures indexed later, on any Mac. A capture also keeps raw evidence that
the tools themselves delete (Claude Code removes transcripts after
`cleanupPeriodDays`, 30 by default): a captured file is never deleted.

```sh
alexandria capture              # every enabled source of this Mac
alexandria capture claude codex # just these (a named source is captured even if disabled)
```

The command prints a JSON summary, exits non-zero if anything failed, and never
opens the catalog. The service offers the same as `POST /api/capture` (optional
body `{"sources": ["claude"]}`), which starts a run in the background and
answers `202`, or `409` while a capture is running. `GET /api/capture` reports
per-source progress (files and bytes copied, snapshots, errors) and
`safe_to_unplug`, which is true when nothing at all is running, as reported by
[`GET /api/library/status`](#library-status-and-eject): no capture (by the
service, the CLI, or anything else on this Mac holding the capture lock), sync,
index, backup, Git lookup, or Library view refresh. Even then the service has
the catalog open, so eject the drive rather than pulling it.

```toml
# Default: "captures" beside library.toml in a library; otherwise
# "<catalog directory>/captures".
capture_root = "captures"
# Previous database snapshots kept besides the latest (see below).
capture_snapshot_generations = 1
```

Outside a library the default is beside the catalog rather than under
`archive_root`, because that drive may be absent. For the same reason the parent
of `capture_root` must already exist: a missing mount point under `/Volumes` is
never recreated on the startup disk.

**Layout.** Captures are kept per Mac (the host ID of `GET /api/sources`) and per
source:

```text
captures/<host-id>/
  host.json                 # id, label, user, first/last capture
  .capture.lock             # held (flock) by a running capture
  <source>/
    manifest.json
    files/...               # latest capture, mirroring the source's layout
    history/<stamp>/...     # superseded versions, named by their captured_at
```

**What is captured** is exactly what the source's adapter reads:

| Kind | Captured |
| --- | --- |
| `claude` | every `*.jsonl` under the path, including `<session>/subagents/**` |
| `codex` | `*.jsonl` under `sessions/` and `archived_sessions/`, and `rollout*` files |
| `conductor` | a snapshot of each SQLite database that has session and message tables (not, for example, `cache.db`) |
| `tl1` | the registry, then for each installation the adapter reads a snapshot of its database (`files/installations/<id>/`), its `transcripts/**/*.jsonl`, and procedural attempts' script logs (`<attempt>-script.log`, `-script.stderr.log`, `-script.stdout.log`) |
| `canonical`, `tl1-export`, `chatgpt-export` | the export file, or the directory's `*.json` (`conversations.json` for ChatGPT) |

**Incremental and interruption-safe.** A file whose size and mtime match the
manifest, and whose captured copy is still intact, is skipped. Anything else is
copied to a temporary name beside its destination, fsynced, and renamed into
place; the copy keeps the source's mtime. The manifest is replaced atomically
after a full device flush (`F_FULLFSYNC`), which also makes every file copied
before it durable, and is checkpointed every few seconds. A run stopped at any
point (error, kill, unplugged drive) leaves the previous captures and manifest
valid; the next run removes half-written temporaries, copies again anything the
manifest does not vouch for, and continues. Only one capture per Mac runs at a
time. JSON Lines logs are copied whole when they change: this Mac's sources
change by about 100 MB an hour, so copying only appended tails was not worth its
complexity. A file whose new content does not begin with the previously captured
bytes (a rewrite rather than an append) has its previous capture moved to
`history/` first. A source file that disappears keeps its capture and is marked
`missing_since`. A source whose path is missing altogether is reported as failed
and nothing is marked missing, because an unmounted or moved location is more
likely than a deletion.

**Databases** are never copied raw (a live database, `-wal`, and `-shm` copied
separately can be inconsistent). Each is snapshotted with SQLite's online backup
API from a read-only, query-only connection. Every step of the copy reads from
one read transaction held throughout, so the snapshot is consistent while the
app keeps committing (in WAL mode the app is not blocked), and the copy can stop
between steps of 16 MB when the service is released. The snapshot is converted to a
self-contained rollback-journal file. A database whose file and WAL sizes and
mtimes are unchanged is not snapshotted again. A session deleted from a database
disappears from its next snapshot, so the previous snapshot is kept as a
generation in `history/`; `capture_snapshot_generations` sets how many (default
1, i.e. latest plus previous: about 17.5 GB for an 8.7 GB Conductor database).
The catalog, not the captures, is the long-term record of indexed sessions; the
previous generation gives indexing a full capture cycle to pick up a session
deleted at the source, and where captures are indexed, a snapshot the index has
not yet seen is kept longer (see [Indexing captures](#indexing-captures)). Raise
it where the drive allows.

**Low impact.** The capture's disk I/O runs in the utility I/O tier on its own
thread, yielding to interactive work without the background tier's long stalls,
while the service's other requests keep their priority. Copies stream through
four 1 MiB buffers, so memory stays bounded whatever the file size. SHA-256 is
computed while copying, on a separate goroutine.

**Manifest** (`version` 1; paths in `captured` fields are slash-separated and
relative to the source's capture directory):

- `host`, `source` (`name`, `kind`, original `path`, `account`), `updated_at`.
- `files[]`: `path` (original, absolute), `captured`, `size`, `mtime`,
  `mtime_ns` (the source's, also set on the captured copy), `sha256`,
  `captured_at` (changes whenever the file is captured again), `missing_since`,
  and `previous[]` versions kept in `history/`.
- `snapshots[]`: `path`, `captured`, `size`, `captured_mtime_ns`,
  `captured_at`, `method` (`sqlite-backup`), `source` (database and WAL size
  and mtime it was taken from), `missing_since`, and `generations[]` (each with
the `source` state it was taken from, when recorded).
- `installations[]` (TL1): each registry entry's original `db_path` and
  `transcripts_dir` with their captures, and its `code_repo` and
  `config_path`, which are not captured; `path_map[]`: every original path
  (source root, databases, transcript directories) and its capture. Entries that
  left the source are kept.
- `last_run`: start and finish (no `finished_at` means the run was interrupted),
  counts of files and bytes copied, unchanged, missing, snapshots, `errors`,
  and `warnings`.

Besides the manifest, a source's directory may hold `indexed.json`, written only
by the index (below).

## Indexing captures

`index` parses captures into the catalog on any Mac that has the library,
whether or not the Mac that captured them is attached. It never reads the
original source paths.

```sh
alexandria index                        # this Mac's captures
alexandria index --host host_… claude   # another Mac's, just its claude source
alexandria index --all-hosts            # every captured Mac
```

The command prints a JSON summary (per source: `host`, `source`, `workspaces`,
`conversations`, `messages` written, `parsed` and `unchanged` parts, `older`
records passed over, `error`) and exits non-zero if a source failed. One index
of a capture runs at a time, whether started by the CLI or the service (a flock
on the source's `.index.lock`); a second one fails at once. The service offers `POST /api/index` with
`{}` (this Mac), `{"host": "host_…"}` or `{"all_hosts": true}`, each optionally
with `"sources": ["claude"]`. It starts a run in the background and answers
`202` with it; `400` for a host or source without captures, `409` while a sync or
another index runs (they share one lock), `503` if the capture root is
unavailable. `GET /api/index` returns `active`, `run` (the latest) and `runs`,
shaped like sync runs (`kind` `capture-index`, `sources` labelled
`<host-id>/<source>`, per-source `results`, running totals), and `hosts`: each
captured host's `id`, `label`, `user`, `current` (this Mac), and `sources[]` with
`kind`, `captured_at`, `capture_finished`, `indexed_at`, `coverage`, `error` and
`needs_index`. A release (before an eject) stops an index between records, and
the next run resumes it. A running index, and the Git lookup that follows one
that wrote records, make `safe_to_unplug` false. An index records the capturing
Mac's source state only when it completes: a capture failing to index, perhaps
on another Mac, leaves that Mac's own sync state as it was. In the app,
onboarding offers **Index now** after its capture, and Settings → Sources
indexes one Mac or all (see [Adding a Mac](#adding-a-mac)).

**Attribution.** Everything an index writes belongs to the Mac that captured the
data (the capture directory's host ID), not the Mac running it: the source's sync
state (the same row a live sync of that source on that Mac writes), per-record
and per-part state, sightings, and each conversation's writer. Origins and
evidence locators name the original source paths, never the capture's (for
Conductor `sqlite:<original database path>`). A live sync and an index of the same
Mac's data therefore agree on each conversation's writer, so neither pays the
check that another copy holds every stored message, and they share per-part
state, so data one has parsed the other passes over. A capture from another Mac
is never matched against this Mac's Git checkouts, since the same path here may
be an unrelated checkout; its repositories are named from recorded remotes and
paths only. What such an index parsed is marked as parsed elsewhere (its
fingerprint and part states), so the capturing Mac's own next sync parses it
again and fills in what only its checkouts know.

**An index never rolls anything back.** Every write records the version of the
source it was read from, per workspace and per conversation copy, on the
capturing Mac's clock (`copy_versions`): a transcript's mtime (captures keep
it), or a database's last commit from its and its WAL's mtimes (read after the
read transaction began when live; from the manifest, taken before the snapshot,
for a capture). A capture's conversation older than what its Mac already wrote
is left alone; the workspace's own fields, work items, changes and pull
requests are replaced, and messages pruned, only by a capture no older in any
part, exactly as a live sync would. A record with nothing newer is counted as
`older` and nothing of it is written. This holds whatever the source's sync
state (failed, interrupted, never run), with or without part states, after a
parser change, and for a renamed source. Rows written before versions were
recorded compare against when their sighting was last written, which is no
earlier than the read they came from. A live sync is authoritative as before:
if it read the source before a capture that was indexed while it ran, its
write stands until the next sync. TL1's analysis tables (tl1_*), which are
replaced per installation, follow the same rule: a capture's database snapshot
older than the one they were read from leaves them as they are, and they
record the Mac the installation is on with its original paths there.

**Finding the captured bytes of a locator.** `conversation_sightings` gives the
`host_id`, `source_name` and `origin` of each copy of a conversation. In
`<capture_root>/<host_id>/<source_name>/manifest.json`, the `files[]` entry whose
`path` is the origin is a transcript's capture; otherwise the longest `path_map[]`
`original` prefix of the origin maps it (database snapshots, TL1 transcript
directories). An evidence locator is the origin followed by `:<line>` (Claude
blocks add `:block:<n>`), or `sqlite:<database>:<table>:<session>:<column>:<n>`.

**Only what changed is parsed**, by `index` and by live syncs alike. A source whose
whole-tree fingerprint is unchanged is skipped as before. Otherwise a Codex
transcript, or a Claude session together with its `subagents/` files, is parsed
only if one of its files' size or mtime differs from what that Mac last indexed
(`source_item_states`), and a new subagent file re-parses its session. A
Conductor session is parsed only if its session, workspace or repository row, or
its messages' count, highest rowid, or latest send or cancellation time changed.
Conductor's own indexes answer those without reading any message: about 7 s for
a freshly captured 8.7 GB database with 1.7 million messages, against 7 minutes
to parse every session (15 with the Git lookups a live sync makes). The
per-record digest still decides whether anything is written, and a capture
older than what that Mac already indexed is not even parsed where its part state
says so. Canonical, ChatGPT and TL1 sources are still parsed whole when their
fingerprint changes. Part and copy versions raised the catalog schema version to
6: a build without them would sync around that state, so older builds refuse
the catalog. The tool ledger, message senders, human authorship, the TL1
analysis tables, and timestamps stored in one sortable format raised it to 7,
for the same reason: a version-6 build would re-ingest conversations without
rebuilding their tool ledger, record TL1 sources as synced without their
analysis, and write timestamps in the old format.

**Captures keep going during an index.** An index takes no capture lock, so a
long index never holds up a capture or "safe to unplug". Captured files are
replaced by rename, so the index reads the version it opened whole and records
that version's size and mtime (not the manifest's); a file replaced meanwhile is
parsed again next time. A manifest naming a captured path that is not under the
source's `files/` or `history/` is refused by both capture and index.

**Superseded database snapshots.** A Conductor session deleted at the source
survives only in older snapshots. After indexing the latest snapshot, the index
takes from each superseded snapshot it has not seen the sessions that no newer
snapshot holds, then lists the snapshots it no longer needs in the source's
`indexed.json` (`{"version": 1, "snapshots": [{"path", "captured_at"}]}`),
except for a database whose snapshots a capture replaced while it was indexed,
since what was read is then unknown.
Once that file exists, capture keeps an unlisted snapshot past
`capture_snapshot_generations`, up to as many again (at most `2 × generations`
previous snapshots per database), and past that prunes the oldest with a warning
in `last_run.warnings`, so an index that never runs cannot fill the drive. With
no `indexed.json` (captures never indexed) retention is as described above.
Superseded TL1 snapshots are listed without that pass: the TL1 adapter cannot
index only what is missing.

**Limits.** A Conductor message edited in place while its session row, count,
highest rowid and send and cancellation times all stay the same is not seen
until the session next changes (Conductor updates the session row as its status
changes). Git lookups (a commit's files, main-branch merges) are not part of a
Conductor session's change signal; main-branch merges are refreshed after every
sync and index anyway. A transcript rewritten rather than appended to is indexed
in its latest version only; earlier versions stay in `history/`.

## Keeping the library safe

The library drive holds every archived conversation, including any secrets
pasted into them, and it may be the only copy. `alexandria doctor` and
`GET /api/health` (under `drive`) check the drive holding the library, or for a
per-user `archive.toml` the drive holding `archive_root`, and say what to do
about each problem. Pharos never runs the commands it recommends: they need an
administrator password or change the drive, so they are yours to run.

| Check | Reported when | What to do |
| --- | --- | --- |
| `encryption` | the volume is not encrypted; on the internal disk, FileVault is off (Apple silicon encrypts it anyway, but it unlocks without a password); on a disk image, neither the volume nor the image is encrypted | In Finder, Control-click the drive in the sidebar and choose **Encrypt "euclid"…**, or run `diskutil apfs encryptVolume /Volumes/euclid -user disk`, which asks for a new password. Either encrypts in place and in the background; the drive stays usable meanwhile. Keep the password in a password manager: without it the library cannot be read on any Mac. For the internal disk: System Settings → Privacy & Security → FileVault, or `sudo fdesetup enable`. |
| `spotlight` | Spotlight indexes the drive | `sudo mdutil -i off /Volumes/euclid`, or System Settings → Spotlight → Search Privacy… (Spotlight Privacy… on older macOS) → + → the drive. On the internal disk, add the library's folder to that list instead (doctor cannot see folder exclusions, so it reports this as `info`); a folder named `*.noindex` is skipped too. |
| `filesystem` | the volume is not APFS | HFS+: after a backup, Disk Utility → Edit → Convert to APFS…, which keeps the data. exFAT or FAT cannot be encrypted and has no journal: move the library to an APFS volume (reformatting erases the drive). Network volumes are not safe for SQLite. |
| `location` | (`info`) the library is on the internal disk or on a disk image | |
| `free_space` | less than 1 GB is free (captures stop), less is free than the library already uses, or, at the growth rate seen across recorded backups, the drive fills within 90 days (30: `problem`) | Free up space or move to a larger drive. |
| `volume_pin` | `library.toml` has no `volume_id`, or pins another volume | Set `volume_id` to what `alexandria volume-id DIR` prints. |
| `backup` | no backup is recorded, or the last is more than 7 days old | `alexandria backup DEST`, below. |

Each check has an `id`, a `status` (`ok`, `info`, `unknown`, `warning` or
`problem`), a `summary` and, where there is something to do, a
`recommendation`, a `command` and an `action` (the Finder or System Settings
route). The report's `status` is the worst of them; it also has the volume's
facts (`mount_point`, `filesystem`, `location`, `encryption`, `spotlight`,
`physical_disk`, ...), the library's `sizes` on that volume (catalog including
its WAL, captures, preserved), `free_bytes`, and `last_backup`.

`GET /api/health/drive` returns just this report, from the same cache, without
the catalog-wide counts of `/api/health`, so it is cheap to poll. Settings →
Health shows it under **Library drive**: each check's status, summary,
recommendation, and Finder or System Settings route, and its command with a
**Copy** button. Pharos still runs none of them.

To learn this, Pharos runs only read-only commands: `diskutil info -plist` and
`mdutil -s` on the volume's mount point, and `hdiutil info -plist` for a disk
image. Health is polled, so it answers from a cache of these (and of the
library's size) that is refreshed in the background every 5 minutes, or when the
directory moves to another volume; until the first check finishes it reports
`"status": "checking"`. The volume identity that `storage` and the pin check
use is cached for a minute and looked up with one `diskutil` call limited to 2
seconds; only the first poll waits for it, later ones get the cached value while
a refresh runs in the background, so a hung `diskutil` holds up neither polls
nor a release. `doctor` checks afresh, never opens the catalog, and also
reports `library_guard`: it runs even when the guards refuse to open the library,
for example while its drive is not mounted.

Why Spotlight: current macOS does not index the contents of JSON or JSON Lines
files, but Spotlight indexes file names (session IDs, project paths) and any
plain-text or Markdown files, keeps its index on the drive, where any Mac it is
plugged into can search it, and its indexer can keep files open when you eject
the drive and competes with captures for I/O.

### Backups

```sh
alexandria backup "/Volumes/Other Drive/Pharos Backup"
alexandria backup --prune "/Volumes/Other Drive/Pharos Backup"
```

A backup mirrors the library into a folder on another drive:

```text
DEST/
  pharos-backup.json        # which library, from which Mac, when, whether the run finished, what was missing
  library.toml
  catalog/catalog.sqlite3   # a consistent snapshot
  captures/  hosts/  preserved/
```

(For a per-user `archive.toml`: `archive.toml`, `catalog.sqlite3`, `captures/`,
and `archive_root` as `preserved/`.) Each part keeps its place relative to the
library, unless that place would overlap another part (for example an
`archive_root` that is the catalog's own directory), in which case it gets its
own name (`preserved/`). Whatever the layout, no part copies the live catalog or
its `-wal`/`-shm`/`-journal` files (the snapshot is the copy), `backups.json`,
staging, or another part. Staging is not backed up. A symlinked `archive_root`,
`captures/`, `hosts/` or host directory is backed up as the directory it points
to; symbolic links inside them are not followed (the run warns how many there
were).

- **Where.** DEST must be an absolute path (the command line resolves a relative
  one against the working directory) to a new or empty folder (hidden files
  such as `.DS_Store` aside) or an earlier backup of the same library; its
  parent must exist, so a missing mount point is never created on the startup
  disk. A folder on the library's volume, on another volume of the same
  physical drive (such as another APFS volume in its container), or on a disk
  image whose file is stored on either, is refused, and so is any folder on the
  drive holding the image file of a library that is itself on a disk image: it
  would be lost with the drive. For a per-user `archive.toml` this applies to
  the catalog's drive and `archive_root`'s alike, so DEST is on a third drive.
  One backup at a time writes to a DEST. The backup holds every conversation,
  so it warns when DEST's volume is not encrypted or is indexed by Spotlight; a
  folder on a Mac with FileVault on, excluded from Spotlight, or another
  encrypted drive is a good place.
- **One library per DEST.** The first backup gives the library a random
  `library_id`, kept in its `backups.json`, and `pharos-backup.json` records it.
  A DEST holding another library's backup is refused, so two libraries never
  overwrite or prune each other's backups. A portable library keeps its id on
  every Mac. A per-user install's identity also includes the Mac's host ID: two
  Macs' installs with the same catalog path, even with a `backups.json` copied
  by Migration Assistant, are different libraries.
- **Consistent.** The catalog is copied with SQLite's online backup API from its
  own read-only connection (as a capture snapshots databases), so a backup runs
  while the service does, never needs the catalog to be writable, and does not
  block its writes. Each host's captures are copied while holding that host's
  capture lock shared, so a capture never changes them mid-copy; a backup that
  finds a capture running waits for it ("waiting for a capture to finish"), and
  a capture started meanwhile reports that one is already running.
- **The catalog's WAL is bounded.** The snapshot holds one read transaction for
  its whole copy, and until it ends no checkpoint can reuse the catalog's WAL,
  so whatever a sync writes meanwhile accumulates there. If the WAL grows by
  more than 1 GiB during the copy, the backup stops with "the catalog's WAL grew
  … back up again once it finishes"; `catalog_wal_peak_bytes` reports how large
  it got. Measured with a 1.57 GB catalog on the internal SSD and a writer
  committing 64 KB rows with full fsync (4 MB/s, about what a large sync writes)
  the whole time: the backup took 38 s, the WAL peaked at 174 MB, and 2 s after
  the backup the writer's next checkpoint had returned it to the 64 MB journal
  size limit. At that rate the 1 GiB bound allows a copy of about four minutes,
  enough for a catalog of tens of gigabytes; back up while no large sync runs.
- **Incremental and interruption-safe.** A file whose size and mtime match its
  copy in DEST is skipped. File systems that store mtimes coarsely (exFAT to
  10 ms, HFS+ to 1 s, FAT to 2 s) match within 2 s, so backups to them stay
  incremental. Anything else is copied to a temporary name, fsynced, given the
  source's mtime, and renamed into place. The catalog is copied again only when
  its file or WAL changed. `pharos-backup.json` says `"complete": false` from
  the start of a run until the end, when it is replaced after a full device
  flush that also makes every file copied before it durable. An interrupted
  backup leaves each file either as it was or complete, and the next run
  carries on.
- **Nothing is deleted** from DEST unless `--prune` is given, which deletes files
  under `captures/`, `hosts/` and `preserved/` that the library no longer has
  (and temporaries of interrupted runs). A file is kept if it is one the run
  just copied or checked (matched by device and inode, not by name, because
  HFS+ and exFAT store accented and Korean names decomposed) or if the library
  still has a file at the name DEST lists it under; macOS's `._` files beside
  kept files on exFAT are kept too. A part that could not be read completely,
  whose source is missing, or that lists no files at all is never pruned (the
  last is reported as a warning); nor is anything where the library now has a
  symbolic link.
- **Partial backups.** A part that is missing (`archive_root` whose drive is
  not mounted, or any part the previous backup had) keeps its earlier copy, and
  the run ends `"state": "partial"` with the parts in `missing`; the command
  exits non-zero, and the `backup` check warns until a full backup runs.
- **Credentials.** `github_token` and `tl1_token`, credentials for other
  services, are left out of the backed-up configuration (replaced by a comment);
  set them again after restoring. `api_token` is kept: it only guards the local
  service, whose data the backup holds anyway, and keeping it lets a restored
  library open as it was.
- **Low impact.** Like a capture, a backup's disk I/O runs in the utility I/O
  tier.
- **Recorded.** Each finished backup is added to `backups.json` beside
  `library.toml` (beside the catalog for a per-user install), under a lock
  (`.backups.json.lock`) and replaced atomically, so concurrent backups never
  lose each other's records; the catalog is not touched. The `backup` check and
  the growth estimate (full backups only) read it.

The command prints the run as JSON (files and bytes copied and unchanged,
duration, throughput, errors, warnings, missing parts) and exits non-zero
unless it completed. The service offers the same: `POST /api/backup` with
`{"destination": "/absolute/path", "prune": false}` checks the destination
(`400` with the reason if it is refused, `409` while a backup is running) and
answers `202`; `GET /api/backup` reports `active`, the latest `run` with its
`phase` and progress, recent `runs`, `last_success`, and `suggested_destination`
(the last destination; otherwise, for a library on another drive,
`~/Pharos Backup` on this Mac; otherwise empty); `POST /api/backup/cancel` stops
it.

Settings → Health → **Backups** does the same: it shows the last backup, a
destination field filled with the suggestion, **Back up now**, the running
backup's phase and progress with **Cancel backup**, and the result, with its
warnings (such as an unencrypted destination). A refused destination, such as
one on the library's own drive, is shown with the reason under the field. A release before an eject also stops it, within a
catalog step or a file chunk; the catalog snapshot in progress is discarded.

Measured on a synthetic 4.6 GB library (1.5 GB catalog, 4,100 files) on a
sparse disk image, backing up to the internal SSD of a heavily loaded Mac
Studio: a first backup took 12–52 s (90–390 MB/s, depending on other I/O), a
backup with nothing changed 0.07 s, and one after 50 files and the catalog
changed 4.4 s, most of it copying the catalog again. A library's catalog of tens
of gigabytes is therefore copied in minutes whenever it has changed.

**Restoring.** Copy the backup folder to a drive, install the app into it
(`macos/install-library.sh DIR` keeps the existing `library.toml`), and set
`volume_id` in `library.toml` to what `alexandria volume-id DIR` prints: until
then the guard refuses to open the copy, as it should for any copy of a library.
Set `github_token` or `tl1_token` again if you used them. `pharos-backup.json`
can be deleted. The restored library gets a new `library_id` on its first
backup, so back it up to a new folder.

## MCP access

The MCP page shows the local stdio command and configuration to copy into an
agent client's MCP settings. For a per-user install that is the configured
`executable` with `--config PATH mcp`; for a portable library it is the
launcher described below. MCP is enabled by default for existing installations;
the page's switch stores its state in the catalog so it takes effect for both
new and already running MCP processes. Enabling access does not start a network
listener or configure an agent client automatically. After enabling it,
reconnect a client that previously saw an empty tool list.

**No open catalog between requests.** An agent client keeps its MCP server
process for the whole session, so the server never holds the catalog open
while idle; otherwise Finder could not eject the library's drive while any
agent session was open. For each `tools/list` and `tools/call` it rereads the
configuration and then:

- if the Pharos service for that library is running, forwards the request to
  it (`POST /api/mcp/rpc` on the loopback port, authenticated with the
  library's `api_token`). The service answers from its open catalog and records
  the call. A service with a different catalog open declines, and the server
  falls back as below;
- otherwise opens the catalog for that one request and closes it again. This
  open skips the migrations and backfills a full open runs when the catalog was
  last initialized by this exact build, and reruns them for another build of
  the same schema version. It never migrates: a call on an older catalog fails
  with `This library needs to be opened by the Pharos app once to upgrade it`,
  and a newer one is refused unchanged. One process initializes a catalog at a
  time (an `flock` on `.initialize.lock` beside it); a call that finds another
  process doing so, such as the service upgrading the catalog, waits up to 2
  seconds and then fails with `Pharos is upgrading this library's catalog`.

The library guards above run before such an open, and a missing catalog is
never created. The volume check takes about 0.1 s, so it is skipped while the
library directory is the one on the same device that last passed it.

**Library not connected.** `alexandria mcp` starts and keeps running while the
library is unavailable. `initialize` and `tools/list` still answer, and tool
calls fail with an error such as `Pharos library is not connected:
/Volumes/euclid is not mounted`. The first call after the drive is back
succeeds; agents do not need to reconnect. The MCP switch applies whenever the
catalog is reachable.

A drive pulled while a call has the catalog open makes that call fail with
`Pharos library disconnected: …`. The server then replaces itself with a fresh
process (same PID and stdio, so the client stays connected), because SQLite's
state for that file is unusable afterwards. When the service releases the
library for an eject, it leaves `.released` beside the catalog; for 30 seconds,
or until the library is served again, calls report the library as released
rather than reopening the catalog while the drive unmounts.

**Launcher.** A process running a binary from the library drive would keep the
drive from ejecting and die when it is unplugged. So for a portable library the
MCP command is `~/Library/Application Support/Pharos/bin/pharos-mcp`, run with no
arguments. The service writes it when it starts, and `alexandria install-mcp`
writes it on demand. It runs

```sh
~/Library/Application Support/Pharos/runtime/current/Contents/MacOS/alexandria mcp \
  --library-json ~/Library/Application Support/Pharos/library.json
```

The Pharos app keeps both current when it runs from a library: `runtime/current`
links to a local copy of the library's `Pharos.app`, and `library.json` names
the library. The service writes `library.json` too whenever it serves a
library, so a library the app runs in place or the CLI serves is found as well:

```json
{"library_dir": "/Volumes/euclid/Pharos", "volume_uuid": "642C2C39-5926-4831-ABE1-34642F37B103", "volume_name": "euclid", "updated_at": "2026-09-24T12:00:00Z"}
```

`library_dir`, the absolute directory holding `library.toml`, is required.
`volume_uuid` (bare or `uuid:`-prefixed) only names the drive in messages;
`library.toml`'s `volume_id` is what the guard checks. Other fields are ignored.
The MCP server rereads the file for every request. Without a local runtime the
launcher runs the app on the library drive instead and warns on stderr, since
unplugging the drive then stops the server; with neither, it exits with an
error. `PHAROS_SUPPORT_DIR` replaces `~/Library/Application Support/Pharos`
when the launcher is written.

`tools/mcp-eject-test.sh` checks this end to end on a disk image it creates:
an idle MCP server does not block `diskutil eject`, reports the library as not
connected while it is away, and answers again after it is re-attached.

The catalog retains the latest 5,000 MCP tool calls. History records tool name,
an allowlisted and shortened argument summary, success or error, result count,
response bytes, approximate output tokens, and duration. It does not store
response bodies. The MCP call-history query-table supports field filters, sorts,
saved views, paging, and aggregate metrics by tool or time period, including
response sizes, errors, and truncation.

The native `tl1` source is read-only and discovers currently registered installations.
Its `path` names either TL1 registry file in TL1's state directory (`~/.tl1`).
Pharos reads both, as TL1 does: every installation in `registry.json`, then
each project in `workspaces.json`, which TL1 releases before `registry.json`
kept, that no installation has registered yet. A `workspaces.json` project's
database and transcripts are where its `tl1.json` puts them (`db_path`,
`transcripts_dir`), by default `<project>.db` and `<project>/transcripts` beside
the registry. Such a project has no installation ID, so Pharos identifies it by
its database path. When a later TL1 registers the project, it gives it a new
installation ID, and Pharos then indexes its tasks again under that ID.

### Mothballed reclamation keys

TL1 reclamation is mothballed (see [`reclamation/README.md`](reclamation/README.md)).
The service still parses `upcoming_days`, `eligible_days`, `snooze_days`,
`enable_reclamation`, `release_hook_proven`, `tl1_url`, and `tl1_token` so
existing configuration files keep loading, but none of them has any effect in
the default build. When reclamation resumes, the old rules apply: `volume_id` is
mandatory, `enable_reclamation = true` authorizes the scheduler, and
`release_hook_proven = true` asserts that the owner hook passed the contract
tests. Both flags and a configured hook are required, and release authority
comes only from owner-produced `tl1-export` records.
