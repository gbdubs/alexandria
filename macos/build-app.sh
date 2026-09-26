#!/bin/sh
# Build Pharos.app into OUTDIR (default: dist/) and code-sign it.
#
# Environment:
#   PHAROS_CODESIGN_IDENTITY  Keychain identity to sign with (name or SHA-1).
#                             Unset: ad-hoc signing, whose identity changes
#                             with the code. See macos/create-signing-identity.sh.
#   PHAROS_HARDENED_RUNTIME   Set to 0 to sign without the hardened runtime
#                             (e.g. to attach a debugger). Default: 1.
#   PHAROS_UNIVERSAL          Set to 1 to build arm64 + x86_64. Default: native.
#   PHAROS_VERSION            Numeric major.minor.patch bundle version. Default: 0.2.0.
#   PHAROS_CODESIGN_TIMESTAMP Set to 1 for a secure signing timestamp (required
#                             for Developer ID notarization). Default: 0.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=${1:-"$ROOT/dist"}
APP="$OUT/Pharos.app"
BUNDLE_ID=local.pharos
SERVICE_ID=local.pharos.service
MACOS_MIN=14.0
VERSION=${PHAROS_VERSION:-0.2.0}
case "$VERSION" in
    *[!0-9.]*|.*|*.|*..*) echo "PHAROS_VERSION must be major.minor.patch (numeric)." >&2; exit 2 ;;
esac
if ! printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "PHAROS_VERSION must be major.minor.patch (numeric)." >&2
    exit 2
fi
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
# A local rebuild can reuse dist/Pharos.app from before the CLI and wrapper
# were renamed. Keep the bundle free of obsolete executables.
rm -f "$APP/Contents/MacOS/alexandria" "$APP/Contents/MacOS/AIWorkArchive"
mkdir -p "$ROOT/internal/archive/assets"
mkdir -p "$ROOT/internal/archive/assets/query-schemas"
# The embedded schema and UI are the source of truth.
for name in schema.sql ui.py; do
    if [ ! -f "$ROOT/internal/archive/assets/$name" ]; then
        echo "internal/archive/assets/$name is missing." >&2
        exit 1
    fi
done
cp "$ROOT/schemas/"*.schema.json "$ROOT/internal/archive/assets/query-schemas/"
mkdir -p "$ROOT/internal/archive/assets/pricing"
cp "$ROOT/pricing/cost_changes.json" "$ROOT/internal/archive/assets/pricing/cost_changes.json"
mkdir -p "$ROOT/internal/archive/assets/carbon"
cp "$ROOT/carbon/co2_factors.json" "$ROOT/internal/archive/assets/carbon/co2_factors.json"
if command -v npm >/dev/null 2>&1; then
    # Always reinstall from the lockfile: an existing node_modules may have
    # drifted from package-lock.json or been modified, and `npm ci` checks
    # every package against its recorded integrity hash.
    (cd "$ROOT/web" && npm ci --no-audit --no-fund)
    (cd "$ROOT/web" && npm run build)
elif [ ! -f "$ROOT/internal/archive/assets/query-tables.js" ] || [ ! -f "$ROOT/internal/archive/assets/query-tables.css" ]; then
    echo "Node.js/npm is required to build the query-table frontend." >&2
    exit 1
else
    echo "warning: npm not found; embedding the checked-in query-table bundle unverified." >&2
    echo "warning: run \`(cd web && npm ci && npm run check-bundle)\` to confirm it matches web/src." >&2
fi

if [ "${PHAROS_UNIVERSAL:-0}" = 1 ]; then
    GOARCHES="arm64 amd64"
else
    GOARCHES=$(go env GOARCH)
fi
# Cross-compiling disables cgo by default; keep every slice built the same way.
CGO=$(go env CGO_ENABLED)
ARCHS=
for goarch in $GOARCHES; do
    case "$goarch" in
        arm64) arch=arm64 ;;
        amd64) arch=x86_64 ;;
        *) echo "Unsupported architecture for macOS: $goarch" >&2; exit 1 ;;
    esac
    ARCHS="$ARCHS $arch"
    echo "Building the Go service ($arch)…"
    (cd "$ROOT" && GOOS=darwin GOARCH="$goarch" CGO_ENABLED="$CGO" go build -o "$WORK/pharos-$arch" ./cmd/pharos)
    echo "Building the Swift wrapper ($arch)…"
    swiftc -parse-as-library -target "$arch-apple-macos$MACOS_MIN" -framework SwiftUI -framework WebKit \
        -framework DiskArbitration -framework Security \
        "$ROOT/macos/PharosApp.swift" "$ROOT/macos/LibraryVolume.swift" "$ROOT/macos/RuntimeCache.swift" \
        -o "$WORK/PharosApp-$arch"
done
for name in pharos PharosApp; do
    target="$APP/Contents/MacOS/$name"
    # Replace rather than overwrite in place, so a running copy keeps its
    # mapped executable.
    rm -f "$target"
    if [ "${PHAROS_UNIVERSAL:-0}" = 1 ]; then
        lipo -create "$WORK/$name"-* -output "$target"
    else
        cp "$WORK/$name-$arch" "$target"
    fi
    if ! lipo "$target" -verify_arch $ARCHS; then
        echo "$target does not contain:$ARCHS" >&2
        exit 1
    fi
    lipo -info "$target"
done

# Regenerate with macos/make-icon.sh after editing macos/AppIcon.svg.
cp "$ROOT/macos/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>PharosApp</string>
<key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
<key>CFBundleName</key><string>Pharos</string>
<key>CFBundleDisplayName</key><string>Pharos</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$VERSION</string>
<key>CFBundleVersion</key><string>$VERSION</string>
<key>LSMinimumSystemVersion</key><string>$MACOS_MIN</string>
</dict></plist>
PLIST

# macOS privacy grants (TCC), such as access to files on a removable volume,
# are keyed on the app's designated requirement. With a keychain identity that
# is the bundle identifier plus the certificate, so grants survive rebuilds; an
# ad-hoc signature pins the exact build (its cdhash). The service is launched
# by the wrapper, so macOS normally attributes its file access to Pharos.app.
IDENTITY=${PHAROS_CODESIGN_IDENTITY:-}
if [ -z "$IDENTITY" ]; then
    IDENTITY=-
    echo "Note: PHAROS_CODESIGN_IDENTITY is unset, so Pharos is ad-hoc signed; that signature changes whenever the code does, so macOS may ask for privacy permissions again after a rebuild."
fi
# Positional parameters double as the codesign argument list.
set -- --force --sign "$IDENTITY"
case "${PHAROS_CODESIGN_TIMESTAMP:-0}" in
    0) set -- "$@" --timestamp=none ;;
    1) set -- "$@" --timestamp ;;
    *) echo "PHAROS_CODESIGN_TIMESTAMP must be 0 or 1." >&2; exit 2 ;;
esac
# The hardened runtime needs no entitlements here: WebKit's JIT runs in Apple's
# WebContent process, the Go service needs no JIT or unsigned memory, and
# launching it with Process() is unrestricted. It blocks DYLD_* injection and
# debugger attachment, making it harder for other local code to borrow Pharos's
# privacy grants.
if [ "${PHAROS_HARDENED_RUNTIME:-1}" != 0 ]; then
    set -- "$@" --options runtime
fi
# Sign nested code first; the bundle seal records its signature.
codesign "$@" --identifier "$SERVICE_ID" "$APP/Contents/MacOS/pharos"
codesign "$@" --identifier "$BUNDLE_ID" "$APP"
if ! codesign --verify --strict --deep --verbose=2 "$APP"; then
    echo "Code signature verification failed for $APP" >&2
    exit 1
fi
echo "Built $APP"
