# Architecture decisions

*papio* records its load-bearing decisions in two places: the original acquisition-stack plan, and a running architecture decision record (ADR) log for everything decided since. Neither is published; this page gives the short version for contributors, in decision order within each topic: preserve these boundaries unless the stated evidence for changing them exists.

## Go core, TypeScript browser plane

**Context:** The durable workload is local orchestration and state: resource-limited HTTP and subprocess calls, SQLite, deterministic transitions, hashing, atomic files, CLI/MCP, and release packaging. An ordinary cross-platform Chrome extension is JavaScript regardless of the core language.

**Decision:** Keep the daemon, CLI, native-messaging host, policy, queue, provenance, validation, and zotio integration in Go. Keep the Manifest V3 extension and provider DOM adapters in TypeScript. Each language stays on its side of the process boundary.

**Why:** One signed Go binary can serve the CLI, daemon, MCP server, and native host, while the extension remains a separately signed browser artifact. This keeps durable policy and untrusted-file handling out of the browser UI-drift plane without adding a Node/Bun runtime and native SQLite dependency to the installed host. All-TypeScript would not remove the extension/native-host boundary or selector drift; Rust adds toolchain and lifecycle burden without a required capability for this supervised-I/O core.

## Ordinary browser, not CDP

**Context:** Institutional access depends on the user's existing, authenticated browser session and requires visible human login, MFA, CAPTCHA, and terms actions. Cloudflare fingerprinted CDP in publisher trials, causing access loops.

**Decision:** Use an ordinary user-authenticated browser through the extension and native host. Do not use CDP, headless Chrome, stealth patches, copied cookies, or anti-bot evasion for publisher access.

**Why:** The browser handoff preserves the user's control and keeps protected-site access inside legitimate, user-authorized sessions. The extension reports narrow, metadata-only outcomes to the daemon; it does not turn browser automation into a credential or evasion channel. See [Browser handoff](../concepts/browser-handoff.md).

## zotio is the Zotero boundary

**Context:** Zotero metadata creation, deduplication, attachments, and mutation plans already belong to zotio.

**Decision:** *papio* never writes Zotero directly. It produces validated, provenance-linked acquisition output and hands it to zotio behind zotio's preview/apply boundary.

**Why:** A single owner for Zotero reads, writes, item schema, deduplication, and attachments prevents a second mutation path with different rules. *papio* owns acquisition; zotio owns Zotero mutation.

## Explicit access modes

**Context:** Open access, licensed APIs, and institutional browser handoff have different authorization and automation limits.

**Decision:** Require an explicit access-mode choice; never silently enable automation. `conservative`, `assisted`, and `delegated` define the allowed behavior, and licensed/TDM adapters remain separately enabled capabilities.

**Why:** Delegated automation is still limited to legitimate, user-authorized access: login, MFA, CAPTCHA, and publisher/library terms remain human actions. Unknown or changed provider UI falls back to assisted behavior rather than guessing. See [Access modes](../concepts/access-modes.md).

## Daemon-owned durable state

**Context:** Browser extension service workers can stop and restart, while acquisition jobs need authoritative transitions, recovery, provenance, and artifact tracking.

**Decision:** The single-user Go daemon owns the SQLite write connection and all state transitions. The MV3 extension is restartable and disposable; it keeps only minimal tab/job correlation and reconnects for authoritative state.

**Why:** A durable daemon can snapshot policy into jobs, schedule capped work, validate artifacts, and retain redacted event history independently of browser lifecycle or provider UI drift. The native host forwards size-limited messages but does not own the queue or persist browser state.

## Reversal triggers

**Context:** Adapter inconvenience alone is not evidence to revisit the architecture.

**Decision:** Reconsider the core language only before the final browser protocol v1 lock after Phase 3, and only under the plan's concrete triggers. A TypeScript core requires 2–3 real adapters showing that more than 70% of core changes necessarily co-change extension code, the cross-language protocol causes more demonstrated defects than it contains, and clean-machine Node/Bun packaging on every supported target for two release cycles. A Rust core requires in-process hostile-document/archive work at parallel scale, a security requirement for a memory-safe native daemon, or a remote multi-user service, with acceptance of the Rust/Pdfium/toolchain burden.

**Why:** Those conditions would change the product's risk profile; routine provider or adapter churn does not. The browser alternative that may be promoted is the optional macOS Apple Events adapter, and only when named required providers repeatedly fail through the extension but succeed through Apple Events. It never becomes a cross-platform architecture or a hidden CDP/stealth path.

## Handoff offers do not hard-expire

**Context:** Browser handoffs need a fresh resolver link when an institution rejects an old authentication request. `papio actions open` constructs the resolver URL at open time, and the browser bridge offers a parked handoff anew per extension session; the extension inbox opens the links a triage item already carries.

**Decision:** *papio* keeps re-issue-on-open instead of hard expiry: `papio actions open` mints a fresh handoff link each run, and the bridge re-offers parked jobs to each new extension session. The wire `expires_at` remains advisory; there is no daemon-side expiry sweeper, and `human_actions.expires_at` remains unenforced.

**Why:** Offers are local-only, so there is no confidentiality window to enforce. Re-issue on open and re-offer per session already handle stale links where they are constructed, while hard expiry would add a user-visible failure mode without a safety benefit. If inbox-opened links prove staleness-prone in practice, the fix is minting fresh links for inbox open — not expiring offers.

## Inbox and badge freshness stay page-local

**Context:** The inbox, the toolbar badge, and the acquisition-history page all need to notice daemon-side change. A live daemon-push design (derived topic-version fingerprints delivered over the browser bridge) was built and measured in ADR-0005, kept as a record of the rejected alternative.

**Decision:** Freshness stays page-local. The inbox refreshes on visibility-change and focus, and otherwise polls a lightweight counts request on a short visible-tab interval, refetching the full snapshot only when the counts signature changes. The history page refreshes on open and on return, with no poll. The toolbar badge keeps riding the extension's existing one-minute keepalive alarm. No daemon-originated push exists on the wire.

**Why:** No consumer needs sub-10-second freshness — the badge is ambient, the inbox is already covered by a visible poll plus refresh-on-return, and the history page is opened deliberately. The MV3 platform caps ambient latency at the extension's own keepalive wake regardless of transport, so push cannot beat that floor when no page is open — the only case where its latency claim would matter. And push turned out to depend on the very poll it was meant to replace: the native host only writes to the port when the daemon has frames, so an idle daemon never resets the service worker's idle timer, and a consumer's poll is what kept the worker awake to receive pushes at all. See ADR-0005 for the full account, including why the derived-fingerprint design it used is still the right approach if a genuinely latency-sensitive surface (live per-job progress, a shared inbox) ever reopens the question.

## Provenance binds to the acquisition, not to the bytes

**Context:** External consumers need to know which version was obtained, under which licence, and how the acquisition ended — a marking or citation-checking consumer cannot safely report a quotation as absent without knowing whether it received the version of record or an accepted manuscript. The first such consumer asked for those fields on the artifact record; see ADR-0007.

**Decision:** Version, reuse licence, access basis, resolver tier, and acquiring principal bind to the acquisition — the job and the candidate actually accepted — never to the content-addressed artifact. The artifact store stays a pure byte store keyed by digest. For a successful acquisition the existing `acquisition-bundle/1` is already the receipt: it carries the candidate's version, access basis, and reuse licence alongside the artifact and validation facts, so nothing is added to it. Failures, which no bundle can describe, get a dedicated method carrying a typed terminal reason. A receipt never presents an *attempted* candidate as the accepted one. Component roles (main file, HTML full text, supplement) are facts papio may report; completeness, citation match, and version adequacy are judgements that stay with the consumer. `unknown` is an honest value where a source cannot report the version, never a default to fill the field — papio does not synthesise a version, and never echoes a request's desired version back as an obtained fact.

**Why:** Artifacts are content-addressed and shared, so the same bytes may be reached by two candidates with different licences — an open-access mirror and an institutional copy of the identical file. Stamping rights onto the digest makes the last writer win and produces a false rights record for any consumer gating retention or transfer on it. A digest identifies bytes; it cannot carry the terms under which they were obtained. Receipts also stay off existing results because nothing on the wire is additive: a widened result makes an older *papio* reject every response from a newer daemon.

**Since ADR-0010:** the bundle is now `acquisition-bundle/2`, which adds one object to the accepted candidate: `entitlement`, recording the *route* by which access was obtained. That does not weaken the rule above — a route is not a rights judgement, it still binds to the acquisition rather than the digest, and `acquisition_mode` is derived from the candidate's existing `access_basis` rather than inferred. The whole object is omitted whenever *papio* has no route to report; a browser-adopted PDF earns one only when the session behind it was recorded fresh, which is ADR-0018 below. Any route *papio* does emit is a bare origin with no path, query, credentials, or fragment. v1 decoding is retained indefinitely.

ADR-0010 also makes the daemon-wide `access_mode` a ceiling: a per-request `access_mode_override` may narrow it, never widen it. The configured mode is the operator's standing decision and the only brake papio has, so a submitting consumer must not be able to raise how much papio does without a human.

## Triage inbox is a full-tab extension page

**Context:** *papio* needs one interactive surface for triaging watch hits, pending human actions such as `verify_identity`, and job-state counts. Two of the three item classes terminate in the browser (viewing a quarantined PDF, focusing a handoff tab), so only the extension knows the relevant browser-local state, and the CLI must stay the single source of truth for daemon capabilities.

**Decision:** ADR-0001 builds the inbox as a full-tab extension page, fed by one daemon-side snapshot RPC merged with a browser-local overlay (connection status, permissions, tab focus) joined by job ID. The wire protocol is typed, versioned, and solicited-only: an extension sends a new request only after the daemon advertises the matching feature, mutations are compare-and-set, and every frame stays under a 256 KiB cap.

**Why:** A popup loses its workflow on focus loss, a side panel has no cross-browser story, and a daemon-served web UI would add a second local HTTP/auth surface for no capability the extension can't already reach. See [Browser handoff](../concepts/browser-handoff.md).

## Reviewing a quarantined PDF without a daemon web app

**Context:** Verifying a candidate PDF (`verify_identity`) means a human inspects a daemon-local file from the browser triage page (ADR-0001), but sending PDF bytes over native messaging violates the protocol's frame-size cap, and `file://` URLs need per-browser setup that isn't dependable.

**Decision:** ADR-0002 was accepted conditionally: default to a minimal loopback-only HTTP endpoint scoped to one action ID and candidate hash (GET/HEAD plus PDF range requests, an unguessable short-lived capability URL, exact `Host` validation, no cookies, no CORS), unless a timeboxed spike proved a workable `file://` route on both Chrome and Firefox. That spike ran and failed on both browsers — Chrome gates extension navigation to `file://` behind an off-by-default per-extension toggle, and Firefox forbids it outright — so the conditional never opened and the loopback endpoint is what shipped. It was later extended with a small in-page review shell so accept/reject can be recorded from the same capability-bound page — through the identical revision-and-hash-bound transition the extension inbox already used, never a parallel review API.

**Why:** A capability URL binds a decision to the exact inspected bytes without opening a second authentication boundary or a general daemon web API; the same scope-creep tripwire from ADR-0001 governs it going forward.

## One browser session at a time, first-hello-wins

**Context:** With *papio* installed in two browsers — commonly a daily driver plus a development profile — every native-host restart silently stole the active session: job offers landed on an arbitrary browser with no visible signal.

**Decision:** ADR-0003 has the native host mint a per-process session ID carried on the daemon-host IPC envelope, not the extension protocol, which stays untouched. The first session to say hello holds it; a competing hello is denied with a visible error and parked as pending, promoted automatically if the holder goes silent past a short timeout, and released cleanly on shutdown. Switching sessions is an explicit CLI command.

**Why:** The fix lives in the daemon-host envelope, which ships with the daemon as one artifact, so every installed extension benefits immediately with no protocol version bump. Stateless requests such as triage still work from any session; only the offer/handoff flow is holder-exclusive.

## Generic on_ready hook instead of bespoke importers

**Context:** *papio*'s value ends at a validated PDF; where it gets filed differs per user — Zotero through zotio, or papis, Calibre, a plain folder, a private script. A bespoke importer per reference manager doesn't scale, and requiring zotio's executable blocked users with no Zotero library at all.

**Decision:** ADR-0004 adds one generic `[hooks] on_ready` shell hook, run once per job reaching `ready`, with job metadata passed through frozen `PAPIO_*` environment variables. The hook is fire-and-forget, bounded by a timeout, never retried, and its raw output is never persisted, since it inherits the daemon environment and can carry credentials. `zotio.executable` becomes optional; zotio stays the only deep integration.

**Why:** One documented recipe line covers every other reference manager for free, instead of maintaining a second stateful pipeline against backends that share none of Zotero's item-key and ownership primitives.

## Version-floor gate for daemon-initiated frames

**Context:** ADR-0001's feature negotiation requires an extension to wait for the daemon to advertise a feature before sending a new request, but that negotiation is one-directional: it cannot tell the daemon whether an old extension can parse a new daemon-initiated frame, such as focusing a handoff tab in response to `papio actions open`.

**Decision:** ADR-0006 keeps ADR-0001's feature negotiation unchanged for every extension-to-daemon request. For a daemon-to-extension frame sent only in direct response to an explicit user action, and expressible no other way, the daemon instead gates emission on a minimum extension version, plus an explicit check against legacy session identity.

**Why:** This is a narrow, directional exception, not a substitute for feature negotiation — it authorizes no background daemon push and no ungated new message type.

## Holdings claims for non-Zotero de-duplication

**Context:** Without a configured Zotero integration, *papio* had no de-duplication: search couldn't mark a result already held, batch acquisition re-fetched owned papers, and backfill watches were impossible.

**Decision:** ADR-0008 introduces a pluggable holdings-provider model that emits positive evidence only — "this record or PDF is present" — and never a negative "not owned" claim; a source failure or incomplete read yields `unknown`, not a skip. Sources are declared file or command exports (for example a BibTeX export of papers already holding a PDF), their claims are unioned across sources, and a generic source can never route into zotio's Zotero-attachment flow, which stays its own concept.

**Why:** A false "owned" verdict silently withholds a paper the user asked for, while a false negative just costs one redundant download. The design biases hard toward the cheaper mistake, the same asymmetry provenance decisions use elsewhere.

## Ratified IPC surface for external consumers

**Context:** *papio*'s first external consumer, a citation-checking tool, needed to code against stable method names instead of an informal, changeable list, and needed papio to say how it could — and could not — identify who obtained a work.

**Decision:** ADR-0009 ratifies six read/act methods (job and action listings, receipts, adopting a component, and the orphan-only repair transition): their names, parameters, and result shapes are frozen and mechanically pinned by tests, and additive change always takes a new method name rather than widening an existing result. Bulk submission, a generic reopen verb, method aliases, and autonomous draining of open human actions are explicitly refused. *papio* never authenticates a person or holds institutional credentials, so it can report an entitlement *route* — for example, which resolver was used — but never an entitlement *subject*; inventing one would be a false rights record.

**Why:** Version skew is routine, since one binary is CLI, daemon, and native host, so a widened result would make an older CLI reject a newer daemon's response outright. Ratification turns that failure into a release-blocking local test instead of a break an external consumer discovers first.

## Ratifying single-work submission

**Context:** The ratified surface from ADR-0009 gave the external consumer no way to ask *papio* to acquire anything: every uncovered work was reported unattempted, and *papio*'s success rate became unmeasurable from the one side that was counting it.

**Decision:** ADR-0010 ratifies one additional method for exactly one work per call — never bulk submission, which stays refused for the reason ADR-0009 gives. Only a narrow field subset is frozen (identifiers, title, authors, year, an idempotency key, and an access-mode override that may only narrow the daemon's configured access mode, never widen it); operator-policy fields such as resolver choice and cost ceiling stay unfrozen so a consumer cannot pin *papio*'s policy vocabulary. The CLI calls the same ratified method rather than being a second surface.

**Why:** Ratifying parameters is the heavier promise: an older daemon receiving an unexpected new field rejects the whole call, not just the field, so only the smallest stable core is frozen. Submission was already reachable by anything that could call another ratified method, since the socket has no per-caller authorization — ratifying the verb adds a promise, not a new exposure.

## Ratified readers for collecting bundles and artifacts

**Context:** Once the external consumer began reading full acquisition results rather than just job state, no ratified method could return a bundle document or locate an artifact's bytes, and the two unratified methods that could were unsuitable to freeze: one writes files to disk as a side effect, the other exposes an artifact field that is unsafe to read across jobs sharing a content hash.

**Decision:** ADR-0011 ratifies two purpose-built readers instead: one returns the bundle as versioned JSON text, so the bundle's own schema can evolve independently of the RPC contract, and the other returns only the digest, size, MIME type, and file path needed to verify bytes — never the underlying persistence record. The two prior unratified methods stay served for the CLI but are not recommended to consumers.

**Why:** Freezing a returned struct freezes every future field on it; a text or narrow-projection reader lets *papio* keep evolving the underlying format without breaking the ratified contract.

## Provider limits are observed, never configured

**Context:** A large acquisition run once froze at zero throughput after hitting an unannounced provider quota. The incident showed *papio* had no way to know its own limits except by hitting them, that a configured number would have been wrong (it measured the unauthenticated tier, not the real one), and that a shared source's quota can be consumed by something other than *papio* entirely.

**Decision:** ADR-0012 holds that operator configuration may only make *papio* more conservative than a provider requires — never treated as evidence of how much headroom a provider actually has. *papio* tracks a provider's own signals (a retry-after header, a remaining-quota count) as observations bounded by a local safety ceiling, not as configured facts, and a negative rate or cost limit is rejected at load rather than silently disabling a gate.

**Why:** A configured limit encodes a belief about a provider that can be wrong the moment it's written down — proven here when the same source's quota changed meaning twice in one day. An unknown that resolves on first contact is safer than a confident, stale number.

## Activity feed, PDF hand-off delivery, and session visibility

**Context:** Operators needed three surfaces around an acquisition already in flight: a bounded view of what *papio* has done, a way to hand a manually obtained PDF back to *papio* without a second manual-download detour, and browser-local visibility into whether an institution session is actually signed in.

**Decision:** ADR-0013 exposes the daemon's append-only event history as a bounded, solicited activity read, also reachable from the CLI — no daemon push, reaffirming ADR-0005's rejection of that mechanism. A "send PDF to *papio*" browser action steers a download into a job-scoped folder and adopts it through the existing legal job-state transitions, never a new acquisition state. An institution-session card in the popup reports sign-in state from ranked, evidence-based browser-local observations — an explicit sign-out control found in the page, or a named-user session token — rather than guessing from URL shape, and that evidence never crosses the wire to the daemon.

**Why:** Every new surface routes through boundaries already established elsewhere: the CLI stays authoritative for capabilities, the browser stays the only place browser-local facts are known, and authentication is never inferred from a timing signal alone. Field validation after the initial design tightened the session verdict to ranked evidence and scoped institution sessions strictly to the resolver origin they were actually observed on, after early heuristics produced false verdicts.

## Consumer attribution and durable validation evidence

**Context:** A real cohort run exposed limitations that had to be worked around outside *papio*: no way to attribute which caller submitted a job for accounting, and structured validation findings — why a PDF passed or failed identity checks — were computed, used, and then discarded rather than kept.

**Decision:** ADR-0014 lets a job carry an optional `consumer` label, supplied at submission and bound to the acquisition rather than the reusable request row, that is explicitly a caller's own accounting tag — never an identity or a rights input. Structured validation evidence is now persisted per job and candidate, never by content hash (which would let one job's finding leak into another's), and returned by a new ratified reader; this reverses ADR-0007's earlier withdrawal of that evidence, whose actual constraint — never project a finding through a shared, content-addressed artifact — the new per-candidate keying still honors. A handoff open longer than a week is labeled stale for visibility, but nothing acts on that label automatically, and a narrow selector lets a consumer open one named handoff without becoming a mechanism for looping through the whole queue unattended.

**Why:** The consumer label answers "who submitted this" for accounting without pretending to answer "who is entitled to these bytes," which stays governed by ADR-0009's stricter rule. Keying validation evidence to a specific job and candidate avoids the exact cross-job leakage that got the evidence withdrawn in the first place.

## Rejected: runtime-configurable adapter amendments

**Context:** Provider adapters — the DOM rules that drive a download on a publisher's page — are compiled into the extension, so fixing one costs a store review cycle. A proposal considered letting a non-maintainer patch just an adapter's element selectors at runtime, on the theory that selectors are "location" and cannot grant new capability the way a URL template or an HTTP method can.

**Decision:** ADR-0015 rejects the proposal outright. Review found that a selector change can still redirect an adapter's authorization guard (which pages it may act on), its download endpoint, or a second, unproven follow-up click — none of which the location/capability split actually contained — and that the compiled fixtures used to certify equivalence cannot model click or navigation effects, only page snapshots.

**Why:** Capability lives in the composition of a guard, a selector target, and an action together, not in any one field, so no field-level split can be frozen safely as an untrusted party's amendment surface. If revisited, the reviewable direction is a mechanism that can only *suppress* automation — pin an adapter off, force it to human-assisted — never one that extends it.

## LibKey as an institution-aware routing layer

**Context:** Institutional handoff today ends at a bare OpenURL link. LibKey, a third-party service some institutions subscribe to, can resolve a DOI or PMID to a direct PDF, an article-landing page, a document-delivery route, or an integrity alert such as a retraction notice, scoped to a specific library's subscriptions.

**Decision:** ADR-0016 keeps LibKey resolution entirely in the daemon's existing institutional-routing stage, after open-access and sibling-version resolution and before the bare OpenURL fallback; the extension never calls LibKey directly or holds a LibKey key. It proposes two resolver modes, a keyless link mode and a keyed structured-API mode, configured per institution profile, with any LibKey error or empty result falling through to the existing OpenURL path. LibKey's own "best" recommendation is deliberately decomposed rather than followed blindly, since it ranks integrity alerts ahead of full text. Only the keyless link mode is decided and built; the keyed API mode and its presentation surfaces remain proposed.

**Why:** Keeping the integration daemon-side matches the rule that the extension originates no external request beyond the resolver page already open in the browser, and treating LibKey's result as several distinct signals — alert, open-access route, institutional route, delivery route — rather than one link avoids silently handing the browser an alert page instead of the article.

## Document delivery and interlibrary loan as a durable route

**Context:** When a work is only obtainable through interlibrary loan, *papio* recognizes that outcome internally but historically just gave up — no request record, no polling, no reconciliation if a human placed the request by hand outside *papio*.

**Decision:** ADR-0017 proposes a new durable, idempotency-keyed request table, fed by up to three route sources: an institution's OpenURL request form, a LibKey-resolved delivery route (ADR-0016), or a direct interlibrary-loan provider API. A pending request keeps the job in an existing retry-wait state rather than a human action, since nothing needs a person's attention while *papio* is only waiting on a reply. Automatic submission — as opposed to opening a prefilled form for a human to send — is proposed only for a narrow, explicitly configured case: digital journal articles, zero patron fee, no required human step, gated by a seven-point per-request check, and only after one supervised submission against the real deployment has succeeded.

**Why:** Treating delivery as a first-class, polled state instead of a dead end closes a real gap in acquisition coverage, but auto-submission on a person's behalf is powerful enough that the design deliberately keeps the bar for it high and explicit rather than inferring consent from configuration alone. This ADR is proposed and not yet built.

## Recording the browser session behind an adopted PDF

**Context:** When a human hands *papio* a PDF obtained through their own signed-in browser session, rather than *papio* fetching it directly, the exported acquisition record could not say so: every browser-obtained file recorded an institutional access basis with no route and no session evidence, which caused a downstream consumer's rights check to refuse every one of them.

**Decision:** ADR-0018 ties the `operator_browser_session` entitlement mode (see "Since ADR-0010" above) to freshly recorded evidence: it is emitted only when the accepted candidate's own row carries a session confirmed authenticated at that origin within a short recency window. A stale or merely-warm session doesn't qualify, and a candidate with no recorded evidence at all stays entitlement-less rather than being backfilled. The route recorded is the actual page origin the browser observed at hand-off time, never a reconstructed or inferred one.

**Why:** The same false-positive/false-negative asymmetry decided under provenance applies here: inventing session evidence that was never actually observed would be a false rights record, while omitting it only costs a field a consumer has to work around. Requiring specifically *fresh* evidence keeps the claim limited to what *papio* has good reason to believe.

## On-page bulk acquisition, one-shot and explicit

**Context:** A person looking at a page with many citations — a reference list, a syllabus, a table of contents — can visually pick out which ones matter in a way neither the CLI nor an automated crawl can. An early draft of this feature proposed a persistent, always-on content-script scanner watching every granted page for citations; that mechanism is rejected in favor of an explicit, one-shot scan.

**Decision:** ADR-0019 proposes a popup action that runs a single scan of the current tab only when clicked, with no persistent scanner, no page-mutation watcher, and no badge count. Detected identifiers open a dedicated selection workspace, reusing the full-tab pattern from ADR-0001, where a person chooses which papers to submit as one ordinary batch under the same cap any other batch submission uses. An existing host permission granted for one publisher page does not by itself authorize scanning; scanning has its own separate, revocable consent.

**Why:** The feature's unique value is a human's visual selection, not ambient detection, so the design deliberately stays invoked-only and reuses *papio*'s existing batch-submission and consent machinery rather than adding a second, browser-only acquisition policy.

## Grabbing the PDF you are already reading

**Context:** A browser tab rendering a PDF has no page to scan — Chrome's viewer exposes no DOM, so even a paper printing its own DOI on page one yields nothing. Meanwhile the application already holds everything needed to identify a PDF it possesses: front-matter identifier extraction, structural validation, and identity scoring against registrar metadata.

**Decision:** ADR-0020 makes the existing scan flow PDF-aware. Scanning a PDF tab first applies the ordinary URL identifier rules to the tab address itself; failing that, it offers a single explicit "grab this PDF" action. Accepting it captures the file through the browser's own authenticated download — steered into a reserved directory, never crossing the native-messaging transport — after which the daemon quarantines, validates, and extracts front-matter identifiers. An identifier yields an ordinary identifier-keyed job with the captured file claimed; no identifier parks as a human action. Capture always precedes job creation, so no job ever exists without a canonical work identity.

**Why:** The transport frame cap makes byte upload a non-starter, identity-less jobs would undermine every dedupe guarantee, and a grab that submitted title guesses would reopen the weak-match risk ADR-0019 closed. One click, one file, the standard pipeline — the wrong-paper guard applies to grabbed PDFs exactly as to acquired ones.

## Packaged behaviour, daemon-first repair, restrictive-only control

**Context:** Repairing a provider adapter costs a hand-written source edit plus two browser-store reviews, which does not scale to hundreds of provider families or to users who try the extension once and write it off. Measurement on a live installation showed page drift (`ui_changed`) dominating browser-side failures, and a primary-source policy check found that remotely updated selector/action rules for authenticated page actions sit in a gray zone Chrome has enforced against in comparable cases, while the closest academic precedent remains unresolved on Chrome's current manifest platform.

**Decision:** ADR-0021 keeps every guard, selector, and action class packaged in the store-reviewed extension. Repair speed comes from three mechanisms instead of a remote rule catalog: URL-shaped provider knowledge lives in the daemon and reaches the browser as candidate navigations through existing packaged primitives, deploying in hours without any store surface; an adapter patch generator turns a reviewed failure capture into a complete source change, fixtures, tests, and store submission; and a signed control document — restrictive only — can suspend or revoke exact packaged revisions immediately, holding nothing that could name a host, selector, or action. Remote activation of packaged-but-inactive revisions is deferred behind an explicit store policy pilot, and the rejected-for-now alternatives are recorded with revisit triggers rather than discarded.

**Why:** A signature authenticates who published logic; it does not make logic inert, and a store rejection would risk the only distribution channel. Restrictive-only control can only reduce what *papio* does with a person's session, so it carries none of the authority risk ADR-0015 refused — while daemon-side URL repair attacks the measured dominant failure class at update speeds no store review constrains.

## Institutional processing authority and staged enablement

**Context:** Institutional processing combines durable daemon decisions with
browser-local tabs, navigation, session evidence, permissions, and downloads.
The daemon must remain the authority for unmaterialized work without pretending
that SQLite can atomically revoke a browser effect already delivered to Chrome.
Strict old IPC clients also require additive diagnosis and feature-gated
protocol evolution.

**Decision:** ADR-0022 makes the daemon authoritative for jobs, policy,
institution profiles and revisions, candidate order, claims, permits,
cooldowns, suppressions, cutover decisions, and artifact winners. The
extension is authoritative only for physical browser resources and
browser-local facts. Claim-to-resource binding is acknowledged by both sides
and fenced by an opaque daemon holder generation. The three business identities
remain distinct: authentication claim, institution profile, and provider safety
domain; holder generation is an execution fence, not another identity. Fresh
external routes are transient and never durable.

Institutional cutover is observed before it is enabled. One closed blocker and
one canary-readiness flag are written transactionally with the decisive
transition, while diagnosis v2 adds detail without changing diagnosis v1.
Automatic first-route behavior requires exact provider/profile/route readiness;
source-gate bypass requires its own canary; and effect concurrency four waits
for broad-at-one evidence. Phase −1 is complete and Phase 0 is current.

**Why:** This preserves ADR-0003's holder boundary, ADR-0013's browser-local
facts, ADR-0016's daemon-side institutional routing, ADR-0018's fresh-evidence
entitlement rule, and ADR-0021's packaged positive behavior. It also keeps
ADR-0006's compatibility gate, ADR-0009's frozen result shapes, ADR-0012's
observed-provider-limit rule, and ADR-0017's configured delivery gates intact.
Direct-to-main implementation stays in small, solo-maintainer-sized changes;
the staged gates make measurement and rollback possible without adding
speculative confirmation prompts.