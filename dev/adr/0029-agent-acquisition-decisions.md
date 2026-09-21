# ADR-0029: Agent decisions share acquisition authority

Status: Accepted (2026-09-20). The operator approved an agent acquisition path
for missing and broken adapters, with interchangeable local and cloud decision
backends. Supplying a TypeSafe key specifically for this feature is its cloud
opt-in. This amends ADR-0015 and ADR-0021's restriction of positive page behavior
to provider-specific packaged plans; it preserves ADR-0022 and ADR-0028's job,
surface and effect authority.

Papio tries deterministic resolution, packaged adapters and generic extraction
first. An eligible delegated attempt may then ask a backend to select an observed
acquisition control, without requiring a registered provider adapter. The
executor and observation logic stay packaged. A model supplies an opaque control
ID, WAIT or BLOCKED; it cannot supply code, selectors, URLs, save paths, new
permissions or job transitions. Store approval remains unverified. Development
authorization does not establish store acceptance.

The daemon owns credentials, cancellation and inference budgets. It reserves
calls in the existing job event history and runs them outside the browser bridge
lock. Calls and answers require the current holder, job, delegated handoff, work
identity and held generic-drive effect permit. The extension rechecks the bound
document and selected element before dispatch. Reload, navigation, operator
takeover or a changed permit cannot revive an old answer. Neither side creates
a second acquisition queue or authority ledger. Cancellation stops further
submissions and invalidates adoption authority. An already submitted browser
injection may finish; local action deadlines reject dispatch after expiry but
cannot revoke a click the browser has already begun.

The first integration handles visible controls on a DOI-identified article
already bound to a managed tab. It supports in-page menus, JavaScript-backed
downloads and explicit same-origin PDF links. A selected original anchor with
its own PDF label and a `.pdf` path can receive an empty `download` attribute
for one native click. Its URL, target, referrer policy and publisher handlers
remain intact; no URL replay, cloned link or filename is introduced. The
temporary attribute is removed afterward unless a publisher handler replaced
it. This requests a download rather than proving one: browser preferences,
redirects or handlers can change the outcome. The existing bound-document and
download-receipt checks still determine ownership. With `agent_navigation_v1`,
an observed same-origin article link may lead to another document in the same
tab. The browser records the exact selected destination before clicking, retires
old control handles, and requires fresh matching DOI metadata, permissions and
no human gate on the destination. It stops on an unexpected redirect, reload or
operator navigation. Navigation uses the original loop deadline and inference
budget. URLs stay browser-local and the model still selects only an opaque ID.
New contexts, cross-origin navigation, publisher search and native viewer saving
remain later milestones. Effective browser host permission remains necessary.
Chrome uses its filename-steering API. Firefox negotiates
`native_click_adoption_v1` and reserves one native download under the same held
generic-drive permit before dispatch. The browser must observe an unambiguous
fresh download with an exact original article referrer. The daemon pins the
configured download directory before the click, rejects pre-existing files and
changed identities, and copies the observed file without deleting the original.
It records the exact digest and producer transactionally before publication to
normal adoption. A reservation is observation evidence, not a new effect permit.

An idle reservation can transfer to that freshly verified document through a
correlated rebind request. The daemon records the new binding in the existing
job events. It preserves the original directory baseline, arm time, expiry,
producer and held permit. Admission checks the latest accepted binding; a late
request with the previous document cannot import. An exact retry of the current
transfer returns the existing result. A busy, observed, expired or lost
reservation cannot transfer. A selected link that starts a download while the
source document stays in place can still use its original reservation. A
replaced document with a competing source download stops the continuation.

A restart loses the private directory baseline and cannot rearm the same permit
or repeat a click. Published bytes can recover through existing adoption. A crash
between durable admission and publication can strand a temporary copy; that
attempt remains incomplete. The current implementation does not collect those
orphan copies automatically. Neither a completed browser download nor admission
alone establishes acquisition success.

The backend-neutral contract carries a DOI, bounded article title, up to 80
role/label/disabled control descriptions and an opaque observation revision.
URLs, document bodies, account areas and form values stay out of the projection.
The browser keeps execution handles and fingerprints locally. TypeSafe is one
implementation; a local backend needs no cloud key or network. The initial
cross-platform credential input was the daemon environment variable
`PAPIO_TYPESAFE_API_KEY`. Profile enrollment now also supports OS credential
storage through `papio config agent set`: Keychain on macOS, Credential Manager
on Windows and Secret Service on Linux. Only `agent.backend = "typesafe"` is
stored in TOML; the secret is scoped to the configuration path and data directory.
No keyring lookup occurs for an unenrolled profile. A supplied environment value
overrides stored credentials, including an empty value to disable this backend.
Setup accepts a hidden prompt or stdin and requires a daemon restart. Removing
the saved key disables profile enrollment before deleting it. Credential-store
failure disables optional inference while ordinary acquisition remains available.
A local model runtime is later work. No unrelated credential is discovered
automatically, and no cloud substitution occurs silently.

An initial attempt permits up to 60 decisions in ten minutes, with a 30-second
backend deadline. These are resource ceilings, not evidence of denied access or
measured optimal settings. Failed calls consume their reservation; restart does
not reset the budget or repeat a consumed request. WAIT can yield another
observation. Only a download followed by existing adoption and PDF validation
establishes success. Challenges, credentials, purchases, terms, permissions and
delivery submissions retain their human-action boundaries.

Local receipts retain request/permit identity, observation revision, outcome and
reported usage, excluding the projection and raw model response. Successful
recoveries can inform source-controlled adapter repairs, subject to adverse-case
regressions and a fresh declarative acquisition. Automatic repair promotion and
shared analytics are separate work.

Credential storage follow-up (2026-09-21): ADR-0030 selects one integration
credential service with explicit references independent of filesystem paths.
Its migration is not implemented yet; the path-scoped behavior described above
remains current until that migration ships. Cloud enrollment stays explicit.
