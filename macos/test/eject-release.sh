#!/bin/sh
# Integration test for releasing a library before an eject and surviving a
# yank. Not part of `go test`: it needs hdiutil and diskutil. It creates,
# attaches, ejects and force-detaches only its own disk image
# (/Volumes/PharosTest-*), and serves on PORT (default 18767), never 8765/8766.
#
#   macos/test/eject-release.sh [PORT]
#
# 1. Serving holds the catalog open, so a plain eject fails.
# 2. POST /api/release during a source sync interrupts it between records,
#    closes the catalog and exits 0; the eject then succeeds.
# 3. After re-attaching, `hdiutil detach -force` during a sync (a yank) makes
#    the service exit non-zero within seconds instead of hanging.
# 4. After re-attaching again, the catalog passes integrity_check and a new
#    sync resumes and completes without error: records committed before the
#    yank are skipped as current, and every workspace ends up in the catalog.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
PORT=${1:-18767}
case "$PORT" in 8765|8766) echo "Use a port other than 8765/8766." >&2; exit 2 ;; esac
WORK=$(mktemp -d)
# Serving writes per-Mac files here, never in ~/Library/Application Support/Pharos.
export PHAROS_SUPPORT_DIR="$WORK/support"
NAME="PharosTest-$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')"
DMG="$WORK/$NAME.sparseimage"
MOUNT=
DEV=
PID=

cleanup() {
    [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
    if [ -n "$DEV" ]; then hdiutil detach -force "$DEV" >/dev/null 2>&1 || true; fi
    rm -rf "$WORK"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; [ -f "$WORK/serve.log" ] && sed 's/^/  serve: /' "$WORK/serve.log" >&2; exit 1; }
pass() { echo "ok: $*"; }

attach() {
    attached=$(hdiutil attach -nobrowse "$DMG")
    DEV=$(echo "$attached" | awk 'NR == 1 {print $1}')
    MOUNT=$(echo "$attached" | awk -F'\t' '/\/Volumes\//{print $NF}')
    case "$DEV" in /dev/disk*) ;; *) fail "unexpected device '$DEV'" ;; esac
    case "$MOUNT" in "/Volumes/$NAME"*) ;; *) fail "unexpected mount point '$MOUNT'" ;; esac
}
serve() {
    "$CLI" --config "$MOUNT/Pharos/library.toml" serve >"$WORK/serve.log" 2>&1 &
    PID=$!
    tries=0
    until curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; do
        tries=$((tries + 1))
        [ "$tries" -lt 200 ] || fail "service did not start"
        kill -0 "$PID" 2>/dev/null || fail "service exited during startup"
        sleep 0.1
    done
}
start_sync() {
    curl -sS -X POST -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/sources/sync" >"$WORK/sync.json" 2>&1 &
    tries=0
    until curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/health" 2>/dev/null | grep -q '"sync_active":true'; do
        tries=$((tries + 1))
        [ "$tries" -lt 100 ] || fail "sync did not start"
        sleep 0.05
    done
    sleep 1
}
# wait_exit SECONDS: waits for the service and sets STATUS to its exit status.
wait_exit() {
    deadline=$(( $(date +%s) + $1 ))
    while kill -0 "$PID" 2>/dev/null; do
        [ "$(date +%s)" -lt "$deadline" ] || fail "service still running after $1 s"
        sleep 0.1
    done
    STATUS=0
    wait "$PID" || STATUS=$?
    PID=
}

echo "Building pharos…"
(cd "$ROOT" && GOTOOLCHAIN=local go build -o "$WORK/pharos" ./cmd/pharos)
CLI="$WORK/pharos"

# A canonical export large enough that a sync is still running a second in.
python3 - "$WORK/export.json" <<'EOF'
import json, sys
workspaces = [{"id": f"w{i}", "title": f"Workspace {i}", "conversations": [{"id": f"t{i}", "provider": "codex",
    "messages": [{"id": f"m{i}-{j}", "role": "user" if j % 2 == 0 else "assistant", "text": f"message {j} of workspace {i} " * 20} for j in range(20)]}]}
    for i in range(6000)]
json.dump({"workspaces": workspaces}, open(sys.argv[1], "w"))
EOF

# The fixture's full catalog is about 370 MB; a sparse image only takes the
# space it uses, so leave generous room for the WAL and the resumed sync.
hdiutil create -quiet -size 2g -type SPARSE -fs APFS -volname "$NAME" "$DMG"
attach
"$CLI" init-library "$MOUNT/Pharos" >/dev/null
CONFIG="$MOUNT/Pharos/library.toml"
grep -q '^volume_id = "uuid:' "$CONFIG" || fail "library was not pinned to its volume"
sed -i '' "s/^port = 8766$/port = $PORT/" "$CONFIG"
cat >>"$CONFIG" <<EOF

[[sources]]
name = "fixture"
kind = "canonical"
path = "$WORK/export.json"
account = "local"
enabled = true
EOF
TOKEN=$(sed -n 's/^api_token = "\(.*\)"$/\1/p' "$CONFIG")
CATALOG="$MOUNT/Pharos/catalog/catalog.sqlite3"

# 1. The open catalog blocks a plain eject.
serve
if diskutil eject "$MOUNT" >/dev/null 2>&1; then DEV=; fail "eject succeeded while the catalog was open"; fi
pass "eject refused while serving"

# 2. Release during a sync.
start_sync
started=$(date +%s)
curl -sS -X POST -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/release" >"$WORK/release.json" || fail "release request failed"
grep -q '"released":true' "$WORK/release.json" || fail "release answered $(cat "$WORK/release.json")"
pass "release answered in $(( $(date +%s) - started )) s: $(cat "$WORK/release.json")"
wait_exit 5
[ "$STATUS" -eq 0 ] || fail "service exited with $STATUS after release"
pass "service exited 0 after release"
wait
grep -q 'interrupted' "$WORK/sync.json" || fail "sync was not interrupted: $(head -c 300 "$WORK/sync.json")"
pass "in-flight sync interrupted: $(grep -o '"workspaces":[0-9]*' "$WORK/sync.json" | head -1)"
[ ! -e "$CATALOG-wal" ] || fail "WAL left behind after release"
curl -s -o /dev/null "http://127.0.0.1:$PORT/api/health" && fail "port still answering after release"
diskutil eject "$MOUNT" >/dev/null || fail "eject failed after release"
DEV=
pass "eject succeeded after release"

# 3. Yank during a sync.
attach
serve
start_sync
started=$(date +%s)
hdiutil detach -force "$DEV" >/dev/null || fail "force detach failed"
YANKED=$MOUNT
DEV=
wait_exit 10
[ "$STATUS" -ne 0 ] || fail "service exited 0 after its drive vanished"
grep -q "drive disappeared" "$WORK/serve.log" || fail "no clear message after the yank"
pass "service exited $STATUS $(( $(date +%s) - started )) s after the yank: $(grep -m1 'drive disappeared' "$WORK/serve.log")"
wait

# 4. The catalog survives.
attach
[ "$MOUNT" = "$YANKED" ] || echo "note: re-attached at $MOUNT"
CONFIG="$MOUNT/Pharos/library.toml"
CATALOG="$MOUNT/Pharos/catalog/catalog.sqlite3"
check=$(sqlite3 "$CATALOG" 'PRAGMA integrity_check;')
[ "$check" = ok ] || fail "integrity_check: $check"
KEPT=$(sqlite3 "$CATALOG" 'SELECT COUNT(*) FROM workspaces;')
pass "catalog integrity_check ok ($KEPT workspaces kept)"
serve
curl -sS -X POST -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/sources/sync" >"$WORK/sync.json"
grep -q '"ok":true' "$WORK/sync.json" || fail "resumed sync failed: $(head -c 600 "$WORK/sync.json")"
if grep -q '"error":"' "$WORK/sync.json"; then fail "resumed sync reported an error: $(head -c 600 "$WORK/sync.json")"; fi
grep -q "\"skipped_current\":$KEPT[,}]" "$WORK/sync.json" || fail "sync did not resume after $KEPT kept workspaces: $(head -c 400 "$WORK/sync.json")"
TOTAL=$(sqlite3 "$CATALOG" 'SELECT COUNT(*) FROM workspaces;')
[ "$TOTAL" -eq 6000 ] || fail "resumed sync left $TOTAL of 6000 workspaces"
pass "sync resumed and completed: skipped the $KEPT kept workspaces, now $TOTAL"
kill -TERM "$PID"
wait_exit 15
[ "$STATUS" -eq 0 ] || fail "service exited $STATUS after SIGTERM"
pass "SIGTERM: exit 0"
diskutil eject "$MOUNT" >/dev/null || fail "final eject failed"
DEV=
echo "PASS"
