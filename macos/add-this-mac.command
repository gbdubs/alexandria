#!/bin/sh
# Double-click this in Finder (it opens in Terminal) to add this Mac's AI
# conversations to the Pharos library on this drive: it finds Claude Code,
# Codex, Conductor, and TL1 history, asks before adding it, copies it onto the
# drive, and indexes it. Run it again any time to bring the library up to date.
# macOS may first ask to let Terminal use files on a removable volume.
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
"$DIR/Pharos.app/Contents/MacOS/pharos" --config "$DIR/library.toml" add-this-mac "$@"
status=$?
echo
if [ "$status" -eq 0 ]; then
    echo "Finished. Press Return to close this window."
else
    echo "Stopped with an error (see above). Press Return to close this window."
fi
read -r _
exit "$status"
