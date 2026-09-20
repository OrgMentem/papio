# Domain Docs

How the engineering skills should consume this repo's domain documentation when
exploring the codebase.

This repo is **single-context**: one ADR directory, one shared vocabulary.

## Before exploring, read these

- **`dev/adr/`**: 29 ADRs, `NNNN-<slug>.md`, from `0001-triage-inbox-surface.md`
  to `0029-agent-acquisition-decisions.md`. Read the ones that touch the area
  you are about to work in. Several carry a `ratified-` prefix in the slug;
  treat those as settled contracts. This repo does **not** use `docs/adr/`; do
  not create it.
- **`AGENTS.md`** at the repo root: the working rules and the hard-won
  behavioral facts.
- **`docs/concepts/`**: the worked explanations (`access-modes.md`,
  `acquisition-pipeline.md`, `browser-handoff.md`,
  `provider-compatibility.md`, and the rest).

**There is no `CONTEXT.md` yet.** That is expected, not a defect. Do not create
one preemptively. The `/domain-modeling` skill (reached via `/grill-with-docs`
and `/improve-codebase-architecture`) writes it lazily, the first time a term
actually needs resolving. Until it exists, take the vocabulary from `AGENTS.md`,
the ADR titles, and `docs/concepts/`.

## File structure

```
/
├── AGENTS.md
├── dev/adr/            ← 29 ADRs, 0001 … 0029
├── dev/active/         ← in-flight working notes
├── dev/plans/          ← plans, specs, wayfinder maps
├── docs/concepts/      ← worked explanations
├── cmd/
└── internal/
```

There is no `CONTEXT-MAP.md` and no per-package glossary. A term means the same
thing in `cmd/` and in every package under `internal/`.

## Use the project's vocabulary

When your output names a domain concept (in a ticket title, a refactor proposal,
a hypothesis, a test name), use the term the ADRs and `docs/concepts/` already
use. Do not drift to synonyms.

If the concept you need has no settled name, that is a signal: either you are
inventing language the project does not use (reconsider), or there is a real
gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than
silently overriding:

> _Contradicts ADR-0009 (ratified consumer IPC contract), but worth reopening
> because…_

A `ratified-` ADR is a stronger bar: contradicting one is a contract change,
so say so plainly instead of routing around it.
