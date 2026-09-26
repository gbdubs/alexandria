#!/bin/sh
# Integration test for adopting a per-user install into a library on a drive
# (macOS only). Not part of `go test`: it needs hdiutil, a build from before
# hosts existed, and that build's service running during the adoption.
#
#   macos/test/adopt-library.sh [LEGACY_REF] [PORT]
#
# LEGACY_REF (default euclid/baseline) is any commit whose catalog predates
# hosts (schema 4); PORT (default: random in 20000-29999) is where its service
# listens, never 8765/8766. The script builds both, then:
#
# 1. creates a legacy install in a temporary directory: archive.toml with
#    sources and a catalog indexed by the old build;
# 2. starts the old build's service and keeps it indexing new sessions during
#    the adoption, so the catalog is written to and has a live WAL;
# 3. adopts the install into a library on a new APFS disk image;
# 4. checks that the library is pinned to the image's volume UUID and passes
#    the guards, that search finds sessions indexed before the adoption, that
#    the host file keeps the sources as written, and that the legacy catalog is
#    the same file, still schema 4, and still served by the old service;
# 5. checks that adopting into the library again, and opening a copy of the
#    library from another volume, are both refused;
# 6. checks that a leftover catalog -wal in the target, a library inside
#    archive_root (spelled in another case), and install-library.sh with an
#    empty or missing --adopt CONFIG are refused without changing the volume.
#
# It touches only its own image (/Volumes/PharosAdoptTest-*), a temporary
# directory, and the port.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
REF=${1:-euclid/baseline}
PORT=${2:-$((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 10000))}
case "$PORT" in 8765|8766) echo "Use a port other than 8765/8766." >&2; exit 2 ;; esac
WORK=$(mktemp -d)
NAME="PharosAdoptTest-$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')"
IMAGE="$WORK/$NAME.sparseimage"
MOUNT=
DEV=
PID=
FEEDER=
export PHAROS_SUPPORT_DIR="$WORK/support"

cleanup() {
    [ -n "$FEEDER" ] && kill "$FEEDER" 2>/dev/null || true
    [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
    wait 2>/dev/null || true
    if [ -n "$DEV" ]; then hdiutil detach -force "$DEV" >/dev/null 2>&1 || true; fi
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM
fail() { echo "FAIL: $*" >&2; [ -f "$WORK/legacy.log" ] && sed 's/^/  legacy service: /' "$WORK/legacy.log" | tail -20 >&2; exit 1; }
pass() { echo "ok: $*"; }

echo "Building pharos and the legacy build ($REF)…"
(cd "$ROOT" && GOTOOLCHAIN=local go build -o "$WORK/pharos" ./cmd/pharos)
mkdir "$WORK/legacy-src"
git -C "$ROOT" archive "$REF" | tar -x -C "$WORK/legacy-src"
(cd "$WORK/legacy-src" && GOTOOLCHAIN=local go build -o "$WORK/pharos-legacy" ./cmd/alexandria)
CLI="$WORK/pharos"
LEGACY="$WORK/pharos-legacy"

# sessions DIR FIRST COUNT writes Claude Code transcripts s<FIRST>… of 20
# messages each; every message of session N contains the word adoptmarkerN.
sessions() {
    i=$2
    while [ "$i" -lt $(($2 + $3)) ]; do
        m=0
        while [ "$m" -lt 20 ]; do
            printf '{"sessionId":"s%d","uuid":"s%d-m%d","type":"user","cwd":"/Users/test/project","timestamp":"2026-09-01T00:%02d:00Z","message":{"content":"adoptmarker%d message %d lorem ipsum dolor sit amet"}}\n' \
                "$i" "$i" "$m" "$m" "$i" "$m"
            m=$((m + 1))
        done >"$1/s$i.jsonl"
        i=$((i + 1))
    done
}

LEGACY_DIR="$WORK/Pharos"
CONFIG="$LEGACY_DIR/archive.toml"
PROJECTS="$WORK/home/.claude/projects/-Users-test-project"
TOKEN=adopt-test-token
mkdir -p "$LEGACY_DIR" "$PROJECTS"
sessions "$PROJECTS" 0 200
cat >"$CONFIG" <<EOF
data_dir = "$LEGACY_DIR"
archive_root = "$WORK/archive"
api_token = "$TOKEN"
host = "127.0.0.1"
port = $PORT
upcoming_days = 21

[[sources]]
name = "claude"
kind = "claude"
path = "$WORK/home/.claude/projects"
account = "local"

[[sources]]
name = "tl1"
kind = "tl1"
path = "~/.tl1-pharos-adopt-test/registry.json"
enabled = false
EOF
"$LEGACY" --config "$CONFIG" ingest >/dev/null
CATALOG="$LEGACY_DIR/catalog.sqlite3"
INODE=$(stat -f %i "$CATALOG")

"$LEGACY" --config "$CONFIG" serve >"$WORK/legacy.log" 2>&1 &
PID=$!
api() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
tries=0
until api "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ "$tries" -lt 200 ] || fail "legacy service did not start"
    kill -0 "$PID" 2>/dev/null || fail "legacy service exited during startup"
    sleep 0.1
done
# Keep the old service indexing new sessions while the adoption runs.
(
    next=1000
    while :; do
        sessions "$PROJECTS" "$next" 5
        next=$((next + 5))
        api -X POST "http://127.0.0.1:$PORT/api/sources/sync" >/dev/null 2>&1 || true
    done
) &
FEEDER=$!
sleep 1
[ -s "$CATALOG-wal" ] || fail "the running legacy service has no WAL"
# Read-only readers need the -wal and -shm files the running service keeps.
schema() { sqlite3 -readonly "$CATALOG" "SELECT value FROM meta WHERE key='schema_version'"; }
[ "$(schema)" = 4 ] || fail "the legacy build wrote schema $(schema), not 4; choose an older LEGACY_REF"
pass "legacy install by $REF (schema 4) served on port $PORT and indexing"

hdiutil create -quiet -size 4g -type SPARSE -fs APFS -volname "$NAME" "$IMAGE"
attached=$(hdiutil attach -nobrowse "$IMAGE")
DEV=$(echo "$attached" | awk 'NR == 1 {print $1}')
MOUNT=$(echo "$attached" | awk -F'\t' '/\/Volumes\//{print $NF}')
case "$MOUNT" in "/Volumes/$NAME"*) ;; *) fail "unexpected mount point '$MOUNT'" ;; esac
touch "$MOUNT/.metadata_never_index"
UUID=$(diskutil info "$MOUNT" | awk -F': *' '/Volume UUID/ {print $2}')
LIBRARY="$MOUNT/Pharos"

"$CLI" init-library "$LIBRARY" --adopt "$CONFIG" >"$WORK/adopt.out" 2>"$WORK/adopt.err" || { cat "$WORK/adopt.err" >&2; fail "adoption failed"; }
kill "$FEEDER" 2>/dev/null || true
wait "$FEEDER" 2>/dev/null || true
FEEDER=
sed 's/^/  /' "$WORK/adopt.out"
[ "$(ls "$LIBRARY/catalog")" = catalog.sqlite3 ] || fail "left files beside the catalog: $(ls "$LIBRARY/catalog")"
pass "adopted while the legacy service kept indexing"

grep -qx "volume_id = \"uuid:$UUID\"" "$LIBRARY/library.toml" || fail "library.toml is not pinned to uuid:$UUID"
grep -qx "port = 8766" "$LIBRARY/library.toml" || fail "library.toml does not use port 8766"
grep -qx "upcoming_days = 21" "$LIBRARY/library.toml" || fail "upcoming_days was not carried over"
grep -q "$TOKEN" "$LIBRARY/library.toml" && fail "library.toml reuses the legacy token"
HOSTFILE=$(ls "$LIBRARY"/hosts/*.toml)
grep -qx 'path = "~/.tl1-pharos-adopt-test/registry.json"' "$HOSTFILE" || fail "the host file rewrote a ~ path"
grep -qx 'path = "'"$WORK"'/home/.claude/projects"' "$HOSTFILE" || fail "the host file lost the claude path"
[ "$(grep -c '^\[\[sources\]\]' "$HOSTFILE")" = 2 ] || fail "the host file does not hold both sources"
pass "library.toml pinned to uuid:$UUID; host file keeps the sources as written"

"$CLI" --config "$LIBRARY/library.toml" health >"$WORK/health.json" || fail "the library does not pass its guards"
# Closed without a WAL, so it can be read as immutable.
version=$(sqlite3 "file:$LIBRARY/catalog/catalog.sqlite3?immutable=1" "SELECT value FROM meta WHERE key='schema_version'")
[ "$version" -gt 4 ] || fail "the copy was not migrated (schema $version)"
"$CLI" --config "$LIBRARY/library.toml" search adoptmarker137 >"$WORK/search.json"
grep -q '"items": \[\]' "$WORK/search.json" && fail "search in the library does not find a session indexed before the adoption"
pass "the library opens, is migrated, and finds legacy sessions"

[ "$(stat -f %i "$CATALOG")" = "$INODE" ] || fail "the legacy catalog was replaced"
[ "$(schema)" = 4 ] || fail "the legacy catalog was migrated to schema $(schema)"
kill -0 "$PID" 2>/dev/null || fail "the legacy service died"
api "http://127.0.0.1:$PORT/api/health" >/dev/null || fail "the legacy service no longer answers"
pass "the legacy catalog is the same file at schema 4, still served"

before=$(shasum "$LIBRARY/library.toml")
if "$CLI" init-library "$LIBRARY" --adopt "$CONFIG" >/dev/null 2>"$WORK/again.err"; then fail "a second adoption was not refused"; fi
grep -q "already exists" "$WORK/again.err" || fail "unexpected refusal: $(cat "$WORK/again.err")"
[ "$(shasum "$LIBRARY/library.toml")" = "$before" ] || fail "a refused adoption changed library.toml"
mkdir "$WORK/clone"
cp -R "$LIBRARY" "$WORK/clone/"
if "$CLI" --config "$WORK/clone/Pharos/library.toml" health >/dev/null 2>"$WORK/clone.err"; then fail "a copy on another volume was opened"; fi
grep -q "pins volume_id" "$WORK/clone.err" || fail "unexpected clone error: $(cat "$WORK/clone.err")"
pass "adopting again and opening a copy elsewhere are refused"

# refused NAME MESSAGE COMMAND...: COMMAND must fail with MESSAGE and leave the
# volume as it was.
refused() {
    name=$1 message=$2
    shift 2
    before=$(find "$MOUNT" | sort)
    if "$@" >"$WORK/refused.out" 2>&1; then fail "$name was not refused"; fi
    grep -q -- "$message" "$WORK/refused.out" || fail "$name: unexpected error: $(cat "$WORK/refused.out")"
    [ "$(find "$MOUNT" | sort)" = "$before" ] || fail "$name changed the volume"
}
mkdir -p "$MOUNT/Stale/catalog"
echo stale >"$MOUNT/Stale/catalog/catalog.sqlite3-wal"
refused "a leftover -wal" "catalog.sqlite3-wal already exists" "$CLI" init-library "$MOUNT/Stale" --adopt "$CONFIG"
mkdir "$MOUNT/Archive"
sed "s|^archive_root = .*|archive_root = \"$MOUNT/Archive\"|" "$CONFIG" >"$LEGACY_DIR/nested.toml"
refused "a library inside archive_root, spelled in another case" "contain one another" \
    "$CLI" init-library "$MOUNT/ARCHIVE/Pharos" --adopt "$LEGACY_DIR/nested.toml"
refused "install-library.sh --adopt ''" "needs the archive.toml" "$ROOT/macos/install-library.sh" "$MOUNT/Empty" --adopt ""
refused "install-library.sh --adopt MISSING" "needs the archive.toml" "$ROOT/macos/install-library.sh" "$MOUNT/Empty" --adopt "$WORK/missing.toml"
pass "leftover catalog files, nested archive roots, and a missing --adopt CONFIG are refused without changes"
echo "PASS"
