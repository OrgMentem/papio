# Provider compatibility

This hand-maintained matrix records observed provider routes, not a promise that
every title, institution, entitlement, or browser session will work. A row marked
**Verified working** records an individually live-verified observation; it is not
a success rate. We do not invent aggregate rates or denominators: where there is
no measured population, none is implied. Unknown or changed provider UI remains
assisted behavior.

The registered adapter list is intentionally narrower than the web. A provider
appears here only when there is a useful observed route to report; the extension
runs an adapter only after the user has granted its provider host permission.

Every adapter below ships with a captured fixture under
`extension/fixtures/<adapter-id>/` (see
[Contribute a provider adapter](../contributing/provider-adapters.md)). A test
(`extension/test/adapters.test.ts`) walks the registered adapter list and fails
if any classify rule — `article`, `login`, `terms`, `no_entitlement`, or
`wrong_work_check` — lacks its matching captured fixture (`success.html` for
`article`, `login-return.html` for `login`, `terms.html` for `terms`,
`no-entitlement.html` for `no_entitlement`, `wrong-work.html` for
`wrong_work_check`), so this table cannot silently drift ahead of the evidence
backing it.

Status values: **Verified working** — an individually confirmed end-to-end
download. **Verified route** — the page structure and file endpoint are
confirmed against a real entitled or public session, without an independent
confirmation that the endpoint itself returns file bytes. **Human-assisted** —
a publisher terms step keeps the download manual unless auto-accept consent is
recorded. **Unverified** — registered and fixture-backed, but no retained
validated artifact has established an end-to-end download; live page captures,
viewer adoption, or provider outcomes alone do not promote it. **No route** —
the adapter recognizes a terminal provider state but has no download control to
invoke.

| Provider | Route observed | Adapter | Status | Last verified | Notes |
| --- | --- | --- | --- | --- | --- |
| ACM Digital Library | PDF/eReader toolbar control (`a.btn--eReader`) gates a derived `/doi/pdf/<doi>?download=true` endpoint | `acm` | Verified working | 2026-07-23 | The bottom-of-page `a#downloadPdfUrl` anchor is not an entitlement signal — ACM renders it even on paywalled "Get Access" pages — so the adapter keys on the eReader control instead; the downloads API was confirmed returning `%PDF` bytes through the session cookie jar. |
| Annual Reviews | Empty PDF POST form, downloaded through the browser API after checking the Open Access marker and full-text container | `annualreviews` | Verified working | 2026-09-19 | A fresh isolated Chrome probe reached `ready` with the correct 21-page PDF and pop-ups blocked. It needed an explicit popup Open after a CLI Open was held behind another sign-in claim; no PDF control was clicked. The institutional resolver still sends this work to a ProQuest no-results page, so this verifies the exact publisher route. |
| APA PsycNet | Stable `#pdf` anchor renders once the full article has loaded; denied records show a "Get Access" control instead | `psycnet` | Verified route | 2026-07-20 | Verified live in a fresh browser against a public full-text article and a denied record; `doi.apa.org` DOI landings route into the same application. |
| BMJ Journals | `citation_access=all` plus a rendered `article-pdf-download` anchor gates the file | `bmj` | Verified route | 2026-07-20 | Restricted to explicit `citation_access=all` pages — closed articles stay assisted even when they publish PDF-shaped citation metadata. |
| Cambridge Core | Action-bar `buttonSavePDFOptions` control plus a rendered `aop-cambridge-core/content/view` anchor | `cambridge` | Verified route | 2026-07-20 | Denied pages still publish `citation_pdf_url`, so the adapter requires the rendered PDF action instead; scoped to journals, not Cambridge's separate books PDF service. |
| ClinicalKey | Article-title span and unique header PDF link to `/service/content/pdf/watermarked/<pii>.pdf` | `clinicalkey` | Verified working | 2026-09-19 | A live adapter download reached `ready` with the requested article in a validated 12-page PDF. A separate manual download also reached `ready`. Both the earlier and current captures produce executable plans. Only `clinicalkey.com.au` is covered; `clinicalkey.com` needs its own capture. The settle timeout remains 15 s. |
| Cochrane Library | Every PDF affordance, including `citation_pdf_url`, names `/pdf/full`, a 1.7 KB HTML viewer wrapper; the file is the nested `/pdf/CDSR/<code>/<code>.pdf` the wrapper's iframe names | `cochrane` | Verified route | 2026-08-24 | Entitlement is the rendered full-review PDF link (`a.pdf-link-full`), because Cochrane ships its institutional sign-in panel on entitled pages too. The abstract link (`a.pdf-link-abstract`) names a different document and is never the target. |
| EBSCOhost | The PDF viewer's `opid`/`recordId` build a call to EBSCO's researcher-edge-aggregator API, which returns the PDF URL as JSON | `ebsco` | Verified route | 2026-07-14 | Entitlement is inferred from the rendered PDF viewer (a `canvas`), since the record page's own download button is absent there; the constructed API URL is not independently confirmed to return a file. |
| Emerald Insight | Rendered PDF anchor — the current platform's `a.article-pdfLink`, or the legacy `a.intent_pdf_link` — read directly by href | `emerald` | Verified route | 2026-08-06 | Emerald migrated its delivery anchor; both selectors are kept until a current page is confirmed to no longer serve the legacy one. Entitlement is proved by the paywall rule not matching first, not by an Open Access badge. |
| Europe PMC | Exact `/api/getPdf?pmcid=PMC<id>` direct PDF route; rendered `#open_pdf` control on `/article/PMC/<id>` also gates a constructed file URL | `europepmc` | Verified working | 2026-09-19 | The direct-PDF browser fallback downloaded and validated `10.3390/ijerph17186469` without intervention. The article planner is fixture-tested: the requested DOI and route PMCID must match page metadata before planning and download. The Chrome PDF shell is not an article fixture. |
| Ex Libris Alma (View It) | No download route — recognizes the resolver's empty-results terminal state so an unresolved holding classifies `no_entitlement` instead of `unknown` | `exlibris-primo` | No route | Not applicable | Alma pages with a real holding forward elsewhere and are not evidenced here; those stay assisted. |
| Ex Libris Primo | `/discovery/sourceRecord` delivery anchor (`a.anchor-tag-style`) read directly by href; a painted record with scoped record availability but no source anchor classifies `no_entitlement` | `primo` | Verified route | 2026-08-03 | Covers hosted Primo instances (`<inst>.primo.exlibrisgroup.com`); a custom-domain discovery front needs its own capture. Ten live captures measured the not-held state on 2026-08-30. The availability wording also appears on held records, so `deferUntilDeadline` keeps the negative rule out of early classification. At the 15 s deadline, the earlier article rule wins if the source link exists; otherwise `nde-record-availability .available-at-button` names non-entitlement. A record that never paints that container stays `unknown`. |
| HAL (open repository) | `citation_pdf_url` meta on a public repository record, fetched directly | `hal` | Verified route | 2026-07-20 | An open repository — no login required. Records without a deposited file omit the meta tag and stay assisted rather than being misclassified. |
| Hogrefe eContent | Article pages expose `citation_journal_title` metadata alongside a provider-owned `a[href^='/doi/pdf/']` anchor, read directly by href | `hogrefe` | Verified route | 2026-08-08 | Abstract and login shells lack the required article-metadata + PDF-anchor pair and stay assisted rather than being misclassified. |
| Informit | SAML terms-consent form gates a rendered `a.pdf-button[href^='/doi/pdf/']` control, clicked directly | `informit` | Human-assisted | 2026-08-03 | Terms consent is auto-accepted only with recorded consent; otherwise the human clicks through the SAML consent form. A `click` adapter — see Browser limitation below. |
| JAMA Network | Page script checks `data-article-url` before downloading, so the adapter clicks the `#pdf-link` control directly (this older JAMA control has no href) | `jamanetwork` | Verified route | 2026-07-20 | Gated on a full-access marker plus a free/open-access class, since sign-in and purchase controls also appear on free pages. A `click` adapter — see Browser limitation below. |
| JSTOR | `mfe-download-pharos-modal` terms-consent step gates a derived `/stable/pdf/<id>.pdf?acceptTC=1` endpoint | `jstor` | Human-assisted | 2026-08-03 | The viewer's own PDF control opens via `window.open`, which carries no user gesture and is blocked by Chrome's popup blocker, so the adapter fetches the direct endpoint instead. `acceptTC=1` is JSTOR's own terms acceptance, so the fetch runs only with recorded consent. |
| LWW / Wolters Kluwer Journals | `wkhealth_pdf_url` meta holds the exact PDF URL, fetched directly even when no anchor is rendered | `lww` | Verified route | 2026-07-20 | Requires the rendered full-text container, not just the metas, so abstract/paywall pages with PDF-shaped metadata stay assisted. |
| MDPI | `citation_pdf_url` meta plus a rendered `a.UD_ArticlePDF` anchor, read directly by href | `mdpi` | Verified route | 2026-08-04 | Verified against a public, fully open-access MDPI article. |
| MIT Press Direct | Silverchair `article-pdfLink` anchor read directly by href | `mitpress` | Verified route | 2026-07-20 | Paywalled abstract routes still publish `citation_pdf_url`; the rendered purchase-wall markers are checked first and take priority over the metadata. |
| Nature.com | `access=Yes` plus a rendered `download-pdf` control gates its href | `nature` | Verified route | 2026-07-20 | Nature publishes `citation_pdf_url` even on paywalled pages, so it is not used as the entitlement signal. Article-in-Press pages can serve `_reference.pdf` while their citation meta points elsewhere. |
| Open-access sources | Unpaywall and Europe PMC direct HTTP sources | None | No adapter needed for direct HTTP | Not applicable | These sources run before browser handoff. A blocked HTTP fetch can still need a browser route; the Europe PMC row covers that separate path. |
| Oxford Academic (OUP) | Silverchair `article-pdfLink` anchor read directly by href | `oup` | Verified route | 2026-07-20 | Denied journal pages render a `js-no-access-jumplink` control, checked before PDF metadata. A chapter wall captured on 2026-09-19 is now classified as login required using its restricted-chapter marker and institutional sign-in control. Chapter download and post-sign-in access remain unverified. |
| ProQuest | OpenURL handler and entitled docview PDF control | `proquest` | Verified working | 2026-09-19 | An isolated Chrome probe reached `ready` with a visually verified 31-page PDF after one Open action. Appending `accountid` unlocks the institutional route before the provider's federated-login fallback. A separate controlled repair test succeeded but recorded one duplicate download; two subsequent probes downloaded once, including one with no Open. This does not cover Ebook Central: two entitled books checked on 2026-09-19 offer partial PDF downloads or a time-limited loan, not a full-book PDF. |
| Psychiatry Online | Silverchair `data-article-access='full'` state plus a rendered `#downloadPdfUrl` anchor, read directly | `psychiatryonline` | Verified working | 2026-09-19 | A fresh isolated probe reached `ready` with the correct 10-page PDF after one explicit Open action and no manual PDF click. The probe used the exact article route; the institution's journal-homepage destination remains unresolved. Denied pages may still render `downloadPdfUrl`, so the full-access marker is checked first, and the PDF href must identify the requested DOI. |
| PubMed Central | Page-authored PDF URL and exact DOI evidence, through the generic acquisition planner | None | Verified working | 2026-09-19 | An isolated Chrome probe downloaded, adopted, and validated the correct 31-page author manuscript after one explicit Open. Generic execution now accepts the live state reached after authentication return; it still requires the daemon's current drive authorization. No PDF control was clicked. |
| SAGE Journals | Rendered `section.format--pdf_epub` panel gates a derived `/doi/pdf/<doi>?download=true` endpoint | `sage` | Verified route | 2026-07-27 | SAGE stopped rendering the earlier `a#downloadPdfUrl` anchor in July 2026; the adapter was rewritten to key on the semantic PDF/EPUB panel instead of a viewer-only eReader href. |
| ScienceDirect | Chrome: rendered access-bar **View PDF** anchor opens the provider viewer; viewer adoption captures its PDF. Firefox: human-assisted, because the route requires a click download | `sciencedirect` | Unverified | Not live-verified | **Two layouts, and only one has an access bar.** The rule carries one selector per layout, each scoped to a single anchor: `.accessbar .ViewPDF > a…` for the access-bar pages, and `.content-details-actions > .content-actions > a…` for an entitled subscription page that has no `.accessbar` container and no `.ViewPDF` at all (captured 2026-08-24, pii/S0747563216303168, control enabled, 261 KB fully rendered, read as a changed provider by the access-bar-only rule). Both match the href by path membership, never by a trailing anchor: re-measured live 2026-08-30 on doi 10.1016/j.sbspro.2014.01.1251, the real href is `/science/article/pii/<pii>/pdf?md5=<32 hex>&pid=<pii>-main.pdf`, and `sanitizeFixture` strips query strings — so the earlier `[href$='/pdf']` matched the sanitized fixture and could never match the live anchor, while `/pdfft` does not apply to that paper's route at all. Every fixture-backed test passed while the field classified `unknown`. The container scopes are what keep a recommended sibling unreachable, and both are pinned by a test that fails if either is widened to the anchor class alone. An anchor with no href (the unpainted signature `requiresVisible` exists for) still cannot match. Classification is live-verified against captures from a real session; the status stays **Unverified** because no validated artifact has been retained. |
| SpringerLink | Rendered `a[data-test='pdf-link']` anchor to `/content/pdf/`, read directly | `springer` | Verified route | 2026-07-14 | Verified live against both entitled and no-entitlement article states. |
| Taylor & Francis Online | Rendered `.downloadPDFLink a.show-pdf` anchor to `/doi/pdf/`, gated on an Open Access or full-access badge | `tandfonline` | Verified route | 2026-08-06 | Journal platform only — distinct from `taylorfrancis.com` books, whose `citation_pdf_url` can be a preview only. `no_entitlement` runs first since Access Denial pages carry no download control at all. |
| Thieme E-Journals | Rendered `#pdfLink` anchor, read directly, gated on the platform's full-text page state | `thieme` | Verified route | 2026-07-20 | Verified against public full-text pages; the abstract-only route stays unknown/assisted since `citation_pdf_url` and `#pdfLink` also appear there. |
| Wiley Online Library | `citation_pdf_url` is a viewer wrapper; the file is `/doi/pdfdirect/<doi>?download=true` | `wiley` | Verified working | 2026-07-17 | The adapter builds the direct endpoint from the DOI rather than downloading the wrapper. |

## Browser limitation

Firefox does not expose Chrome's `downloads.onDeterminingFilename` hook, so
*papio* cannot correlate a download back to its job by tab or provider host
there. The three `click`-method adapters — `informit`, `sciencedirect`,
and `jamanetwork` — stay human-assisted in Firefox by design:
the adapter does not invoke the page's download control at all, and the human
clicks it and then uses **Send PDF to papio** to adopt the file. Direct `href`,
`url`, `meta`, `post`, and `api` adapters carry their own job-scoped filename and are
unaffected; they remain subject to their individual route status above.

## Reporting a broken provider

When a previously working route changes, keep the job ID and run
`papio adapter diagnose <job-id>`. Report the provider, route, and diagnostic
output without credentials, cookies, or page contents.
