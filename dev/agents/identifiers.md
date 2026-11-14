# Identifier contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

### Identifiers (two deliberate normalization semantics — do not merge them)
- **`internal/work` and `internal/ownership` normalize identifiers to DIFFERENT
  equivalence relations on purpose.** `work.Normalize*` serves **acquisition** and is
  version-**preserving** and strict (`2301.08745v2` stays `v2`; `doiCoreRE`; PMID capped at
  ten digits). `ownership.normalize*` (unexported) serves **"is this the same work?"** and is
  version-**collapsing** and lenient (ADR-0008; `ownership.go`'s comment "a version suffix
  names the same work, so v2 must match v1", pinned by `ownershipsnapshot`'s "matched across
  a version suffix" test). A dead-code or DRY pass will look like two divergent copies of
  one function; consolidating them is a **bug** in both directions, and
  `rule://ownership-work-normalizers-separate` fires on the import that starts it.
- **The version axis is the ONLY axis. Both normalizers preserve slash runs, and
  a repeated slash is not a typo.** `10.48612//monograph-2025-2` and
  `10.48612/monograph-2025-2` are two separately registered DataCite works with
  different titles by the same creators — verified against the live API, and
  pinned as a pair in `ownership_test.go`'s `TestNormalizeIdentifier`. Collapsing runs merges two real
  names, so it was tried and reverted (`5d1adce`). The blast radius is wider than
  holdings: `liveJobForCanonicalWork` keys on `work.Describe()`, so collapsing in
  the **work** normalizer would make `acquire.submit_v2` return `existing: true`
  pointing a ratified consumer at a job for a **different work** (ADR-0010). Do
  not add a second equivalence axis to this split without checking both.
- **`papio acquire <arg>` is the ONLY bare-string identifier classifier in the tree**
  (`inferBareIdentifier` in `internal/cli/acquire.go`). Every other entry point — the
  identifier flags, `papio batch`/MCP via `batch.ParseWork`, the daemon's
  `protocol.Identifiers` ingress, and `ownership.QueryFor` — takes **named fields**. Do not
  add a second guesser or promote this one into `internal/work`: guessing what someone typed
  at a shell prompt is a CLI affordance, not a domain rule.
- **A bare ten-digit string must stay an error.** It is simultaneously a valid ISBN-10
  (`isbnDigitsRE`) and a valid PMID (`pmidRE`), so the bare path caps PMIDs at nine digits
  while `--pmid` accepts ten — the flag already states the scheme, the bare form cannot.
  Widening that guard silently acquires the wrong work for every bare ISBN-10.

