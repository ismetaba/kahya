#!/usr/bin/env bash
# W0-04 — idempotent creation of the self-signed "Kahya Dev" code-signing identity.
#
# Why: on Apple Silicon, Go's ad-hoc signature changes on every build and breaks the
# Keychain ACLs attached to the kahyad binary (HANDOFF §7). Signing with one stable
# self-signed identity keeps `security add-generic-password -T ~/bin/kahyad` ACLs
# valid across rebuilds, so rebuilding never re-triggers Keychain prompts.
#
# Runs on the user's Mac only (needs `security`, `codesign`, the login keychain and a
# sudo prompt for the trust step). Idempotent: exits 0 early if the identity exists.
#
# Fallback if scripting fights the keychain (per the W0-04 task file): Keychain Access →
# Certificate Assistant → Create a Certificate… → Name: "Kahya Dev", Certificate Type:
# Code Signing — then re-run this script; it will detect the identity and exit 0.
set -euo pipefail

IDENTITY="Kahya Dev"
LOGIN_KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "setup-signing.sh must run on macOS (security/codesign tooling required)" >&2
  exit 1
fi

if security find-identity -v -p codesigning | grep -q "$IDENTITY"; then
  echo "codesign identity '$IDENTITY' already present — nothing to do"
  exit 0
fi

WORKDIR="$(mktemp -d)"
chmod 700 "$WORKDIR"
trap 'rm -rf "$WORKDIR"' EXIT

# Key material lives only in WORKDIR (wiped on exit) and the keychain — never the repo.
P12_PASS="$(openssl rand -hex 16)"

cat >"$WORKDIR/ext.cnf" <<'EOF'
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

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout "$WORKDIR/kahya-dev.key" -out "$WORKDIR/kahya-dev.crt" \
  -config "$WORKDIR/ext.cnf"

openssl pkcs12 -export \
  -inkey "$WORKDIR/kahya-dev.key" -in "$WORKDIR/kahya-dev.crt" \
  -name "$IDENTITY" -out "$WORKDIR/kahya-dev.p12" -passout "pass:$P12_PASS"

security import "$WORKDIR/kahya-dev.p12" -k "$LOGIN_KEYCHAIN" \
  -P "$P12_PASS" -T /usr/bin/codesign

echo "Trusting '$IDENTITY' for code signing — sudo/keychain prompts expected…"
sudo security add-trusted-cert -d -r trustRoot -p codeSign \
  -k /Library/Keychains/System.keychain "$WORKDIR/kahya-dev.crt"

security find-identity -v -p codesigning | grep "$IDENTITY"
echo "done — 'make install' will now sign bin/kahyad with '$IDENTITY'"
