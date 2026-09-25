#!/bin/sh
# Create a self-signed code-signing identity, "Pharos Local Code Signing", in
# your login keychain, for Macs without an Apple Development or Developer ID
# certificate. Then build with it:
#
#     macos/create-signing-identity.sh
#     PHAROS_CODESIGN_IDENTITY="Pharos Local Code Signing" macos/build-app.sh [OUTDIR]
#
# Why: macOS records privacy (TCC) decisions, such as "access files on a
# removable volume", against the app's designated requirement (DR). An ad-hoc
# signature's DR is its cdhash, which changes whenever the code changes, so a
# rebuilt Pharos can look like a new app. For a certificate that Apple did not
# issue, codesign's default DR is
#     identifier "local.ai-work-archive" and certificate leaf = H"<SHA-1 of the certificate>"
# which stays the same across rebuilds for as long as you sign with this
# certificate.
#
# Limitations (read before relying on it):
# - Apple documents DR-based tracking (TN3127) and recommends Apple-issued
#   identities; it does not document TCC's handling of self-signed identities.
#   Others report that grants persist with them, but this is not verified for
#   Pharos.
# - Not for distribution. Gatekeeper rejects the signature (spctl --assess
#   fails) and it cannot be notarized. Gatekeeper blocks such apps only when
#   they are quarantined (downloaded or AirDropped copies); a bundle built on a
#   Mac, or onto an external drive and launched from there, is not.
# - Only this Mac holds the private key. Other Macs can run builds signed here
#   without installing the certificate (signature checks treat the embedded
#   self-signed certificate as its own anchor), but a build signed on another
#   Mac with a different certificate has a different DR. To build on several
#   Macs, export this identity from Keychain Access (.p12) and import it on
#   each.
# - Grants are per Mac and per user: expect one prompt on each Mac, not one per
#   build. Nothing carries grants between Macs.
# - Deleting or replacing the certificate, including when it expires after ten
#   years, changes the DR and macOS asks again. Keep a .p12 backup to avoid
#   that.
# - macOS does not treat an untrusted self-signed certificate as a valid
#   signing identity (security find-identity -v omits it), so this script
#   trusts it for the Code Signing policy only, in your user trust settings.
#   macOS asks for your password to allow that, so run it from a logged-in GUI
#   session, not over SSH. TLS, email and other policies are unaffected.
# - The private key allows /usr/bin/codesign without a prompt, so any program
#   running as you can sign code that satisfies Pharos's DR and so inherit its
#   privacy grants. Protect this account as you would any signing key.
#
# This script changes your login keychain and trust settings. It refuses to
# replace an existing certificate with the same name.
set -eu
NAME="Pharos Local Code Signing"
DAYS=3650
# LibreSSL writes PKCS#12 files that `security import` accepts.
OPENSSL=/usr/bin/openssl

if [ "$(uname -s)" != Darwin ]; then
    echo "This script creates a macOS keychain identity; run it on macOS." >&2
    exit 1
fi
if [ "$(id -u)" -eq 0 ]; then
    echo "Run this as yourself, not root: the identity belongs in your login keychain." >&2
    exit 1
fi
if security find-identity -v -p codesigning | grep -F "\"$NAME\""; then
    echo "\"$NAME\" is already a valid code-signing identity; nothing to do."
    exit 0
fi
if security find-certificate -c "$NAME" >/dev/null 2>&1; then
    echo "A certificate named \"$NAME\" exists but is not a valid code-signing identity." >&2
    echo "Inspect it with: security find-identity -p codesigning" >&2
    echo "If it is merely untrusted, trust it for code signing:" >&2
    echo "    security find-certificate -c \"$NAME\" -p > pharos-signing.pem" >&2
    echo "    security add-trusted-cert -r trustRoot -p codeSign pharos-signing.pem" >&2
    echo "Otherwise delete it in Keychain Access and run this again (macOS will then" >&2
    echo "ask for Pharos's privacy permissions again)." >&2
    exit 1
fi

KEYCHAIN=$(security login-keychain | sed -e 's/^[[:space:]]*"//' -e 's/"[[:space:]]*$//')
umask 077
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cat > "$WORK/certificate.conf" <<EOF
[ req ]
distinguished_name = subject
prompt = no
[ subject ]
CN = $NAME
[ codesign ]
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
EOF
if ! "$OPENSSL" req -x509 -new -newkey rsa:2048 -nodes -sha256 -days "$DAYS" \
    -config "$WORK/certificate.conf" -extensions codesign \
    -keyout "$WORK/key.pem" -out "$WORK/certificate.pem" 2>"$WORK/openssl.log"; then
    cat "$WORK/openssl.log" >&2
    exit 1
fi

# A throwaway password: `security import` rejects PKCS#12 files without one.
# It protects only the temporary file, which is deleted on exit.
PASSWORD=$("$OPENSSL" rand -hex 16)
LEGACY=
case $("$OPENSSL" version) in
    # OpenSSL 3's default PKCS#12 encryption is not importable by `security`.
    "OpenSSL 3"*) LEGACY=-legacy ;;
esac
"$OPENSSL" pkcs12 -export $LEGACY -name "$NAME" \
    -inkey "$WORK/key.pem" -in "$WORK/certificate.pem" \
    -out "$WORK/identity.p12" -passout "pass:$PASSWORD"

echo "Importing \"$NAME\" into $KEYCHAIN…"
security import "$WORK/identity.p12" -k "$KEYCHAIN" -f pkcs12 -P "$PASSWORD" -T /usr/bin/codesign
echo "Trusting \"$NAME\" for code signing only; macOS will ask for your password…"
security add-trusted-cert -r trustRoot -p codeSign "$WORK/certificate.pem"

if ! security find-identity -v -p codesigning | grep -F "\"$NAME\""; then
    echo "\"$NAME\" was imported but is not a valid code-signing identity." >&2
    echo "Inspect it with: security find-identity -p codesigning" >&2
    exit 1
fi
echo "Created \"$NAME\". Build with:"
echo "    PHAROS_CODESIGN_IDENTITY=\"$NAME\" macos/build-app.sh [OUTDIR]"
echo "Back it up by exporting it from Keychain Access as a .p12 file."
