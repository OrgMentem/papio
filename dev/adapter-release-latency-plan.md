# Adapter release latency — make the source path fast, then measure the harm

## Context

Governed by **ADR-0015**, which rejected runtime adapter amendments. Read its
Decision sections before proposing anything runtime-shaped here.

The problem amendments were reaching for is real: a provider breaks, papio has a
working fix in minutes, and operators wait for a store review papio does not
control. The rejected proposal tried to bypass that queue with a new runtime trust
boundary. This plan attacks the queue instead — and, critically, **measures how
much harm it actually causes**, because that number has never been collected and
the case for any future runtime mechanism depends on it.

Nothing here needs a new trust boundary, a protocol change, a config field, or a
store migration.

## What already exists

| piece | where | state |
|---|---|---|
| capture the page that failed | `extension/src/observe.ts` | shipped; budget re-keyed to failure shape 2026-08-06 |
| evaluate a spec offline | `extension/tools/adapter-try.ts` | shipped 2026-08-06; per-rule breakdown, names the selector that cost the match |
| fixture corpus | `extension/fixtures/<id>/` | shipped; six scenarios, seven providers carry a deliberate `drift.html` |
| dual-target build | `extension/build.ts` | shipped; emits Chrome `dist/` and Firefox `firefox/` |

The diagnosis half of the loop is already fast. Everything below is about the
distance from *diagnosis* to *installed*.

## Step 1 — instrument the interval nobody has measured

This comes first because it is the evidence every later step is justified by, and
because it is cheap.

Per unique provider defect, record: capture taken → source fix merged → build
submitted → store approved (**separately per store**) → update installed →
first successful acquisition. And across that window: **how many works and
operators were blocked.**

Deduplicate defects by `(adapter id, adapter version, host, capture digest)`. Raw
`ui_changed` counts conflate one defect hit many times with many defects, and the
113/103/4 figures in ADR-0015 have exactly that weakness.

Two of these are already derivable from the events table; the store timestamps are
manual until Step 4. Start with a hand-maintained table in this file rather than
building a pipeline for a number that may turn out to be small.

**Decision point.** If the blocked-work integral is small, the whole area is
closed and ADR-0015 was right for a second, stronger reason. If it is large, that
is the first real argument for a runtime mechanism — and it must then clear
ADR-0015's bar, not route around it.

## Step 2 — capture to source patch, mechanically

`adapter-try` already says *which selector failed*. The remaining manual work is
turning that into a committed change.

Extend it (or add a sibling command) to emit, from a stored capture: the proposed
`types.ts` edit, the fixture file copied into `extension/fixtures/<id>/`, the test
case, and the adapter `version` bump. The version bump must be **enforced, not
suggested** — an adapter whose behaviour changed without a version bump breaks
provenance, and ADR-0015 relies on version identity meaning something.

Constraint from the house rule, and it is the one this step is most likely to
violate: **do not add a third implementation of rule matching or URL resolution.**
`adapter-try` already reimplements `href`/`meta` resolution locally with a
lockstep comment, and a lockstep comment is a promise, not a mechanism. Prefer
exporting the real extractors from `background.ts` in a form importable without
`chrome.*` at module load, and have both the tool and production use them. If that
refactor is too large for this step, say so explicitly rather than adding a
fourth copy.

## Step 3 — a documented local-build path for technical operators

The honest interim answer for someone blocked today. `make dev-deploy` already
builds both bundles and deploys the daemon; what is missing is a written,
tested path for *loading the rebuilt extension* on each browser, and a clear
statement of what that costs (unpacked extension, manual reload, no auto-update).

This is not a workaround to be ashamed of — it is the same authority model as a
store build, just with the operator doing the distribution. It is strictly safer
than a runtime amendment because the artifact is still a reviewed source change.

## Step 4 — automate the release path

In dependency order:

1. CI builds both targets and runs `bun run typecheck`, `bun test`, and the Go
   suite on every adapter change.
2. Automated store submission for both Chrome Web Store and AMO.
3. A patch-release cadence for adapter-only changes, distinct from feature
   releases — adapter fixes should not wait behind unrelated work.
4. Update diagnostics: `papio doctor` should say which extension version is
   installed, which is current, and whether a known adapter fix is missing from
   it. The daemon already learns `extension_version` at hello, so this needs no
   new message.

Step 4.4 is the one with a real compatibility trap: resist reporting anything
richer by widening an existing result. `papio-browser/1` rejects unknown fields on
both sides, and ADR-0013 requires new messages to be typed, feature-gated **and
solicited**.

## Step 5 — fix the two live defects ADR-0015 surfaced

Independent of everything above, and not blocked by Step 1.

1. **`followupSelector` clicks an unproven target** (`background.ts:1024-1052`):
   whole-document search, a pre-click match accepted as though it had appeared in
   response, no containment or purpose check. Scope it to the clicked element's
   subtree or a containing dialog, and require it to have *appeared* rather than
   merely to exist.
2. **`extractDownloadURL` has no origin policy**: a wrong selector yields a
   credentialed cross-origin download. Default to same-origin, with any exception
   declared in the spec and fixture-backed.

Both need a captured fixture proving the affected adapters still classify, since
`followupSelector` is load-bearing for the providers that use it.

## Out of scope

Runtime adapters and runtime amendments (ADR-0015). Any mechanism that lets a
non-maintainer increase what papio does with the operator's session. If a
suppression-only mechanism is ever wanted — disable a rule, force a provider to
human-assisted — that is a separate proposal and a much easier one, because it can
only reduce automation.
