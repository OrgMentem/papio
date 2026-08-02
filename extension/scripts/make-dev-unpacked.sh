#!/bin/sh
# Build a load-unpacked Chrome dev copy with a deterministic extension ID.
#
# The store manifest must never carry a "key" (CWS rejects it), so this
# assembles extension/dev-unpacked/: symlinks to dist/ and icons/ plus a
# manifest.json copy with the local dev public key injected. The private key
# lives outside the repo (~/.config/papio/dev/extension-dev-key.pem); the
# derived ID is stable across rebuilds, so it can sit permanently in
# [browser] extension_ids and the native-host allowlist.
#
# Usage: extension/scripts/make-dev-unpacked.sh   (run from anywhere)
# Re-run after editing extension/manifest.json; dist/ symlinks track builds.
set -eu

ext_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
keydir="${HOME}/.config/papio/dev"
priv="${keydir}/extension-dev-key.pem"
out="${ext_dir}/dev-unpacked"

mkdir -p "$keydir"
[ -f "$priv" ] || { umask 077; openssl genrsa -out "$priv" 2048 >/dev/null 2>&1; }

key_b64=$(openssl rsa -in "$priv" -pubout -outform DER 2>/dev/null | base64)
ext_id=$(openssl rsa -in "$priv" -pubout -outform DER 2>/dev/null \
  | shasum -a 256 | cut -c1-32 | tr '0123456789abcdef' 'abcdefghijklmnop')

mkdir -p "$out"
ln -sfn ../dist "$out/dist"
ln -sfn ../icons "$out/icons"
jq --arg key "$key_b64" '. + {key: $key}' "$ext_dir/manifest.json" > "$out/manifest.json"

echo "dev-unpacked ready: $out"
echo "extension id:       $ext_id"
