---
name: ownership-work-normalizers-separate
description: "internal/ownership must never import internal/work — the two identifier normalizers implement different equivalence relations on purpose"
condition:
  - 'papio/internal/work"'
scope:
  - "tool:edit(internal/ownership/*.go)"
  - "tool:write(internal/ownership/*.go)"
interruptMode: always
---

**`internal/work` and `internal/ownership` normalize identifiers to DIFFERENT equivalence relations on purpose. Importing one into the other is the consolidation bug, not the cleanup.**

- `work.Normalize*` serves **acquisition**: version-**preserving** and strict (`2301.08745v2` stays `v2`; `doiCoreRE`; PMID capped at ten digits).
- `ownership.normalize*` (unexported) serves **"is this the same work?"**: version-**collapsing** and lenient (ADR-0008; `ownership.go`'s comment "a version suffix names the same work, so v2 must match v1", pinned by `ownershipsnapshot`'s "matched across a version suffix" test).

A dead-code or DRY pass sees two divergent copies of one function. Consolidating them is a **bug in both directions**: routing acquisition through the collapsing form fetches the wrong version, and routing ownership through the preserving form stops `v2` matching `v1` and silently breaks holdings deduplication. No test can detect the *intent* of a consolidation pass — the import is the tell, which is why this rule watches for it.

Two related invariants while you are in here:

- **The version axis is the ONLY axis.** Both normalizers preserve slash runs, and a repeated slash is not a typo: `10.48612//monograph-2025-2` and `10.48612/monograph-2025-2` are two separately registered DataCite works with different titles by the same creators (verified live, pinned as a pair in `ownership_test.go`'s `TestNormalizeIdentifier`). Collapsing runs was tried and reverted (`5d1adce`).
- Blast radius beyond holdings: `liveJobForCanonicalWork` keys on `work.Describe()`, so collapsing in the **work** normalizer would make `acquire.submit_v2` return `existing: true` pointing a ratified consumer at a job for a **different work** (ADR-0010).

If the import is genuinely unrelated to identifier normalization, say which symbol you need and why it cannot live behind a narrower boundary, then continue.
