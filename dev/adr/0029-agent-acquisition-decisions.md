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
The shared service and explicit migration are now implemented. New setup uses
references; the path-scoped behavior above remains a compatibility reader until
each existing profile is migrated. Cloud enrollment stays explicit.

Local repair learning addendum (2026-09-23): a validated recovery may label a
source-controlled adapter repair; it never changes adapter behavior at runtime.
`papio adapter repair --recovery-job` accepts the link only from the daemon's own
record of one job: the `browser.page_capture` event naming the capture path, a
later declarative failure (a `ui_changed` outcome from that adapter version, or
the first agent decision reservation, which replaces that report when the
fallback runs), and a later validating-to-`ready` transition for the job's
artifact after a browser or native delivery. A transition marked
`human_identity_override` never counts: the shared artifact row's identity result
can be overwritten by another job that accepted the same bytes, so the job's own
transition is the record. The artifact row must still be a PDF with a page count.
A job later imported keeps its link. That link counts as daemon correlation for
the revision gate, because the agent path reports no provider outcome and so
never marks its capture independent. A reservation is fallback evidence only when
the same request's `browser.agent_decision_completed` receipt follows it before
`ready`; the daemon writes the reservation before inference, so a reservation
alone is recorded as route `temporal_correlation` in `repair.json`, which labels
the regression but does not unlock the revision. The recovered DOI labels the generated
regression, which must also refuse a different DOI. It is a label for an article
page only: it does not identify the control the fallback used. Promotion needs a
fresh model-free canary whose validated artifact matches the recovery; `canary.md`
in the workspace gives the procedure with the agent fallback disabled.

Firefox signed-viewer capture amendment (2026-09-24): the operator rejected the
OS helper apps (the macOS Accessibility helper and the Windows UI Automation
helper that press the PDF viewer's Save control) because their install and
maintenance cost falls on every Firefox user. Firefox instead saves a signed
viewer's PDF inside the extension. In a handoff tab papio armed for a delegated
job (the same armed-tab set as Chrome's declarativeNetRequest rules), a blocking
`webRequest.onHeadersReceived` listener attaches a StreamFilter to the one PDF
response. Every chunk passes to the viewer unchanged, and the extension keeps a
copy. When the copy starts with `%PDF-` and, for an unencoded body, matches its
Content-Length, `downloads.download` saves it from an object URL under
`papio/<job>/`, and the daemon adopts and validates it as for any browser
download. The signed URL is never requested a second time. A failed or
incomplete capture reports the existing `native_viewer_download_required`
outcome with its reason. The new Firefox-only permissions are `webRequest`,
`webRequestBlocking` and `webRequestFilterResponse`. The same day the helper was
removed from the product: its daemon driver, the `native_viewer_save_v1` feature,
the `native_viewer_save_request_v1` and `native_viewer_save_result_v1` messages
and the `browser.native_viewer_helper` setting, which a config may still set
and papio now ignores. Git history keeps the helper for extension-free work.

Residual gap: the capture sees only a response that arrives after papio armed
the tab. A PDF opened before that, or on a host papio has no access to, has
already spent its one response. The viewer's own Download button can still keep
a copy, but Firefox gives papio no way to file that download.

Armed-child amendment (2026-09-24, job_272d01737a): a View PDF child tab stays
armed while its job can still take the one viewer response, whether or not its
opener is still open, and a handoff-drive timeout that finds such a child alive
only releases the drive slot instead of re-queueing the job and closing its
tab. The listener takes `main_frame`, `sub_frame` and `object` responses, so a
PDF that Firefox's viewer shows inside a page is captured too. A PDF that a
page script fetches (`xmlhttprequest`) is outside the capture by design: a
blocking listener on every script request would wake the event page for each
one, in every tab.
