---
name: provenance-digest-is-an-external-contract
description: "provenance_digest is Go-encoding-dependent and externally unverifiable — specify a canonicalization before changing how it is computed"
condition:
  - 'ProvenanceDigest'
  - 'provenance_digest'
  - 'func digest\('
  - 'sha256\.(Sum256|New)'
scope:
  - "tool:edit(internal/bundle/*.go)"
  - "tool:write(internal/bundle/*.go)"
interruptMode: never
---

**ADR-0011 records this as unresolved, not settled.** `digest()` computes `sha256(json.Marshal(bundle with the digest field blanked))` (`internal/bundle/export.go:405-410`), so the value depends on **Go's** `encoding/json`: field order from struct declaration order, `omitempty` behaviour, and — the sharp edge — HTML escaping of `<`, `>` and `&`, which are routine characters in URLs. A consumer in another language computing "sha256 of the canonical JSON" gets a different digest for a bundle papio considers valid.

The measured position: a real corpus reproduced 55/55 only with the undocumented Go recipe. `internal/protocol/protocol.go:550-552` validates the field's *syntax* only, and `internal/bundle/export_test.go:120-131` checks self-consistency, the `sha256:` prefix, and stability across repeat exports — that is papio agreeing with papio, which is exactly the property that cannot detect this class.

So before changing how the digest is computed, or what it covers:

1. **Decide which contract you are in.** Either specify a stable cross-language canonicalization (sorted keys, no HTML escaping, explicit number/empty-field handling) and document it beside the field, or state plainly that the digest is papio-internal integrity only and stop implying external verifiability.
2. **A recipe change is a breaking change** for anyone who stored a digest, even though no schema version moves and no test fails. Neither the protocol validator nor the export test can see it.
3. `dev/provenance-digest-reproducibility.py` is measurement evidence, not a guard — re-run it, do not cite it as one.

Adding a field to the bundle also changes every future digest. That is fine and expected; just do not describe it as compatible.
