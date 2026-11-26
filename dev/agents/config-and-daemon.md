# Config, store, adoption and daemon operations

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Config & daemon deploy
- **Config is strict-mode: unknown fields are rejected.** Adding a `[browser]` field
  (e.g. `shibboleth_entity_id`, `proquest_account_id`) means the **old daemon binary
  rejects the whole config** the moment the field is present. So a config change and the
  binary that understands it **must deploy together** — otherwise every CLI call fails to
  parse. Deploy order: build binary → `mv` into place → `papio daemon stop` → next command
  autostarts the new daemon. There is no `daemon restart`; `daemon status`/`stop` don't
  autostart, other commands do.
- **`DisallowUnknownFields` covers struct fields, NOT map keys — but `[sources.*]` names
  are now whitelisted separately.** `Config.Sources` is a `map[string]Source`, so strict
  decoding never rejected an unknown source name; `[sources.unpaywal]` used to parse cleanly
  and silently do nothing. `validate()` now fails closed against the `sourceCatalog`
  in `internal/config/config.go`, so **adding a resolver source means a `Source*`
  const plus a `sourceCatalog` row** (name, roles, shipped defaults), which also
  fixes its resolver precedence through the acquisition role; each role has one
  constructor in `internal/bootstrap` (resolver chain, discovery backends,
  metadata enrichers, retraction sentinel), pinned both ways by the role tests.
  Removing a source needs one more edit: add it to `removedSourceNames`, which is
  tolerated-and-dropped on load, because `papio init`/`config save` writes every default
  source into the user's config — a hard rejection would make every existing config
  unparseable on upgrade. The same map-vs-struct distinction applies to
  `[browser.resolvers.*]`, which validates its own key names.
- **This machine has TWO papio binaries** and both must be updated on deploy:
  `/opt/homebrew/bin/papio` (CLI/daemon on PATH) and `~/.local/bin/papio` (target of
  the native-messaging symlink `~/.config/papio/bin/papio-native-host`). Updating only
  the first leaves browsers spawning the OLD native host (daemon shows a `legacy`
  browser session). After replacing it, kill the running `papio-native-host` process;
  Chrome respawns it from the new binary within seconds.
- **Use `make dev-deploy` for local daemon changes — don't hand-roll the dance.**
  It builds a version-stamped binary, installs it to a stable `~/.local/bin/papio`,
  repoints the native-host symlink there, restarts the host + daemon, and runs
  `doctor`. Doing it by hand and skipping the `native-host install` step leaves the
  extension talking to a stale (or dead) host.
- **`brew upgrade` can dangle the native-host symlink → "Could not establish a
  current daemon session".** `papio native-host install` now pins the symlink to
  the *invocation path* (e.g. the brew-stable `/opt/homebrew/bin/papio`, relinked on
  upgrade), not the resolved `…/Caskroom/papio/<ver>/papio` — brew deletes the old
  versioned dir on upgrade, which used to leave the host pointing at nothing. If a
  session still won't connect, run `papio native-host status` (`target_exists`) or
  `papio doctor` (now fails on a missing host executable) and re-run `native-host
  install`. On a dev box, prefer a single papio: put `~/.local/bin` ahead of
  `/opt/homebrew/bin` in PATH (bare `papio` = your dev build; autostart is then
  unambiguous) and/or `brew pin papio` so it can't silently upgrade mid-session.
- **Never `cp` over an existing papio binary on macOS** — reusing the inode of a
  previously-executed signed binary poisons the kernel signature cache and the next
  exec dies with SIGKILL (137). Guarded by `rule://no-cp-over-papio-binary`.
- **A new store migration bumps `user_version`, and FOUR test files pin the
  latest number** — plus two historical fixtures that must NOT be bumped, which is
  where this goes wrong. Bump: `internal/cli/clean_install_test.go` ("schema
  version N", twice, plus a `user_version` compare), `internal/doctor/doctor_test.go`,
  `internal/store/migrate_forward_test.go`'s two post-migration `user_version`
  compares, and `internal/store/migrate_guard_test.go`'s
  `TestOpenRefusesSchemaNewerThanBinary`
  — the easiest to miss, because it is the only site naming *two* numbers, setting
  `user_version` to latest+1 and asserting the exact refusal string `"database
  schema version <N+1> is newer than this binary supports (<N>); refusing to open"`.
  Leave alone: that file's sibling `TestGuardCapableSchema33RefusesSchema34`
  deliberately models a historical ceiling, and `migrate_forward_test.go`'s
  schema-33 tests pin their fixture at 33 on purpose — `schemaFixture(t, 33, …)`
  selects migrations by `migrationNumber(filename) > version` rather than a
  hardcoded prefix list, so the bound stays 33 and a new migration needs no entry
  there. `go test ./...` fails
  after adding `internal/store/migrations/NNNN_*.sql` until the four latest-version
  assertions are bumped.
- **Every stored timestamp is `store.FormatTime` text, never `time.RFC3339Nano`.**
  SQLite compares TEXT byte by byte and papio compares timestamps as text (lease
  and retry checks, `[since, until)` counts, keyset cursors, ORDER BY).
  `RFC3339Nano` trims trailing fraction zeros, and trimmed text misorders inside
  one second (`…:05Z` sorts after `…:05.1Z`). Write and bind with
  `store.FormatTime`/`store.Now` (UTC, nine digits, 30 bytes); parse with
  `time.RFC3339Nano`, which reads both widths. Migration 0056 rewrote the old
  values. A bound of "now" on a half-open period also drops an event recorded
  in the same clock reading — Windows advances the wall clock about once a
  millisecond — so an omitted end must leave the period open, as
  `job.ProducerStats` does.
- **NEVER edit an applied migration in place.** A constraint or column change needs a
  *new* migration, even when the old one is "obviously wrong" — no schema version
  records an in-place edit, and every test migrates a fresh database and so never sees
  the drift. Guarded by `rule://applied-migration-immutable` and
  `rule://no-inplace-migration-rewrite`, which carry the `0025_pdf_grabs.sql` incident.
- **Adding a `job.TerminalReason` is three edits, and the third is the one you
  forget**: the const block and `NormalizeTerminalReason`'s switch (both
  `internal/job/job.go`), plus `terminalReasonWriters` in
  `internal/job/terminal_reason_test.go`. That table used to be a plain list, so
  omitting a reason made it silently cover less instead of failing —
  `doi_not_registered` shipped that way. `TestTerminalReasonVocabularyIsExhaustive`
  now parses `job.go` and fails on any declared reason missing from the table or
  from the switch, so the trap is closed; do not "simplify" that test back into a
  hand-maintained list. A user-facing reason also wants an `internal/errcat`
  explanation, and — if correcting the metadata makes the work fetchable
  immediately — an exemption in `internal/zotio`'s `unavailableCooldown`.
- **`papio daemon stop` can look hung for 15-30s** when the daemon is mid-graceful-shutdown,
  not deadlocked — don't jump to "infinite loop" from growing `ps` CPU time alone. A `SIGKILL`'d
  daemon leaves its unix socket file behind (never runs its own cleanup), so a subsequent
  command hangs connecting to a dead socket; `rm` the socket at the configured `--socket` path
  and restart before assuming a code-level bug.
- **A daemon owns its data directory's instance lock for its whole life, and
  autostart waits for a long migration instead of killing it.** The daemon claims
  `<data_dir>/papio.daemon.lock` before it opens the store and holds it until the
  process exits, recording its pid, socket and phase (`starting`, `upgrading`,
  `checking`, `running`, `stopping`) in `<data_dir>/papio.daemon.json`. The claim is
  keyed by the data directory, not the socket: a daemon started with another
  `--socket` for a claimed directory exits without opening the database. Autostart
  (`internal/daemon/autostart.go`) never launches a daemon while the lock is held,
  terminates its own child at `StartTimeout` only while the child still reports
  `starting` (it re-reads the phase right before any signal; a claim held with no
  readable status counts as store work, and a daemon that cannot write a
  store-work phase retries, then deletes the stale status instead of leaving
  `starting` behind, and exits before reading credentials if it can do neither), and gives up on an
  `upgrading`/`checking` daemon after `MaxWait` (10 min) without stopping it. This
  replaced a 5 s socket deadline that SIGTERMed the daemon partway through migration
  0056 (~600k timestamps, 20-30 s); the migration rolled back and the next starter,
  CLI or a native host that the browser respawns every ~2 s, began it again from
  scratch. A daemon from before the claim holds no lock, so `papio daemon stop`
  (`daemon.StopDaemon`) reads the serving pid from the socket's peer credentials,
  holds `papio.sock.start.lock` (which every autostart takes, old binaries too) and
  returns only once that pid has exited (or belongs to a process that began at
  another time, so a recycled pid does not fail a successful stop), up to 2 min. Consequences: `daemon stop`
  no longer returns while the daemon drains; a daemon with no socket says what it is
  doing in `papio.daemon.json`; and `papio mcp`/`papio init`, which open the store
  in-process, do not take the lock.
- **macOS TCC can hang the daemon's `open(2)` on `~/Downloads/papio` — and every
  `make dev-deploy` re-arms it.** `download_adoption_root` lives under `~/Downloads`
  (it must: Chrome's `onDeterminingFilename` can only steer downloads to relative
  paths inside the browser's download root), and Downloads is TCC-protected. That
  is now the *default* — `EffectiveAdoptionRoot()` resolves to
  `<user downloads dir>/papio` (`internal/config/downloads.go`), and `papio init`
  creates it so the consent prompt is paid once, interactively, rather than by a
  background daemon that cannot answer it. `<data_dir>/adoptions` is the
  superseded default and survives only as a drain-only entry in
  `Config.AdoptionRoots()`: sweeps still adopt from it and collect its terminal
  job dirs, nothing is ever written there. A
  daemon launched from a context without the Files-and-Folders grant doesn't get a
  clean `EPERM` — the syscall blocks in-kernel on tccd (the process ignores SIGTERM
  mid-open; `kill -9` only). Consent is per-binary-signature, so every dev rebuild
  resets it. This used to wedge EVERY daemon RPC (adoption scans ran under the
  bridge mutex — three goroutine dumps in one night's `daemon.log` before diagnosis);
  scans are now bounded+latched so the daemon stays responsive and `papio doctor`
  names the adoption root's health with the grant remediation.
  **A failed adoption listing is inconclusive, never "empty", and
  `ErrAdoptionScanTimeout` is not the only way one fails.** A permission or
  consent refusal and a plain I/O error say exactly as little, and the latch
  returns the timeout *instantly* (the single-flight gate after one deadline)
  for a directory that is perfectly readable and removable — which is what
  makes the mistake destructive rather than theoretical. Only `os.ErrNotExist`
  is conclusive.
  `sweepGrabsIn` (quarantine-only fallback) and `sweepTerminalAdoptionsIn`
  (`adoptionDirMayHoldBytes`) both read a failed listing as "bytes may be
  there", preserve them and retry on the next tick; a sweep that deletes or
  supersedes landing bytes must never be authorised by a listing that did not
  happen. Grab bytes also reach an adoption name through a dot-prefixed
  temporary published by hard link (`copyGrabFile`), or by the platform's
  exclusive rename (`renameNoReplace`: `renameat2(RENAME_NOREPLACE)`,
  `renamex_np(RENAME_EXCL)`, `MoveFileEx` without `MOVEFILE_REPLACE_EXISTING`)
  on a volume without hard links, because a kill mid-copy otherwise leaves a
  truncated file at a name the poll scan reads as the user's own download.
  Never publish there with a plain rename, a check-then-rename, or an in-place
  write: a rename replaces a file that took the name after `uniqueAdoptionDest`
  chose it, and an in-place write is the truncated file. With neither primitive
  the copy fails closed and the source copy stays for a retry. macOS exFAT has
  neither (both return ENOTSUP, checked natively): that is a documented
  compatibility limit, not a fallback to fix with an in-place write. Doctor also **fails**
  `adoption_root` outright when the effective root is not a `papio` directory a
  browser could steer into — the pre-fix default was exactly that, and it adopted
  nothing with no error at all. If doctor shows the
  adoption-root check failing after a deploy: System Settings → Privacy & Security →
  grant the papio binary folder access (or start the daemon from a terminal that has
  it). Diagnose live hangs with `kill -QUIT <pid>` — the dump lands in
  `<data-dir>/daemon.log`; a goroutine parked in `os.ReadDir` under
  `scanAdoptionDir`'s call stack is this footgun.
  The managed filing folder (`[filing] folder`) is the same class of dependency: its
  I/O (`internal/filing`) runs single-flight under a no-progress deadline, a stalled
  call latches (`filing.Stalled()`) so retries fail fast, and an abandoned `Publish`
  undoes a late link. Never call `RetryManagedFiling` from mandatory scheduler startup
  (`RecoverPreparedPublications`); the maintenance pass owns it.
- **A long-running local dev `papio.db` can hold rows that predate a later validation.**
  `job.WithHumanActionBinding` (quarantine_path/sha256 non-empty) only applies to actions
  created *through* that code path going forward — pre-existing rows from before the guard
  existed sit with an empty binding forever. A feature that assumes "this required field is
  always populated" can be correct for every current code path and still break on old dev data.
  Check row/job `created_at` and whether the schema/validation predates it before assuming a
  "shouldn't happen" state is a live bug.
- **An IPC/transport cap fix only lands when the *native host* binary is new too.**
  `internal/ipc/client.go` enforces `MaxRequestBytes` **client-side, inside the host
  process**, so a stale host keeps rejecting oversized `browser.sync` requests against an
  upgraded daemon — and treats that rejection as fatal (see the transport-cap footgun
  in [Browser protocol](browser-protocol.md)). With two papio binaries installed this is the default failure, not
  an edge case: `papio doctor`'s `native host (version)` check now fails on host/daemon
  skew, and `papio native-host install` repoints the symlink at the binary you ran it from.

