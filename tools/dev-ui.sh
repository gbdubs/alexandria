#!/bin/sh
# Try this checkout's UI against the Pharos service already running for a
# library, without building or installing the app. The page and its assets
# come from internal/archive/assets; refresh the browser to load saved changes.
# API calls go to that service, so they act on the real library. Carbon API
# responses use this checkout's factors and sources for the local preview.
# Changes under web/src are rebuilt into query-tables.js as they are saved
# (skip with --no-watch).
#
# It opens the UI in the browser unless --no-open is given; a tab left open
# by an earlier run is replaced with a newly opened tab. Any dev UI already
# running from a checkout of this repository, including other worktrees, is
# stopped first.
#
# Usage: tools/dev-ui.sh [--no-watch] [--no-open] [--port N]
#
# Environment:
#   PHAROS_LIBRARY  Library directory holding library.toml.
#                   Default: /Volumes/euclid/Pharos
#   PHAROS_CONFIG   Configuration to use instead, e.g. a per-user archive.toml.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CONFIG=${PHAROS_CONFIG:-"${PHAROS_LIBRARY:-/Volumes/euclid/Pharos}/library.toml"}
if [ ! -f "$CONFIG" ]; then
    echo "$CONFIG does not exist. Is the drive mounted? Set PHAROS_LIBRARY or PHAROS_CONFIG." >&2
    exit 1
fi
WATCH=1
for arg do
    shift
    case "$arg" in
        --no-watch) WATCH=0 ;;
        *) set -- "$@" "$arg" ;;
    esac
done

# Earlier runs: this script, the dev server (`go run` and the binary it
# builds, both given `dev-ui --assets .../internal/archive/assets`), and the
# esbuild watcher writing into internal/archive/assets. Skip this script, the
# subshell running this function (both share its command line), and whatever
# launched it (e.g. `zsh -c "tools/dev-ui.sh"`).
earlier_runs() {
    ps -axo pid=,ppid=,command= | awk -v self="$$" '
        { parent[$1] = $2; line[$1] = $0 }
        END {
            for (pid = self; pid > 1; pid = parent[pid]) skip[pid] = 1
            for (pid in line) {
                if (skip[pid] || parent[pid] == self) continue
                if (line[pid] ~ / dev-ui --assets .*\/internal\/archive\/assets( |$)/ ||
                    line[pid] ~ /(^| |\/)tools\/dev-ui\.sh( |$)/ ||
                    line[pid] ~ /--outfile=\.\.\/internal\/archive\/assets\/query-tables\.js --watch=forever/) print pid
            }
        }'
}
PIDS=$(earlier_runs)
if [ -n "$PIDS" ]; then
    echo "Stopping the dev UI already running (pids $(echo $PIDS))."
    kill -TERM $PIDS 2>/dev/null || true
    attempt=0
    while [ -n "$(earlier_runs)" ] && [ "$attempt" -lt 30 ]; do
        sleep 0.1
        attempt=$((attempt + 1))
    done
    PIDS=$(earlier_runs)
    if [ -n "$PIDS" ]; then
        kill -KILL $PIDS 2>/dev/null || true
    fi
fi

cd "$ROOT"
if [ "$WATCH" -eq 1 ]; then
    if [ ! -d web/node_modules ]; then
        echo "web/node_modules is missing; run (cd web && npm ci), or pass --no-watch." >&2
        exit 1
    fi
    (cd web && exec npm run --silent watch) &
    WATCHER=$!
    trap 'kill "$WATCHER" 2>/dev/null || true' EXIT INT TERM
fi
go run ./cmd/alexandria --config "$CONFIG" dev-ui --assets "$ROOT/internal/archive/assets" "$@"
