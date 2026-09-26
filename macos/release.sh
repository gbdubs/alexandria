#!/bin/sh
# Build and publish a universal Pharos.app from origin/master.
# Usage: macos/release.sh 0.3.0
#        PHAROS_CODESIGN_IDENTITY='Developer ID Application: ...' \
#        PHAROS_NOTARY_PROFILE=pharos macos/release.sh 0.3.0 --developer-id
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
REPO=gbdubs/alexandria
VERSION=${1:-}
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || ! printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "Usage: $0 major.minor.patch [--developer-id]" >&2
    exit 2
fi
case "${2:-}" in
    ''|--adhoc) MODE=adhoc ;;
    --developer-id) MODE=developer-id ;;
    *) echo "Usage: $0 major.minor.patch [--developer-id]" >&2; exit 2 ;;
esac
TAG="v$VERSION"
IDENTITY=${PHAROS_CODESIGN_IDENTITY:-}
PROFILE=${PHAROS_NOTARY_PROFILE:-}
if [ "$MODE" = developer-id ] && { [ -z "$IDENTITY" ] || [ -z "$PROFILE" ]; }; then
    echo "A Developer ID Application signing identity and notarytool keychain profile are required." >&2
    echo "Omit --developer-id to publish an ad hoc signed release instead." >&2
    exit 2
fi
for command in git gh go swiftc lipo npm xcrun codesign ditto shasum spctl awk; do
    command -v "$command" >/dev/null 2>&1 || { echo "$command is required." >&2; exit 2; }
done
case "$(git remote get-url origin)" in
    https://github.com/$REPO|https://github.com/$REPO.git|git@github.com:$REPO|git@github.com:$REPO.git) ;;
    *) echo "origin must point to github.com/$REPO." >&2; exit 2 ;;
esac
gh auth status -h github.com >/dev/null 2>&1 || { echo "Sign in with gh auth login before releasing." >&2; exit 2; }
if [ -n "$(git status --porcelain)" ]; then
    echo "The checkout must be clean before making a release." >&2
    exit 2
fi
git fetch origin master --tags
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/master)" ]; then
    echo "Check out origin/master before releasing; the artifact and tag must contain the same reviewed commit." >&2
    exit 2
fi
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
    echo "$TAG already exists; release versions are immutable." >&2
    exit 2
fi
if gh release view "$TAG" -R "$REPO" >/dev/null 2>&1; then
    echo "A GitHub release for $TAG already exists." >&2
    exit 2
fi
LATEST=$(gh release view -R "$REPO" --json tagName --jq .tagName 2>/dev/null || true)
if printf '%s\n' "$LATEST" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    if ! awk -v candidate="$VERSION" -v current="${LATEST#v}" 'BEGIN {
        split(candidate, n, "."); split(current, c, ".")
        for (i = 1; i <= 3; i++) {
            if (n[i] + 0 > c[i] + 0) exit 0
            if (n[i] + 0 < c[i] + 0) exit 1
        }
        exit 1
    }'; then
        echo "$TAG must be newer than the latest release ($LATEST)." >&2
        exit 2
    fi
fi

OUT="$ROOT/dist/releases/$TAG"
if [ -e "$OUT" ]; then
    echo "$OUT already exists; move it aside before retrying." >&2
    exit 2
fi
mkdir -p "$OUT/stage"
if [ "$MODE" = developer-id ]; then
    PHAROS_VERSION="$VERSION" PHAROS_UNIVERSAL=1 PHAROS_HARDENED_RUNTIME=1 \
        PHAROS_CODESIGN_TIMESTAMP=1 "$ROOT/macos/build-app.sh" "$OUT/stage"
else
    PHAROS_CODESIGN_IDENTITY= PHAROS_VERSION="$VERSION" PHAROS_UNIVERSAL=1 \
        PHAROS_HARDENED_RUNTIME=1 PHAROS_CODESIGN_TIMESTAMP=0 \
        "$ROOT/macos/build-app.sh" "$OUT/stage"
fi
APP="$OUT/stage/Pharos.app"
if [ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$APP/Contents/Info.plist")" != "$VERSION" ]; then
    echo "The built bundle has the wrong version." >&2
    exit 1
fi
if [ "$MODE" = developer-id ]; then
    if ! codesign -dvvv "$APP" 2>&1 | grep -q 'Authority=Developer ID Application:'; then
        echo "The app is not signed with a Developer ID Application certificate." >&2
        exit 1
    fi
    if ! codesign -dvvv "$APP" 2>&1 | grep -q '^Timestamp='; then
        echo "The app signature lacks a secure timestamp." >&2
        exit 1
    fi
elif ! codesign -dvvv "$APP" 2>&1 | grep -q '^Signature=adhoc'; then
    echo "The app is not ad hoc signed as requested." >&2
    exit 1
fi
if [ -n "$(git status --porcelain)" ]; then
    echo "The build changed tracked files; review them before publishing." >&2
    exit 1
fi

ARCHIVE_NAME="Pharos-$TAG-macos-universal.zip"
ARCHIVE="$OUT/$ARCHIVE_NAME"
if [ "$MODE" = developer-id ]; then
    ditto -c -k "$OUT/stage" "$ARCHIVE"
    xcrun notarytool submit "$ARCHIVE" --keychain-profile "$PROFILE" --wait
    xcrun stapler staple "$APP"
    xcrun stapler validate "$APP"
    spctl -a -vvv -t exec "$APP"
    # Stapling changes the bundle, so package the final bytes.
    rm "$ARCHIVE"
fi
codesign --verify --strict --deep --verbose=2 "$APP"
ditto -c -k "$OUT/stage" "$ARCHIVE"
(cd "$OUT" && shasum -a 256 "$ARCHIVE_NAME" > "$ARCHIVE_NAME.sha256")

git tag -a "$TAG" -m "Pharos $VERSION"
git push origin "refs/tags/$TAG"
if [ "$MODE" = adhoc ]; then
    gh release create "$TAG" "$ARCHIVE" "$ARCHIVE.sha256" -R "$REPO" \
        --verify-tag --draft --title "Pharos $VERSION" --generate-notes \
        --notes 'This app is ad hoc signed and is not notarized. On first launch, macOS may block it. After attempting to open it, use System Settings → Privacy & Security → Open Anyway if you trust this download.'
else
    gh release create "$TAG" "$ARCHIVE" "$ARCHIVE.sha256" -R "$REPO" \
        --verify-tag --draft --title "Pharos $VERSION" --generate-notes
fi
gh release edit "$TAG" -R "$REPO" --draft=false
rm -rf "$OUT/stage"
gh release view "$TAG" -R "$REPO" --json url --jq .url
