#!/bin/sh
# Installs Pharos as a portable library: builds Pharos.app into DIR and, unless
# DIR already holds a library, creates library.toml and its catalog beside it.
# An existing library's configuration and catalog are left untouched.
#
# With --adopt CONFIG the new library starts from an existing per-user install
# instead of an empty catalog: see "Moving an existing install onto a drive" in
# docs/configuration.md. The install itself is only read.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

usage() {
    echo "Usage: $0 LIBRARY_DIR [--adopt CONFIG]   (for example /Volumes/euclid/Pharos)" >&2
    exit 2
}
ADOPTING=0
ADOPT=
case "$#" in
    1) ;;
    3) [ "$2" = --adopt ] || usage; ADOPTING=1; ADOPT=$3 ;;
    *) usage ;;
esac
# Everything is checked before anything is created.
# A missing parent is usually an unmounted drive; never recreate its mount
# point on the startup disk.
PARENT=$(dirname -- "$1")
if [ ! -d "$PARENT" ]; then
    echo "$PARENT does not exist; is its drive mounted?" >&2
    exit 2
fi
DIR="$(CDPATH= cd -- "$PARENT" && pwd -P)/$(basename -- "$1")"
if [ -e "$DIR" ] && [ ! -d "$DIR" ]; then
    echo "$DIR is not a directory." >&2
    exit 2
fi
[ -d "$DIR" ] && DIR=$(CDPATH= cd -- "$DIR" && pwd -P)
APP="$DIR/Pharos.app"
CLI="$APP/Contents/MacOS/pharos"

# launch.sh rebuilds and opens dist/Pharos.app for the per-user configuration;
# a library.toml beside it would silently take that app over.
if [ "$DIR" = "$(CDPATH= cd -- "$ROOT" && pwd -P)/dist" ]; then
    echo "launch.sh builds into $DIR; choose another directory for a library." >&2
    exit 2
fi

if [ "$ADOPTING" -eq 1 ]; then
    if [ -z "$ADOPT" ] || [ ! -f "$ADOPT" ]; then
        echo "--adopt needs the archive.toml of the install to adopt; there is none at '$ADOPT'." >&2
        exit 2
    fi
    if [ -e "$DIR/library.toml" ]; then
        echo "$DIR already holds a library; --adopt only creates a new one." >&2
        exit 2
    fi
fi

# processes_under DIR lists processes running an executable under DIR.
processes_under() {
    ps -axo command= | while read -r command; do
        case "$command" in
            "$1"/*) echo "$command" ;;
        esac
    done
}
# Replacing a running executable can kill it mid-write to the catalog. That
# only concerns Pharos running from the drive itself (run in place, or an MCP
# server started while this Mac had no local copy); copies in this Mac's
# runtime cache run from the local disk and are not touched.
running=$(processes_under "$APP/Contents/MacOS")
if [ -n "$running" ]; then
    echo "Pharos processes are running from $APP itself" >&2
    echo "(the app run in place, its service, or MCP servers started by agent clients)." >&2
    echo "Quit them before reinstalling:" >&2
    echo "$running" >&2
    exit 1
fi
RUNTIME="${PHAROS_SUPPORT_DIR:-$HOME/Library/Application Support/Pharos}/runtime"
local_copies=$(processes_under "$RUNTIME")

had_dir=0
[ -d "$DIR" ] && had_dir=1
had_app=0
[ -d "$APP" ] && had_app=1
mkdir -p "$DIR"
"$ROOT/macos/build-app.sh" "$DIR"
# The one step for each Mac this drive is plugged into.
cp "$ROOT/macos/add-this-mac.command" "$DIR/Add This Mac.command"
chmod +x "$DIR/Add This Mac.command"

if [ "$ADOPTING" -eq 1 ]; then
    # The adoption prints its own summary and next steps.
    if ! "$CLI" init-library "$DIR" --adopt "$ADOPT"; then
        # Leave no app beside a directory that holds no library: opened, it
        # would fall back to the per-user install.
        [ "$had_app" -eq 1 ] || rm -rf "$APP"
        [ "$had_dir" -eq 1 ] || rmdir "$DIR" 2>/dev/null || true
        echo "Adoption failed; $DIR holds no library. Fix the error above and rerun this script." >&2
        exit 1
    fi
    exit 0
fi

if [ -f "$DIR/library.toml" ]; then
    echo "Kept the existing library configuration $DIR/library.toml."
else
    "$CLI" init-library "$DIR"
fi

if [ -n "$local_copies" ]; then
    cat <<EOF

Note: Pharos is running from this Mac's local copy of the previous build in
  $RUNTIME
which this install leaves alone. Opening the new Pharos.app asks that copy to
quit and then opens the new build. MCP servers already running keep the
previous build until their agent clients restart them.
EOF
fi

cat <<EOF

Pharos library: $DIR
On each Mac, double-click "Add This Mac.command" beside the app (or run
  "$CLI" add-this-mac
in Terminal). It finds this Mac's Claude Code, Codex, Conductor, and TL1
history, asks before adding it, captures it onto the drive, and indexes it.
Then open $APP to browse the library.
EOF
