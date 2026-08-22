---
name: zotio-item-key-is-not-an-ownership-answer
description: "Never skip a job from an owned-with-PDF answer because it already names a Zotero item — that is the job most likely to be finished already"
condition:
  - 'ZotioItemKey[\s\S]{0,140}?(continue|return)'
  - 'ownedParents[\s\S]{0,140}?ZotioItemKey'
scope:
  - "tool:edit(internal/zotio/*.go)"
  - "tool:write(internal/zotio/*.go)"
interruptMode: always
---

**A job that already carries a `ZotioItemKey` must still get an ownership answer. Excluding it — "it names an item, so the existing-item route will settle it" — is the bug, and that exact reasoning was wrong in two files at once.**

The existing-item route is precisely the one that cannot settle it:

- `zotio attachments add` is **Web-API-only**. On a library whose files live on the operator's own file store (WebDAV), a stored upload always lands in Zotero's own cloud storage instead, so zotio refuses it with `precondition_unmet` / `zotero_file_storage` — no HTTP status, correctly, because nothing was sent.
- An item that **already holds** the paper's PDF has nothing to upload at all.
- `ready` is terminal, so a job that keeps choosing that route retries a refused upload on every pass, forever.

Measured on the operator's library: papers whose Zotero item carried papio's **own artifact** — the attachment filename byte-equal to the job's `job_artifacts.artifact_sha256` — had been re-attached and refused since 13 August. Five cleared on the first pass once the exclusion was removed, with no Zotero mutation attempted at all (`ready` 38 → 33, `imported` 293 → 298).

Two invariants to preserve while you are in here:

- **One definition of "holds a PDF."** `itemsHoldingPDF` (`service.go`) is it: an item absent from zotio's missing-PDF queue holds one, which is the signal `LookupWorks` already trusts. `skipOwnedReadyImport` (`plan.go`) and `resolveImportBackfillOwnership` (`import_backfill.go`) both route through it so the dry-run's prediction and apply's behaviour cannot diverge — they were two independent exclusions of the same jobs before. Adding a stricter artifact-hash rule on one path alone would give papio two meanings of owned-with-PDF; the hash equality is the **evidence** that found this bug, not a second rule.
- **Apply must not re-attempt what classification already answered.** `importBackfillAlreadyOwned` still calls `PlanAndApply`, and that is deliberate for keyless jobs (zotio's own dedup records the parent). It is only safe because `skipOwnedReadyImport` short-circuits first. If you change either, check the other.

A guard that skips a keyed job for some reason **other** than ownership (a route decision, a validation) is fine — name the reason and continue.
