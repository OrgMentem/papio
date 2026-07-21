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
| `access_mode` | string | empty | Required before acquisition. Allowed values are `conservative`, `assisted`, and `maximal`; a fresh guided `papio init` chooses `conservative`. Conservative records institutional OpenURL availability without opening a handoff; assisted and maximal can route eligible exhaustion to browser handoff. |
| `email` | string | empty | Contact identity for polite API pools. Doctor fails when enabled Unpaywall has no email; enabled OpenAlex also requires an email and API key. |
| `data_dir` | path string | `~/.local/share/papio` (Windows: `%LOCALAPPDATA%\papio`) | Private writable data directory for the database, artifacts, socket, and default browser-adoption directory. |

## `[fetch]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `max_bytes` | integer bytes | `104857600` (100 MiB) | Maximum artifact-download size. It must be at least `1048576` (1 MiB). |
| `timeout_seconds` | integer seconds | `120` | Fetch deadline. It must be at least 5 seconds. |
| `allow_http_loopback` | boolean | `false` | Development and test override that permits HTTP loopback. Doctor warns while it is enabled; production policy is HTTPS-only. |

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
| `download_adoption_root` | path string | empty | Root for browser-download adoption. When empty, the effective value is `<data_dir>/adoptions`; adoption is confined to a job subdirectory beneath this root. |
| `action_expiry_seconds` | integer seconds | `1800` | Maximum open time for one browser handoff. It must not be negative. |

The browser path uses the user's ordinary Chrome session. It is not configured
with passwords, MFA, CAPTCHA tokens, or publisher credentials.

`papio init` collects `extension_id` and `firefox_extension_id` during setup
(Firefox defaults to the built add-on's `papio@orgmentem.com`), or set them
non-interactively with `--extension-id` / `--firefox-extension-id`, so the
native messaging host installs on the first run.

### `[browser.resolvers]`

Named resolver profiles are per-institution tables keyed by a lowercase
alphanumeric name. Each carries its own OpenURL base and, optionally, the same
`shibboleth_entity_id` and `proquest_account_id` login fields as the default
`[browser]` institution — so a multi-institution user routes each job's login
to the right library instead of inheriting the default's identity:

```toml
[browser.resolvers.campus]
openurl_base_url = "https://library.example.edu/discovery/openurl?institution=EXAMPLE"
shibboleth_entity_id = "https://idp.example.edu/idp/shibboleth"  # optional
proquest_account_id = "12345"                                     # optional
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

On a tracked Alma/Primo resolver page, the extension may follow the first
same-origin `resolveService` link selected by the institution's Online Services
order. This emulates resolver direct linking without accepting provider terms
or initiating physical-item, scan, or interlibrary-loan requests. Script access
remains constrained by `extension/manifest.json` host permissions; an unlisted
custom resolver origin stays in assisted mode.

## `[zotio]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `executable` | path or command string | `zotio` | zotio executable *papio* invokes at the Zotero boundary. Optional: an empty value disables the deep Zotero integration (auto-import, plan/apply, queue); ownership lookup then classifies every work as not-owned. Required only when `auto_import = true`. |
| `timeout_seconds` | integer seconds | `120` | zotio command deadline. It must be between 5 and 600 seconds inclusive. |
| `attachment_mode` | string | `stored` | zotio attachment mode. Allowed values are `stored` and `linked-file`. |
| `auto_import` | boolean | `false` | Default acquisition policy for automatic zotio plan-and-apply after a job is ready. An `acquire --auto-import` request can opt in per job. |
| `auto_enrich` | boolean | `true` | After the first applied auto-import, enables the conservative scoped zotio enrichment of missing DOI and abstract fields for the imported parent. |

*papio* invokes zotio but does not read or store Zotero credentials. Manual
mutation remains preview-first: `papio zotio plan` returns immutable plans and
`papio zotio apply` requires the exact confirmation SHA-256.

## `[hooks]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `on_ready` | shell command string | empty | When set, runs once via the system shell (`/bin/sh -c`; `cmd /C` on Windows) each time a job reaches `ready` (validated artifact). Job metadata arrives as `PAPIO_*` environment variables. Fire-and-forget: a failing hook is recorded as a `hook.on_ready` job event but never fails or retries the job. Empty disables it. See the [hooks guide](../guide/hooks.md). |
| `timeout_seconds` | integer seconds | `120` | Deadline for one hook run. Validated (5..600) only when `on_ready` is set. |

## `[notify]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Enables best-effort local desktop notifications from the daemon. The daemon coalesces park and applied-import notices in a 60-second window. |
| `webhook_url` | string URL | empty | When set, every notification is also delivered as a JSON POST (`{source, event, message, watch_id, watch_label, count, sent_at}`; plain notices carry only `source`, `message`, `sent_at`). Independent of `enabled`, which governs only the desktop channel. Must be an absolute http(s) URL. Delivery is best-effort and never fails the triggering work. |
| `webhook_secret` | string | empty | Sent as `Authorization: Bearer <secret>` on webhook posts. Requires `webhook_url`. |

## `[discovery]`

| Key | Type | Default | Effect and constraints |
| --- | --- | --- | --- |
| `sources` | string array | empty (= `["openalex"]`) | Discovery backends for `papio search` and watches, in merge-preference order. Valid entries: `openalex`, `semanticscholar` (each at most once). Results merge with DOI-then-title deduplication; earlier backends win ties. Per-backend API keys live under `[sources.<name>]` (e.g. `sources.semanticscholar.api_key`, optional — Semantic Scholar works keyless at public rate limits). |

## `[sources.<name>]`

`[sources]` is a map of resolver policies. The supported built-in names are
`arxiv`, `europepmc`, `unpaywall`, `openalex`, `openalex_content`, `core`,
`crossref_tdm`, and `semanticscholar` (discovery-only; see `[discovery]`).
Each named section accepts these keys:

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
| `openalex_content` | `false` | 0 | 0 |
| `core` | `false` | 0.4 | 1 |
| `crossref_tdm` | `false` | 1 | 1 |

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
