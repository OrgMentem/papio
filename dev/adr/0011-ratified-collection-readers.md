# ADR-0011: Ratified collection readers, and why the existing two were not the ones frozen

Status: Accepted (2026-08-01). Extends ADR-0009 and ADR-0010; governed by
ADR-0001. Records a finding that constrains ADR-0007's provenance digest.

## Context

The first external consumer finished acquisition — 309 works, 123 obtained — and
began *collection*: reading the acquisition bundle and artifact bytes for each
ready job so it can revalidate route and entitlement itself before recording a
rights attestation.

None of the eight methods ratified by ADR-0009 and ADR-0010 can produce a bundle
or locate an artifact. Two unratified methods already could:

- `bundle.export_v2` — `{job_id, output_dir}` → `{path, bundle}`, with the whole
  `protocol.AcquisitionBundle` inline.
- `artifacts.get` — `{job_id | sha256}` → `{artifact}`, the whole
  `job.Artifact`.

So the gap looked like ratification rather than implementation, and the
consumer — whose transport's entire discipline is that it calls ratified names
and no others — declined to call unratified ones or to read *papio*'s data
directory, which would have made the on-disk layout a contract nobody agreed to.

The question was whether to add those two names to `ratifiedConsumerMethods`.

## Decision: ratify two purpose-built readers, not the two that existed

`bundle.document` and `artifacts.locate` are ratified. `bundle.export_v2` and
`artifacts.get` remain served and unratified.

```
bundle.document   { job_id } -> { schema_version, document }
artifacts.locate  { job_id } -> { sha256, size_bytes, mime, path }
```

### Why not `artifacts.get`

It returns `job.Artifact`, which is the **persistence** struct written by
`UpsertArtifact`. Freezing it makes every future `artifacts` column a breaking
change for older consumers, because results decode with
`DisallowUnknownFields`. ADR-0010 rejected returning `job.Row` from a ratified
reader for precisely this reason; the same rule has to apply here.

Decisively, `job.Artifact` carries `identity_result`, and that value is
**last-writer-wins across every job sharing the digest** —
`UpsertArtifact` ends `ON CONFLICT(sha256) DO UPDATE SET identity_result =
excluded.identity_result`. ADR-0007 forbids projecting identity from a
content-addressed artifact for exactly that reason, and `internal/bundle`
already refuses to, reading `AcquisitionIdentity(jobID, sha)` instead with the
rule cited in a comment. Ratifying `artifacts.get` would have permanently handed
a rights-conscious consumer the one field the domain says is unsafe to read, and
it would have been *right* to use it.

`ArtifactLocation` therefore carries only what locating and verifying bytes
requires. The remaining metadata already lives in the bundle's `artifact` block.
Returning `path` rather than bytes is deliberate: the location is data papio
owns, which is better than a consumer hardcoding `artifacts/<sha256>.pdf`.

### Why not `bundle.export_v2`

Two reasons, and the second is the one that decides it.

It **materialises**: it requires `output_dir` and writes a directory. A ratified
reader whose caller wants the document should not force a filesystem write the
caller must then clean up. (Cheap to fix — `Export` already defaults an empty
destination to `<data-dir>/bundles/<job>`; only the handler mandates the
parameter.)

It returns `*protocol.AcquisitionBundle` **inline**, and `DisallowUnknownFields`
is recursive. Ratifying it would therefore promise never to add a field to the
bundle struct — so every future bundle schema version would need a
`bundle.export_v3`, defeating the `schema_version` the document already carries.
`acquisition-bundle/2` was cut the same day this decision was taken and amended
twice under review within it; freezing that shape permanently, hours old, is
what ADR-0010 warns against.

`bundle.document` returns the document as **JSON text**. The bundle then evolves
under its own versioning while the RPC contract holds still, and the consumer
parses it with the dual v1/v2 decoder it already has. `schema_version` is lifted
out of the text so a consumer can route to a decoder without parsing first. The
bytes are produced by `bundle.EncodeDocument`, the same encoder that writes
`bundle.json`, so reading over IPC and reading the exported file give an
identical document.

`Exporter.Export` was split: `Document` builds and validates, `Export` calls it
and then materialises. The reader cannot drift from what gets exported.

## Constraint recorded: `provenance_digest` is not portably verifiable

Found while deciding whether a consumer could verify the document it receives.
`digest()` is `sha256(json.Marshal(bundle with ProvenanceDigest blanked))`, so
its canonicalisation is whatever `encoding/json` does with that Go struct.

Measured against the real corpus by
`dev/provenance-digest-reproducibility.py`, which reproduces digests from
recorded bundles (ground truth *papio* already computed) and observes Go's
encoder by shelling out rather than asserting from documentation:

```
55/55 reproduced  compact separators + ensure_ascii=False
37/55 reproduced  json.dumps defaults
 0/55 reproduced  sorted keys
 0/55 reproduced  default separators
```

`37 = 55 − 18`, and exactly 18 bundles contain non-ASCII — the mechanism
confirmed by arithmetic rather than by argument. Go escapes `<`, `>`, `&` and
emits non-ASCII raw; Python never HTML-escapes and escapes non-ASCII under
`ensure_ascii=True`. **No `json.dumps` configuration reproduces Go once a bundle
contains `&`, `<`, or `>`**, and none handles a diacritic and an ampersand
together.

The accurate statement is therefore not "cannot be verified". It is: **verifiable
today across the whole corpus, but only by an undocumented recipe, and only
because no bundle has yet contained an HTML-escapable character.** Zero of 55 do.
That is more damning than an honest no, because a consumer will build
verification, watch it pass on every current bundle, and later read a mismatch as
evidence of tampering when it is an encoding artefact — and the trigger is
routine, since download URLs carry query strings.

Not fixed here. The options are a documented canonicalisation (RFC 8785 JCS or
equivalent) or dropping the claim that the digest is externally checkable. That
needs its own decision and must not ride along with a ratification.

## Consequences

- Two more names that can never be renamed or removed. Both are narrow
  projections rather than exported domain structs, so neither pins internal
  vocabulary.
- `bundle.export_v2` and `artifacts.get` stay served for the CLI and for older
  callers, and stay unratified. `artifacts.get` should not be recommended to
  consumers while it exposes `identity_result`.
- `papio bundle document <job-id>` and `papio artifacts locate <job-id>` satisfy
  ADR-0001 as real operator commands, not reachability shims.
- A consumer receiving `document` can compute the digest only under the recipe
  above; until canonicalisation is decided, treat a mismatch as unproven rather
  than as tampering.
