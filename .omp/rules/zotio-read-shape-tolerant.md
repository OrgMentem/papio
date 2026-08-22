---
name: zotio-read-shape-tolerant
description: "zotio is migrating every record read onto a {meta,results} envelope — decode through the tolerant helper, never straight into a slice"
condition:
  - 'json\.Unmarshal\([^\n]*&\w*[Ii]tems?\b'
  - '"--agent"[^\n]*"(items|collections|tags)"[^\n]*"(find|missing-pdf|get|children|list)"'
scope:
  - "tool:edit(internal/zotio/*.go)"
  - "tool:write(internal/zotio/*.go)"
interruptMode: always
---

**zotio is migrating every record read from a bare JSON array onto its documented `{"meta":…,"results":[…]}` envelope, one command at a time. A read that decodes straight into a Go slice therefore breaks on a zotio upgrade, and it breaks SILENTLY.**

Decode through the tolerant helpers, which accept both shapes: `decodeRows` (`client.go`) and `decodeFoundItems` (`service.go`). Do not add a third copy of the shape logic, and do not pin either shape.

Why silence is the real hazard here:

- `RunJSON` returns the child's bytes verbatim and only validates that they are *some* JSON, so nothing in papio normalizes shape.
- `json.Unmarshal` of an object into a `[]T` fails with `cannot unmarshal object into Go value of type []T` — and no caller logged it.
- `resolveImportBackfillOwnership` converts any lookup error into "ownership undetermined" for the **whole batch of 50**, so one wrong decode silently answers "you do not have this" for every paper in the batch.

Both incidents this class has already produced, measured live:

- `findParentItemKeys` decoded `items find` into a slice. Every ownership lookup failed, so an import-backfill dry-run promised `would_import: 43` while the apply filed **0** and found **40** papers already in the operator's library. papio had also re-acquired papers it already owned.
- `MissingPDF` decoded `items missing-pdf` into a slice. That command was still a bare array in the installed 0.19.0 and is an envelope at zotio `HEAD`, so the next `brew upgrade` would have reported an **empty** Zotero-sourced acquisition queue: `n=0` plus an unlogged decode error, presenting as "nothing to do" rather than as a failure.

Blast radius when a read decode is wrong: `internal/watch/runner.go` (watch feeds), `internal/browser/bridge.go` (the extension's own-paper check), `internal/zotio/plan.go` (plan-time dedup), `internal/discovery/ownership.go`. Ownership failure surfaces to the operator as "you do not have this paper", which is indistinguishable from the truth.

If you are editing `decodeRows` or `decodeFoundItems` themselves, or adding a fixture, that is the sanctioned helper — say so and continue. Every pre-existing fixture in `service_test.go` and `client_test.go` used a bare array, which is exactly why 1,290 tests proved a shape zotio no longer emits: a new read wants a fixture in **both** shapes.
