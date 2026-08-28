#!/usr/bin/env bash
# Provision the isolated launch-demo rehearsal environment. Run from any directory:
#   scripts/launch-demo-env.sh init
#
# The rehearsal needs a Zotero that is not the operator's own library, because a
# rehearsal writes real items and Zotero Sync would copy them to the cloud. Only
# one process can hold the connector port, so the real Zotero must be closed
# while this one runs. See dev/active/launch-demo.md for the full gate.
set -Eeuo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: scripts/launch-demo-env.sh <command>

Commands:
  init         Create or refresh the isolated environment. Safe to re-run; it
               never touches an existing Zotero library.
  zotero-cmd   Print the exact command that starts the demo Zotero.
  snapshot     Record the pristine library. The demo Zotero must be stopped.
  restore      Reset the library to the pristine snapshot. Zotero must be stopped.
  status       Report what exists, which versions, and who holds the port.

Environment:
  PAPIO_DEMO_ROOT  Environment root.
                   Default: $HOME/.local/state/papio-launch-demo
  ZOTIO_SRC        zotio checkout to build from. Default: $HOME/@dev/zotio
  ZOTIO_BIN        Released zotio binary to install instead of building.
                   The release gate requires this; a build is for rehearsal only.

Never use /tmp as the root. macOS clears it, and the whole environment is then
lost between sessions.
USAGE
  exit 2
}

ROOT="${PAPIO_DEMO_ROOT:-$HOME/.local/state/papio-launch-demo}"
ZOTIO_SRC="${ZOTIO_SRC:-$HOME/@dev/zotio}"
PORT=23119
ZOTERO_APP="/Applications/Zotero.app/Contents/MacOS/zotero"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROOF_PDF="$REPO/internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# lsof exits non-zero when nothing listens, which pipefail would turn into a
# fatal error. A free port is the expected case here, so absorb it.
port_owner() {
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print $1" pid "$2; exit}' || true
}

require_port_free() {
  local owner
  owner="$(port_owner)"
  if [[ -n "$owner" ]]; then
    die "port $PORT is held by $owner; stop the demo Zotero first"
  fi
}

# Zotero's own count, read while Zotero is stopped. A pristine library is 0.
library_item_count() {
  local db="$ROOT/zotero-data/zotero.sqlite"
  [[ -f "$db" ]] || { printf 'no-library\n'; return; }
  sqlite3 "file:$db?mode=ro" 'select count(*) from items;' 2>/dev/null || printf 'unreadable\n'
}

cmd_init() {
  mkdir -p "$ROOT"/{bin,zotero-profile,zotero-data,downloads} \
           "$ROOT"/zotio/{config,data,state,cache}

  [[ -f "$PROOF_PDF" ]] || die "missing committed proof PDF: $PROOF_PDF"

  # zotio binary: a released artifact for the recording, a build for rehearsal.
  local zotio_origin zotio_head
  if [[ -n "${ZOTIO_BIN:-}" ]]; then
    [[ -x "$ZOTIO_BIN" ]] || die "ZOTIO_BIN is not executable: $ZOTIO_BIN"
    install -m 0755 "$ZOTIO_BIN" "$ROOT/bin/zotio-current"
    zotio_origin="released:$ZOTIO_BIN"
  else
    [[ -d "$ZOTIO_SRC" ]] || die "no zotio checkout at $ZOTIO_SRC; set ZOTIO_SRC or ZOTIO_BIN"
    log "building zotio from $ZOTIO_SRC"
    ( cd "$ZOTIO_SRC" && go build -o "$ROOT/bin/zotio-current" ./cmd/zotio )
    zotio_head="$(cd "$ZOTIO_SRC" && git rev-parse --short HEAD)"
    zotio_origin="built:$ZOTIO_SRC@$zotio_head"
  fi

  # The wrapper is the isolation boundary: it clears every inherited Zotero
  # credential so a rehearsal cannot reach the operator's cloud library, and
  # pins zotio at the demo profile's local API. zotio reads ZOTERO_*, not ZOTIO_*.
  cat > "$ROOT/bin/zotio-demo-profile" <<WRAPPER
#!/bin/sh
set -eu

unset ZOTIO_DEMO
unset ZOTERO_API_KEY
unset ZOTERO_USER_ID
unset ZOTERO_GROUP
unset ZOTERO_HOME

export ZOTERO_CONFIG=$ROOT/zotio/config/config.toml
export ZOTERO_CONFIG_DIR=$ROOT/zotio/config
export ZOTERO_DATA_DIR=$ROOT/zotio/data
export ZOTERO_STATE_DIR=$ROOT/zotio/state
export ZOTERO_CACHE_DIR=$ROOT/zotio/cache
export ZOTERO_PROFILE_DIR=$ROOT/zotero-profile
export ZOTERO_BASE_URL=http://127.0.0.1:$PORT/api/users/0

exec $ROOT/bin/zotio-current "\$@"
WRAPPER
  chmod 0755 "$ROOT/bin/zotio-demo-profile"

  cat > "$ROOT/zotio/config/config.toml" <<CONFIG
base_url = "http://127.0.0.1:$PORT/api/users/0"
user_id = "0"
CONFIG

  # user.js, not prefs.js: Zotero rewrites prefs.js on exit and would drop these.
  # Port $PORT is Zotero's default, so it needs no pref of its own.
  cat > "$ROOT/zotero-profile/user.js" <<PREFS
user_pref("extensions.zotero.dataDir", "$ROOT/zotero-data");
user_pref("extensions.zotero.useDataDir", true);
user_pref("extensions.zotero.firstRun2", false);
user_pref("extensions.zotero.httpServer.localAPI.enabled", true);
user_pref("extensions.zotero.sync.autoSync", false);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
PREFS

  # One manifest entry against a committed PDF, so the connector route can be
  # proven without touching an approved paper.
  cat > "$ROOT/zotio-parent-key-proof.json" <<MANIFEST
{
  "schema_version": 2,
  "dir": "$(dirname "$PROOF_PDF")",
  "entries": [
    {
      "path": "$PROOF_PDF",
      "classification": "new",
      "action": "create",
      "identifier_type": "doi",
      "identifier": "10.0000/papio-demo-key-proof",
      "title": "Demo connector key proof",
      "item": {
        "DOI": "10.0000/papio-demo-key-proof",
        "itemType": "journalArticle",
        "title": "Demo connector key proof"
      },
      "status": "resolved"
    }
  ]
}
MANIFEST

  cat > "$ROOT/manifest.json" <<META
{
  "provisioned_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "root": "$ROOT",
  "zotio": "$zotio_origin",
  "papio_repo_commit": "$(cd "$REPO" && git rev-parse --short HEAD)",
  "connector_port": $PORT
}
META

  log "provisioned $ROOT"
  log ""
  log "Manual steps this script cannot do:"
  log "  1. Close the operator's real Zotero. It holds port $PORT."
  log "  2. Start the demo Zotero:  scripts/launch-demo-env.sh zotero-cmd"
  log "  3. Run 'snapshot' once, before any rehearsal writes items."
  log "  4. Install the released extension and sign in to the institution by hand."
}

cmd_zotero_cmd() {
  [[ -x "$ZOTERO_APP" ]] || die "Zotero not found at $ZOTERO_APP"
  printf '%s -profile %s -no-remote\n' "$ZOTERO_APP" "$ROOT/zotero-profile"
}

cmd_snapshot() {
  require_port_free
  [[ -f "$ROOT/zotero-data/zotero.sqlite" ]] || die "no library yet; start the demo Zotero once first"
  local count
  count="$(library_item_count)"
  if [[ "$count" != "0" && "${1:-}" != "--force" ]]; then
    die "library holds $count item row(s); a snapshot must be pristine (override with --force)"
  fi
  tar -C "$ROOT" -cf "$ROOT/zotero-data-pristine.tar" zotero-data
  log "pristine snapshot recorded ($count item rows)"
}

cmd_restore() {
  require_port_free
  [[ -f "$ROOT/zotero-data-pristine.tar" ]] || die "no snapshot; run 'snapshot' on a pristine library first"
  rm -rf "$ROOT/zotero-data"
  tar -C "$ROOT" -xf "$ROOT/zotero-data-pristine.tar"
  log "library restored to pristine ($(library_item_count) item rows)"
}

cmd_status() {
  printf 'root              %s\n' "$ROOT"
  printf 'exists            %s\n' "$([[ -d $ROOT ]] && echo yes || echo no)"
  printf 'zotio wrapper     %s\n' "$([[ -x $ROOT/bin/zotio-demo-profile ]] && echo yes || echo no)"
  printf 'pristine snapshot %s\n' "$([[ -f $ROOT/zotero-data-pristine.tar ]] && echo yes || echo MISSING)"
  printf 'library items     %s\n' "$(library_item_count)"
  local owner; owner="$(port_owner)"
  printf 'port %-13s %s\n' "$PORT" "${owner:-free}"
  if [[ -f "$ROOT/manifest.json" ]]; then cat "$ROOT/manifest.json"; fi
}

case "${1:-}" in
  init)       cmd_init ;;
  zotero-cmd) cmd_zotero_cmd ;;
  snapshot)   shift; cmd_snapshot "${1:-}" ;;
  restore)    cmd_restore ;;
  status)     cmd_status ;;
  *)          usage ;;
esac
