# ADR-0002: Delivering quarantined PDFs to the browser for review

Status: Accepted (2026-07-21; conditional resolved 2026-08-07) — Option B, the
preview-only loopback capability endpoint. The one-day timeboxed Option-A spike
ran and failed on BOTH Chrome and Firefox (see Decision), so the `file://`
conditional never opened. Shipped inside the inbox v1 build (ext-v0.5.0) so
in-page `verify_identity` acceptance was not deferred.

## Context

`verify_identity` review means a human inspects a quarantined PDF that exists
only as a daemon-local file. ADR-0001 puts that review in a browser extension
page, but choosing an extension tab does not make a daemon-local file
browser-readable:

- Sending PDF bytes over native messaging violates the protocol boundary and
  the 256 KiB frame cap.
- `file://` URLs require Chrome's per-extension "Allow access to file URLs"
  user setting, expose absolute paths, and need separate Firefox validation —
  not a dependable default path.

## Options

### A. `file://` spike

Prove, on both Chrome and Firefox, that a `file://` preview works without
unreasonable user setup (per-extension file-access toggle, path exposure,
Firefox behavior). Accept only with evidence.

### B. Preview-only loopback capability endpoint (leading candidate)

A deliberately minimal HTTP listener in the daemon:

- binds a literal loopback address on an **ephemeral** port — no config field
  (strict-mode config stays untouched);
- GET/HEAD only, plus PDF-viewer range requests;
- unguessable short-lived capability URL bound to one action ID + candidate
  SHA-256 — no directory access, no generic file parameter;
- exact `Host` validation (DNS-rebinding), `Content-Type: application/pdf`,
  `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, no CORS;
- issued via the native-messaging `review_preview_request`; accept/reject still
  travel over native messaging, never HTTP;
- the capability is an intent/TOCTOU safeguard binding the decision to the
  inspected bytes — not a new authentication boundary (native-messaging
  `allowed_extensions` remains that).

## Constraints on whichever option wins

- ADR-0001's accept flow requires: preview occurred for this candidate, accept
  carries action revision + SHA-256, daemon CASes in one transaction.
- Scope creep tripwire: if the endpoint (option B) ever grows JSON APIs,
  sessions, or general UI assets, the daemon-served web UI alternative from
  ADR-0001 must be reopened rather than grown into by accident.

## Decision

Option B. The Option-A spike ran during inbox v1 Phase 0 and failed its
"unreasonable user setup" bar on **both** browsers, so the conditional in the
status line never opened:

- **Chrome 118+** blocks extension `tabs.create`/`tabs.update` navigation to
  `file://` unless the per-extension "Allow access to file URLs" toggle is set,
  and that toggle is off by default.
- **Firefox** forbids `file://` in `tabs.create`/`tabs.update` outright
  (Bugzilla 1266960 open, 1617594 REOPENED as of 2026-07-10). Firefox 153
  (released 2026-07-21) added an off-by-default "Access local files" user
  permission, but it is content-script-scoped only (Bug 2034168 comment #1) and
  does not enable extension-page or `tabs.*` file navigation.

A `file://` route would therefore have needed per-user, per-browser manual setup
merely to function, and would still have exposed absolute quarantine paths.
Option B needs no user setup and exposes no path.

### Durable review binding

Option B's accept flow above (revision + SHA-256, CAS in one transaction) had no
schema to stand on: the SHA computed at fetch time was never persisted, and the
candidate under review could only be inferred from the latest validate attempt.
Migration `0010_human_action_review_binding.sql` adds `candidate_id`,
`quarantine_path`, `quarantine_sha256` and `revision` to `human_actions`, so
preview issuance and CAS acceptance read the same fields instead of inferring.

Rows created before that migration carry an empty binding forever; a feature
assuming the binding is always populated is correct for every current code path
and will still break on old local dev data.

## Addendum: in-page review verdict (2026-08-03)

The capability URL now renders a minimal review shell rather than returning
PDF bytes directly. A fixed citation bar asks whether the quarantined file is
the requested work; the PDF remains available at the capability-bound
`/p/<token>/file` sibling with the original inline, range, and no-store
semantics.

Accept and reject may be recorded from that bar through
`POST /p/<token>/verdict`. This is not a parallel review API: the handler calls
the same revision-and-SHA-bound `ResolveReviewCAS` transition as the extension
inbox, and a capability permits only one decisive verdict. The token remains
the sole capability; the listener remains literal IPv4 loopback with exact
Host validation, no cookies or CORS, and neither the shell nor its assets
expose the quarantine path. The script is capability-bound and served
separately under a restrictive CSP.

This narrowly supersedes Option B's GET/HEAD-only and native-messaging-only
verdict constraints. It does not create sessions, a general JSON API, or a
daemon web application, so the scope-creep tripwire remains in force.

## Addendum: embedded files are stripped, not reviewed (2026-08-27)

papio never files a PDF carrying active content, and that stands. What changed
is what papio does with the commonest cause of it. Publisher PDFs routinely
bundle one supplementary attachment, so `has_embedded_files` was the ONLY
marker on most quarantined papers, and the quarantine had no exit: `accept` was
refused outright for `unsafe_pdf`, and `reject` asked the operator to fetch by
hand a file papio already held. Measured on the live store 2026-08-27: three
parked reviews, the oldest twelve days old, two of them embedded-file-only with
correct titles, no JavaScript and no encryption.

A PDF whose only marker is an embedded file is now REWRITTEN without its
attachments and re-validated end to end, and the rewrite is adopted when it
comes back clean. Encryption and JavaScript are deliberately excluded: each
would be a different rewrite carrying a different risk, and neither is the
publisher case this exists for.

Three properties make this safe to do without asking:

- The worker reports on the REWRITE, never on the source, so the parent never
  has to trust that the removal was complete. A surviving marker simply keeps
  the file quarantined.
- The rewrite is re-validated in full, so text, metadata and identity are read
  from the bytes that will actually be adopted. Reusing the source's report
  would file a document papio never checked.
- `validateCandidate`'s encrypted/active branch has no `review_override`
  escape, so nothing here can launder a file papio fails to sanitize.

The cost is that the adopted bytes are not the publisher's bytes. Provenance
therefore records both digests: a `job.pdf_sanitized` event carries the source
SHA-256 and the adopted SHA-256, and the artifact is stored under the digest of
its own content.

With sanitizing in place, accepting an `unsafe_pdf` review means "re-validate
these exact bytes", so the blanket accept refusal is withdrawn — the operator
can ask papio to re-check a file it once could not handle, and a file it still
cannot make safe parks again rather than reaching the library.
