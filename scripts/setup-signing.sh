#!/bin/bash
# setup-signing.sh — create the self-signed "Kahya Dev" code-signing identity (W0-04).
#
# Why: on Apple Silicon, Go's ad-hoc signature changes on every build, which breaks
# the Keychain ACL granted to kahyad (HANDOFF §7 ⚑). A stable self-signed identity
# signed via `codesign -s 'Kahya Dev'` keeps the designated requirement constant.
#
# Idempotent: exits 0 immediately if the identity already exists.
# Requires: user present for the keychain/sudo prompts (trust step touches the
# System keychain).
#
# FALLBACK if this script fights the keychain (e.g. SecKey/import errors):
#   Keychain Access → Certificate Assistant → Create a Certificate…
#   Name: "Kahya Dev", Identity Type: Self-Signed Root, Certificate Type: Code Signing.
#   That GUI path produces the same identity; then re-run `make install`.
set -euo pipefail

IDENTITY="Kahya Dev"
DAYS=3650
LOGIN_KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

if security find-identity -v -p codesigning | grep -q "$IDENTITY"; then
  echo "OK: '$IDENTITY' code-signing identity already present."
  exit 0
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

# Re-run after a failed trust step: cert already imported but not yet trusted.
# Reuse it — generating again would leave duplicate "Kahya Dev" certs behind.
if security find-certificate -c "$IDENTITY" "$LOGIN_KEYCHAIN" >/dev/null 2>&1; then
  echo "Cert already imported; retrying the trust step only."
  security find-certificate -c "$IDENTITY" -p "$LOGIN_KEYCHAIN" > "$WORKDIR/kahya-dev.crt"
  sudo security add-trusted-cert -d -r trustRoot -p codeSign \
    -k /Library/Keychains/System.keychain "$WORKDIR/kahya-dev.crt"
  # Let codesign use the private key without a GUI prompt on every build
  # (prompts for your login keychain password once).
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -l "$IDENTITY" \
    "$LOGIN_KEYCHAIN" >/dev/null
  security find-identity -v -p codesigning | grep "$IDENTITY" \
    && echo "OK: '$IDENTITY' identity trusted."
  exit 0
fi

# Throwaway export password: the .p12 lives only inside this script's tmpdir.
P12PASS=$(openssl rand -hex 16)

cat > "$WORKDIR/ext.cnf" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = v3_codesign
prompt = no
[dn]
CN = Kahya Dev
[v3_codesign]
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
basicConstraints = critical,CA:false
EOF

openssl req -x509 -newkey rsa:2048 -nodes -days "$DAYS" \
  -keyout "$WORKDIR/kahya-dev.key" -out "$WORKDIR/kahya-dev.crt" \
  -config "$WORKDIR/ext.cnf"

openssl pkcs12 -export -legacy \
  -inkey "$WORKDIR/kahya-dev.key" -in "$WORKDIR/kahya-dev.crt" \
  -name "$IDENTITY" -out "$WORKDIR/kahya-dev.p12" -passout "pass:$P12PASS"

security import "$WORKDIR/kahya-dev.p12" \
  -k "$LOGIN_KEYCHAIN" -P "$P12PASS" -T /usr/bin/codesign

# Trust the cert for code signing system-wide so `codesign` accepts the identity.
# USER: this triggers a sudo prompt (admin password).
sudo security add-trusted-cert -d -r trustRoot -p codeSign \
  -k /Library/Keychains/System.keychain "$WORKDIR/kahya-dev.crt"

# Let codesign use the private key without a GUI prompt on every build
# (prompts for your login keychain password once).
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -l "$IDENTITY" \
  "$LOGIN_KEYCHAIN" >/dev/null

security find-identity -v -p codesigning | grep "$IDENTITY" \
  && echo "OK: '$IDENTITY' identity created and trusted."
