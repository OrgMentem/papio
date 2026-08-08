# ADR-0020: Grab open PDFs from the browser

Status: Accepted (2026-08-08). Drafted from the 2026-08-08 field session: an
operator reading `pdf.sciencedirectassets.com/...main.pdf` and a repository
`..._lead.pdf` asked papio to grab the paper they were already looking at;
the selection workspace honestly reported "no recognizable identifiers"
because a browser PDF tab has no DOM to scan.

## Context

The scanner (ADR-0019) reads rendered HTML. A PDF tab renders inside
Chrome's viewer: the injected scan context sees only the viewer wrapper, so
even a paper whose first page prints its own DOI yields zero detections.
Meanwhile the daemon already owns everything needed to identify a PDF it
holds: `internal/pdf` extracts front-matter DOIs (`documentDOIs`) and scores
identity (`MatchIdentity`), quarantine performs structural validation, and
browser-download adoption (steered `papio/<dir>/` downloads) is a proven
transport that rides the user's authenticated session.

Constraints that bound the design:

- **Native messaging frames cap at 256 KiB** (`MaxBrowserMessageBytes`); PDF
  bytes must never ride the wire.
- **Jobs are keyed by canonical work identity at creation** (ADR-0010's
  dedupe invariants — `liveJobForCanonicalWork` on `work.Describe()`).
  Identity-less jobs would undermine every dedupe guarantee, so the capture
  must precede the job, not the reverse.
- **ADR-0019's title-only stance**: nothing browser-sourced is submitted on
  a title guess; v1 of the detector refuses weak matches. The same applies
  here — a PDF whose front matter yields no identifier parks for a human,
  it does not enrich-and-hope.
- Firefox cannot steer downloads (`onDeterminingFilename` absent), so the
  capture transport is Chrome-only; Firefox degrades to guidance.

## Decision

1. **Entry point: the existing scan flow.** Scanning a PDF tab (detected by
   the viewer wrapper / URL shape) renders the selection workspace with a
   single "grab this PDF" row (tab title + URL host) instead of the empty
   "no identifiers" state. Same consent copy, same explicit invocation —
   the grab is one more thing "Select papers on this page" can mean.
2. **Tab-URL identifiers first, bytes second.** Before offering a grab, the
   scan applies the ordinary URL identifier rules to the *tab URL itself*
   (arXiv `/pdf/` shapes, DOI-shaped path segments). A hit becomes a normal
   identifier row — no grab needed, full ordinary pipeline. The grab
   affordance covers the remainder (asset CDNs, repositories, signed URLs).
3. **Capture before job.** Accepting the grab:
   - extension → daemon `pdf_grab_request` (new message pair behind a
     `pdf_grab_v1` hello-ack feature): tab URL + title; daemon allocates a
     grab id and returns the steering path `papio/grabs/<grab-id>/` under
     the adoption root;
   - extension `chrome.downloads.download(tab.url)` steered to that path —
     the browser's own cookies/session fetch the bytes; nothing is
     re-requested through papio and no bytes cross native messaging;
   - the daemon's grab sweeper (same bounded, latch-aware reader as
     adoption) picks up the settled file.
4. **Identity from the file, then an ordinary job.** The daemon quarantines
   the capture, runs structural validation, and extracts front-matter
   identifiers (`documentDOIs`, plus the existing arXiv/PMID front-matter
   patterns where present):
   - **Identifier found** → create the ordinary identifier-keyed job
     (consumer `browser-pdf:<host>`), with the captured file injected as a
     top-ranked local candidate. Resolution proceeds normally — metadata
     from the registrar, `MatchIdentity` against the captured bytes — so a
     wrong PDF under a right-looking DOI still fails identity review
     exactly as any acquisition would. Ledger dedupe applies naturally; an
     already-owned work reports "already in your library" as the grab
     outcome instead of a duplicate job.
   - **No identifier** → the grab parks as a human action carrying the
     extracted title guess and the quarantine path. Never a title-only
     submission (ADR-0019's line holds).
5. **Privacy.** The grab is explicit per-click consent; the captured bytes
   stay on the machine; no scholarly service is contacted until the
   extracted identifier enters the ordinary resolution pipeline — the same
   moment consent semantics already cover for every submitted work.

## Consequences

- New protocol pair + feature flag (`pdf_grab_request`/`pdf_grab_result`,
  `pdf_grab_v1`), all three protocol artifacts, fail-closed.
- New reserved `grabs/` namespace under the adoption root; the grab sweeper
  shares the bounded-reader latch, and `SweepTerminalAdoptions`'s
  unknown-dir hygiene must learn the reserved prefix.
- A grab store row (id, url host, state, quarantine binding) — one
  migration — so grabs survive restarts and surface in doctor/stats.
- Firefox: the workspace shows the grab row disabled with honest copy (no
  download steering on Firefox); the URL-identifier path (Decision 2) works
  everywhere.
- The identity-corpus wrong-accept doctrine is untouched: no new acceptance
  path exists — grabs converge into the standard resolution + identity
  pipeline.

## Rejected alternatives

- **Uploading PDF bytes over native messaging** — the 256 KiB transport cap
  is fail-fatal (the MDPI page-capture incident), and chunking protocols
  for multi-MB files over stdin framing buy nothing over a steered
  download that already exists.
- **In-extension text extraction (pdf.js)** — a heavyweight dependency in a
  zero-dependency extension, duplicating a better extractor the daemon
  already has, against bytes the daemon will hold anyway.
- **Identity-less jobs re-keyed after extraction** — breaks ADR-0010's
  canonical-identity dedupe; two grabs of the same paper would race to
  distinct jobs and reconcile never.
- **Automatic grabbing of every PDF tab** — the Nomad lesson (ADR-0019):
  ambient collection is the product papio refused to be; grabs are
  explicit, one click, one file.
