# Attempt five: institutional sign-in handoffs without tab litter

Supersedes the reverted restructure (`dev/scratch/oracle/login-restructure-reverted.patch`).
Written after a four-angle plan review and one iteration round with the
reviewers; findings in `dev/scratch/oracle/login-review-findings.md`. This
document is the plan of record.

## Audit 2026-08-16 — verified against the tree, not against this document

- **SHIPPED** — claim/tab state stays browser-side — `extension/src/state.ts:284-288`, `extension/src/federated-claim.ts:10-88`; ADR-0003 and ADR-0013 retain holder-exclusive handoff and browser-local authority
- **SHIPPED** — Slice 1 guarded close primitive — `extension/src/background.ts:3219-3280` `closeOwnedTab` (fresh `tabs.get`, content/active/surface checks, then remove); lifecycle callers at 4087, 5526, 6066, 9466, 10987, 12622; completeness pinned by `extension/test/tab-window-close-ast.test.ts`. This is the shipped NARROWER REPLACEMENT for the reverted total-closure redesign, not that redesign.
- **SHIPPED** — Slice 2 truthful harness and SAML journey — `extension/test/fake-tabs.ts:63-225` (cloned snapshots, absent-ID errors, lifecycle events, completeNavigation/userNavigate/userActivate/userClose) and `:300-328` (fresh execution tokens, expired replay)
- **SHIPPED** — Slice 3 cold tabless offers and explicit engagement — `extension/test/background.test.ts:1775-1912` proves no tab/request/persisted URL before click, one correlated request+tab after, reservation before create, re-request after restart, manual park on timeout; implementation `extension/src/background.ts:5384-5516` and `:5591-5630`
- **SHIPPED** — Slice 3 shared fresh URL resolution — `internal/app/action_url.go:87-105` `ResolveHumanActionURL`, used by `internal/browser/bridge.go:2785-2845` for `handoff_link` (shared route, not duplicated)
- **SHIPPED** — Slice 3 protocol/schema/corpus parity — `internal/protocol/protocol.go:1179-1194`, `extension/src/protocol.ts:779-794` and `:4038-4054`, `protocol/browser-v1.schema.json:2139-2188`, plus `testdata/protocol/{valid,invalid}/browser-handoff*`
- **DEFERRED-BY-DESIGN** — Slice 4 explicit refresh — the plan's own rule: "If Slice 3 removes the accumulation, stop"; no unconditional refresh redesign is present
- **DEFERRED-BY-DESIGN** — accepted mid-engagement takeover boundary — the plan says "We accept that boundary for now ... Revisit if takeover-during-sign-in is ever observed in the field"; the tree has holder-only dispatch but no session-epoch fence
- **DEFERRED-BY-DESIGN** — the explicit "Do not re-attempt" list (forceNew, cross-job locks, federatedLoginVisited, owner-age/URL liveness, universal backstops, automatic waiter closure, rewritten removal state machine)

Trim candidate — Slices 1-3 shipped, while Slice 4, the accepted takeover boundary, and the do-not-re-attempt list are the still-normative remainder worth salvaging to an ADR.

## Trimmed 2026-08-17

Sections describing shipped work were removed. The pre-trim text is recoverable in
full at `git show 2d29e7a:dev/active/login-handoff-plan.md`. Cut: Slice 1 — one
guarded close primitive (LANDED), Slice 2 — make the harness tell the truth
(LANDED), Slice 3 — cold offers are tabless until engagement (LANDED).

## The failure being fixed

papio parks a job needing an institution sign-in behind a per-institution claim,
tabless. A ten-minute governor cleared each waiter's marker; a job with no marker
is an ordinary parked job, so it drove again and opened its own login tab.
Fifteen handoffs over twenty-one hours produced fourteen SSO tabs, every one
stale — a Shibboleth flow token expires in minutes, so they were dead forms
inviting credentials into flows that could not succeed.

The governor is already deleted (shipped). This plan removes the remaining
sources: papio creating institutional login surfaces nobody asked for, and papio
being able to close tabs it should not.

## Resolved: where claim state lives

Two reviewers reached opposite conclusions; after one iteration both converged on
the same answer, which the ADRs support.

- Claim/tab state stays **browser-side**, with `storage.session` as the
  authority. ADR-0013: "login visibility is a browser-local overlay", "the
  extension remains the reporter of browser-local facts". The daemon cannot
  observe tab removal or active-tab state — the very facts that retire a claim —
  so a daemon-side owner would arbitrate on delayed observations.
- The cross-process hazard is closed by **holder-exclusivity, not migration**.
  ADR-0003: "only the offer/handoff flow is holder-exclusive". The reverted patch
  admitted `handoff_link_request` from non-holders, letting a former holder with
  stale state open a handoff in the wrong browser. Holder-only dispatch closes
  that without moving state.
- The daemon's only new responsibility is **durable action validation and fresh
  URL resolution**, through one resolver shared with `papio actions open`.

**Accepted boundary (decide, do not discover):** holder-only dispatch guarantees
one owner under a stable holder. It does not fence an explicit mid-engagement
takeover — an old holder can hold an authorized URL and execute `tabs.create`
after demotion, and an old holder with an already-driving tab can process a
login-wall event after takeover without making any request. We accept that
boundary for now and state it; fencing it needs only daemon session-epoch /
pending-engagement state, not daemon ownership of the tab lifecycle. Revisit if
takeover-during-sign-in is ever observed in the field.

## Sequence

Each slice ships independently and is verified before the next starts.
Ordering note: closure-ban precedes harness work deliberately. Its safety is
structural (capabilities deleted + AST test) and does not depend on event
fidelity, whereas building a faithful fake first would model `closingTabs` and
cancellation semantics that Slice 1 immediately deletes.

### Slice 4 — only if field evidence still warrants it

Explicit operator refresh of an already-owned stale in-flight tab, minting a new
URL into that same tab. **If Slice 3 removes the accumulation, stop.**

## Explicitly dropped

Do not re-attempt: `forceNew`, cross-job engagement locks, `federatedLoginVisited`,
sign-in-count rewrites, owner-age or URL-shape liveness, unknown-state re-probing,
universal backstops, automatic waiter-tab closure, ledger-based close
authorization, the rewritten `onTabRemoved` state machine, or changes to the
central removal/governor/work-window/page-capture paths beyond the explicit
per-caller transitions in Slice 1. These delivered none of the operator value and
caused the final round's TOCTOU, persisted-URL, intentional-close and
drive-release regressions.

Relocated from the trimmed Slice 1-3 text: papio never auto-closes content —
only papio's own scaffolding (job/handoff/capture tabs) closes. The forbidden
pattern is UNRELATED awaits between check and act, not the one authoritative
freshness read; no shadow-state reconstruction. The work window is never closed
directly — Chrome discards it when its last tab closes. Do not auto-complete
navigation in the harness — Chrome completes later and tests must control it.
Missing federation metadata is a structured engagement failure, never
permission to pre-open. A mutex wrapped around the reverted order (navigate,
then write the owner) is not sufficient — the owner write must land before
`tabs.update`/`tabs.create`, never after.

## Working discipline

- **One owner for `background.ts` per slice.** No two agents edit it
  concurrently. Parallel work only across frozen contracts and disjoint surfaces:
  daemon resolver/handler, protocol+schema+corpus parity, harness.
- **Do not split the 9,000-line file first.** Only two of the final eight
  findings were collateral deletions; the rest were design/integration defects,
  so a whole-file split is another large zero-value rewrite. New transition logic
  goes in a small pure `handoff-lifecycle` reducer with explicit inputs and
  commands, leaving `background.ts` as the Chrome/port dispatcher.
- **Exact deletion manifest.** Six times this session an edit silently dropped an
  adjacent line — twice changing unrelated semantics. Every edit round states
  what it intends to delete, and the diff's removed lines are checked against
  that manifest before review.
- **Full-suite runs only.** Per-file runs hid breakage repeatedly; a hang counts
  as a failure.
- **No assertion may be weakened to make a test pass.** Softening was explicitly
  forbidden and still happened twice. A changed expectation needs a stated
  justification naming the mechanism that no longer exists.

## Abort criteria

Stop and reassess rather than starting another fix round when any holds:

1. A review round introduces a new defect of a class already fixed in an earlier
   round — the signature of accretion rather than convergence.
2. Two consecutive rounds fail to reduce open findings.
3. Any slice exceeds roughly 400 changed lines outside tests; the reverted
   attempt was 2,300.
4. A fix requires weakening an invariant listed above.

## Outcome

Cold institutional offers no longer create a browser surface before explicit
engagement. One click mints one fresh route and one managed tab; sibling papers
share its opaque institution claim, and closing or completing the owner resumes
them without opening tabs on their behalf. Slice 4 stays conditional: collect
field evidence before adding an explicit refresh for an already-owned stale
in-flight tab.
