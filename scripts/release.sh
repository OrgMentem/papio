#!/usr/bin/env bash
# Build the complete local release bundle. Run from any directory:
#   scripts/release.sh <version> [--pin-id]
set -Eeuo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: scripts/release.sh <version> [--pin-id]

Builds papio, zotio, the browser extension, SBOMs, license inventories,
checksums, and release-manifest.json into dist/release/<version>/.

--pin-id adds the preserved public Chrome manifest key from
~/.config/papio/keys/manifest-key.b64 to the packaged extension manifest.
It never reads or copies extension-signing-key.pem.
USAGE
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage
  exit 64
fi

VERSION=$1
PIN_ID=false
if [[ $# -eq 2 ]]; then
  if [[ $2 != "--pin-id" ]]; then
    usage
    exit 64
  fi
  PIN_ID=true
fi
if [[ ! $VERSION =~ ^[0-9][0-9A-Za-z.+-]*$ ]]; then
  printf 'release version must start with a digit and contain only [0-9A-Za-z.+-]: %s\n' "$VERSION" >&2
  exit 64
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PAPIO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)
ZOTIO_DIR=${ZOTIO_DIR:-"$PAPIO_ROOT/../zotio"}
if [[ ! -f "$ZOTIO_DIR/go.mod" ]]; then
  printf 'zotio repository not found at ZOTIO_DIR=%s (set ZOTIO_DIR to its checkout)\n' "$ZOTIO_DIR" >&2
  exit 1
fi
ZOTIO_ROOT=$(CDPATH= cd -- "$ZOTIO_DIR" && pwd -P)

RELEASE_ROOT="$PAPIO_ROOT/dist/release"
OUTPUT_DIR="$RELEASE_ROOT/$VERSION"
EXTENSION_DIR="$PAPIO_ROOT/extension"
METADATA_HELPER="$SCRIPT_DIR/release_metadata.py"
TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/papio-release.XXXXXX")
CURRENT_STEP=initializing
LICENSE_METHOD=
PINNED_EXTENSION_ID=

cleanup() {
  rm -rf -- "$TEMP_DIR"
}

on_error() {
  local status=$?
  printf 'release failed during step: %s (exit %d)\n' "$CURRENT_STEP" "$status" >&2
  exit "$status"
}

trap cleanup EXIT
trap on_error ERR

run_step() {
  CURRENT_STEP=$1
  shift
  printf '==> %s\n' "$CURRENT_STEP"
  "$@"
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'required command is unavailable: %s\n' "$1" >&2
    return 1
  fi
}

check_prerequisites() {
  require_command go
  require_command git
  require_command bun
  require_command zip
  require_command python3
  require_command shasum
  [[ -f "$METADATA_HELPER" ]]
  [[ -f "$EXTENSION_DIR/manifest.json" ]]
  [[ -f "$EXTENSION_DIR/package.json" ]]
}

read_release_metadata() {
  PAPIO_COMMIT=$(git -C "$PAPIO_ROOT" rev-parse HEAD)
  ZOTIO_COMMIT=$(git -C "$ZOTIO_ROOT" rev-parse HEAD)
  RELEASE_EPOCH=$(git -C "$PAPIO_ROOT" show -s --format=%ct HEAD)
  PAPIO_VERSION="$VERSION"
  EXTENSION_VERSION=$(python3 "$METADATA_HELPER" manifest-version \
    --manifest "$EXTENSION_DIR/manifest.json")
}


prepare_output() {
  rm -rf -- "$OUTPUT_DIR"
  mkdir -p "$OUTPUT_DIR/sbom" "$OUTPUT_DIR/licenses"
}

build_papio() {
  (
    cd "$PAPIO_ROOT"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false \
      -ldflags "-X papio/internal/api.Version=$VERSION" \
      -o "$OUTPUT_DIR/papio-darwin-arm64" ./cmd/papio
  )
}

build_zotio() {
  (
    cd "$ZOTIO_ROOT"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false \
      -ldflags "-X zotio/internal/cli.version=$VERSION" \
      -o "$OUTPUT_DIR/zotio-darwin-arm64" ./cmd/zotio
  )
}

check_cross_artifact_compatibility() {
  python3 "$METADATA_HELPER" compat \
    --repo-root "$PAPIO_ROOT" \
    --papio-version "$PAPIO_VERSION" \
    --zotio-binary "$OUTPUT_DIR/zotio-darwin-arm64"
}

build_extension() {
  (
    cd "$EXTENSION_DIR"
    bun run build
  )
}

package_extension() {
  local stage="$TEMP_DIR/extension"
  local extension_archive="$OUTPUT_DIR/papio-extension-$VERSION.zip"
  local key_file="${PAPIO_CONFIG_DIR:-$HOME/.config/papio}/keys/manifest-key.b64"
  local entries=(manifest.json dist)

  mkdir -p "$stage"
  cp "$EXTENSION_DIR/manifest.json" "$stage/manifest.json"
  cp -R "$EXTENSION_DIR/dist" "$stage/dist"
  if [[ -d "$EXTENSION_DIR/icons" ]]; then
    cp -R "$EXTENSION_DIR/icons" "$stage/icons"
    entries+=(icons)
  fi

  if [[ $PIN_ID == true ]]; then
    [[ -r "$key_file" ]]
    python3 "$METADATA_HELPER" pin-manifest \
      --manifest "$stage/manifest.json" \
      --key-file "$key_file" \
      --id-file "$TEMP_DIR/extension-id"
    PINNED_EXTENSION_ID=$(<"$TEMP_DIR/extension-id")
  fi

  python3 "$METADATA_HELPER" normalize-timestamps \
    --directory "$stage" --epoch "$RELEASE_EPOCH"
  (
    cd "$stage"
    zip -X -q -r "$extension_archive" "${entries[@]}"
  )
  python3 "$METADATA_HELPER" verify-extension-archive --zip-path "$extension_archive"

  local firefox_stage="$TEMP_DIR/extension-firefox"
  local firefox_archive="$OUTPUT_DIR/papio-extension-firefox-$VERSION.zip"
  local firefox_entries=(manifest.json dist)

  mkdir -p "$firefox_stage"
  cp "$EXTENSION_DIR/firefox/manifest.json" "$firefox_stage/manifest.json"
  cp -R "$EXTENSION_DIR/firefox/dist" "$firefox_stage/dist"
  if [[ -d "$EXTENSION_DIR/firefox/icons" ]]; then
    cp -R "$EXTENSION_DIR/firefox/icons" "$firefox_stage/icons"
    firefox_entries+=(icons)
  fi

  python3 "$METADATA_HELPER" normalize-timestamps \
    --directory "$firefox_stage" --epoch "$RELEASE_EPOCH"
  (
    cd "$firefox_stage"
    zip -X -q -r "$firefox_archive" "${firefox_entries[@]}"
  )
  python3 "$METADATA_HELPER" verify-extension-archive --zip-path "$firefox_archive"
}

capture_sboms() {
  (
    cd "$PAPIO_ROOT"
    go list -m all
  ) >"$OUTPUT_DIR/sbom/papio-go-modules.txt"
  (
    cd "$ZOTIO_ROOT"
    go list -m all
  ) >"$OUTPUT_DIR/sbom/zotio-go-modules.txt"
  (
    cd "$EXTENSION_DIR"
    bun pm ls
  ) >"$OUTPUT_DIR/sbom/extension-bun-packages.txt"
}

capture_license_inventory() {
  if command -v go-licenses >/dev/null 2>&1; then
    LICENSE_METHOD='go-licenses report'
    (
      cd "$PAPIO_ROOT"
      go-licenses report ./cmd/papio
    ) >"$OUTPUT_DIR/licenses/papio-go-licenses.csv"
    (
      cd "$ZOTIO_ROOT"
      go-licenses report ./cmd/zotio
    ) >"$OUTPUT_DIR/licenses/zotio-go-licenses.csv"
  else
    LICENSE_METHOD='go list -m -json fallback (go-licenses unavailable)'
    printf 'go-licenses was unavailable; module metadata is the license inventory fallback.\n' \
      >"$OUTPUT_DIR/licenses/README.txt"
    (
      cd "$PAPIO_ROOT"
      go list -m -json all
    ) | python3 "$METADATA_HELPER" fallback-licenses \
      --output "$OUTPUT_DIR/licenses/papio-go-license-fallback.csv"
    (
      cd "$ZOTIO_ROOT"
      go list -m -json all
    ) | python3 "$METADATA_HELPER" fallback-licenses \
      --output "$OUTPUT_DIR/licenses/zotio-go-license-fallback.csv"
  fi
}

generate_release_manifest() {
  python3 "$METADATA_HELPER" release-manifest \
    --directory "$OUTPUT_DIR" \
    --output "$OUTPUT_DIR/release-manifest.json" \
    --version "$VERSION" \
    --papio-commit "$PAPIO_COMMIT" \
    --papio-version "$PAPIO_VERSION" \
    --zotio-commit "$ZOTIO_COMMIT" \
    --extension-version "$EXTENSION_VERSION" \
    --extension-id "$PINNED_EXTENSION_ID" \
    --license-method "$LICENSE_METHOD"
}

generate_checksums() {
  python3 "$METADATA_HELPER" checksums --directory "$OUTPUT_DIR"
}

verify_checksums() {
  (
    cd "$OUTPUT_DIR"
    shasum -a 256 -c SHA256SUMS
  )
}

run_step 'checking release prerequisites' check_prerequisites
run_step 'reading release source metadata' read_release_metadata

run_step 'preparing release directory' prepare_output
run_step 'building papio darwin-arm64 binary' build_papio
run_step 'building zotio darwin-arm64 binary' build_zotio
run_step 'checking cross-artifact compatibility' check_cross_artifact_compatibility
run_step 'building browser extension' build_extension
run_step 'packaging browser extension' package_extension
run_step 'capturing dependency SBOMs' capture_sboms
run_step 'capturing license inventories' capture_license_inventory
run_step 'writing release manifest' generate_release_manifest
run_step 'writing SHA-256 checksums' generate_checksums
run_step 'verifying SHA-256 checksums' verify_checksums
printf 'release completed: %s\n' "$OUTPUT_DIR"
