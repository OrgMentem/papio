# Page-first identity: already shipped, and two live defects in it

Status: **both defects fixed 2026-08-16** (extension-only; no protocol, ADR, or
migration change). What remains open is the structural front-matter parser, which
is workstream 3 of the roadmap and gated on the measurement in
`candidate-binding-measurement.md`. This file stays in `active/` until that
lands; the defect sections below are kept because the *reason* the naive repair
was unsafe is the durable part.

Originally: findings + fix plan, **superseding this file's own first draft**,
whose premise was wrong three separate ways — see "What the first draft got
wrong" for the retraction, because the errors are instructive and one of them is
a trap the next reader would fall into too.

## What shipped

- `doiFromURL` (`extension/src/deliver.ts`) is the authority for URL-origin
  extraction: `URL`/`searchParams` structure only; a `doi` query parameter read
  as an exact bounded value; a declared `.pdf` suffix stripped **only on a
  publisher path**, never on a DOI resolver, where the whole path is the
  identifier and `10.1234/article.pdf` is a DOI that ends in `.pdf`; DOI text
  never normalized (slash runs preserved); viewer wrappers and one level of proxy
  `url=` wrapping unwrapped; two reject lists — non-article namespaces (`suppl`,
  `citedby`, …) and route words (`full`, `epdf`, …) — plus a decline for a view
  marker fused onto the DOI (bioRxiv's `…v1.full`), which cannot be trimmed back
  without version-collapsing.
  Three structures can name a work — a wrapped inner URL, a `doi` parameter, the
  path — so all are gathered and must **agree**; disagreement declines. Racing
  them was a cardinal path: a supplement URL carrying `?doi=<article>` returned
  the article while addressing the appendix. A non-article route now disqualifies
  the URL before any candidate is read.
  Recognition is keyed on a DOI-shaped **path segment**, not per host. Be precise
  about what that does and does not buy: it accepts a DOI-shaped run under *any*
  host and path, so "unrecognized structure declines" is **false** as a general
  claim — what declines is a *recognized non-article* route, or a candidate that
  fails strict validation. Host keying was rejected because it would decline
  Springer, IET and every unenumerated publisher; the safety comes from strict
  validation plus the reject lists, and a bogus candidate surviving both fails
  loudly at the daemon's DOI-registration check rather than misfiling.
- `sniffDOI` is text-origin only and has no URL-origin caller. Every URL-origin
  candidate in `extractMetaDOI`/`extractPageDOI` routes through `doiFromURL`,
  resolved against the page as a base so a relative `canonical`/`citation_pdf_url`
  still works — **except** the final JSTOR tier, which is the documented
  `/stable/<id>` → `10.2307/<id>` mapping in `deriveStablePageDOI`, not URL
  parsing.
- `collectPageMetadata` (`extension/src/popup.ts`) is a **probe harvester**: it
  returns raw candidates (`PageProbeResult`) and makes no identifier decision.
  That dissolves the injected-function problem *for the popup path*. It does not
  remove papio's second URL extractor: `scanDocument`'s nested `identifierFromURL`
  (`page-scan.ts`) is under the same serialization constraint and cannot import
  the authority, so the duplication is structural. The mechanism is a **drift
  test** — `page-scan.test.ts` runs both over one shared table and requires
  agreement, with an explicit `differsByDesign` set. It caught a real divergence
  the moment it was written: the scanner accepted the ACM supplement.
  Body text is located in page scope, and the locator deliberately matches
  `sniffDOI`'s semantics — decode first, require a word boundary, never truncate a
  run — because each omission changed which DOI won.
- `pageAcquireOrigin` (`deliver.ts`) reduces `page_acquire.url` to scheme and host
  **inside `requestPageAcquire`**, the single wire boundary, so it covers every
  caller. Doing it at one call site left the other sending a full landing URL
  including a Springer `?sharing_token=`. A URL-shaped `title` is dropped on both
  reduced frames too, via the `isURLLike` predicate `state.ts` already applies on
  disk — the daemon *persists* title while discarding url, so the wire boundary
  must not be weaker than the disk boundary.
- Regression fixtures: `extension/test/deliver.test.ts` (provider URLs; the
  supplement, cited-by, route-suffix and view-marker declines; the
  two-different-works declines; `?doi=…&token=…`; the viewer wrapper; the
  repeated-slash case; relative resolution; userinfo), `background.test.ts` (the
  emitted frame is origin-only, a URL-shaped title is dropped, an unrepresentable
  address is refused with no frame sent), and `page-scan.test.ts` (the drift
  table).

Known limits, stated rather than implied:

- A real DOI whose own suffix ends `.pdf` on a publisher path is lost to the
  suffix strip (the Crossref component `10.1107/s160057671801289x/ks5605sup1.pdf`).
- **A legacy SICI DOI containing an encoded delimiter is lost, deliberately.**
  Crossref's own example `10.1002/(SICI)1521-3951(199911)216:1<135::AID-PSSB135>3.0.CO;2-#`
  is written `…2-%23` in a URL; `DOI_STRICT_RE` rejects the decoded `#`. The
  argument for relaxing it is sound — inside `url.pathname` a percent-encoded `#`
  or `?` cannot be a delimiter, because a real one would have been split off by
  the parser, so those characters there are data. It is kept anyway: the guard is
  redundant only *while* every caller parses structurally, and the failure it
  guards against — a credential absorbed into a stored identifier — shipped as a
  live bug twice in one session. A lost paper is visible and recoverable; a stored
  token is neither.
- MDPI's `/article/<doi>/s1` supplement yields a bogus candidate rather than an
  explicit decline. It fails loudly at the daemon's registration check rather than
  misfiling, but `s1` is not caught by name, and `s1` is too plausible as a real
  DOI suffix segment to add to a reject list without evidence.
- **The reject list is deliberately wider than the cardinal risk, and this is the
  one entry to revisit first.** Only `suppl`/`media`/`figure`/`table` name a
  *different* document; `references`, `citations`, `citedby` and `metrics` are
  views of the article itself, so declining them loses a paper that could have
  been filed correctly. Losing a paper is visible and recoverable while misfiling
  is neither, so the conservative side was chosen — but widening acceptance is a
  measurable question, not a judgement call, and belongs with the measurement
  workstream rather than in this fix. Note the asymmetry that makes it non-obvious:
  Taylor & Francis's References tab is `/doi/ref/<doi>`, which is *not* on the list
  and correctly yields the article, so the list already treats two spellings of
  one idea differently.

## The retraction, first

The question was *how does the Zotero Connector always get this right?* The
answer: it binds identity at the publisher page from declared metadata and
attaches the PDF as a child — identity by construction, never inference from
bytes. The proposal was to build that for papio's Send PDF path.

**papio already implements it.** Three verified premises of the first draft were
false:

1. **"The declared identifier is discarded at the grab boundary."** False.
   `startPDFDelivery` (`background.ts:5273`) takes `payload.doi` and resolves
   `findByTab` → `deliveryJobForOpener` → `deliveryJobForDOI`
   (`background.ts:4977`). The branch at 5419 enters the grab path **only** when
   `doi === undefined || doi.trim() === ""`. With a DOI present and no job
   matching, 5455-5461 calls `requestPageAcquire({url, doi, title})`, creates the
   job, and files the bytes in hand. `requestPdfGrab` has no `doi` parameter
   because it is the **no-identifier** path by construction.

2. **"A PDF viewer tab yields no identity, so URL-derived DOI is new capability."**
   False. `readCurrentPageMetadata` (`popup.ts:601`) tries injection, catches the
   viewer's rejection at 620-623 — the comment states the case exactly — and then
   at **631** does `const inferredDOI = metadata?.doi ?? sniffDOI(pageURL)`. The
   URL-derived fallback already ships, and it already reaches `requestPageAcquire`.

3. **"61.1% of Send PDF clicks park."** Category error of mine. 61.1% is the share
   of the operator's *library corpus* whose 1 KiB front-matter window is empty
   (`dev/active/candidate-binding-measurement.md`) — a property of documents, not
   a failure rate of this path. The two populations are unrelated and I conflated
   them.

Also wrong in the first draft, recorded so the tier table is not trusted: tier 1
is `popup.ts:396-408` and **includes `citation_pdf_url`** among the DOI-bearing
meta names, so papio already mines the declared PDF URL for a DOI. Tier 2
(409-413) is **not** publisher self-assertion — `firstDOI(canonical) ||
firstDOI(ogURL) || firstDOI(location.href)` has no host or route check, so an
aggregator, proxy, `doi.org` interstitial or search URL can supply a foreign DOI.
Tier 5 (430-446) is a host-scoped deterministic JSTOR mapping, not a publisher
assertion. `PdfGrabResultPayload`'s outcome enum also includes `steering`,
`existing`, `not_supported` and `unavailable` beyond the five listed.

So there is no feature to build. What the research found instead is **two live
defects in the shipped path**, both verified end to end.

## Defect 1 — a path-greedy DOI creates a job under an identifier that names nothing

`firstDOI` (`popup.ts:384`) and `sniffDOI` (`deliver.ts:42`) match
`/\b10\.\d{4,9}\/[^\s"'<>?#]+/`. The class stops at `?`, `#`, whitespace and
quotes — but **not at `/` and not at a file extension.** Run against real
provider PDF URLs through the actual exported function:

| provider PDF URL | `sniffDOI` result |
|---|---|
| `dl.acm.org/doi/pdf/10.1145/3630106.3660000.pdf` | `10.1145/3630106.3660000.pdf` ❌ |
| `link.springer.com/content/pdf/10.1007/s11192-024-04901-y.pdf` | `10.1007/s11192-024-04901-y.pdf` ❌ |
| `journals.sagepub.com/doi/pdf/10.1177/01634437251234567?download=true` | `10.1177/01634437251234567` ✅ |
| `onlinelibrary.wiley.com/doi/pdfdirect/10.1111/jcc4.12345?download=true` | `10.1111/jcc4.12345` ✅ |
| `mdpi.com/2076-3417/14/5/1234/pdf` | `undefined` ✅ (declines) |
| `pdf.sciencedirectassets.com/…/main.pdf?X-Amz-Signature=…` | `undefined` ✅ (declines) |

Two of six, both major publishers, yield a DOI with `.pdf` glued on.

**It fails silently, not loudly.** `work.NormalizeDOI` validates against
`doiCoreRE = ^10\.[0-9]{4,9}/\S{1,200}$` (`internal/work/identifiers.go:67`), and
`\S` matches `/` and `.`. Confirmed by running it:

```
NormalizeDOI("10.1145/3630106.3660000.pdf")     = "10.1145/3630106.3660000.pdf",     err=<nil>
NormalizeDOI("10.1007/s11192-024-04901-y.pdf")  = "10.1007/s11192-024-04901-y.pdf",  err=<nil>
NormalizeDOI("10.1177/01634437251234567/full")  = "10.1177/01634437251234567/full",  err=<nil>
```

So the chain is: Send PDF on an ACM or Springer PDF → malformed DOI → accepted →
`requestPageAcquire` creates a job for a DOI that does not exist → resolution
fails, and the user sees papio fail for no visible reason on a paper whose
identifier was sitting in the URL, correct but for four characters.

### The same root cause carries a session secret across the wire, and persists it

The class excludes `?`, `#`, whitespace and quotes — but a match need not *start*
at the URL's beginning, and `&`, `=` and `%` are **not** excluded. So a DOI
carried as a query *value* absorbs everything after it. Verified through the real
function:

```
sniffDOI("https://cdn.example/file.pdf?doi=10.1234/paper&token=SECRET123")
  = "10.1234/paper&token=SECRET123"
sniffDOI("https://prov.example/doi/pdf/10.1111/abc?ticket=ST-9f8e7d&sid=SESSION")
  = "10.1111/abc"                                    ← clean, the `?` terminated it
```

`NormalizeDOI` accepts the first (`\S{1,200}` again), `pageAcquireRequest`
normalizes it and **sets the job's identity** from it (`bridge.go:4288-4315`). So
the secret does not merely cross the native link — it becomes a job's durable
identifier and is written to disk. `?doi=…&…` is a routine OpenURL/resolver
shape, so this is reachable, not theoretical.

This outranks the `.pdf` case in severity: it is simultaneously a privacy
violation of a documented boundary, a persisted wrong identifier, and unbounded
in what it can capture.

Neither is the cardinal failure — no wrong paper is filed under a right citation,
because a nonexistent DOI matches nothing. But both are squarely the class the
user named at the start of this thread: something that looks right and quietly
isn't.

### One case that works only by accident, and must keep working

A hypothesis of mine was **refuted** by running it, and the refutation is a
constraint on the fix. I expected `popup.ts:631` to lose the DOI inside a Chrome
viewer wrapper, since it sniffs `pageURL` while line 630 has already computed the
unwrapped `viewerPDFURL`. It does not:

```
sniffDOI("chrome-extension://…/index.html?file=https%3A%2F%2Fdl.acm.org%2Fdoi%2Fpdf%2F10.1145%2F3630106.3660000")
  = "10.1145/3630106.3660000"
```

It works because `sniffDOI` runs `decodeURIComponent` over the **whole** URL
first, turning `%2F` into `/` so the regex matches inside the `file=` parameter —
i.e. it works *via the same unrestricted full-string scan that causes both
defects above.* So the fix cannot simply narrow to `url.pathname`: that would
silently break the viewer-wrapper case, which is the most common Send PDF shape
on Chrome. Unwrap first (`pdfSourceURL`), then parse the unwrapped URL's
components. This is why the fix needs the regression fixtures before the change.

### A closer sibling exists, but it is not the answer either

`page-scan.ts:105-107`, under ADR-0019 Decision 3's recognized-link ordering:

```js
const doiPath = /\/doi\/(?:abs\/|full\/|e?pdf\/|full-xml\/)?(10\.\d{4,}\/[^?#]+)/i.exec(path);
if (doiPath?.[1]) {
  const doi = trimTrailingPunct(decodeSafe(doiPath[1]).replace(/\.pdf$/i, ""));
  if (STRICT_DOI_RE.test(doi)) return { kind: "doi", value: doi };
}
```
Host- and route-aware, strips `.pdf$`, validates against `STRICT_DOI_RE`. papio
therefore has **two DOI-from-URL extractors of different rigour**, and the
delivery/grab path uses the loose one. That is the whole defect: drift between a
careful parser written for bulk selection and a loose one written for delivery.

Note it is a better model, not a finished answer: `page-scan.ts`'s own route
regex is host-agnostic and its `.pdf$` strip is unconditional, and a registered
DOI name may legitimately end in `.pdf`. Existing adapter `idPattern`s are
similarly broad (`[^?#]+` at `adapters/types.ts:713,787,815,927`) and their tests
only cover happy-path suffix-free URLs (`acm.test.ts:40-44`,
`adapters.test.ts:422-442`), so nothing in the tree currently proves a correct
grammar. The tightest correct design is **finite host/route declarations plus an
explicit suffix contract**, falling through to no-DOI for unrecognized shapes.

**Separate URL parsing from text parsing.** Every current consumer shares one
grammar across sources of very different trust: `collectPageMetadata`'s `firstDOI`
over meta values, canonical/og/location and `doi.org` anchors
(`popup.ts:381-426`); `extractMetaDOI`/`extractPageDOI` over metadata, canonical,
hrefs and body text (`deliver.ts:103-143`); `classifyPage` for non-PDF pages
(`deliver.ts:208-223`); and `readCurrentPageMetadata`'s URL fallback
(`popup.ts:630-633`). A URL has components and delimiters; free text does not.
Fixing one helper touches all of them, so the change must split the URL-origin
path from the text-origin path rather than tighten a shared regex — and a DOI
candidate containing URL delimiters must be **rejected**, never trimmed into
shape.

### The trap in the obvious fix, which is worse than the bug

The obvious fix — "strip the trailing junk" — converts a loud failure into the
**cardinal** one. ACM publishes supplements in a **parent-DOI namespace**:
`/doi/suppl/<article DOI>/suppl_file/<file>`. Today that yields a bogus
identifier, verified:

```
sniffDOI(".../doi/suppl/10.1145/3630106.3660000/suppl_file/appendix.pdf")
  = "10.1145/3630106.3660000/suppl_file/appendix.pdf"   ← bogus, resolves to nothing
```

A suffix-stripping fix turns that into `10.1145/3630106.3660000` — **the article's
DOI** — and files the appendix as the paper. Wrong document under a right
citation, silently, which is the one failure this project treats as cardinal. The
bug's current form is *safer* than its naive repair.

So `suppl` must be an **explicit reject**, not an unrecognized route that gets
trimmed. That inverts the fix's default: unrecognized structure must decline, and
recognized-but-non-article structure must decline *loudly*.

**Fix: a finite route registry, not a lifted closure.** My earlier instruction to
lift `identifierFromURL` and single-source it was wrong; the reviewers showed
three ways it fails:

- Its `/doi/` branch captures `[^?#]+` greedily, so
  `/doi/pdf/10.1177/01634437251234567/full` → `10.1177/01634437251234567/full`
  (verified). Only `.pdf$` is stripped, not route suffixes.
- Its **generic fallback** (`page-scan.ts:113-118`) matches *any* host and path:
  `/random/path/10.9999/whatever` → `10.9999/whatever` (verified). Lifting it
  reproduces exactly the unbounded guessing this plan forbids.
- `STRICT_DOI_RE` (`page-scan.ts:74`) is `/^10\.\d{4,}\/\S+$/` — the same `\S`
  breadth as `doiCoreRE`, so it gates nothing a URL suffix would violate. It is
  not the guard its name suggests.

Also do not inherit `trimTrailingPunct` (`page-scan.ts:83-85`), whose
`.replace(/\/+$/, "")` collapses trailing slash runs — and slash runs are
load-bearing. Strip only a **declared URL delimiter**, never normalize DOI text.

What must be specified before coding, per route: host, the article-PDF path
shape, the external suffix contract, and an explicit non-article reject list.
Consequence to accept honestly: the Springer row in the table above
(`/content/pdf/{doi}.pdf`) is served *today* only by the unsafe generic fallback,
so declining unknown routes means Springer needs its own registry entry or that
row's expectation is unmet. Enumerate the routes; do not claim coverage the
registry does not contain.

**Do not "fix" this in `doiCoreRE`.** A real DOI may legitimately contain dots
and slashes, and slash runs are load-bearing (`10.48612//monograph-2025-2` and
`10.48612/monograph-2025-2` are two separately registered DataCite works). The
defect is in URL extraction; the validator is the wrong layer, and tightening it
would reject legitimate identifiers.

## Defect 2 — a signed PDF URL crosses the native link on this path

The grab request is deliberately URL-free. `protocol.go:1202-1204`:

> *"PdfGrabRequestPayload asks the daemon to allocate a capture slot for a
> browser PDF tab (ADR-0020). The extension keeps the full tab URL local; the
> daemon receives only its bare hostname and title."*

That was a release-blocking privacy fix: publisher signed URLs are bearer-grade
credentials. `docs/concepts/browser-handoff.md:209-211` states the boundary:

> *"The link to the browser carries metadata only… PDF bytes, cookies,
> credentials, page contents, screenshots, and secret- or signed-URL values never
> cross that link."*

**The page-acquire path was missed.** When Send PDF resolves a DOI it calls
`requestPageAcquire` (`background.ts:5455-5461`) with `url`, which at 5408 is
`viewerPDFURL ?? this.comparableDeliverySourceURL(tabURL)` — and
`comparableDeliverySourceURL` returns `pdfSourceURL(rawURL)`, which only unwraps
a viewer `file` query parameter and **does not strip query strings**.
`PageAcquirePayload.URL` (`protocol.go:1131`) is required, `json:"url"`. The
daemon discards it in `pageAcquireRequest` (`bridge.go:4288-4315`), but the
documented promise is that it never crosses, and it crosses.

Reachability is narrower than the grab case and should be stated honestly: it
needs a URL carrying **both** a DOI and a secret, so the pure signed-CDN shapes
(ScienceDirect) are excluded — `sniffDOI` declines those, and the grab path takes
over. The live shape is a **proxied or ticketed provider URL**, e.g. EZproxy
rewriting a Wiley `pdfdirect` path while appending a session ticket: DOI present,
secret present, page-acquire taken.

**Fix:** make this path carry what the grab path carries. The daemon already
ignores the field, so the extension alone closes it — no wire change, no feature
flag, no skew risk. Concrete shape, per reviewer: the background still needs the
full URL **locally** for `startDeliveryDownload`, so send a redacted but still
valid http(s) URL in the `page_acquire` frame — **origin only, no path, no query,
no fragment** — and retain the full URL only in local state. An identifier may
cross; PDF URL structure and token-bearing suffixes may not.

## Work

Ordering: the route registry lands before anything consumes it, and the
regression fixtures land before the registry changes behaviour.

1. **Write the fixtures first**, as a table of real provider PDF URL shapes with
   expected extraction — including every case that must yield `undefined` rather
   than a guess, the ACM `suppl` reject, the Chrome viewer wrapper that must keep
   working, and the `?doi=…&token=…` case that must not absorb the secret. These
   fail today and they are what makes the registry reviewable.
2. **Build a finite route registry** for URL-origin DOI extraction: per host, the
   article-PDF path shape, the external suffix contract, and an explicit
   non-article reject list. Unrecognized structure declines. Keep it separate
   from text-origin parsing, which has no delimiters and needs different rules.
3. **Retire the loose URL path** from `sniffDOI`/`extractPageDOI`
   (`deliver.ts:12,42-143`) and from `collectPageMetadata`'s URL tiers. Do not
   lift `page-scan.ts`'s closure; make `page-scan.ts` a consumer of the registry
   too, so there is one implementation and its generic fallback stops being a
   second door.
   **Unsolved mechanism, and it must be solved rather than waved at:**
   `collectPageMetadata` is injected with `scripting.executeScript({ func })`
   (`popup.ts:612-615`) and `popup.ts:463` records that it must stay fully
   self-contained — Chrome serializes only that function's own source, so an
   imported helper is unresolved in the page context. `extension/build.ts` has
   **no** transform that inlines helper bodies into a function literal (its only
   string surgery is the capture-panel `replace`). So pick a real mechanism —
   a generated self-contained function with a drift test asserting it matches the
   shared source, or inject a bundled file and return the result by message — and
   state it. Leaving two copies contradicts the whole point of the fix.
4. **Stop sending the URL** on the Send-PDF-resolved page-acquire path. Scope it
   to that path: `extension/test/background.test.ts:1243-1257` pins the exact
   outbound `url` for ordinary page acquire, so either keep ordinary page-acquire
   semantics intact or update that contract test deliberately and record why the
   daemon ignoring the field (`bridge.go:4288-4315`) makes it safe.
5. **Both changelogs.** Extension-only, and **no ADR amendment**: this implements
   ADR-0020 **Decision 2** (`0020-grab-open-pdfs.md:40-45`) rather than altering
   Decision 4. Decision 2 reads: *"Tab-URL identifiers first, bytes second.
   Before offering a grab, the scan applies the ordinary URL identifier rules to
   the tab URL itself … A hit becomes a normal identifier row — no grab needed,
   full ordinary pipeline."* Defect 1 is precisely a failure to apply the
   **ordinary** rules, so the ADR already specifies the fix. Decision 4
   (`:65-79`) remains the no-identifier fallback. Grabs become *less
   frequent*, not differently authorized, so no new acceptance path exists and
   `docs/contributing/architecture-decisions.md` needs no amendment. Privacy docs
   need no change either — the fix makes code match the already-documented rule.

## Boundaries

Two safety notes that survived review and belong to whoever implements this:

- **Multiple identifier sets must park, not pick.** An erratum's own page
  legitimately declares its own DOI, and `CreateRequestForWork`'s canonical
  dedupe will not merge it with the corrected article — so an explicit Send PDF
  on an erratum correctly creates an erratum job. The hazard is any rule that
  lets the *file's* front matter override that when it cites the original DOI
  first. If a document yields more than one candidate identifier set, park and
  ask; never rank them.
- **This path's wrong bind is harder to notice than the withdrawn rule's.** The
  disabled auto-bind attached bytes to an *existing pending* job
  (`bridge.go:7637-7720`), so the operator was already expecting that paper. The
  page-acquire path **submits a fresh job** (`bridge.go:4266-4290`) which
  adoption validation can then promote (`app.go:3021-3093`), so a wrong bind
  arrives looking like a successful new citation nobody was watching for. Same
  zero-wrong-accept bar, worse visibility — which is why declining is the
  correct default here.

- **No new acceptance path, no autonomous binding, no protocol change, no
  migration.** `candidate_auto_bind/2` stays disabled. Both fixes are
  extension-local.
- **Declining is the correct outcome** whenever the route is unrecognized. The
  grab path plus the popup picker already handle "papio cannot tell," and they
  are strictly better than a guessed identifier.
- The structural front-matter parser is unaffected and remains the eventual fix
  for genuinely identifier-less documents. It does **not** move up: this
  workstream turned out to be a bug fix, not the feature that was going to
  displace it.

## What this says about the roadmap

The measurement instrument was built to stop a rule from shipping on assertion.
This round is the same lesson at a smaller scale and it cost nothing to learn:
the feature was already built, and **two rounds of reasoning about what to build
were worth less than one reviewer running the actual function.** The plan that
preceded this file cited real line numbers, quoted real comments, and was
confidently wrong about the control flow those lines implement.

Standing correction for the next reader: before proposing anything in this area,
run the extractor. `bun run` against `extension/src/deliver.ts` takes seconds and
settles questions that four paragraphs of inference cannot.
