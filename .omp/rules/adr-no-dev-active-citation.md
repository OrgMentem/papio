---
name: adr-no-dev-active-citation
description: "An ADR must never depend on a dev/active/ file for normative content — active plans are deleted when the work ships"
condition:
  - 'dev/active/'
scope:
  - "tool:write(dev/adr/*.md)"
  - "tool:edit(dev/adr/*.md)"
interruptMode: always
---

**An ADR must never depend on a `dev/active/` file for normative content.** `dev/` is tiered by **lifetime**: a file leaves `dev/active/` when its work ships (salvage anything normative into an ADR, then delete it — git history is the archive, there is deliberately no `archive/`). So an ADR citing an active plan is left pointing at nothing the moment the work lands.

This is a live hazard, not a hypothetical: `dev/adr/0021-packaged-behaviour-and-restrictive-control.md` says its acceptance tests "live in `dev/active/adapter-release-latency-plan.md`", which means that plan **cannot be deleted** until the tests move into the ADR.

Put the normative content — acceptance tests, invariants, the decision itself — **inside the ADR**. A `dev/active/` reference is acceptable only as a non-normative pointer to work in flight, clearly marked as such, that the ADR remains complete without.

Two related rules for this directory:

- Never link into `dev/` from `docs/` — it 404s on the live site. `docs/contributing/architecture-decisions.md` is the curated public summary of ADRs.
- **Every `file:symbol` citation must actually resolve**, because a citation that does not reads as verified evidence. An audit pass caught nine fabricated ones (`TestActionKindCoverage` for `TestActionKindDispositionIsExhaustive`, `grab.go:Identify` where `internal/grab` has no `Identify` at all). Verify each one you write here.
