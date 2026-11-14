# Provider and resolver contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Provider / resolver quirks (institution-specific; current setup = Example University)
- **Example University's resolver routes many titles to ProQuest** (`proquest.com/openurl/handler/…`), not
  the publisher — so publisher adapters (Wiley/SAGE) only fire when routing lands there
  (title/holdings dependent). ProQuest is the highest-volume destination.
- **ProQuest openurl-handler needs the `accountid`.** The Shibboleth-DS federated route
  authenticates ProQuest's *main* context but the openurl handler re-walls. Appending
  `?accountid=<id>` unlocks Example University access with **no sign-in** (config `proquest_account_id`,
  adapter `accountIdParam`). A title still shows "no results" if Example University doesn't *hold* it.
- **Provider PDF-URL shapes differ**: Wiley `citation_pdf_url` (`/doi/pdf/`) is an HTML
  *viewer wrapper* — the real file is `/doi/pdfdirect/<doi>?download=true`. SAGE
  `/doi/pdf/<doi>?download=true` *is* the file. Always confirm which URL returns `%PDF`.
- **OA short-circuits institutional routing**: if the OA resolvers (unpaywall/EuropePMC/etc.)
  find an open copy, the daemon fetches it during `resolving` and never hits the browser. To
  exercise an institutional/provider path, use a **non-OA** title.
- Institution federated-login entityID for Example University is `https://idp.example.edu/entity` (NOT
  `/idp/shibboleth`); ProQuest account id is `12345`. These live in `config.toml`, not code.
- **Never derive pacing from a provider's `x-ratelimit-*` header — it may report a
  different tier's ceiling than the one your request is in.** Verified live 2026-08-17:
  an **unauthenticated** OpenAIRE request is answered `x-ratelimit-limit: 7199`, while
  OpenAIRE documents 7200/hour for *authenticated* and **60/hour for unauthenticated**
  requests (`graph.openaire.eu/docs/apis/terms`). Anything that trusted that header on
  a keyless install would raise its own rate ~120× and get the operator rate-limited.
  Pace from the provider's **documented** ceiling for the tier the request is actually
  made in; ADR-0024 records this as the reason the header-derived floor does not
  generalise. `openaire`'s `rate_per_sec: 0.016` is therefore *correct* (0.016 × 3600 =
  57.6/hour, just under the documented 60) and must not be "fixed" by raising `burst`,
  since `burst + rate × 3600 ≤ 60` allows a burst of at most 2.4.
- **An OpenAIRE personal access token expires one hour after OpenAIRE issues it, so
  `[sources.openaire].api_key` cannot run a daemon** — it authenticates through your
  test and then fails every request for the rest of the day, presenting as a provider
  outage rather than an expiry. This note previously claimed the 120× authenticated
  tier was "already wired, a config change not a code change"; that was wrong, and it
  was wrong because `Options.APIKey` → `Authorization: Bearer` *looks* like complete
  support. The durable credential is a **registered service** (Basic level, at
  `develop.openaire.eu/apis`): a client id and secret that do not expire, exchanged for
  short-lived tokens by `openaire.ClientCredentials` (`internal/resolvers/openaire/auth.go`)
  and wired in `bootstrap.openAIRETokens`. Two consequences to preserve: the exchange
  runs against the AAI host, so it is deliberately outside the source's budget gate and
  its Graph rate ceiling; and `config.EffectiveSourcePolicy` raises pacing for client
  credentials but **never** for `api_key`, because pacing to 7,200/hour on a credential
  that can vanish mid-hour leaves papio at 120× a keyless ceiling.
- **Before tuning any threshold against job history, plot both distributions first.**
  The [PDF identity](pdf-identity.md) `make identity-corpus` discipline generalises. Measured on the live store:
  jobs that reached `ready` needed p50 **11** wire attempts, p90 29, p99 616, max
  **1,376**, while the worst pathological job sat at 3,404 — so an attempt-count cap
  intended to kill runaway jobs would have killed a job that succeeded on pass 1,376.
  When the healthy and pathological distributions overlap by a decimal order of
  magnitude, the count is the wrong instrument, not the wrong number.

