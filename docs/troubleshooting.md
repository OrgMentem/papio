# Troubleshooting

Start with the local facts before retrying a job:

```sh
papio doctor
papio daemon status
papio status
papio actions list
```

`papio daemon status` checks the existing daemon without starting one. Most
other daemon-backed client commands autostart the daemon when needed. Use
`papio jobs get <job-id>` to see one job's events and actions, and use
`papio batch report <batch-id-or-latest> --markdown` to see a research set's
joined outcome.

## Chrome MV3 worker is stale

An unpacked MV3 extension can continue running an older service worker even
when the files on disk changed. The recorded fixes require an explicit extension
reload; a Chrome restart, process kill, manifest version bump, or reloading the
extension path alone is not a substitute.

### Preferred UI reload

1. Open `chrome://extensions`.
2. Turn on **Developer mode** if it is not already enabled.
3. Find Papio and use its reload arrow.
4. Open the Papio details page and confirm its optional host permissions still
   cover only the publisher sites you intend to use.
5. Open the popup or run `papio actions list` to give the worker a reason to
   reconnect.

### Automated Chrome reload

Where local Chrome automation is already authorized, use Chrome's
`developerPrivate.reload` operation for the Papio extension. This is the
programmatic equivalent of the reload arrow; it is not a Papio CLI command and
must target the installed Papio extension, not an arbitrary extension.

### Last resort: purge the service-worker cache

Use this only with Chrome completely closed:

1. Close every Chrome process.
2. In the affected Chrome profile, remove `Service Worker/ScriptCache` and
   `Service Worker/Database` (for the default profile these are beneath
   `Default/Service Worker/`).
3. Reopen Chrome and use the `chrome://extensions` reload procedure above.

This purge also clears service-worker state for web PWAs in that profile; those
PWAs re-register on their next use. Do not delete the Papio data directory or
its database as a substitute for reloading the extension.

## Daemon restart and recovery

Stopping the daemon is explicit:

```sh
papio daemon stop
papio daemon status
papio status
```

The second command confirms that it is stopped without autostarting it; the last
command is a normal daemon-backed call and can start a fresh daemon. On a
healthy recovery, the extension treats the fresh daemon's `expected_hello`
response as recoverable, closes its stale native-messaging port, reconnects,
and re-offers durable browser jobs. The recorded recovery re-offers jobs on the
next approximately two-second host poll and does not duplicate browser tabs.

If a handoff remains stale after the daemon is back, reload the extension using
the MV3 procedure above, then inspect `papio actions list` and `papio status`.
Do not cancel a job merely because its old popup row is stale.

## Keepalive asks you to sign in again

The browser extension keeps one pinned, muted resolver tab while nonterminal
handoff jobs exist. If a reload lands on an identity-provider redirect, it pauses
keepalive, brings the tab forward, and marks a re-authentication request.

1. Open the extension popup and use Focus for the needs-you job, or run
   `papio actions open` to open the current handoff queue.
2. Sign in through the ordinary Chrome page, including any MFA or institution
   step required by that site.
3. Return to the provider page. The extension detects the return and resumes
   keepalive.

Do not put credentials in Papio configuration, native-messaging payloads, or
an MCP tool call. Papio is designed to reuse the ordinary browser session, not
to automate authentication.

## Read `doctor` output

`doctor` prints stable `PASS`, `WARN`, and `FAIL` rows. Any `FAIL` makes the
report not OK. The checks below explain every check the command can emit.

| Check | PASS means | WARN or FAIL: what to do |
| --- | --- | --- |
| `access_mode` | An explicit allowed access mode is configured. | Set `access_mode` to `conservative`, `assisted`, or `maximal`; `papio init` creates a conservative profile. |
| `fetch_policy` | HTTPS-only fetch policy is active. | A warning means `fetch.allow_http_loopback` is on; disable it outside loopback fixture work. |
| `data_dir` | The data directory is private and writable. | Correct ownership or permissions so Papio can create and write the configured directory. |
| `config_permissions` | The config is user-only. | A missing config is a warning; create it with `papio init`. A group/world-readable config is a failure; set it to mode `0600`. |
| `database` | SQLite integrity check and schema-version read succeeded. | If unavailable in this run, use doctor through the daemon. For an integrity failure, restore a verified backup before acquiring more work. |
| `pdf_worker` | The current Papio binary can run its isolated pdfcpu worker. | Reinstall or rebuild the `papio` binary and retry doctor. |
| `pdftotext` | Poppler semantic extraction is available. | Install Poppler; this is a failure. |
| `pdfinfo` | Poppler's independent page-count check is available. | Install Poppler for the structural cross-check; this is a warning. |
| `ocr` | Bounded OCR dependencies are available when OCR is enabled. | Install Poppler and Tesseract, or explicitly disable OCR. A disabled OCR fallback is a warning because image-only papers need review. |
| `source_unpaywall` | An enabled Unpaywall source has a contact email. | Set `email` or disable the source. |
| `source_openalex` | An enabled OpenAlex source has email and API key. | Set `email` and `sources.openalex.api_key`, or disable the source. |
| `source_core` | An enabled CORE source has an API credential. | Configure `sources.core.api_key`, or disable the source. |
| `source_crossref-tdm` | An enabled Crossref TDM source has an API credential. | Configure `sources.crossref_tdm.api_key`, or disable the source. |

See [`config-reference.md`](config-reference.md) for exact keys and allowed
values.

## Zotio-boundary error classes

Papio stores and prints the following stable, privacy-safe error classes for
Zotio-boundary failures. Their hints are sanitized and bounded; use the class
and hint from `papio batch report`, `papio jobs get`, or JSON output rather than
copying credentials or filesystem paths into a ticket.

| Error class | Meaning | What to do |
| --- | --- | --- |
| `zotero_http_4xx` | Zotio reported a Zotero HTTP 4xx response. | Check the local Zotio/Zotero authorization and the operation shown by the sanitized hint, correct it there, then make and inspect a new Zotio plan. |
| `zotero_field_validation` | Zotio rejected an item field, such as an unknown item field. | Update the incompatible field mapping or compatible Zotio version, then create a new plan rather than reusing the failed one. |
| `mirror_sync_failed` | Synchronizing the Zotio mirror failed. | Restore local Zotio connectivity and synchronization, then retry planning. |
| `zotio_exec_timeout` | A Zotio command exceeded its deadline. | Confirm the executable works; if the operation legitimately needs more time, set `[zotio].timeout_seconds` within 5–600 and retry. |
| `zotio_not_configured` | Papio has no usable Zotio integration. | Run `papio init` with the correct `--zotio-path`, or set `[zotio].executable` to a usable command. |
| `plan_confirmation_mismatch` | The supplied confirmation SHA-256 did not match the immutable plan. | Run `papio zotio plan` again, inspect its preview, and pass that plan's exact SHA-256 to `papio zotio apply`. |
| `reservation_conflict` | The apply reservation conflicted or was not finalized. | Let any concurrent apply finish, then make a new plan and apply it; do not force a stale reservation. |
| `local_db_locked` | A local database was locked. | Let the competing local process finish or close it, then retry the Zotio operation. |
| `network` | Network connection setup failed. | Restore network or DNS/TLS connectivity and repeat the affected operation. |
| `unknown` | No stable classifier matched the failure. | Inspect the job events and sanitized hint, run `papio doctor`, and retain the class in any bug report. |

A manual `papio zotio plan` and `papio zotio apply` recovery is preview-first:

```sh
papio zotio plan <job-id>
papio zotio apply <plan-id> --confirm-sha256 <exact-sha256>
```

Do not use a SHA-256 from a different preview, and do not treat an import error
as evidence that the validated PDF itself failed.

## Browser handoff or review is still parked

Use the report reason to choose the next step:

- `institutional`: the ordinary Chrome OpenURL handoff needs the user's
  institution session.
- `oa_browser`: an OA URL needs the browser path after bounded broker fetching
  did not complete it.
- `terms`: the user must read and decide on publisher terms; Papio does not
  accept them.
- `needs_review`: inspect the quarantine path in the open `verify_identity`
  action, then explicitly accept or reject it with `papio actions resolve`.

The exact report reason is preferable to a blind `papio jobs retry`; browser and
identity states are intentionally parked for a human decision.
