#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CONFIG_DIR="${AIWA_CONFIG_DIR:-${HOME}/Library/Application Support/AI Work Archive}"
CONFIG="$CONFIG_DIR/archive.toml"
APP="$ROOT/dist/Pharos.app"
CLI="$APP/Contents/MacOS/alexandria"
# Builds before the Pharos rename produced this bundle; its service may still be running.
LEGACY_CLI="$ROOT/dist/Alexandria.app/Contents/MacOS/alexandria"

case "${1:-}" in
    "") OPEN_APP=1 ;;
    --no-open) OPEN_APP=0 ;;
    *) echo "Usage: $0 [--no-open]" >&2; exit 2 ;;
esac

"$ROOT/macos/build-app.sh"

mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG" ]; then
    "$CLI" init "$CONFIG"
fi

# Finder-launched apps do not inherit the shell's PATH. Point the wrapper at
# the Go service embedded in this build without rewriting unrelated TOML.
"$CLI" config-executable "$CONFIG" "$CLI"

if [ "$OPEN_APP" -eq 1 ]; then
    # `open` normally focuses an existing process. That leaves a wrapper which
    # started before first-run initialization stuck on its original error, and
    # also leaves a rebuilt executable running old code. Restart this app cleanly.
    if pgrep -x AIWorkArchive >/dev/null 2>&1; then
        osascript -e 'tell application id "local.ai-work-archive" to quit' >/dev/null 2>&1 || true
        attempt=0
        while pgrep -x AIWorkArchive >/dev/null 2>&1 && [ "$attempt" -lt 20 ]; do
            sleep 0.1
            attempt=$((attempt + 1))
        done
        if pgrep -x AIWorkArchive >/dev/null 2>&1; then
            pkill -TERM -x AIWorkArchive
            attempt=0
            while pgrep -x AIWorkArchive >/dev/null 2>&1 && [ "$attempt" -lt 20 ]; do
                sleep 0.1
                attempt=$((attempt + 1))
            done
        fi
        if pgrep -x AIWorkArchive >/dev/null 2>&1; then
            echo "Could not stop the existing Pharos app." >&2
            exit 1
        fi
    fi
    # A force-quit or an older wrapper can leave its child service behind.
    # Stop only a service launched by this workspace with this exact config.
    ps -axo pid=,command= | while read -r pid command; do
        if [ "$command" = "$CLI --config $CONFIG serve" ] || [ "$command" = "$LEGACY_CLI --config $CONFIG serve" ]; then
            kill -TERM "$pid"
        fi
    done
    echo "Launching $APP"
    open "$APP"
else
    echo "Ready to launch: $APP"
fi
