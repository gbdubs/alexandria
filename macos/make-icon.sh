#!/bin/sh
# Regenerate macos/AppIcon.icns from macos/AppIcon.svg. The SVG uses filters
# and gradients that sips/qlmanage render poorly, so rasterise with Chrome.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
    --default-background-color=00000000 --window-size=1024,1024 \
    --screenshot="$WORK/icon_1024.png" "file://$ROOT/macos/AppIcon.svg" >/dev/null 2>&1
ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
    sips -z "$size" "$size" "$WORK/icon_1024.png" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
    double=$((size * 2))
    sips -z "$double" "$double" "$WORK/icon_1024.png" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$ROOT/macos/AppIcon.icns"
echo "Wrote $ROOT/macos/AppIcon.icns"
