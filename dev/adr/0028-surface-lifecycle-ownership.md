# ADR-0028: papio owns every surface it creates

Status: **Accepted** (2026-09-03). This decision fixes the authority split, the
cardinality rule, the storage tiers, and the invariants for every browser
surface papio creates for institutional work. It supersedes and inlines the
normative remainder of five earlier attempts at the same problem, so no plan
document is load-bearing for it any more.

This ADR is self-contained on purpose. Earlier rounds of this work left their
banned-approach list, their accepted boundary, and their abort criteria in plan
files that pointed at each other, so no file could be deleted. Everything still
normative is restated below.

## Context

The field failure, observed in the operator's own browser on 2026-08-18: one
papio tab group held about seventeen tabs. Six or more were identity-provider
sign-in tabs. Several were unrendered discovery-layer tabs. One was a provider
error page. Extension reloads produced duplicate groups and windows. Drives
fired into a dead network after the machine woke. Siblings stayed stranded at
sign-in walls after the operator authenticated in one tab.

The operator's directive: papio owns the full lifecycle of every surface it
creates. A tab passes to the operator only when papio asks for a human action,
which is rare by design, or when the operator takes the tab over.

Five earlier attempts did not land this, for three reasons worth recording:

- **The replacement architecture already existed, disabled.** ADR-0022 shipped
  daemon-owned materialization claims, the two-party claim, bind, route,
  navigated and reconcile sequence, opaque self-identifying scaffolds,
  paginated reconciliation, and effect permits. Every earlier attempt patched
  the legacy offer and drive path that machinery replaces, and the ADRs forbid
  a second authority beside it.
- **The scope boundary sat on the wrong path.** Attempt five fixed cold
  human-action offers. The pileup lived in the automated warm-session drive
  path.
- **Risk asymmetry became taboo.** Reviews punished closing a tab, which had
  produced real time-of-check-to-time-of-use defects, and never punished
  leaving one open. The banned-approach list was then read as "lifecycle
  ownership is forbidden", which froze policy along with mechanism.

## Decision 1: The authority split

The **daemon** owns jobs, candidate ordering, the opaque authentication claim
and its authentication-entry lease, holder generation, materialization claims
and bindings, typed human-gate occurrences, dependent counts, durable park and
retry state, sibling resume scheduling, effect permits, and terminal and detach
dispositions.

The **extension** owns browser-local facts: physical tabs, groups and windows,
binding acknowledgements, wall, landing and error observations, operator
engagement and cession, the current connectivity observation with a short probe
lease, and the guarded close primitive.

Loss of worker memory never authorizes a replacement tab, a close, or a gate
resolution.

## Decision 2: Cardinality is per authentication claim

One unresolved human sign-in surface exists per **daemon-issued authentication
claim**. The rule is not "per institution".

Institution-profile evidence, which names an exact profile and revision, and
provider safety domains are separate axes. A claim may group profiles that
share one human entry. A profile may span unrelated provider safety domains,
whose parked scaffolds are limited independently, alongside the global effect
permit. Resolving a claim never asserts entitled session evidence for every
profile grouped under it.

## Decision 3: Storage tiers

`chrome.storage.session` holds re-derivable mirrors only: the binding-to-tab
map, the observation outbox, papio-issued action tokens, page epochs, the
pending close transaction, and advisory deadlines. Loss of that tier means
retain and do not drive. It never licenses inference.

`storage.local` holds settings plus a URL-free birth certificate for each owned
surface: an opaque binding id, a tab-id hint, the purpose, the browser-session
epoch, the extension generation, the creation timestamp, the cession state, and
a pending-close tombstone. It records no route URLs, titles, digital object
identifiers, hosts, entity material, candidate ordering, or accepted-work queue.
The extension is never a durable queue, per ADR-0022 Decision 1. A browser-start
epoch invalidates old tab-id authority. After a browser restart, only a
self-identifying scaffold is remapped automatically.

**Scope note.** This digest-only promise covers the lifecycle state this
decision introduces: the birth certificate above, and the claim, observation and
close frames. It does not narrow the pre-existing
`institutional_materialization_v1` candidate-offer, claim, bind and route
family, which shipped earlier in `0b716b3`. Those frames carry provider hosts, a
bounded title or identifier hint, and institution login identifiers by design,
because the extension needs them to navigate and verify a route. That route
material is never itself persisted.

## Decision 4: Banned approaches, and the two narrow amendments

The following stay banned. Each one delivered no operator value and caused a
regression: `forceNew`, cross-job engagement locks, `federatedLoginVisited`,
sign-in-count rewrites, owner-age or URL-shape liveness, unknown-state
re-probing, universal backstops, ledger-based close authorization, a rewritten
tab-removal state machine, and changes to the central removal, governor,
work-window or page-capture paths beyond explicit per-caller transitions.

Two amendments, taken as operator decisions:

- **Automatic waiter-tab closure is re-permitted, narrowly.** It applies to
  scaffolds only, through the close transaction in Decision 6, and never to
  engaged, active, PDF or adopted content. Unknown engagement means retain.
  *Superseded 2026-09-23 for content:* PDF and adopted content now close too,
  and "active" means in front of the operator; see the amendment below.
- **Owner-age and URL-shape liveness stays banned.** Retirement and resume ride
  daemon claim state and explicit transitions only.

Reaffirmed rules, restated so they survive without the plans that held them:

- Missing federation metadata is a structured engagement failure. It is never
  permission to pre-open a surface. Work that requires authentication goes
  tabless without a granted claim.
- papio does not retain content (amended 2026-09-23, see "Amendment
  2026-09-23" below). A filed, terminal or superseded paper's tab closes
  through the close transaction; only a pinned or moved-out tab is kept, and
  the tab in front of the operator waits for a later pass.
- The forbidden pattern is an unrelated await between check and act, not the one
  authoritative freshness read. No shadow state is reconstructed.
- The work window is never closed directly. Chrome discards it when its last tab
  closes.
- A mutex wrapped around a navigate-then-write order is not sufficient. The
  owner write must land before `tabs.update` or `tabs.create`, never after.

## Decision 5: The accepted takeover boundary

Holder-exclusive dispatch guarantees one owner under a stable holder. It does
not fence an explicit mid-engagement takeover: a demoted holder can hold an
authorized URL and still call `tabs.create`, and a demoted holder with an
already-driving tab can process a login-wall event without making any request.

papio accepts that boundary and states it rather than discovering it later.
Fencing it needs daemon session-epoch and pending-engagement state, not daemon
ownership of the tab lifecycle. Revisit only if a takeover during sign-in is
observed in the field.

## Decision 6: The lifecycle invariants

These are the invariants the shipped implementation enforces.

- **A claim precedes a surface.** No tab exists for work that requires
  authentication without a daemon-granted claim. Creation is scaffold-first, and
  every create, bind and cancel path rolls back, including the reuse branch.
- **One unresolved human surface per authentication claim.** The daemon claim
  transaction and a monotonic event reducer enforce it.
- **Resume is a one-shot transition.** It is keyed by gate-occurrence identity
  and event ordinal. Duplicates acknowledge idempotently. A stale holder,
  binding or ordinal mutates nothing. Warm probe evidence never mints a surface.
  Owner closure without success commits abandonment and leaves dependants
  tabless.
- **Warm evidence is not a lease.** It is scoped to an exact profile and admits
  an attempted route only. A wall bounce converges on the claim at wall
  observation.
- **In-place renavigation is fenced.** A claim-resume redrive re-reads the tab
  immediately before it updates it, and never renavigates an operator-active
  tab.
- **Positive evidence closes; absence retains.** A close requires a one-use
  daemon authorization. Absence from a bounded offer batch, a `goodbye` frame,
  timer expiry and transport loss are all insufficient.
- **Operator cession is causal.** Only pinning a tab or moving it out of
  papio's container cedes it (amended 2026-09-23: activation and a touch
  during a close defer the close instead, so papio's focus tokens are gone).
  Ambiguity about whether the operator is looking means defer, not cede.
- **Every dead end has a daemon-side disposition.** A navigation error is
  observed before authentication detection, charges no authentication attempt
  and applies no cooldown. Classify exhaustion is reported by the extension with
  the exhausted page epoch, and the daemon commits the terminal park. Daemon
  cancel is the third disposition.
- **One readiness barrier gates every effect-producing entry point.** The
  barrier covers managed-state load, birth-record validation, a scaffold scan, a
  complete paginated reconcile, tombstone replay, and group and window adoption.
  Native offers, runtime opens, queue drains, materialization retries and close
  paths all await it. Hello and poll frames and read-only requests stay
  responsive. A bounded scan fails closed to no adoption and no close. A
  session-restore grace pass runs once.
- **Lifecycle work never rides the global effect permit.** Adoption scans, lease
  renewal, claim observations, group folding and terminal reconciliation use a
  separate lifecycle mutex. Only irreversible provider navigation, page mutation
  and download initiation acquire the effect permit.

## Decision 7: An in-flight provider effect is not a free sign-in slot

The authentication-entry lease getter expires a past-deadline reserved lease in
place, so a genuinely lapsed slot arrives as expired and needs no extra help.
The one case where it deliberately returns a past-deadline lease still marked
reserved is when that binding's institutional effect permit is held or of
unknown completion, because an irreversible provider action may be in flight at
that moment.

A freshness or lapse check at that call site reads exactly that state as free
and offers a second paper a sign-in at the same institution. Such a check is
therefore forbidden. Do not reintroduce one as an obvious guard.

## Amendment 2026-09-23: papio does not retain content

The operator, 2026-09-23, verbatim: "Papio should not retain tabs for articles or PDFs … The idea is that the tab would be automatically closed after it's acquired or other relevant lifecycle states, and a toast or something would allow the user to re-open it if they needed (or they can do so via the browsers own tools). Fix the defects that cause multiple tabs like the redrives, etc. and the lifecycle tab management."

papio no longer retains content. A surface papio owns (birth record, not ceded,
inside its group or work window, same browser epoch) closes, PDF or article
included, through the close transaction when its job is adopted, reaches a
terminal state, or is superseded by a newer attempt (redrive, re-offer, fresh
link, retry). A cold parked surface closes even on a PDF; the inbox and
`papio actions open` reopen it on demand. Exactly two guards remain. A tab the
operator pinned or moved out of papio's container is ceded and never closed.
The tab the operator is looking at right now (the active tab of a focused,
non-minimized window, or a tab touched during the close) is deferred, never
ceded, and a later pass closes it. Activating a tab no longer cedes it. Before
a new or reused surface is recorded for a job, papio retires the job's other
owned surfaces as `surface_superseded`. When papio closes the tabs of papers it
filed, one toast per batch offers to reopen them from URLs held in worker
memory only.

The daemon side: `job_inactive` and `surface_superseded` also authorize the
tab of a binding whose own claim is settled or abandoned, because such a
binding drives nothing; an unsettled effect permit on that claim still vetoes.
No enum or wire field changed.

## Acceptance scenarios

These are the scenarios that verify this decision. They are stated here, not in
a plan, so they cannot vanish.

1. **Old daemon, or the feature not negotiated.** Zero autonomous tabs for work
   that requires authentication. An explicit Open still works. No legacy path
   pre-opens a surface.
2. **Extension update.** Session storage is wiped and the fakes survive. Zero
   new groups or windows appear. Scaffolds are adopted or retained, and none
   closes without a one-use authorization. Re-offers create no duplicate claim
   surface.
3. **N institutional jobs on a cold session.** Exactly one sign-in tab exists
   per authentication claim, and the daemon parks the other N minus one. One
   successful login resumes all N on fresh routes, and the scaffolds retire. An
   owner closed without success creates zero new surfaces. Duplicate or late
   claim events, carrying an old holder generation or a lower ordinal, mutate
   nothing.
4. **Wake flood.** With four offers and the network down, zero tabs appear,
   because the probe precedes release. Once the machine is online and the probe
   passes, drives are paced.
5. **Navigation error and classify exhaustion.** The daemon commits the park and
   the scaffold closes. No authentication attempt is charged. No cancellation is
   emitted, because the tombstone is consumed. Exhaustion survives a worker
   restart.
6. **Operator-active parked tab.** It is never renavigated, and it is not
   closed while it is in front of the operator. Once it is not, and cold, it
   closes (amended 2026-09-23). Pinning cedes it, and that survives a worker
   restart.
7. **Firefox below version 139.** Group identity degrades to the work window
   without silently becoming work-window ownership. A full event-page teardown
   occurs between every protocol step.

Live-only behaviour is deliberately not covered by any suite. Synthesized
activation is refused by extension gesture tracking, so a handoff smoke run
needs the operator surface and a real pointer click for an extension reload.

## Abort criteria

Stop and reassess, rather than starting another fix round, when any of these
holds:

1. A review round introduces a new defect of a class already fixed in an earlier
   round. That is the signature of accretion rather than convergence.
2. Two consecutive rounds fail to reduce the count of open findings.
3. Any unit of work exceeds roughly four hundred changed lines outside tests.
4. A fix requires weakening an invariant above.
5. A change requires widening the IPC response shape or the timing-only
   authentication frames. Skew is fail-closed, so such a change stops and is
   redesigned behind a new method or a negotiated feature.

## Working discipline

- One owner for the extension background module per unit of work. Two agents
  never edit it at the same time. Parallel work is allowed only across frozen
  contracts and disjoint surfaces: the daemon resolver and handler, protocol,
  schema and corpus parity, and the harness.
- Do not split the background module first. Most findings in the reverted
  attempt were design and integration defects, not collateral deletions, so a
  whole-file split is another large rewrite with no value. New transition logic
  goes into a small pure reducer with explicit inputs and commands.
- State an exact deletion manifest for every edit round, and check the diff's
  removed lines against it before review.
- Run the full suite. Per-file runs hid breakage repeatedly. A hang counts as a
  failure.
- Never weaken an assertion to make a test pass. A changed expectation needs a
  stated justification that names the mechanism which no longer exists.
- **Re-verify any document that gates work against the tree before honouring
  it.** This is recorded as a decision because it cost real time twice: a
  harness-gap list named four missing seams as the reason work could not
  proceed, and three of the four had already shipped. A blocker list ages
  exactly like a claim list, and only the claim list was being audited.

## Consequences

Protocol work for this decision lands at four-site parity: the Go validator,
the TypeScript parser, the JSON schema, and the fixture corpus, behind a
negotiated feature. The timing-only authentication frames are never widened.

The mechanism shipped across five units of work, from `b550a9d` through
`5b866d2`, plus later field-round fixes including `d6ef1df` for Decision 7 and
`f196ded`, which gates the materialization pipeline on the same connectivity
clause every legacy drive path already used.

Two named limits remain accepted, not resolved: the takeover boundary in
Decision 5, and the fact that field evidence from real institutional traffic,
rather than a green suite, is what confirms the invariants above hold in
practice.
