# Instruction policy and resolution evidence

Read this file before changing instruction scope or reference organization.
Paths and line citations below refer to the repository root at the 2026-11-15 refactor.

## Task-specific checks

The root routes work to the relevant contracts before the operation starts.
A cross-surface change must load every matching reference. Moving a contract does not waive it.
Behavior changes retain the original race, migration, fixture, identity-corpus, and live-smoke requirements.
The extension lifecycle reference still requires a real browser smoke run for behavior changes in that subsystem.
Live cohort runs still create real jobs and retain their privacy and cleanup limits.

Documentation-only moves require local link and anchor checks, technical-content conservation, and representative task-routing checks.
They do not require application tests, live jobs, browser smoke runs, or deployment.
This is an explicit scope clarification, not evidence that runtime behavior passed those checks.

Correct an instruction conflict that affects the current task and has current repository evidence.
Report unrelated conflicts instead of assigning every future task automatic ownership of unrelated documentation fixes.
`INTENT.md` remains authoritative for product behavior; read it before adding gates or human steps.

`INTENT.md` holds the owner's own words: maximal automation, the complete list of limits, and rules against the conservative ratchet.

## Two document lifetimes

The previous root described all of `dev/` as lifetime-tiered and named only `dev/active/` for plans.
Its own issue-tracker section directed specs and maps to `dev/plans/<feature-slug>/`.
These statements omitted the distinction that the existing domain documents already make:

- `dev/agents/issue-tracker.md:99-110` assigns tracked specs and maps to `dev/plans/` and keeps in-flight notes in `dev/active/`.
- `dev/agents/domain.md:29-38` lists both directories with those distinct purposes.
- `dev/plans/primo-follow-link/design.md` is an existing design record; `dev/active/` contains the existing working notes and backlogs.
- `cmd/docs-gen/docs_test.go:27-29,77-81` treats `dev/plans/` as an unpublished internal tier.

The resolved rule preserves both directories. New multi-ticket specs and maps use `dev/plans/<feature-slug>/`.
Temporary working notes use `dev/active/`; after shipping, salvage normative decisions into an ADR before deleting those notes.
Do not apply that deletion rule to tracked design records, durable runbooks, or field reports.
Git history remains the archive for deleted working notes; this refactor creates no `archive/` and moves no workstream files.
ADRs remain permanent and must not depend on temporary notes for normative content.

## Extension reload

The old manual-only statement conflicts with current executable behavior:

- `Makefile:132-158` builds both bundles, installs the binary, restarts the host and daemon, and invokes `browser reload`.
  The recipe prefixes reload with `-`, so a failure does not fail the deploy.
- `internal/browser/bridge.go:142-144,951-969` gates `dev_reload` at extension version `0.15.0`.
- `extension/src/background.ts:16820-16846` refuses missing support, a failed install-type lookup, or an install type other than `development`.
- `internal/cli/browser.go:138-249` selects the current holder or `--session`, waits 15 seconds by default, and verifies a new holder.
  `--timeout=0` does not verify reconnect. A timeout does not prove a store install, and sibling reconnects can prevent attribution.

Use the [current reload procedure](extension-lifecycle.md#current-reload-procedure).
An older loaded bundle needs one manual load; store-installed copies do not read local build files.
No connected extension or daemon requires the browser's manual reload control, followed by session verification.

The stale manual-only comment at `Makefile:99-100` remains an explicitly reported conflict.
The executable recipe, not that comment, supplies the evidence above. This documentation-only change does not edit code or Makefile comments.
