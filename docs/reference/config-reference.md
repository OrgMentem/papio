# Configuration reference

*papio* loads TOML from `~/.config/papio/config.toml` (on Windows,
`%APPDATA%\papio\config.toml`) unless the global
`--config <path>` option selects another file. Configuration is layered over the
built-in defaults; unknown TOML fields are rejected. `papio init` writes a
validated user-only config file and `papio doctor` reports readiness.

The tables below list every decoded key in `internal/config`. Paths beginning
with `~/` are expanded when *papio* loads them.

## Top-level keys

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `access_mode` | string | empty | Required before acquisition. Allowed values are `conservative`, `assisted`, and `delegated`; a fresh guided `papio init` chooses `conservative`. Conservative records institutional OpenURL availability without opening a handoff; assisted and delegated can route eligible exhaustion to browser handoff. |
| `email` | string | empty | Contact identity for polite API pools. **Sent to third parties**: as a query parameter to Unpaywall (required) and OpenAlex (required), to Crossref when set, and in the `User-Agent` of DOI-registration lookups. See [Privacy](../privacy.md). Doctor fails when enabled Unpaywall has no email; enabled OpenAlex also requires an email and API key. |
| `data_dir` | path string | `~/.local/share/papio` (Windows: `%LOCALAPPDATA%\papio`) | Private writable data directory for the database, artifacts, socket, and default browser-adoption directory. |

## `[fetch]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `max_bytes` | integer bytes | `104857600` (100 MiB) | Maximum artifact-download size. It must be at least `1048576` (1 MiB). |
| `timeout_seconds` | integer seconds | `120` | Fetch deadline. It must be at least 5 seconds. |
| `allow_http_loopback` | boolean | `false` | Development and test override that permits HTTP loopback. Doctor warns while it is enabled; production policy is HTTPS-only. |

## `[captures]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Stores sanitized diagnostic page captures received from the extension in the local data directory. Disable it to keep no diagnostic page HTML. |
| `max_per_host` | integer | `10` | Maximum retained captures for each host. It must be between 1 and 1000; when the limit is exceeded, the oldest captures for that host are removed. |
| `max_age_days` | integer days | `14` | Maximum capture age. It must be between 1 and 365; older captures are removed. |

The `[captures]` section is strict-mode configuration: an older daemon rejects a
config containing it. Deploy the binary that supports this section together with
the configuration change.

## `[pdf]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `ocr_enabled` | boolean | `true` | Enables the OCR fallback. If it is enabled, doctor requires both `pdftoppm` and `tesseract`; disabling it makes image-only papers require review. |
| `min_text_chars` | integer | `400` | Minimum extracted-text threshold used by PDF validation before OCR fallback is relevant. |
| `max_ocr_pages` | integer | `4` | Maximum pages processed by the OCR fallback. |
| `title_match_threshold` | number | `0.6` | PDF title-match threshold. It must be greater than 0 and no greater than 1. |

## `[browser]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `extension_id` | string | empty | The Chrome extension ID allowed to use the native host. It must be 32 characters from `a` through `p`; an empty value disables the bridge. |
| `extension_ids` | list of strings | empty | Additional Chrome-family extension IDs allowed to reach the native host alongside `extension_id` — e.g. an Edge Add-ons store copy or a second keyed build, which carry different IDs than the Chrome Web Store package. Each is 32 chars `a`–`p`. The manifest's `allowed_origins` lists `extension_id` plus every entry here. |
| `firefox_extension_id` | string | empty | The Firefox (Gecko) add-on ID allowed to use the native host — `papio@orgmentem.com` for the built extension. Accepts an email-style ID or a braced GUID; an empty value disables the Firefox bridge. |
| `openurl_base_url` | string URL | empty | Legacy/default institutional OpenURL resolver base. It must use `https://`; an empty value prevents default-profile institutional routing. Existing query parameters are preserved when *papio* adds citation metadata. Prefer the institution's direct-link-enabled endpoint so a single electronic service bypasses the resolver menu. |
| `shibboleth_entity_id` | string URL | empty | Default institution's Shibboleth IdP entityID (`https://`). When set, a provider login wall is auto-routed to this IdP (skipping the WAYF selector). Empty disables federated login-routing for the default profile. |
| `proquest_account_id` | string digits | empty | Default institution's ProQuest account id (digits, max 64). When set, *papio* appends `?accountid=<id>` to unlock the institution's ProQuest link-resolver without a manual sign-in. Empty disables the append. During `papio init` you may paste a ProQuest URL containing `accountid=` instead of the bare id. |
| `libkey_mode` | string | empty | LibKey routing for the default institution: empty or `off` disables it; `link` routes DOI/PMID handoffs through the keyless `libkey.io/libraries/<id>/<doi-or-pmid>` institution link ahead of the bare OpenURL resolver, falling back to OpenURL for works without a DOI or PMID and whenever LibKey is unavailable. `api` is reserved and rejected (not implemented). Requires `libkey_library_id` when set to `link`. |
| `libkey_library_id` | integer | `0` | The institution's numeric Third Iron library id — the number in its BrowZine/LibKey.io URL (`…/libraries/<id>/…`). Required (positive) when `libkey_mode = "link"`; setting it without `link` mode is rejected rather than left silently dead. |
| `default_resolver` | string | empty | Named `[browser.resolvers.<name>]` profile used when a request omits `resolver` (e.g. `papio acquire` without `--resolver`). Empty preserves the historical default institution. Must name a configured profile — the implicit `default` profile (when `openurl_base_url` is set) or a `[browser.resolvers.<name>]` key; an unconfigured name is rejected at load. |
| `download_adoption_root` | path string | empty | Root for browser-download adoption. When empty, the effective value is `<data_dir>/adoptions`; adoption is confined to a job subdirectory beneath this root. |
| `action_expiry_seconds` | integer seconds | `1800` | Browser-handoff expiry and the initial age before an open human action is reminded. Later reminders double their per-action interval through 24 hours. It must not be negative. |


`papio init` can derive `openurl_base_url` from a pasted library discovery URL
(`--institution-url`) or from the resolver configured in Zotero.

The browser path uses the user's ordinary Chrome session. It is not configured
with passwords, MFA, CAPTCHA tokens, or publisher credentials.

`papio init` collects `extension_id` and `firefox_extension_id` during setup
(Firefox defaults to the built add-on's `papio@orgmentem.com`), or set them
non-interactively with `--extension-id` / `--firefox-extension-id`, so the
native messaging host installs on the first run.

### `[browser.resolvers]`

Named resolver profiles are per-institution tables keyed by a lowercase
alphanumeric name; `default` is reserved for the implicit top-level
institution and is rejected at load if used as a profile key. Each carries
its own OpenURL base and, optionally, the same `shibboleth_entity_id`,
`proquest_account_id`, `libkey_mode`, and `libkey_library_id` fields as the
default `[browser]` institution — so a multi-institution user routes each
job's login to the right library instead of inheriting the default's
identity:

```toml
[browser.resolvers.campus]
openurl_base_url = "https://library.example.edu/discovery/openurl?institution=EXAMPLE"
shibboleth_entity_id = "https://idp.example.edu/idp/shibboleth"  # optional
proquest_account_id = "12345"                                     # optional
libkey_mode = "link"                                              # optional
libkey_library_id = 1234                                          # required for link mode
```

A profile may also be written as a bare string — `campus =
"https://library.example.edu/discovery/openurl?institution=EXAMPLE"` — which is
shorthand for a table with only `openurl_base_url` set. This keeps older
single-base configs valid; add the table form when a profile needs its own
login identity.

Select one with `papio acquire --resolver campus`, `papio acquire --batch
works.json --resolver campus`, or the corresponding MCP field. The selected
name is snapshotted in the job policy, so re-opened actions cannot silently
fall back to another institution.

Set `browser.default_resolver` to one of these names (or leave it empty) to
choose which profile an omitted `resolver` uses; an explicit
`--resolver`/MCP `resolver` value always takes precedence, and an
unconfigured `default_resolver` value is rejected at load.

On a tracked Alma/Primo resolver page, the extension may follow the first
same-origin `resolveService` link selected by the institution's Online Services
order. This emulates resolver direct linking without accepting provider terms
or initiating physical-item, scan, or interlibrary-loan requests. Script access
remains constrained by `extension/manifest.json` host permissions; an unlisted
custom resolver origin stays in assisted mode.

### `[browser.document_delivery]`

Configures the default institution's document-delivery / interlibrary-loan
route (ADR-0017). Omitting the table disables it — a job that exhausts every
acquisition candidate falls back to the profile's plain OpenURL route, the
same behavior as before this table existed. The identical table nests under
`[browser.resolvers.<name>.document_delivery]` for a named institution
profile, alongside that profile's own `openurl_base_url`,
`shibboleth_entity_id`, `proquest_account_id`, `libkey_mode`, and
`libkey_library_id`.

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `kind` | string | none (required) | Delivery adapter: `openurl` \| `libkey` \| `illiad` \| `custom`. Required — *papio* never guesses which ILL system an institution runs from branding or a landing page. `oclc` and `rapido` are named as intended future providers but rejected: a `kind` whose adapter has not shipped must not parse, the same fail-closed rule sources apply to `[sources.<name>]`. |
| `base_url` | string URL | empty | Request form or API base. Used as the request-form URL for `openurl`/`custom`, and as the ILLiad Web Platform base for `illiad`. Must use `https://`. |
| `patron_web_base_url` | string URL | empty | Patron-facing ILLiad web client base (the `illiad.dll` URL patrons use), permitted only for `kind = "illiad"`. Must use `https://`. Never derived from `base_url` — shared-server sites and customised directories make derivation unreliable. When set, a fulfilled request's "View PDF" page (form 75) routes through the ordinary browser handoff for retrieval; when empty, fulfillment falls back to a recorded human action and the compiled profile reports `fulfillment: none` even if submission is auto-capable. |
| `allowed_hosts` | list of strings | empty | Hosts a prefilled request form or API base may reach. |
| `submit_policy` | string | `never` | `never` (default) \| `prefill_only` \| `auto_if_unconditional`. Narrows what the daemon-wide `access_mode` permits — it never widens it: under `conservative` the route is discovered and recorded only, never opened or submitted; under `assisted` the prefilled form opens but submission stays human. `auto_if_unconditional` is accepted only when `kind = "illiad"`: `openurl`, `libkey`, and `custom` route to a form with no deterministic submission-and-reconciliation contract, so they compile permanently `prefill_only`. |
| `request_classes` | list of strings | empty | Request classes this profile is declared for. v1 recognizes only `digital_journal_article`; any other value is rejected as not yet modelled. |
| `legal_basis` | string | `unknown` | `institution_policy` \| `copyright_act_s49` \| `unknown`. Configured, never inferred from a hostname. `copyright_act_s49` (Australian document supply) compiles `prefill_only` permanently, by statute: the patron's declaration is an affirmative, per-request statutory act — "not previously supplied" — that no standing declaration can truthfully cover, and *papio* must never tick, script, or represent it. An AU-jurisdiction profile defaults `patron_attestation` to `unknown` until the institution confirms otherwise. |
| `patron_attestation` | string | `unknown` | `not_required` \| `standing_completed` \| `per_request` \| `unknown`. `standing_completed` counts only when the institution has confirmed a registration-time agreement covers API-created requests of this class — never inferred from an account's existence, a missing checkbox in one render, an API accepting a request, or the institution's country or hostname. |
| `patron_fee_policy` | string | `unknown` | `zero_standard` \| `per_request` \| `unknown`. Only `zero_standard` can ever compile `auto_capable` — v1 auto-submission covers zero-patron-fee digital journal articles only; books, chapters, theses, physical loans, rush service, and any nonzero or provider-quoted fee stay `prefill_only`. |
| `monthly_request_cap` | integer | `0` (no cap) | Bounds auto-submitted requests per calendar month; `0` means no declared cap. Values below zero are tolerated and behave as `0` (no cap). |
| `status_poll_minutes` | integer minutes | `0` (adapter default) | Delivery status poll cadence; `0` uses the delivery service's own default. Values below zero are tolerated and behave as `0` (the default). Delivery polling draws on its own budget, never on ordinary resolver/HTTP retry counts, so a slow ILL turnaround cannot exhaust the acquisition waterfall's retry budget. |
| `api_key` | string | empty | Institution-issued application credential, permitted only for `kind = "illiad"` — a key on a form-kind profile is rejected as dead config. Read only by the daemon's delivery service; never sent to, stored in, or observable from the extension or the browser wire. 0600 config only. |
| `patron_ref` | string | empty | Configured, non-secret patron reference used to map requests to the institution's system. Personal identity data, not a secret: 0600 config only, redacted from events, diagnostics, and delivery provenance. |

A compiled `auto_capable` verdict additionally requires one recorded live
acceptance — a supervised submit-and-reconcile against the real deployment
under the institution's authority — so a compiled adapter plus matching
config is necessary but not sufficient. `papio init` prints the compiled
gate class before saving (`AUTO-CAPABLE` with its evidence, or
`PREFILL ONLY` with the specific blocker) and `papio doctor` verifies what
is verifiable while keeping `DECLARED` configuration and `PASS`/`OBSERVED`
facts strictly separate: neither command claims automatic submission for a
profile that cannot actually reach it.

```toml
[browser.resolvers.campus.document_delivery]
kind = "illiad"
base_url = "https://ill.campus.example.edu/illiadwebplatform"
allowed_hosts = ["ill.campus.example.edu"]
submit_policy = "auto_if_unconditional"      # or "prefill_only" / "never"
request_classes = ["digital_journal_article"]
legal_basis = "institution_policy"           # or "copyright_act_s49"
patron_attestation = "standing_completed"    # or "not_required" / "per_request"
patron_fee_policy = "zero_standard"
monthly_request_cap = 25
status_poll_minutes = 60
api_key = "..."
patron_ref = "configured-non-secret-reference"
```

## `[zotio]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `executable` | path or command string | `zotio` | zotio executable *papio* invokes at the Zotero boundary. Optional: an empty value disables the deep Zotero integration (auto-import, plan/apply, queue). When no generic `library.sources` authority is configured, ownership lookup then classifies every work as not-owned. Required only when `auto_import = true`. |
| `timeout_seconds` | integer seconds | `120` | zotio command deadline. It must be between 5 and 600 seconds inclusive. |
| `attachment_mode` | string | `stored` | zotio attachment mode. Allowed values are `stored` and `linked-file`. |
| `auto_import` | boolean | `false` | Default acquisition policy for automatic zotio plan-and-apply after a job is ready. An `acquire --auto-import` request can opt in per job. |
| `auto_enrich` | boolean | `true` | After the first applied auto-import, enables the conservative scoped zotio enrichment of missing DOI and abstract fields for the imported parent. |
| `exception_tags` | boolean | `false` | Enables the reconciled exception-tag ledger: *papio* maintains `papio:needs-action` and `papio:unavailable` as Zotero *automatic* tags on provenance-confirmed personal-library items, reconciling job state with current attachment state (`papio zotio tags reconcile` runs one pass on demand). Requires `executable` and zotio ≥ 0.13.0. Lifecycle states are never tagged; a same-name manual tag is never retyped or removed. After a daemon reload, turning this off makes the next pass remove papio-owned tags. |
| `unavailable_recheck_days` | integer days | `14` | How long an `unavailable` outcome parks an item before backfill re-checks it (open-access availability drifts upward). Must be between 1 and 365 inclusive. |

*papio* invokes zotio but does not read or store Zotero credentials. Manual
mutation remains preview-first: `papio zotio plan` returns immutable plans and
`papio zotio apply` requires the exact confirmation SHA-256.

## `[hooks]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `on_ready` | shell command string | empty | When set, runs once via the system shell (`/bin/sh -c`; `cmd /C` on Windows) each time a job reaches `ready` (validated artifact). Job metadata arrives as `PAPIO_*` environment variables. Fire-and-forget: a failing hook is recorded as a `hook.on_ready` job event but never fails or retries the job. Empty disables it. See the [hooks guide](../guide/hooks.md). |
| `timeout_seconds` | integer seconds | `120` | Deadline for one hook run. Validated (5..600) only when `on_ready` is set. |

## `[[library.sources]]`

Libraries *papio* consults to answer "do I already hold this paper?" for users
who do not run Zotero. Repeat the table for each source (maximum 8 — every one is
read on every search, batch, and discovery acquire-watch pass). Generic
`library.sources` are ignored while `zotio.executable` is configured; otherwise
they are the ownership authority for discovery acquire watches. Alert watches
retain their historical zotio ownership path and do not consult generic sources.

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `name` | string | — | Required, unique, and must have no leading or trailing whitespace. Identifies the source in `papio doctor` output and in warnings. |
| `kind` | string | — | Required. `file` is the only supported kind; anything else is rejected rather than ignored. |
| `path` | string | — | Required for `kind = "file"`. The bibliographic export to read. `~` is expanded and the resulting path must be absolute. |
| `format` | string | empty | `bibtex`, `ris`, `csl-json`, or `nbib`. Empty detects from the path and content. |
| `claim` | string | — | Required, **no default**: `pdf_present` (entries whose full text you hold, so a match may skip acquisition) or `record_present` (citations only — annotates `papio search` but never skips). |

Matching is exact on identifiers represented by the source format. No format
supports every identifier, and titles are never matched. ISBN is excluded,
because an edited volume shares one ISBN with every chapter in it. *papio* never
infers PDF presence from per-manager attachment fields (BibTeX `file`, papis
`files`) — the source declares it via `claim`.

| Format | DOI | arXiv | PMID |
| --- | --- | --- | --- |
| BibTeX | yes | yes | yes |
| CSL-JSON | yes | no | yes |
| NBIB | yes | no | yes |
| RIS | yes | no | no |

A source unreadable to *papio* is reported as unreadable, not as holding
nothing: `papio acquire --batch` then refuses to create jobs rather than
re-downloading the whole batch. Before the fifth consecutive failure, each
cadence attempts another run; a successful run resets the failure count. The
fifth consecutive failure disables the watch; there is no re-enable command.
After fixing the source, you may force-run it once with `papio watch run <id>`,
but scheduled execution resumes only if you recreate the watch.
`--include-owned` is available only for `papio acquire --batch`, meaning
"proceed despite ownership uncertainty". Because a bibliographic export cannot
say *which* manifestation it holds, such a source never satisfies an explicit
`--desired-version published` request. `papio doctor` performs a fresh one-shot
probe of each source and reports that read's record count and outcome; it does
not report daemon cached age, count-collapse detection, or retained failure
state. See the [filing guide](../guide/hooks.md).

## `[notify]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Enables best-effort local desktop notifications from the daemon. The daemon coalesces park and applied-import notices in a 60-second window. |
| `webhook_url` | string URL | empty | When set, every notification is also delivered as a JSON POST (`{source, event, message, watch_id, watch_label, count, sent_at}`; plain notices carry only `source`, `message`, `sent_at`). Independent of `enabled`, which governs only the desktop channel. Must be an absolute http(s) URL. Delivery is best-effort and never fails the triggering work. |
| `webhook_secret` | string | empty | Sent as `Authorization: Bearer <secret>` on webhook posts. Requires `webhook_url`. |

## `[updates]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `check` | boolean | `false` | Enables the daemon's once-daily background check for a newer *papio* or zotio release against GitHub's public release API. Sends no identifier, count, or telemetry beyond the anonymous request GitHub receives for any web hit. See [Privacy](../privacy.md). `papio init`'s guided setup suggests enabling it (non-interactive `--check-updates` also defaults `true`); the config default when the key is absent (or on a config written before this key existed) is `false`. `papio doctor` reports the check as skipped while it is off. |

## `[discovery]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `sources` | string array | empty (= `["openalex"]`) | Discovery backends for `papio search` and watches, in merge-preference order. Valid entries: `openalex`, `semanticscholar` (each at most once). Results merge with DOI-then-title deduplication; earlier backends win ties. Per-backend API keys live under `[sources.<name>]` (e.g. `sources.semanticscholar.api_key`, optional — Semantic Scholar works keyless at public rate limits). |

## `[actions]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `stale_after_seconds` | integer seconds | `604800` (7 days) | How long an open human action may wait before listings report it stale. `0` selects the default; a negative value is rejected. `papio actions list` marks stale rows (`stale` and `age_seconds` in `--json`), and nothing else happens: *papio* never cancels, expires, or sweeps a handoff on a timer. |

This is deliberately not `browser.action_expiry_seconds`. That one (30 minutes by
default) is a *reminder* cadence — how soon to nudge you again about an open
action — so reusing it as a staleness threshold would report a handoff queued
over lunch as abandoned. This one answers "has anyone given up on this?", which
is measured in days.

## `[sources.<name>]`

`[sources]` is a map of resolver policies. Its keys are whitelisted
separately from *papio*'s general strict-unknown-field rejection: the map
itself decodes freely, and `validate()` rejects an unrecognized key at load
with the list below. The valid names are `arxiv`, `europepmc`, `unpaywall`,
`openalex`, `core`, `crossref_tdm`, `crossref_metadata`, `retraction_watch`,
`semanticscholar`, and `openaire`. One further name, `openalex_content`, is
a removed key: an earlier *papio* release wrote it into `Default()` and no
adapter for it ever shipped, so a config carrying it loads normally and
drops the key silently rather than breaking on upgrade — it is not a valid
key to add yourself. For `semanticscholar`, `enabled` governs
the acquisition resolver (open-access PDF lookup by exact DOI, arXiv id, or
PMID); selection as a *search* backend is separate and lives in `[discovery]`
(which reads this section's `api_key`). For `openaire`, candidates come from
the OpenAIRE Graph (metadata licensed CC-BY, acknowledged here and in
candidate provenance); the keyless public limit is 60 requests/hour — the
default `rate_per_sec` honors it, and a personal-token `api_key` raises the
ceiling. Each named section accepts these keys:

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | source-specific; see below | Enables the resolver policy. |
| `api_key` | string | empty | Credential or token for a source that requires one. Doctor requires it for enabled `openalex`, `core`, and `crossref_tdm`; enabled OpenAlex also needs `email`. |
| `rate_per_sec` | number | source-specific; see below | Per-source request-rate budget. |
| `burst` | integer | source-specific; see below | Per-source burst budget. |
| `max_cost_usd` | number | `0` | Monthly budget for paid sources. |
| `base_url_for_dev` | string URL | empty | Test/development endpoint override. If set, it must start with `http://127.0.0.1` or `http://localhost`; do not use it for a remote production endpoint. |

### Built-in source defaults

| Source name | `enabled` | `rate_per_sec` | `burst` |
| --- | ---: | ---: | ---: |
| `arxiv` | `true` | 1 | 1 |
| `europepmc` | `true` | 2 | 2 |
| `unpaywall` | `true` | 1 | 1 |
| `openalex` | `false` | 2 | 2 |
| `openaire` | `true` | 0.016 | 1 |
| `core` | `false` | 0.4 | 1 |
| `crossref_tdm` | `false` | 1 | 1 |
| `crossref_metadata` | `true` | 1 | 1 |
| `retraction_watch` | `true` | 1 | 1 |
| `semanticscholar` | `true` | 1 | 1 |

## Watch configuration

There is no `[watch]` section or watch-specific key in *papio*'s TOML config.
Watch query, year filters, OA filter, collection, cadence, and per-run cap are
stored with each watch created by `papio watch add` or the corresponding MCP
tool. Use `papio watch list` to inspect them and `papio watch remove <id>` to
remove one.

## Validation and file permissions

*papio* validates configuration when loading and saving it. It writes the config
file with mode `0600` and its config directory with mode `0700`; doctor reports
a configuration permission failure when group or other read bits are present.
Use `papio doctor` rather than weakening these permissions to diagnose a setup
problem.
