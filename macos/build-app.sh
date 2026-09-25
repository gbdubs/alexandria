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
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=${1:-"$ROOT/dist"}
APP="$OUT/Pharos.app"
BUNDLE_ID=local.ai-work-archive
SERVICE_ID=local.ai-work-archive.service
MACOS_MIN=14.0
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
mkdir -p "$ROOT/internal/archive/assets"
mkdir -p "$ROOT/internal/archive/assets/query-schemas"
# The embedded schema and UI are the source of truth now that the Python
# reference in src/ is retired. While a checkout still has it, keep the two
# byte-for-byte aligned.
for name in schema.sql ui.py; do
    if [ -f "$ROOT/src/ai_work_archive/$name" ]; then
        cp "$ROOT/src/ai_work_archive/$name" "$ROOT/internal/archive/assets/$name"
    elif [ ! -f "$ROOT/internal/archive/assets/$name" ]; then
        echo "internal/archive/assets/$name is missing." >&2
        exit 1
    fi
done
cp "$ROOT/schemas/"*.schema.json "$ROOT/internal/archive/assets/query-schemas/"
mkdir -p "$ROOT/internal/archive/assets/pricing"
cp "$ROOT/pricing/cost_changes.json" "$ROOT/internal/archive/assets/pricing/cost_changes.json"
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
    (cd "$ROOT" && GOOS=darwin GOARCH="$goarch" CGO_ENABLED="$CGO" go build -o "$WORK/alexandria-$arch" ./cmd/alexandria)
    echo "Building the Swift wrapper ($arch)…"
    swiftc -parse-as-library -target "$arch-apple-macos$MACOS_MIN" -framework SwiftUI -framework WebKit \
        -framework DiskArbitration -framework Security \
        "$ROOT/macos/AIWorkArchiveApp.swift" "$ROOT/macos/LibraryVolume.swift" "$ROOT/macos/RuntimeCache.swift" \
        -o "$WORK/AIWorkArchive-$arch"
done
for name in alexandria AIWorkArchive; do
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
<key>CFBundleExecutable</key><string>AIWorkArchive</string>
<key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
<key>CFBundleName</key><string>Pharos</string>
<key>CFBundleDisplayName</key><string>Pharos</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.2.0</string>
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
set -- --force --timestamp=none --sign "$IDENTITY"
# The hardened runtime needs no entitlements here: WebKit's JIT runs in Apple's
# WebContent process, the Go service needs no JIT or unsigned memory, and
# launching it with Process() is unrestricted. It blocks DYLD_* injection and
# debugger attachment, making it harder for other local code to borrow Pharos's
# privacy grants.
if [ "${PHAROS_HARDENED_RUNTIME:-1}" != 0 ]; then
    set -- "$@" --options runtime
fi
# Sign nested code first; the bundle seal records its signature.
codesign "$@" --identifier "$SERVICE_ID" "$APP/Contents/MacOS/alexandria"
codesign "$@" --identifier "$BUNDLE_ID" "$APP"
if ! codesign --verify --strict --deep --verbose=2 "$APP"; then
    echo "Code signature verification failed for $APP" >&2
    exit 1
fi
echo "Built $APP"
