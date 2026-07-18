#!/usr/bin/env bash
# Package and upload the papio Chrome extension to the Chrome Web Store.
#
#   extension/scripts/submit-chrome.sh            # upload a draft (safe default)
#   extension/scripts/submit-chrome.sh --publish  # upload AND submit for review
#
# Builds the Chrome target, zips dist/ + manifest.json + icons/ into
# web-ext-artifacts/, then uploads it to the existing Chrome Web Store item via
# chrome-webstore-upload-cli.
#
# The store ITEM must already exist: create it once by uploading a first ZIP by
# hand in the Chrome Web Store Developer Dashboard (the API cannot create the
# initial listing). After that, this script drives version updates. Chrome Web
# Store review can take days; store-installed users auto-update once approved.
#
# Credentials come from extension/.env (see docs/chrome-web-store-listing.md for
# how to mint the OAuth token):
#   CWS_CLIENT_ID       OAuth2 client id
#   CWS_CLIENT_SECRET   OAuth2 client secret
#   CWS_REFRESH_TOKEN   OAuth2 refresh token
#   CWS_EXTENSION_ID    the Web Store item id (from the dashboard URL)
set -Eeuo pipefail

PUBLISH=false
if [[ $# -gt 0 ]]; then
  case "$1" in
    --publish) PUBLISH=true ;;
    *)
      echo "usage: $(basename "$0") [--publish]" >&2
      exit 64
      ;;
  esac
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
EXTENSION_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
cd "$EXTENSION_DIR"

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi
for var in CWS_CLIENT_ID CWS_CLIENT_SECRET CWS_REFRESH_TOKEN CWS_EXTENSION_ID; do
  if [[ -z "${!var:-}" ]]; then
    echo "error: $var missing (set it in extension/.env)" >&2
    exit 1
  fi
done

# chrome-webstore-upload-cli reads OAuth credentials from these exact env names.
export CLIENT_ID="$CWS_CLIENT_ID"
export CLIENT_SECRET="$CWS_CLIENT_SECRET"
export REFRESH_TOKEN="$CWS_REFRESH_TOKEN"

echo "==> building extension (Chrome dist/ + Firefox firefox/)"
bun run build

VERSION=$(python3 -c "import json; print(json.load(open('manifest.json'))['version'])")
ZIP="$EXTENSION_DIR/web-ext-artifacts/papio-chrome-${VERSION}.zip"
mkdir -p web-ext-artifacts
rm -f "$ZIP"
echo "==> packaging $ZIP"
entries=(manifest.json dist)
[[ -d icons ]] && entries+=(icons)
zip -X -q -r "$ZIP" "${entries[@]}"

# No command = upload + publish; the `upload` command uploads a draft only.
cli_args=(--source "$ZIP" --extension-id "$CWS_EXTENSION_ID")
if $PUBLISH; then
  echo "==> uploading + publishing to Chrome Web Store item $CWS_EXTENSION_ID"
  bunx chrome-webstore-upload-cli@4.0.1 "${cli_args[@]}"
else
  echo "==> uploading draft to Chrome Web Store item $CWS_EXTENSION_ID (not publishing)"
  bunx chrome-webstore-upload-cli@4.0.1 upload "${cli_args[@]}"
fi

echo "==> done."
