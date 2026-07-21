# Troubleshooting

Start with the local facts before retrying a job:

```sh
papio doctor
papio daemon status
papio status
papio actions list
```

`papio daemon status` checks the background service without starting it. Most
other commands start the service automatically when they need it. Use
`papio jobs get <job-id>` to see one job's events and actions, and use
`papio batch report <batch-id-or-latest> --markdown` to see a research set's
joined outcome.

## The Chrome extension is outdated

An unpacked extension can keep running outdated code even after you change the
files on disk. The fix is an explicit extension reload; a Chrome restart, killing the
process, or reloading the extension's folder alone is not enough.

### Preferred UI reload

1. Open `chrome://extensions`.
2. Turn on **Developer mode** if it is not already enabled.
3. Find *papio* and use its reload arrow.
4. Open the *papio* details page and confirm its optional host permissions still
   cover only the publisher sites you intend to use.
5. Open the popup or run `papio actions list` to give the worker a reason to
   reconnect.

### Automated Chrome reload

Where local Chrome automation is already authorized, use Chrome's
`developerPrivate.reload` operation for the *papio* extension. This is the
programmatic equivalent of the reload arrow; it is not a *papio* CLI command and
must target the installed *papio* extension, not an arbitrary extension.

### Last resort: purge the service-worker cache

Use this only with Chrome completely closed:

1. Close every Chrome process.
2. In the affected Chrome profile, remove `Service Worker/ScriptCache` and
   `Service Worker/Database` (for the default profile these are beneath
   `Default/Service Worker/`).
3. Reopen Chrome and use the `chrome://extensions` reload procedure above.

This purge also clears service-worker state for web PWAs in that profile; those
PWAs re-register on their next use. Do not delete the *papio* data directory or
its database as a substitute for reloading the extension.

## Restarting the background service

Stopping the background service is explicit:

```sh
papio daemon stop
papio daemon status
papio status
```

The second command confirms it is stopped without starting it again; the last
command is a normal call and can start a fresh service. On a healthy recovery,
the extension recognizes the fresh service, closes its stale connection,
reconnects, and re-offers your saved browser jobs. It re-offers them within
about two seconds and does not duplicate browser tabs.

If a handoff is still stale after the service is back, reload the extension using
the procedure above, then inspect `papio actions list` and `papio status`.
Do not cancel a job merely because its old popup row is stale.

## Version mismatches and updates

Run `papio doctor` first when an update or browser integration seems wrong. It
checks *papio* first, then the pieces it depends on:

```text
PASS  access_mode              explicit access mode configured
PASS  pdftotext                Poppler semantic extraction available
...
PASS  config                   parsed /Users/me/.config/papio/config.toml
PASS  daemon                   reachable; version 0.1.0
WARN  extension                extension has not connected since daemon start
PASS  native host (Chrome)     manifest allows configured extension
PASS  native host (Firefox)    manifest allows configured extension
PASS  zotio                    version 1.0.0; required capabilities available
```

To update *papio*, build or install the new version, then stop the running
service:

```sh
papio daemon stop
```

The next command that needs it starts the new service; there is no
`papio daemon restart` command. If the command-line tool and the service are
different versions, the tool prints a warning. An unknown-field configuration
error means your config was written by a newer *papio*; install a matching or
newer version before continuing.

The extension popup reports **daemon unreachable**, ***papio* daemon out of
date**, and **extension out of date** when it needs attention; the toolbar shows
`!` in those states. When healthy, the popup shows the daemon version, and its
options page shows extension and daemon versions together. Extension updates
arrive through the browser store, while the daemon is updated manually, so an
extension newer than its daemon is the common direction.

Store-installed extensions update automatically. Manually loaded builds from
`about:debugging` or an unpacked `dist/` do not; download the new ZIP from the
release bundle instead. The daemon's extension floor and the popup states flag
an extension that is too old.

### Learning about new releases

*papio* never installs updates on its own, and it never contacts a server
without being told to. Two mechanisms tell you a newer release exists:

- **Through the extension (no network use by *papio*).** Store-delivered
  extension updates carry the daemon version they were released with. When the
  popup notices the connected daemon is older, its version line changes to
  `papio <new> is available` with the upgrade command. *papio* itself sends
  nothing anywhere; the browser's normal store update is the only network
  activity involved.
- **Opt-in release check.** With `check = true` under `[updates]` in the
  configuration (the `papio init` prompt offers this, defaulting to yes), the
  daemon asks the *papio* and zotio GitHub releases APIs for their latest versions
  at most once a day each. The requests carry no identifying payload beyond the
  connection itself, and GitHub already hosts the binaries you would download.
  Results appear in `papio doctor`, in daemon status, and as a single
  standard-error hint (at most once per day). Configurations without the
  `[updates]` section never check.

## You are asked to sign in again

The browser extension keeps one pinned, muted tab while handoff jobs are still
open. If a reload lands on your institution's login page, it stops reloading,
brings the tab forward, and flags a sign-in request.

1. Open the extension popup and use Focus for the needs-you job, or run
   `papio actions open` to open the current handoff queue.
2. Sign in on the ordinary Chrome page, including any two-factor or institution
   step required by that site.
3. Return to the provider page. The extension detects your return and resumes.

Do not put credentials in *papio* configuration, in messages to the extension,
or in an MCP tool call. *papio* is designed to reuse the ordinary browser session, not
to automate authentication.

### The sign-in page says the request is stale or expired

Institutional sign-ins are time-boxed. If login plus a two-factor step takes
long enough, the identity provider may reject the original handoff link with a
"stale request" or "expired" page even though your session is now valid. Sign
in first, then re-run `papio actions open` — every open generates a fresh
resolver link. The extension recognizes the common OpenAthens/Shibboleth
failure pages, records a `browser.handoff_failed` event on the job (visible in
`papio jobs get <id>`), and retries the handoff tab once on its own.

## Two browsers fight over *papio*

With the extension enabled in more than one browser or profile, only one
browser holds the offer/handoff flow; the others wait as pending and their
popups may look idle. Symptoms: handoff tabs open in the "wrong" browser, or
`doctor` reports "N other browser(s) waiting".

```sh
papio browser sessions          # holder + pending, versions, last contact
papio browser use --latest      # hand the session to the newest pending browser
papio browser use <session-id>  # or pick one explicitly
```

Quitting the holding browser releases the session immediately; a crashed
holder yields within about ten seconds. If you never want a browser to hold
the session, disable the *papio* extension there. Page acquisition and the inbox
keep working from every connected browser either way.

## Read `doctor` output

`doctor` prints stable `PASS`, `WARN`, and `FAIL` rows. Any `FAIL` makes the
report not OK. The checks below explain every check the command can emit.

| Check | PASS means | WARN or FAIL: what to do |
| --- | --- | --- |
| `access_mode` | An explicit allowed access mode is configured. | Set `access_mode` to `conservative`, `assisted`, or `maximal`; `papio init` creates a conservative profile. |
| `fetch_policy` | HTTPS-only fetch policy is active. | A warning means `fetch.allow_http_loopback` is on; disable it outside loopback fixture work. |
| `data_dir` | The data directory is private and writable. | Correct ownership or permissions so *papio* can create and write the configured directory. |
| `config_permissions` | The config is user-only. | A missing config is a warning; create it with `papio init`. A group/world-readable config is a failure; set it to mode `0600`. |
| `database` | The local database passed its integrity and version checks. | If unavailable in this run, run doctor through the background service. For an integrity failure, restore a verified backup before acquiring more work. |
| `pdf_worker` | The current *papio* can run its isolated PDF worker. | Reinstall or rebuild `papio` and retry doctor. |
| `pdftotext` | Poppler semantic extraction is available. | Install Poppler; this is a failure. |
| `pdfinfo` | Poppler's independent page-count check is available. | Install Poppler for the structural cross-check; this is a warning. |
| `ocr` | OCR dependencies are available when OCR is enabled. | Install Poppler and Tesseract, or explicitly disable OCR. A disabled OCR fallback is a warning because image-only papers need review. |
| `source_unpaywall` | An enabled Unpaywall source has a contact email. | Set `email` or disable the source. |
| `source_openalex` | An enabled OpenAlex source has email and API key. | Set `email` and `sources.openalex.api_key`, or disable the source. |
| `source_core` | An enabled CORE source has an API credential. | Configure `sources.core.api_key`, or disable the source. |
| `source_crossref-tdm` | An enabled Crossref TDM source has an API credential. | Configure `sources.crossref_tdm.api_key`, or disable the source. |

See [`config-reference.md`](../reference/config-reference.md) for exact keys and allowed
values.

## zotio-boundary error classes

*papio* stores and prints the following stable, privacy-safe error classes for
zotio-boundary failures. Their hints are sanitized and truncated; use the class
and hint from `papio batch report`, `papio jobs get`, or JSON output rather than
copying credentials or filesystem paths into a ticket.

| Error class | Meaning | What to do |
| --- | --- | --- |
| `zotero_http_4xx` | zotio reported a Zotero HTTP 4xx response. | Check the local zotio/Zotero authorization and the operation shown by the sanitized hint, correct it there, then make and inspect a new zotio plan. |
| `zotero_field_validation` | zotio rejected an item field, such as an unknown item field. | Update the incompatible field mapping or compatible zotio version, then create a new plan rather than reusing the failed one. |
| `mirror_sync_failed` | Synchronizing the zotio mirror failed. | Restore local zotio connectivity and synchronization, then retry planning. |
| `zotio_exec_timeout` | A zotio command exceeded its deadline. | Confirm the executable works; if the operation legitimately needs more time, set `[zotio].timeout_seconds` within 5–600 and retry. |
| `zotio_not_configured` | *papio* has no usable zotio integration. | Run `papio init` with the correct `--zotio-path`, or set `[zotio].executable` to a usable command. |
| `plan_confirmation_mismatch` | The supplied confirmation SHA-256 did not match the immutable plan. | Run `papio zotio plan` again, inspect its preview, and pass that plan's exact SHA-256 to `papio zotio apply`. |
| `reservation_conflict` | The apply reservation conflicted or was not finalized. | Let any concurrent apply finish, then make a new plan and apply it; do not force a stale reservation. |
| `local_db_locked` | A local database was locked. | Let the competing local process finish or close it, then retry the zotio operation. |
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
- `oa_browser`: an open-access URL needs the browser after *papio*'s own download
  did not complete it.
- `terms`: the user must read and decide on publisher terms; *papio* does not
  accept them.
- `needs_review`: inspect the quarantine path in the open `verify_identity`
  action, then explicitly accept or reject it with `papio actions resolve`.

The exact report reason is preferable to a blind `papio jobs retry`; browser and
identity states are intentionally parked for a human decision.
