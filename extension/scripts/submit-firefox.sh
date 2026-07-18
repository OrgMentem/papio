#!/usr/bin/env bash
# Sign and submit the papio Firefox extension to addons.mozilla.org (AMO).
#
#   extension/scripts/submit-firefox.sh [listed|unlisted]
#
# listed (default): submits the built version for AMO review. The FIRST listed
#   submission creates the public listing; the signed .xpi may not be available
#   immediately (review can take days). Store-installed users auto-update.
# unlisted: self-distribution signing; the signed .xpi lands in
#   web-ext-artifacts/ for manual `about:addons` installation.
#
# The Firefox bundle is produced by bun (build.ts, IIFE background), so AMO
# reviewers cannot read the shipped code directly. This script therefore also
# builds a human-readable source archive and passes it with --upload-source-code
# (an AMO requirement for processed sources). See docs/amo-listing.md for the
# listing text and reviewer build instructions.
#
# AMO API credentials come from extension/.env:
#   WEB_EXT_API_KEY     JWT issuer   (addons.mozilla.org -> Manage API Keys)
#   WEB_EXT_API_SECRET  JWT secret
set -Eeuo pipefail

CHANNEL="${1:-listed}"
case "$CHANNEL" in
  listed | unlisted) ;;
  *)
    echo "usage: $(basename "$0") [listed|unlisted]" >&2
    exit 64
    ;;
esac

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
EXTENSION_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
cd "$EXTENSION_DIR"

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi
if [[ -z "${WEB_EXT_API_KEY:-}" || -z "${WEB_EXT_API_SECRET:-}" ]]; then
  echo "error: WEB_EXT_API_KEY / WEB_EXT_API_SECRET missing (set them in extension/.env)" >&2
  echo "       generate a key at https://addons.mozilla.org/developers/addon/api/key/" >&2
  exit 1
fi

VERSION=$(python3 -c "import json; print(json.load(open('manifest.json'))['version'])")

echo "==> building extension (Chrome dist/ + Firefox firefox/)"
bun run build

SOURCE_ZIP="$EXTENSION_DIR/web-ext-artifacts/papio-firefox-source-${VERSION}.zip"
mkdir -p web-ext-artifacts
rm -f "$SOURCE_ZIP"
echo "==> packaging reviewer source archive $SOURCE_ZIP"
# Human-readable inputs needed to reproduce the shipped bundle. Excludes build
# outputs (dist/, firefox/), the dev profile, secrets, and node_modules.
source_entries=(manifest.json build.ts package.json tsconfig.json bun.lock web-ext-config.mjs src)
[[ -d icons ]] && source_entries+=(icons)
[[ -d test ]] && source_entries+=(test)
[[ -f docs/amo-listing.md ]] && source_entries+=(docs/amo-listing.md)
zip -X -q -r "$SOURCE_ZIP" "${source_entries[@]}"

echo "==> web-ext sign (channel=$CHANNEL) from firefox/"
sign_args=(
  --channel="$CHANNEL"
  --source-dir="firefox"
  --artifacts-dir="web-ext-artifacts"
  --upload-source-code="$SOURCE_ZIP"
  --api-key="$WEB_EXT_API_KEY"
  --api-secret="$WEB_EXT_API_SECRET"
  # Return once the version is submitted; listed review is a multi-day human
  # step, so don't block the shell polling for approval.
  --approval-timeout=0
)
# Listed versions require a license (AMO rejects the version without one);
# amo-metadata.json carries it. Unlisted self-distribution does not need it.
if [[ $CHANNEL == listed ]]; then
  sign_args+=(--amo-metadata="amo-metadata.json")
fi
bunx web-ext sign "${sign_args[@]}"

echo "==> done. Artifacts in extension/web-ext-artifacts/"
