# ADR-0016: LibKey as an institution-aware routing layer

Status: Proposed (2026-08-07). Drafted from the integration consult
(dev/scratch/oracle/papio-integrations-r1.md, -r2.md), then reviewed against
the tree the same day. Keyless `link` mode (Decisions 1, 2's link half, and
6's fallback rule) is implemented in the Unreleased changelog; `api` mode,
the Decision 4 presentation carriers, and Decision 7's init/doctor surfaces
remain unimplemented. Formal acceptance is the maintainer's call.

## Context

The acquisition waterfall already runs OA/API resolution, then sibling-version
resolution, then bare OpenURL institutional handoff (ADR-0001's triage model;
`[browser.resolvers.*]` in `internal/config/config.go`). LibKey/Third Iron sits
between the two institutional stages: it can return a direct PDF, an
article-landing page, a link-resolver route, a document-delivery route, or an
integrity alert (retraction / expression-of-concern / problematic-journal),
selected per DOI or PMID against a subscribing library. The consult's
recommendation (r1 §3.2, ranked #2 in the integration portfolio) is to add it
as a daemon-side router, not a browser integration, and to decompose its
response rather than trust Third Iron's own ranking of "best" result.

Two constraints already in force shape every decision below:

- **The extension talks only to the local native host.** ADR-0013's operator
  surfaces and the shipped privacy contract mean the extension originates no
  external request beyond the resolver page already open in the browser.
  LibKey resolution — an HTTP call to `libkey.io` or
  `public-api.thirdiron.com` — therefore cannot live in the extension.
- **Config is strict and deploys in lockstep.** Per AGENTS.md, `[browser]` and
  `[sources.*]` decode with `DisallowUnknownFields`, and `[sources.*]` map
  keys are separately whitelisted (`validSourceNames` in
  `internal/config/config.go`). A `libkey` source and new
  `[browser.resolvers.*]` fields are both **decoded fields**, not free-form —
  the config change and the daemon binary that understands it must ship
  together, and adding the source name is a second edit beyond adding its
  struct fields.

LibKey Nomad (the companion extension) is proprietary — its Firefox package is
marked "All Rights Reserved" (r1 §1.5) — so this ADR uses only Third Iron's
documented public integration surfaces: the keyless LibKey.io DOI/PMID
redirect and the `public-api.thirdiron.com` article-lookup API (consult
citations of Third Iron's BrowZine API docs).

## Decision 1: LibKey lives in the daemon's institutional-routing layer

LibKey resolution is a Go component in the institutional-routing stage,
ordered:

```text
OA/API candidates
  → typed sibling versions
  → LibKey (structured API or LibKey.io link)
  → existing institution OpenURL fallback
  → document delivery (ADR-0017)
```

It runs after OA and sibling resolution have been exhausted and before the
bare OpenURL fallback, per r1 §3.2. The extension receives only the resolved
browser route through the existing handoff contract (the same mechanism that
already carries an OpenURL institutional handoff); it never holds a LibKey
key, calls Third Iron, or parses LibKey JSON. This is a correction from an
earlier design pass that had considered extension-side LibKey calls — rejected
below.

LibKey *querying* is access-mode-independent metadata resolution, like every
other resolver lookup; what access mode gates is whether the resulting route
is **opened**. That gating is not re-implemented here: the route flows
through the existing handoff contract, whose `job_offer` already carries the
effective access mode and whose conservative path records institutional
routes without opening them (r1 §3.2's access-mode table maps one-to-one
onto the behaviour that contract already ships).

## Decision 2: two resolver modes, config precedence, and the lockstep cost

Two modes per named resolver profile (`[browser.resolvers.*]`, the existing
per-institution profile keyed by name and already snapshotted into each job's
`Policy.Resolver` so a multi-institution config never lets one job silently
inherit another institution's identity):

- **`link`** — keyless. Construct the documented URL
  `https://libkey.io/libraries/{library_id}/{doi-or-pmid}` and hand it to the
  browser as a route. No credential leaves the daemon.
- **`api`** — call `public-api.thirdiron.com`'s article-lookup endpoint with a
  key, which returns availability, OA status, PDF/landing/resolver/delivery
  URLs, and integrity fields in one structured response.

Configuration, mirroring the existing `[sources.*]` / resolver-profile split
(r1 §3.2, r2 §4C):

```toml
[sources.libkey]
enabled = true
api_key = ""                 # optional universal/integration-partner key
rate_per_sec = 2
burst = 2
max_cost_usd = 0

[browser.resolvers.campus]
openurl_base_url = "https://resolver.example.edu/openurl"
shibboleth_entity_id = "https://idp.example.edu/idp/shibboleth"

libkey_mode = "link"         # off | link | api
libkey_library_id = 1234
libkey_api_key = ""          # optional individual-library override
```

Precedence, most to least specific:

1. Profile-specific `libkey_api_key` (an individual-library key scoped to one
   institution profile).
2. Global `[sources.libkey].api_key` (a universal integration-partner key).
3. Keyless `link` mode.
4. Existing OpenURL fallback (Decision 6 covers when LibKey itself fails).

**Consequence, stated up front because it is easy to miss:** every one of
`libkey_mode`, `libkey_library_id`, `libkey_api_key` under
`[browser.resolvers.*]`, and the whole `[sources.libkey]` table, is a new
decoded field under strict decoding. Per AGENTS.md, shipping this config
change and the daemon binary together is not a suggestion — an old binary
handed a config file with any of these keys present rejects the **entire**
file, not just the unknown field, and `[sources.libkey]` additionally needs
its name added to `validSourceNames` or it parses cleanly and silently does
nothing (the exact `[sources.unpaywal]` failure mode AGENTS.md already
documents for source names).

## Decision 3: `bestIntegratorLink` is decomposed, never followed blindly

Third Iron's own `bestIntegratorLink` field ranks retraction,
expression-of-concern, and problematic-journal alerts **ahead of** full text,
followed by PDF, article page, document delivery, and resolver routes (r1
§3.2, r2 §4C). Treating "best" as "downloadable" would silently hand the
browser handoff an alert page instead of the article, or worse, treat an
integrity notice as a dead end instead of surfacing it. The resolution
contract keeps these apart instead of collapsing them into one link:

```go
type InstitutionResolution struct {
    IntegrityNotices []IntegrityNotice
    OpenCandidate    *CandidateObservation
    FullTextRoute    *BrowserRoute
    LandingRoute     *BrowserRoute
    ResolverRoute    *BrowserRoute
    DeliveryRoute    *DeliveryRoute
}
```

Per-field handling:

1. **Integrity alert** (retraction / EoC / problematic-journal): recorded as
   an integrity notice through the existing retraction/triage model (the
   inbox already has a retraction item kind and `!` glyph — LibKey expands
   that producer set, it does not invent a new one). An alert is an integrity
   notice, not an acquisition route: acquisition continues by default: notices
   never require confirmation, since a researcher may be acquiring a retracted
   work specifically to study or cite the retraction (r2 §4C). Deduplicate
   against an existing Crossref/Retraction Watch notice by canonical work
   identity and notice type, keeping all source evidence rather than
   overwriting one source's finding with another's.
2. **OA full-text route**: emitted as an ordinary OA `CandidateObservation`
   and run through the existing fetch/validation path — the same pipeline
   every other OA resolver candidate takes, not a LibKey-specific shortcut.
3. **Institutional full-text or landing route**: emitted as a browser route
   through the existing handoff.
4. **Document-delivery route**: preserved, untouched, for the delivery layer —
   see ADR-0017. LibKey is a router, not a delivery-request submitter; it
   never itself calls an ILL/document-delivery API.
5. **Link-resolver route**: used as the final structured fallback ahead of
   bare OpenURL, since it is a Third Iron-selected library link rather than a
   generic search-your-catalog fallback.
6. **No result**: falls through (Decision 6).

## Decision 4: auth is a tri-state; the existing boolean stays an execution gate

r2 §1 corrects an earlier design pass that had mapped `unknown` auth state to
`requires_auth=true`. That direction was a regression: it turns uncertainty
into a false instruction and recreates exactly the trust failure the shipped
`requires_auth` classification (already distinguishing OA rendering from
institutional sign-in) was built to fix. This ADR keeps the corrected
three-way contract:

```go
type AuthRequirement string

const (
    AuthNotRequired AuthRequirement = "not_required"
    AuthRequired    AuthRequirement = "required"
    AuthUnknown     AuthRequirement = "unknown"
)
```

- Explicit OA route → `not_required`.
- Explicit institutional sign-in route → `required`.
- Library route whose current session state is unknown → `unknown`.

The existing wire boolean — `job.HumanAction.RequiresAuth` /
`protocol.JobOfferPayload.RequiresAuth` (`internal/app`, `internal/browser`) —
is **not widened**. It is demoted to what it already functionally is: an
execution gate, defined narrowly as `requiresAuth := authRequirement ==
AuthRequired`:

| Tri-state      | Existing boolean |
| -------------- | ----------------: |
| `required`     |              `true` |
| `not_required` |             `false` |
| `unknown`      |             `false` |

For `unknown`, delegated mode tries the configured route instead of stopping
for a speculative login; if the page produces a login wall, the extension
surfaces it and the state becomes `required` on the next observation. The
boolean must never again drive presentation copy such as "open access" or
"sign in first" — only `route_class` plus `auth_requirement` may produce that
copy (r2 §1B). Building that presentation is out of scope here: it depends on
a **future** negotiated triage-snapshot schema version — the consult named it
"triage-snapshot/2", but schema 2 is already shipped and closed
(`internal/browser/bridge.go`, `internal/protocol/protocol.go`; see
ADR-0017's correction of the same stale claim), so the carrier is the next
version reached through the existing `schema_versions` negotiation
(ADR-0001's immutable, feature-negotiated
protocol — `papio-browser/1` itself does not bump) and a
feature-gated `handoff_access_observation_v1` message carrying a narrow
post-contact auth observation (`direct_pdf` / `accessible_article` /
`login_wall` / `inconclusive`) that can move `unknown` to a resolved state
after the browser attempt. Both are **declared dependents of this decision,
not decided here** — this ADR fixes only that the existing boolean stays a
narrow execution gate and never a presentation signal, so that later ADR does
not have to re-litigate it.

## Decision 5: credential hygiene

Third Iron's article-lookup API places `access_token` in the request query
string (r1 §3.2, r2 §4C), which makes existing daemon-wide hygiene rules
load-bearing rather than cosmetic for this integration specifically:

- The request URL, with its token, exists only in active memory. It is never
  logged, never placed in an event payload, and never written to SQLite.
- Logs, error messages, and `events` redact the **entire query string**, not
  just the token parameter — a request URL is treated the same way ADR-0014's
  `app.safeType`/`api.safeMessage` discipline already treats upstream error
  text: assume it is not safe to persist verbatim.
- The LibKey source budget is keyed by a **credential fingerprint**, following
  the existing `source_budgets` identity pattern (`internal/budget`): a
  truncated SHA-256 of the credential, written to the database and read back
  in diagnostics, never the credential itself. A profile-specific key and the
  global integration-partner key are different identities even when both
  resolve the same library.
- LibKey never calls `nomad-api.*` or reuses a credential extracted from
  Nomad. Nomad's package is proprietary ("All Rights Reserved"); inspecting it
  does not create a licence to reuse its endpoints or credential (r1 §1.5).

## Decision 6: LibKey is never a single point of failure

Any LibKey error, empty result, timeout, or credential rejection falls
through to the existing OpenURL path — LibKey augments institutional routing,
it does not replace it. Specifically:

| Condition                                       | Behaviour                                                          |
| ------------------------------------------------ | -------------------------------------------------------------------- |
| No library ID or mode `off`                       | Skip LibKey entirely                                                 |
| `api` mode without a usable key                   | Doctor/config error; fall back to `link` mode or OpenURL              |
| Unknown DOI/PMID                                  | Continue to OpenURL                                                  |
| Library does not subscribe, or work unavailable   | Continue to OpenURL / document delivery                              |
| `401`/`403`                                       | Disable that credential's API attempts until repaired; do not retry-loop |
| `429`                                             | Respect the provider limit via the existing source-budget mechanism; current job continues through OpenURL |
| Third Iron outage / 5xx / timeout                 | Continue through OpenURL                                             |
| Returned URL violates host/HTTPS policy           | Reject the route                                                     |

Third Iron documents that its article IDs are unstable and may change over
time (r1 §3.2). LibKey article IDs are therefore **never persisted**; a job
that needs the same lookup again re-resolves rather than trusting a stored ID.

## Decision 7: `init` and `doctor` never perform a real acquisition

**`papio init`.** After the existing OpenURL/institution setup step, offer
library-ID discovery:

- With an integration-partner key configured: call the Library List endpoint
  once (requires that key), cache the returned ID/name/homepage data for 24
  hours, and match the operator's institution by exact normalized name, then
  exact homepage registrable domain, then substring search. One decisive match
  selects automatically; several plausible matches present a numbered choice;
  no match falls through to manual entry.
- Without a partner key, or on no match: accept a bare numeric library ID, or
  a pasted BrowZine/LibKey.io URL (`https://browzine.com/libraries/1234`,
  `https://libkey.io/libraries/1234`) — Third Iron documents the ID as present
  in that URL shape.
- Noninteractive flag: one `--libkey-library-id <number-or-BrowZine/LibKey-URL>`
  (implemented; blank clears both fields). The consult sketched three flags
  (`--libkey-mode`, `--libkey-library-id`, `--libkey-url`), but the repo's
  ProQuest precedent is a single flag accepting a bare id or a pasted URL,
  and link mode is implied by supplying an id; a mode-only flag could only
  say "off", which blank already says.
  **No `--libkey-api-key` flag** — a flag value leaks into shell history and
  process listings. The key is collected via a no-echo interactive prompt or
  `PAPIO_LIBKEY_API_KEY` during noninteractive init, written into the
  existing `0600` config using the established source-key pattern.
- Nomad's storage is never inspected, unpacked, or scraped for a credential.

**`papio doctor`.** Verifies the integration with bounded, no-follow probes —
never a real acquisition (no work request, candidate, attempt, artifact,
browser tab, or acquisition event):

- Local checks: mode is valid, library ID is a positive integer, `api` mode
  has an applicable key, `link` mode has a library ID, no key appears in a
  URL-valued config field.
- Partner-key `api` mode: re-check the cached/fresh Library List for the
  configured ID — the strongest check, since it verifies institution identity
  as well as the ID.
- Individual-library `api` mode: one bounded article lookup against a fixed
  probe DOI, never following any returned full-text, content, notice,
  delivery, or resolver URL. The request URL exists only in active memory;
  the doctor output records only the result class, library ID, and safe
  institution name — never the token-bearing URL.
- Keyless `link` mode: a redirect-disabled HTTP request (no cookie jar) to the
  probe URL, never following a redirect to LibKey content, a publisher, or an
  institution. This verifies URL viability only, not institutional identity —
  doctor states that limitation explicitly (`WARN`, not `PASS`) rather than
  implying library-identity verification it did not perform.
- The doctor probe shares the LibKey source budget, is cached 24 hours, and
  never produces acquisition statistics.

## Rejected alternatives

**Extension-side LibKey calls.** Technically possible — the extension could
call `libkey.io` directly — but it contradicts the shipped privacy contract
that the extension originates no external request beyond the resolver page
already open (r1 §1.4). Keeping LibKey daemon-side keeps that contract intact
without an exception.

**Reusing Nomad's endpoint or credential.** Nomad's package is proprietary
("All Rights Reserved"); inspecting an installed package does not grant a
licence to reuse its endpoints, requests, or credential (r1 §1.5, §3.2).
LibKey uses only Third Iron's publicly documented integration surfaces.

**Treating `bestIntegratorLink` as a download URL.** Rejected because Third
Iron's own priority order ranks integrity alerts ahead of full text (Decision
3); following it blindly would occasionally hand the browser an alert page
instead of the article, or silently swallow a retraction notice that should
have surfaced.

**GetFTR-first.** Deferred, not rejected outright. GetFTR provides an
entitlement decision and a smart link but not authentication or delivery, and
its intended integration point is server-side rather than a resolver route
(r1 §2 rank 14). LibKey is the better first target because it aligns more
directly with library holdings, resolver, document-delivery, and
article-integrity routing — the shape this ADR's routing layer already needs.
GetFTR remains available as a future addition to the same routing layer, not
a competing design.

## Consequences

- `internal/config`'s strict decoding and `validSourceNames` whitelist both
  need the `libkey` source and the three new `[browser.resolvers.*]` fields
  before any daemon can read this config; that binary and the config change
  deploy together (Decision 2).
- `InstitutionResolution.DeliveryRoute` is the seam this ADR hands to
  ADR-0017: LibKey only preserves a document-delivery route it observes, it
  never submits, polls, or reconciles one. ADR-0017 owns everything after
  that route is preserved.
- Decision 4's tri-state auth and the `unknown → false` execution-gate mapping
  apply to every institutional route, not only LibKey's — an implementer
  should not special-case LibKey's auth handling separately from the OpenURL
  path's.
- A LibKey-fronted offer's origin is `libkey.io`, so every extension
  mechanism keyed to "the offer origin is the institution's resolver" must
  derive the institution differently: the per-job institution origin is the
  offer origin when the daemon's config-derived resolver origins know it,
  else the first `provider_hosts` entry those origins recognize — which is
  why Decision 1's offer keeps the resolver host beside `libkey.io`.
  Session-evidence recording, the `auth_returned` origin hint, and Alma/Primo
  resolver auto-routing all use this derivation; keying any future mechanism
  to the raw offer origin re-breaks it under link mode. The default profile
  additionally refuses `link` mode without an OpenURL base, because the
  handoff gates key on the base and a base-less LibKey config would sit
  validated but unreachable.
- The privacy-policy table needs an update describing what the daemon sends
  to Third Iron (requested DOI/PMID and library ID during resolution; a fixed
  public probe DOI and library ID during `doctor`; the configured API key
  where `api` mode is enabled) — nothing crosses the extension boundary.
- No browser protocol revision and no new extension permission or store
  release are required for this ADR's daemon-side routing and LibKey.io link
  construction. A future LibKey-specific page adapter (for provider UI LibKey
  cannot resolve structurally) would need its own fixture evidence and
  possibly a host permission, but is not part of this decision.
