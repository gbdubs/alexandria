#!/bin/sh
# Tests the app's runtime cache (trampoline pieces), its DiskArbitration
# eject handling, and the Eject button's release-then-eject, without launching
# Pharos: macos/test/Harness.swift is built with macos/LibraryVolume.swift and
# macos/RuntimeCache.swift.
#
#   macos/test/swift-harness.sh [PORT]
#
# It builds Pharos.app into a temporary directory (never launched), creates
# and attaches its own disk image (/Volumes/PharosTest-*), serves a test
# library on PORT (default 18768), and uses a temporary support directory in
# place of ~/Library/Application Support/Pharos. The one app it opens through
# LaunchServices is a background-only stub bundle that records its arguments.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
PORT=${1:-18768}
case "$PORT" in 8765|8766) echo "Use a port other than 8765/8766." >&2; exit 2 ;; esac
WORK=$(mktemp -d)
# Every pharos run below (and the service the harness starts) writes
# per-Mac files here, never in ~/Library/Application Support/Pharos.
export PHAROS_SUPPORT_DIR="$WORK/service-support"
NAME="PharosTest-$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')"
DEV=
FAKE=
cleanup() {
    if [ -n "$DEV" ]; then hdiutil detach -force "$DEV" >/dev/null 2>&1 || true; fi
    if [ -n "$FAKE" ]; then { kill "$FAKE"; wait "$FAKE"; } 2>/dev/null || true; fi
    rm -rf "$WORK"
}
trap cleanup EXIT
cdhash() { codesign -dvvv "$1" 2>&1 | awk -F= '/^CDHash=/{print $2}'; }
sign() { codesign --force --timestamp=none --sign - --options runtime "$@" 2>/dev/null; }

echo "Building Pharos.app (not launched)…"
"$ROOT/macos/build-app.sh" "$WORK/real" >"$WORK/build.log" 2>&1 || { cat "$WORK/build.log" >&2; exit 1; }
REAL="$WORK/real/Pharos.app"
# The same build with a different nested pharos.
mkdir -p "$WORK/real-v2" && cp -R "$REAL" "$WORK/real-v2/"
sign --identifier local.pharos.service.v2 "$WORK/real-v2/Pharos.app/Contents/MacOS/pharos"
sign --identifier local.pharos "$WORK/real-v2/Pharos.app"

# Background-only stubs: with --library DIR they write their path and
# arguments to DIR/stub-launched.txt; with --sleep N they sleep. The app
# executable (Swift) also takes --stay, to run until asked to quit, and
# --stubborn, to refuse to.
cat >"$WORK/stub.swift" <<'EOF'
import AppKit
let arguments = CommandLine.arguments
func value(after flag: String) -> String? {
    guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else { return nil }
    return arguments[index + 1]
}
if let seconds = value(after: "--sleep") { sleep(UInt32(seconds) ?? 0); exit(0) }
if let library = value(after: "--library") {
    var executable = Bundle.main.executablePath ?? arguments[0]
    if let resolved = realpath(executable, nil) { executable = String(cString: resolved); free(resolved) }
    try! (([executable] + arguments.dropFirst()).joined(separator: "\n") + "\n").write(toFile: library + "/stub-launched.txt", atomically: false, encoding: .utf8)
    let environment = ProcessInfo.processInfo.environment
    try! "PHAROS_SUPPORT_DIR=\(environment["PHAROS_SUPPORT_DIR"] ?? "")\nHOME=\(environment["HOME"] ?? "")\n".write(toFile: library + "/stub-env.txt", atomically: false, encoding: .utf8)
}
final class Delegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        arguments.contains("--stubborn") ? .terminateCancel : .terminateNow
    }
}
if arguments.contains("--stay") || arguments.contains("--stubborn") {
    let delegate = Delegate()
    NSApplication.shared.delegate = delegate
    NSApplication.shared.run()
}
EOF
swiftc -o "$WORK/stub-app" "$WORK/stub.swift"
cat >"$WORK/stub.c" <<'EOF'
#include <mach-o/dyld.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
int main(int argc, char **argv) {
    for (int i = 1; i + 1 < argc; i++) {
        if (!strcmp(argv[i], "--sleep")) { sleep(atoi(argv[i + 1])); return 0; }
        if (!strcmp(argv[i], "--library")) {
            char path[4096], exe[4096];
            uint32_t size = sizeof exe;
            snprintf(path, sizeof path, "%s/stub-launched.txt", argv[i + 1]);
            FILE *f = fopen(path, "w");
            if (!f || _NSGetExecutablePath(exe, &size)) return 1;
            fprintf(f, "%s\n", exe);
            for (int j = 1; j < argc; j++) fprintf(f, "%s\n", argv[j]);
            return fclose(f);
        }
    }
    return 0;
}
EOF
cc -O2 -o "$WORK/stub-bin" "$WORK/stub.c"
stub_bundle() {
    app="$WORK/$1/Pharos.app"
    mkdir -p "$app/Contents/MacOS"
    cp "$WORK/stub-app" "$app/Contents/MacOS/PharosApp"
    cp "$WORK/stub-bin" "$app/Contents/MacOS/pharos"
    cat >"$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>PharosApp</string>
<key>CFBundleIdentifier</key><string>local.pharos-test.stub</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSBackgroundOnly</key><true/>
</dict></plist>
EOF
    sign --identifier "local.pharos-test.stub.$1.service" "$app/Contents/MacOS/pharos"
    sign --identifier local.pharos-test.stub "$app"
}
stub_bundle stub
stub_bundle stub-v2

echo "Building the harness…"
swiftc -parse-as-library -framework DiskArbitration -framework Security \
    "$ROOT/macos/test/Harness.swift" "$ROOT/macos/LibraryVolume.swift" "$ROOT/macos/RuntimeCache.swift" \
    -o "$WORK/harness"

hdiutil create -quiet -size 300m -fs APFS -volname "$NAME" "$WORK/$NAME.dmg"
attached=$(hdiutil attach -nobrowse "$WORK/$NAME.dmg")
DEV=$(echo "$attached" | awk 'NR == 1 {print $1}')
MOUNT=$(echo "$attached" | awk -F'\t' '/\/Volumes\//{print $NF}')
case "$MOUNT" in "/Volumes/$NAME"*) ;; *) echo "unexpected mount point '$MOUNT'" >&2; exit 1 ;; esac
CLI="$REAL/Contents/MacOS/pharos"
"$CLI" init-library "$MOUNT/Pharos" >/dev/null
CONFIG="$MOUNT/Pharos/library.toml"
sed -i '' "s/^port = 8766$/port = $PORT/" "$CONFIG"
TOKEN=$(sed -n 's/^api_token = "\(.*\)"$/\1/p' "$CONFIG")
UUID=$(diskutil info -plist "$MOUNT" | plutil -extract VolumeUUID raw -)
VOLUME_DEV=$(diskutil info -plist "$MOUNT" | plutil -extract DeviceIdentifier raw -)

echo "== runtime cache"
status=0
"$WORK/harness" runtime "$WORK/support" "$REAL" "$WORK/real-v2/Pharos.app" "$(cdhash "$REAL")" \
    "$WORK/stub/Pharos.app" "$WORK/stub-v2/Pharos.app" "$MOUNT/Pharos" "$UUID" || status=1
echo "== service"
# A service still finishing a write answers a release with 503.
python3 - "$WORK/stopping-port" <<'EOF' &
import http.server, os, sys
class Stopping(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = b'{"released":false,"error":"Pharos is finishing a write; try again in a moment"}'
        self.send_response(503)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args): pass
server = http.server.HTTPServer(("127.0.0.1", 0), Stopping)
open(sys.argv[1] + ".tmp", "w").write(str(server.server_address[1]))
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
server.serve_forever()
EOF
FAKE=$!
tries=0
until [ -s "$WORK/stopping-port" ]; do tries=$((tries + 1)); [ "$tries" -lt 100 ] || { echo "fake service did not start" >&2; exit 1; }; sleep 0.1; done
"$WORK/harness" service "http://127.0.0.1:$(cat "$WORK/stopping-port")/" || status=1
echo "== library volume"
"$WORK/harness" volume "$UUID" "$MOUNT" "$VOLUME_DEV" "$CLI" "$CONFIG" "$PORT" "$TOKEN" "$DEV" || status=1
echo "== eject button"
# The volume tests end by force-detaching the image; attach it again.
hdiutil detach -force "$DEV" >/dev/null 2>&1 || true
attached=$(hdiutil attach -nobrowse "$WORK/$NAME.dmg")
DEV=$(echo "$attached" | awk 'NR == 1 {print $1}')
MOUNT=$(echo "$attached" | awk -F'\t' '/\/Volumes\//{print $NF}')
case "$MOUNT" in "/Volumes/$NAME"*) ;; *) echo "unexpected mount point '$MOUNT'" >&2; exit 1 ;; esac
"$WORK/harness" eject "$UUID" "$MOUNT" "$CLI" "$MOUNT/Pharos/library.toml" "$PORT" "$TOKEN" || status=1
exit $status
