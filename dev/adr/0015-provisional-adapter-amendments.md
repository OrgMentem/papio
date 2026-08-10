# ADR-0015: Runtime adapter amendments are refused; capability is not a field

Status: **Rejected** (2026-08-06). The proposal recorded here was written, reviewed
against the code, and rejected the same day. It is kept because the refutation is
reusable: the field-classification error it makes is the one anybody will make
next time this comes up, and the review turned up two live defects in shipped
code that have nothing to do with amendments.

Governs `extension/src/adapters/`. Constrains any future proposal to let a
non-maintainer change adapter behaviour at runtime. Supersedes nothing; the
adapter registry's compiled-in, fixture-backed contract stands unchanged.

## Context

An adapter is a declarative spec in `extension/src/adapters/types.ts`, interpreted
by one pure DOM-only function, compiled into the extension bundle. Repairing one
costs an extension release, and two builds are store-distributed, so there is a
review queue papio does not control.

Measured on one real install: **113 `browser.provider_outcome` events, 103
`ui_changed`, exactly 4 with no adapter at all.** Runtime *adapters* would chase
the 4. That was refused on the obvious grounds — a spec carries `method` and
`urlTemplate`, so an untrusted one fetches an arbitrary HTTPS endpoint through the
privileged downloads API with the operator's institutional cookies.

The follow-up question was whether a *narrower* runtime object — an **amendment**
that changes only selectors on an adapter that already exists — could chase the 99
safely. This ADR was drafted to say yes. It was wrong.

## The rejected proposal

`DownloadRule` was claimed to split cleanly into two field classes: **location**
(`selector`, `shadowSelector`, `postClickWaitFor`, `followupSelector`, and the
selectors inside `classify`) and **capability** (`method`, `urlTemplate`,
`idPattern`, `jsonField`, `requiresTermsConsent`, `requireKind`, `hosts`). An
amendment would touch location only, so — the argument went — it could not
introduce an endpoint or change a fetch mechanism, and its worst case was
"downloading a different link from a page the operator already granted, caught
downstream by payload validation."

## Decision: refuse it. The split is false

**Capability is not contained in a field. It is the composition of a guard, a
selected target, and an action:** `effect(page) = if guard(page) then
action(locator(page))`. Freezing `method` while letting an untrusted amendment
move either the guard or the locator does not freeze the capability.

Field by field, by effect rather than by name:

| "location" field | what it actually decides |
|---|---|
| `classify.*` selectors | the **authorization guard** — which pages papio may act on at all |
| `download.selector` + `href` | which **endpoint** is requested |
| `download.selector` + `url`/`api` | on which page states papio issues the compiled **authenticated request** |
| `download.selector` + `click` | which **arbitrary DOM action** fires |
| `shadowSelector` | which action inside an open component |
| `followupSelector` | a **second** arbitrary action |
| `postClickWaitFor` | state-machine timing, and whether reclassification proceeds |

Three consequences kill the proposal outright.

**Classification selectors are authorization.** A decisive verdict has operational
consequences: `article` starts a download or a click; `login` can append an
institutional account id or walk into federated login; `terms` can accept terms
when consent was previously recorded; `no_entitlement` and `wrong_work` settle the
handoff and close the governed tab. Freezing `requireKind: "article"` is
meaningless when an amendment redefines which pages satisfy `article`.

**`href` does not bound the endpoint.** `extractDownloadURL` requires an anchor,
HTTPS, no userinfo, and not-the-page-itself. It imposes **no same-origin or
compiled-path policy** — verified in `background.ts`. A moved selector can pick an
anchor to any origin. Worse, the fixture sanitizer strips query strings while
production reads the live `href`, so an offline checker does not possess the URL
it would be authorising. And issuing the request is itself the effect: validation
runs after, and a structurally valid PDF is not proof it is the intended work.

**`followupSelector` is a second arbitrary authenticated click.** This is the
clearest instance and it was hiding in plain sight in a field the draft called
"location". Verified in `clickDeclaredDownload`:

- the follow-up is searched with `document.querySelector` over the **whole
  document** (`background.ts:1024`), not scoped to a modal or the clicked subtree;
- a match that **already existed before the first click** is accepted, because
  `findAppeared()` runs before any wait (`background.ts:1030`);
- it is then clicked (`background.ts:1052`) with no proof of containment, causal
  appearance, purpose, visibility, or terms-consent status.

The spec comment — "for provider-owned download modals; never terms/consent
controls" — is documentation, not enforcement. The article path latches
`download_initiated` and then performs this action, and **downstream PDF
validation cannot undo a form submission, a navigation, an accepted terms
agreement, or a purchase.**

Excluding `method: "click"` (three adapters: `informit`, `annualreviews`,
`jamanetwork`) does not repair the split, because `href` still chooses endpoints
and `url`/`api` selectors still gate privileged requests. The minimum honest rule
would have been *no `download.*` field is amendable by an untrusted party,
regardless of method* — at which point the mechanism no longer addresses the
failures it was proposed for.

## Decision: a static corpus cannot become an author-independent security boundary

The draft's admissibility rule — an amendment is admissible if it changes no
verdict across every committed fixture and changes the verdict on the motivating
capture — was the part it leaned on hardest. It does not carry that weight.

The fixtures are **final-DOM snapshots with query strings stripped and scripts
emptied**. They cannot model what a click does, what a follow-up activates, what a
live `href` resolves to, or any temporal behaviour. A corpus that intentionally
omits action semantics cannot certify an object whose entire risk is action
semantics. Equivalence against it is a **regression check**, which is what it was
built for, and mistaking a regression check for an authorization proof is the
central error.

It is also weakest exactly where it would be needed most: an adapter with one
`success` fixture has almost no gate, and a thin corpus is correlated with a
poorly-understood provider.

## Decision: the safe untrusted runtime object reduces automation, it does not extend it

The reviewable insight worth keeping. If a non-maintainer is to influence adapter
behaviour at runtime without a new trust boundary, the structurally safe direction
is **suppression** — disabling a rule, forcing a provider to human-assisted, or
pinning an adapter off — not amendment. Suppression can only ever cause papio to
do *less* with the operator's session. Amendment teaches papio *when and where* to
exercise browser authority it already has, which is the thing that needs proving.

Any future proposal in this area should start there.

## Decision: the gate in the draft measured the wrong thing

The draft gated the work on "what fraction of `ui_changed` outcomes are drift
rather than original under-specification". That ratio does not measure the value of
amendments: a source fix for either cause waits in the same store queue, and the
claimed benefit — avoided release latency — applies equally to both. It was also
three hand-picked repairs with no stated threshold, which is not an actionable
gate.

The honest numerator is **works blocked during the interval between a source fix
existing and an updated extension being installed.** Measure that, not causes.
`dev/active/adapter-release-latency-plan.md` records what to build and what to count.

Note also that the 99 figure does not establish papio "reached the right article
page". `ui_changed` proves only that a registered adapter did not classify the
eventual page; ScienceDirect's purchase wall is a live example where the page was
the wrong one.

## Live defects this review surfaced, independent of amendments

Both are in shipped code and neither requires an amendment mechanism to matter.
Recorded here because they were found here; fixing them belongs to
`extension/src/background.ts`.

1. **`followupSelector` clicks an unproven target.** Whole-document search, a
   pre-click match accepted as if it had appeared in response, no containment or
   purpose check, and only a comment standing between it and a terms or purchase
   control. It should at minimum be scoped to the clicked element's subtree or a
   containing dialog, and required to have *appeared* rather than merely to exist.
2. **`extractDownloadURL` has no origin policy.** A compromised or merely wrong
   selector yields a credentialed cross-origin download. A same-origin default,
   with any exception declared in the spec and fixture-backed, would bound it.

## What stands

The adapter registry stays compiled-in and fixture-backed. The path from a broken
provider to a working one stays: capture → `adapter-try` offline → source change →
release. The investment goes into making that path fast, per
`dev/active/adapter-release-latency-plan.md`.

If someone returns to this, the bar is: a mechanism whose safety comes from what
it structurally cannot express, evaluated by an oracle that models the *action*
and not just the DOM, or a suppression-only design that cannot increase what papio
does with the operator's session.
