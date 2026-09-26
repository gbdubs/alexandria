#!/bin/sh
# Integration test for MCP with a library on a removable drive (macOS only):
#
#   tools/mcp-eject-test.sh
#
# It builds pharos, installs it as the local runtime in a temporary
# PHAROS_SUPPORT_DIR, creates a library on a new 300 MB APFS disk image, and
# starts one MCP server through the pharos-mcp launcher. It then checks that:
#
#   - the idle server holds no file on the drive, and `diskutil eject` succeeds;
#   - while the drive is away, tools/list still answers and tools/call reports
#     "not connected"; after re-attaching, the same process answers again;
#   - with the Pharos service running, calls are forwarded to it (its history
#     grows) and the MCP process still holds nothing on the drive;
#   - right after the service released the library for an eject, calls do not
#     reopen the catalog, so the eject succeeds;
#   - a forced detach (a yank) is survived the same way, also while calls
#     have the catalog open.
#
# It touches only the image it creates (volume PharosMCPTest-*), a temporary
# directory, and a random loopback port, and cleans all of them up.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d /tmp/pharos-mcp-eject.XXXXXX)
name="PharosMCPTest-$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')"
volume="/Volumes/$name"
mcp_pid=""
service_pid=""

cleanup() {
	exec 3>&- 2>/dev/null || true
	[ -n "$service_pid" ] && kill "$service_pid" 2>/dev/null || true
	[ -n "$mcp_pid" ] && kill "$mcp_pid" 2>/dev/null || true
	wait 2>/dev/null || true
	if [ -d "$volume" ]; then hdiutil detach -force "$volume" >/dev/null 2>&1 || true; fi
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
	echo "FAIL: $*" >&2
	[ -s "$work/err" ] && sed 's/^/  mcp stderr: /' "$work/err" >&2
	exit 1
}
pass() { echo "ok: $*"; }

[ "$(uname)" = Darwin ] || fail "needs macOS"
[ ! -e "$volume" ] || fail "$volume already exists"

echo "building pharos"
support="$work/support"
app="$support/runtime/build-1/Pharos.app"
bin="$app/Contents/MacOS/pharos"
mkdir -p "$(dirname "$bin")"
(cd "$root" && GOTOOLCHAIN=local go build -o "$bin" ./cmd/pharos)
ln -s build-1/Pharos.app "$support/runtime/current"
export PHAROS_SUPPORT_DIR="$support"
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')

echo "creating $volume"
hdiutil create -quiet -size 300m -fs APFS -volname "$name" "$work/drive.dmg"
attach() { hdiutil attach -quiet -nobrowse "$work/drive.dmg"; [ -d "$volume" ] || fail "$volume did not mount"; }
attach
library="$volume/Pharos"
"$bin" init-library "$library" >/dev/null
sed -i '' "s/^port = .*/port = $port/" "$library/library.toml"
cat >"$work/export.json" <<'EOF'
{"workspaces":[{"id":"work-1","title":"Parser maintenance","activity_at":"2026-09-20T12:00:00Z",
 "conversations":[{"id":"parser-session","provider":"codex","messages":[
  {"id":"q","role":"user","text":"Fix the tokenizer parser bug"},
  {"id":"a","role":"assistant","text":"Resolved the parser issue."}]}]}]}
EOF
printf '\n[[sources]]\nname = "fixture"\nkind = "canonical"\npath = "%s"\n' "$work/export.json" >>"$library/library.toml"
"$bin" --config "$library/library.toml" ingest >/dev/null
uuid=$("$bin" volume-id "$library" | sed 's/^uuid://')
printf '{"library_dir": "%s", "volume_uuid": "%s", "updated_at": "%s"}\n' "$library" "$uuid" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$support/library.json"
"$bin" install-mcp >/dev/null
launcher="$support/bin/pharos-mcp"
[ -x "$launcher" ] || fail "install-mcp wrote no launcher"
token=$(sed -n 's/^api_token = "\(.*\)"$/\1/p' "$library/library.toml")

echo "starting the MCP server"
mkfifo "$work/in"
: >"$work/out"
"$launcher" <"$work/in" >"$work/out" 2>"$work/err" &
mcp_pid=$!
exec 3>"$work/in"

request() {
	lines=$(wc -l <"$work/out")
	printf '%s\n' "$1" >&3
	tries=0
	while [ "$(wc -l <"$work/out")" -le "$lines" ]; do
		tries=$((tries + 1))
		[ "$tries" -le 300 ] || fail "no answer to $1"
		kill -0 "$mcp_pid" 2>/dev/null || fail "the MCP server exited"
		sleep 0.1
	done
	tail -n 1 "$work/out"
}
search='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_work","arguments":{"query":"parser"}}}'
expect_results() {
	response=$(request "$search")
	case "$response" in *'"isError":false'*'Parser maintenance'* | *'Parser maintenance'*'"isError":false'*) pass "$1" ;; *) fail "$1: $response" ;; esac
}
expect_disconnected() {
	response=$(request "$search")
	case "$response" in *'"isError":true'*'Pharos library is not connected'*'is not mounted'* | *'Pharos library is not connected'*'is not mounted'*'"isError":true'*) pass "$1" ;; *) fail "$1: $response" ;; esac
	tools=$(request '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
	case "$tools" in *search_conversations*get_receipt*) pass "tools/list answers while the drive is away" ;; *) fail "tools/list while away: $tools" ;; esac
}
expect_nothing_open() {
	if lsof -n -P -p "$mcp_pid" 2>/dev/null | grep -F "$volume" >/dev/null; then
		lsof -n -P -p "$mcp_pid" | grep -F "$volume" >&2
		fail "$1: the MCP server holds files on $volume"
	fi
	pass "$1: the MCP server holds nothing on the drive"
}

response=$(request '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}')
case "$response" in *serverInfo*) pass "initialize" ;; *) fail "initialize: $response" ;; esac
expect_results "direct call from the library"
expect_nothing_open "idle after a direct call"

diskutil eject "$volume" >/dev/null || fail "diskutil eject was blocked"
pass "diskutil eject succeeded with the MCP server running"
expect_disconnected "call with the drive ejected"
attach
expect_results "call after re-attaching, same process"

start_service() {
	echo "starting the Pharos service on port $port"
	"$bin" --config "$library/library.toml" serve >"$work/service.log" 2>&1 &
	service_pid=$!
	tries=0
	until curl -fs -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/mcp" >/dev/null 2>&1; do
		tries=$((tries + 1))
		[ "$tries" -le 100 ] || fail "the service did not start: $(cat "$work/service.log")"
		sleep 0.1
	done
}
start_service
calls() { curl -fs -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/mcp/calls?limit=1" | sed -n 's/.*"total_calls":\([0-9]*\).*/\1/p'; }
before=$(calls)
expect_results "call with the service running"
[ "$(calls)" -gt "$before" ] || fail "the forwarded call was not recorded by the service"
pass "the call was forwarded and recorded by the service"
expect_nothing_open "idle while the service is running"
kill "$service_pid"
wait "$service_pid" 2>/dev/null || true
service_pid=""
expect_results "call after the service stopped (direct again)"

# A release before an eject: once the service has exited, a call must not
# reopen the catalog before the drive unmounts.
start_service
curl -fsS -X POST -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/release" | grep -q '"released":true' || fail "release failed"
wait "$service_pid" 2>/dev/null || true
service_pid=""
response=$(request "$search")
case "$response" in *'"isError":true'*'released it so that its drive can be ejected'* | *'released it so that its drive can be ejected'*'"isError":true'*) pass "call right after a release leaves the catalog closed" ;; *) fail "call right after a release: $response" ;; esac
expect_nothing_open "after a release"
diskutil eject "$volume" >/dev/null || fail "diskutil eject after a release was blocked"
pass "diskutil eject succeeded after a release"
attach
# The app serves the library again once its drive is back, which ends the release.
start_service
expect_results "call once the library is served again"
kill "$service_pid"
wait "$service_pid" 2>/dev/null || true
service_pid=""
expect_results "direct call after that service stopped"

hdiutil detach -force "$volume" >/dev/null || fail "forced detach failed"
pass "yanked the drive"
expect_disconnected "call after the yank"
attach
expect_results "call after re-attaching the yanked drive, same process"

# A yank while calls run back to back, so one has the catalog open: that call
# fails (on a memory fault the server restarts itself in place), and every
# call gets an answer.
lines=$(wc -l <"$work/out")
calls=1500
python3 -c 'import sys
for i in range(int(sys.argv[1])): print("{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"tools/call\",\"params\":{\"name\":\"search_work\",\"arguments\":{}}}" % (1000 + i))' "$calls" >"$work/burst"
cat "$work/burst" >&3 &
sleep 0.3
hdiutil detach -force "$volume" >/dev/null || fail "forced detach during calls failed"
tries=0
while [ "$(wc -l <"$work/out")" -lt $((lines + calls)) ]; do
	tries=$((tries + 1))
	[ "$tries" -le 600 ] || fail "only $(($(wc -l <"$work/out") - lines)) of $calls calls answered after the yank"
	kill -0 "$mcp_pid" 2>/dev/null || fail "the MCP server exited on a yank during a call"
	sleep 0.1
done
wait $! 2>/dev/null || true
if grep -q "restarting after a memory fault" "$work/err"; then note="a call faulted and the server restarted in place"; else note="no call faulted this time"; fi
pass "yank during calls: all $calls answered, same process ($note)"
attach
expect_results "call after re-attaching, same process"
kill -0 "$mcp_pid" 2>/dev/null || fail "the MCP server exited"
diskutil eject "$volume" >/dev/null || fail "diskutil eject was blocked at the end"
pass "diskutil eject succeeded at the end"
echo "PASS"
