# Releases and app updates

## Publish a release from a Mac

Pharos releases are built locally. GitHub Actions is not involved. A release
contains a universal macOS app (arm64 and x86_64) in
`Pharos-vX.Y.Z-macos-universal.zip`, plus a matching `.sha256` file. The tag,
GitHub release, and both bundle version fields use the same `X.Y.Z` version.
Sign in to `gh` with permission to publish releases. Run the script only after
the release commit has been merged into `master`, from a clean checkout at
`origin/master`.

By default, publish an ad hoc signed release:

```sh
macos/release.sh 0.3.0
```

GitHub will host it, but it cannot be notarized. macOS may block the downloaded
app on first launch. A user who trusts the download can try opening it, then
choose **System Settings → Privacy & Security → Open Anyway**. See
[Apple's instructions](https://support.apple.com/guide/mac-help/open-a-mac-app-from-an-unknown-developer-mh40616/mac).
Ad hoc code signatures also change when the app changes, so macOS may ask again
for privacy permissions after an update.

For a notarized release, set up a **Developer ID Application** certificate in
the Mac's keychain and a `notarytool` keychain profile. Apple requires Developer
ID signing, the hardened runtime, and a secure timestamp for notarization. See
Apple's [Developer ID](https://developer.apple.com/developer-id/) and
[notarization](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)
guides. Run `xcrun notarytool store-credentials pharos` to create the keychain
profile.

For a Developer ID release:

```sh
export PHAROS_CODESIGN_IDENTITY='Developer ID Application: Your Name (TEAMID)'
export PHAROS_NOTARY_PROFILE=pharos
macos/release.sh 0.3.0 --developer-id
```

The script fetches `origin/master`, refuses a dirty or different checkout and
an existing tag, builds and verifies the app, then creates an annotated tag
and publishes a GitHub release. The Developer ID path also notarizes and
staples the app. It keeps the final archive and checksum under
`dist/releases/vX.Y.Z/`. If publication fails after the tag is pushed, inspect
the tag and draft release before retrying; never replace the asset of a
published version.

To create a portable library from the downloaded archive, extract it, put
`Pharos.app` in the library folder, and initialize that folder with the
embedded CLI:

```sh
"/Volumes/YOUR-DRIVE/Pharos/Pharos.app/Contents/MacOS/alexandria" init-library "/Volumes/YOUR-DRIVE/Pharos"
open "/Volumes/YOUR-DRIVE/Pharos/Pharos.app"
```

For an existing library, quit Pharos and any MCP process running directly from
its old bundle before replacing `Pharos.app`; leave `library.toml` and the
catalog in place. The source checkout's `macos/install-library.sh` remains
useful for development builds and for adoption from a per-user install.

## In-app update design

The durable app in a portable library is the `Pharos.app` beside
`library.toml`. Opening it launches a **separate local runtime copy** so that
the library's drive can be ejected. An updater running in that local copy must
replace the durable app on the drive, then use the existing trampoline to
launch the new local copy. Updating only the running copy would disappear at
the next launch. This makes Sparkle's default replacement of its host bundle
unsuitable without a custom installer, despite Sparkle being a good fit for an
ordinary Mac app.

The recommended first implementation is a small native Swift updater in the
wrapper, with these steps:

1. On a user-initiated **Check for Updates** action, read GitHub's latest
   published release for `gbdubs/alexandria`. Offer periodic checks only after
   the user opts in, since this contacts GitHub. Compare numeric version
   components from the `vX.Y.Z` tag with `CFBundleVersion`; ignore drafts,
   prereleases, and versions at or below the running one.
2. Before enabling automatic installation, introduce a separate Ed25519
   release-signing key, embed its public key in the app, and update the local
   release script to sign each archive. Keep the private key off GitHub. A
   `.sha256` file on the same release detects damage but cannot authenticate
   an artifact if the GitHub release is replaced. An ad hoc code signature
   cannot authenticate a new build because its designated requirement is its
   exact code hash. An existing app without the pinned public key would need
   one manual update to an updater-enabled version.
3. Download only the exact `Pharos-vX.Y.Z-macos-universal.zip` asset into a
   private staging directory. Verify its Ed25519 signature **before**
   extraction, then extract with path traversal protection. Validate the
   extracted bundle's internal code signature, expected bundle ID, both
   architecture slices, and bundle versions. For Developer ID releases, also
   validate the signing identity and notarization. Reject older builds.
4. Find the durable source bundle: the app beside `library.toml` in library
   mode, or the installed app in per-user mode. Never modify a source checkout
   or the runtime cache directly. If the drive is missing, read-only, or the
   source app is currently executing, keep the staged update and explain why
   installation is deferred. Stage a verified copy on the same volume as the
   durable app, replace it while retaining the prior bundle for recovery, and
   restart through the existing trampoline. Leave `library.toml`, catalog,
   captures, and user configuration alone.
5. Show version, release notes, download progress, install errors, and a
   **Restart to Update** choice in the native UI. The Go service should exit
   cleanly before relaunch. Existing MCP processes may keep the old binary
   until their clients restart them. Test interrupted downloads and installs,
   missing drives, unwritable folders, signature failures, and a restart with
   a catalog migration before enabling unattended installation.

The release archive and version scheme above provide the stable inputs for
this updater. The updater itself is not enabled yet. Ed25519 archive signing
and verification are required before enabling automatic installation, whether
or not the app has a Developer ID. With ad hoc builds, macOS may still require
the user to approve a downloaded update; test that behavior on a clean Mac
before promising unattended updates.
