# Provider compatibility

This hand-maintained matrix records observed provider routes, not a promise that
every title, institution, entitlement, or browser session will work. A row marked
**Verified working** records an individually live-verified observation; it is not
a success rate. We do not invent aggregate rates or denominators: where there is
no measured population, none is implied. Unknown or changed provider UI remains
assisted behavior unless the optional agent fallback is configured.

The registered adapter list is intentionally narrower than the web. A provider
appears here only when there is a useful observed route to report; the extension
runs an adapter only after the user has granted its provider host permission.

The agent fallback also handles publishers with **no adapter**. In delegated
mode, after packaged and generic routes fail, it can select visible controls on
an already bound article with matching DOI metadata. It needs effective browser
access to that site, but no publisher entry in the adapter registry. The
implementation supports in-page menus and download controls. With a compatible
daemon, it can also follow an observed same-origin link in the same tab and
continue after verifying the destination's DOI. It stops on an unexpected
redirect or human gate. New tabs, cross-origin navigation, publisher search and
native PDF viewer saving remain future work.
An HTML wrapper that exposes one matching PDF is supported; this does not save
bytes already held inside the browser's native PDF viewer.
Chrome uses filename steering. Firefox uses a matching original article referrer
and an observed file in the configured download directory, with the daemon's
`native_click_adoption_v1` capability. Missing or ambiguous download provenance
remains assisted.
See [agent configuration](../reference/config-reference.md#agent-acquisition).
An isolated Chrome run of the integrated loop acquired and validated a five-page
IOS Press PDF after one explicit Open and one Jev decision. The test deliberately
omitted the packaged adapter. It proves that fallback mechanism, not a success
rate across publishers. A separate Windows Firefox run acquired and validated a
17-page eLife paper with no adapter, after one explicit Open and one Jev decision.
These are assisted starts followed by automatic acquisition. A normal-profile
Chrome run also acquired and validated a ten-page IEEE PDF through one Jev
decision and an HTML wrapper, 49.2 seconds after fresh submission. That attempt
needed no Open, publisher retry or manual PDF control; institutional sign-in
had been completed earlier. These individual results do not establish general
navigation or menu reliability.

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
| ACS Publications | Subscribed badge and primary `contentPdf` anchor with a same-origin `/article-pdf/` PDF path, read by href | `acs` | Unverified | 2026-09-23: entitled page capture | An entitled article for DOI `10.1021/acs.jcim.6c00481` exposed the primary `.pdf` link. Clicking Open PDF opened a viewer window without a browser download event. The fixture-backed adapter instead uses the browser downloads API. File bytes and PDF validation still need a live check. Supplementary links use a different route and stay out of scope. |
| Annual Reviews | Empty PDF POST form, downloaded through the browser API after checking the Open Access marker and full-text container | `annualreviews` | Verified working | 2026-09-21 | A fresh normal-profile Chrome job again reached a ProQuest no-results page through the institutional route. Explicit publisher retry reached `ready` in 13.1 seconds with the correct 21-page PDF; first page and page count were checked. No Jev call or manual PDF control was needed. Earlier sign-in and the retry are separate interventions. This confirms publisher recovery after the earlier isolated proof; the institutional route still fails. |
| APA PsycNet | Stable `#pdf` anchor renders once the full article has loaded; denied records show a "Get Access" control instead | `psycnet` | Verified route | 2026-07-20 | Verified live in a fresh browser against a public full-text article and a denied record; `doi.apa.org` DOI landings route into the same application. |
| BMJ Journals | `citation_access=all` plus a rendered `article-pdf-download` anchor gates the file | `bmj` | Verified route | 2026-07-20 | Restricted to explicit `citation_access=all` pages — closed articles stay assisted even when they publish PDF-shaped citation metadata. |
| Cambridge Core | Action-bar `buttonSavePDFOptions` control plus a rendered `aop-cambridge-core/content/view` anchor | `cambridge` | Verified route | 2026-07-20 | Denied pages still publish `citation_pdf_url`, so the adapter requires the rendered PDF action instead; scoped to journals, not Cambridge's separate books PDF service. |
| ChemRxiv | Rendered article-toolbar PDF link, bound to the primary self-citation DOI and exact preprint version | `chemrxiv` | Verified working | 2026-09-20 | An isolated Chrome probe downloaded, adopted and validated an 18-page PDF for the exact preprint version after two Open actions and an extension reload. The page omits `citation_doi`; its version-of-record DOI is a different identifier. Metadata alone, hidden download controls, supplements and other versions remain insufficient. |
| ClinicalKey | Article-title span and unique header PDF link to `/service/content/pdf/watermarked/<pii>.pdf` | `clinicalkey` | Verified working | 2026-09-19 | A live adapter download reached `ready` with the requested article in a validated 12-page PDF. A separate manual download also reached `ready`. Both the earlier and current captures produce executable plans. Only `clinicalkey.com.au` is covered; `clinicalkey.com` needs its own capture. The settle timeout remains 15 s. |
| Cochrane Library | Every PDF affordance, including `citation_pdf_url`, names `/pdf/full`, a 1.7 KB HTML viewer wrapper; the file is the nested `/pdf/CDSR/<code>/<code>.pdf` the wrapper's iframe names | `cochrane` | Verified route | 2026-08-24 | Entitlement is the rendered full-review PDF link (`a.pdf-link-full`), because Cochrane ships its institutional sign-in panel on entitled pages too. The abstract link (`a.pdf-link-abstract`) names a different document and is never the target. |
| EBSCOhost | Rendered PDF viewer, exact DOI metadata, and its researcher-edge-aggregator API; the returned file may use the declared EBSCO content endpoint | `ebsco` | Verified working | 2026-09-20 | Adapter 0.3.0 downloaded, adopted, and validated the correct 10-page PDF after one explicit Open and a previously completed institutional sign-in. The job stayed queued during 78 seconds unattended. No PDF control was clicked. Cold viewer rendering needs a bounded extra window; record-only pages and papers without matching DOI metadata stay assisted. Zotero import was not tested. |
| Emerald Insight | Rendered PDF anchor — the current platform's `a.article-pdfLink`, or the legacy `a.intent_pdf_link` — read directly by href | `emerald` | Verified route | 2026-08-06 | Emerald migrated its delivery anchor; both selectors are kept until a current page is confirmed to no longer serve the legacy one. Entitlement is proved by the paywall rule not matching first, not by an Open Access badge. |
| Europe PMC | Exact `/api/getPdf?pmcid=PMC<id>` direct PDF route; rendered `#open_pdf` control on `/article/PMC/<id>` also gates a constructed file URL | `europepmc` | Verified working | 2026-09-19 | The direct-PDF browser fallback downloaded and validated `10.3390/ijerph17186469` without intervention. The article planner is fixture-tested: the requested DOI and route PMCID must match page metadata before planning and download. The Chrome PDF shell is not an article fixture. |
| Ex Libris Alma (View It) | No download route — recognizes the resolver's empty-results terminal state so an unresolved holding classifies `no_entitlement` instead of `unknown` | `exlibris-primo` | No route | Not applicable | Alma pages with a real holding forward elsewhere and are not evidenced here; those stay assisted. |
| Ex Libris Primo | `/discovery/sourceRecord` delivery anchor (`a.anchor-tag-style`) read directly by href; a painted record with scoped record availability but no source anchor classifies `no_entitlement` | `primo` | Verified route | 2026-08-03 | Covers hosted Primo instances (`<inst>.primo.exlibrisgroup.com`); a custom-domain discovery front needs its own capture. Ten live captures measured the not-held state on 2026-08-30. The availability wording also appears on held records, so `deferUntilDeadline` keeps the negative rule out of early classification. At the 15 s deadline, the earlier article rule wins if the source link exists; otherwise `nde-record-availability .available-at-button` names non-entitlement. A record that never paints that container stays `unknown`. |
| Figshare | Explicit unavailable-file status heading on a rendered repository record | `figshare` | No route | 2026-09-20 | A fresh Chrome probe reported the captured “File(s) not publicly available” state and automatically offered institutional fallback. Only the unique status heading supplies that evidence; matching words elsewhere on the page are insufficient. This ends the repository route without claiming institutional non-entitlement. No PDF download route is declared. |
| HAL (open repository) | `citation_pdf_url` meta on a public repository record, fetched directly | `hal` | Verified route | 2026-07-20 | An open repository — no login required. Records without a deposited file omit the meta tag and stay assisted rather than being misclassified. |
| Hogrefe eContent | Article pages expose `citation_journal_title` metadata alongside a provider-owned `a[href^='/doi/pdf/']` anchor, read directly by href | `hogrefe` | Verified working | 2026-09-20 | A fresh isolated Chrome probe downloaded once, adopted, and validated the correct 13-page article in 17.37 seconds without an Open action or PDF click. Validation removed an embedded attachment and retained the article. Zotero import was disabled. Abstract and login shells lack the required article-metadata + PDF-anchor pair and stay assisted. |
| Informit | SAML terms-consent form gates a rendered `a.pdf-button[href^='/doi/pdf/']` control, clicked directly | `informit` | Human-assisted | 2026-08-03 | Terms consent is auto-accepted only with recorded consent; otherwise the human clicks through the SAML consent form. A `click` adapter — see Browser limitation below. |
| JAMA Network | Page script checks `data-article-url` before downloading, so the adapter clicks the `#pdf-link` control directly (this older JAMA control has no href) | `jamanetwork` | Verified route | 2026-07-20 | Gated on a full-access marker plus a free/open-access class, since sign-in and purchase controls also appear on free pages. A `click` adapter — see Browser limitation below. |
| IOS Press ebooks | Open-access article's scoped PDF POST form, activated through its rendered control | `iospress` | Verified working | 2026-09-20 | A fresh isolated Chrome probe downloaded once, adopted and validated the correct five-page paper in 5.34 seconds, with no Open action or manual PDF click. The same session had been used for diagnostic capture beforehand. The adapter requires exact DOI evidence and a Creative Commons license marker. Book-series pages, supplements, disabled controls and ambiguous targets remain excluded. Subscription access is unverified; Zotero import was disabled. |
| JSTOR | Article title and primary control's stable record ID bind a consent-gated `/stable/pdf/<id>.pdf?acceptTC=1` endpoint | `jstor` | Human-assisted | 2026-09-21: article and identity binding | The control's `data-doi` contains a numeric JSTOR record ID, not a DOI. Adapter 0.3.1 matches the page title and checks that the primary control's ID matches the URL route, including immediately before download. Its direct endpoint avoids the viewer's gesture-dependent popup. `acceptTC=1` accepts JSTOR's terms, so the fetch still requires recorded consent. A new live downloaded artifact remains unverified. |
| LWW / Wolters Kluwer Journals | `wkhealth_pdf_url` meta holds the exact PDF URL, fetched directly even when no anchor is rendered | `lww` | Verified route | 2026-07-20 | Requires the rendered full-text container, not just the metas, so abstract/paywall pages with PDF-shaped metadata stay assisted. |
| MDPI | `citation_pdf_url` meta plus a rendered `a.UD_ArticlePDF` anchor, read directly by href | `mdpi` | Verified route | 2026-08-04 | Verified against a public, fully open-access MDPI article. |
| MIT Press Direct | Silverchair `article-pdfLink` anchor read directly by href | `mitpress` | Verified route | 2026-07-20 | Paywalled abstract routes still publish `citation_pdf_url`; the rendered purchase-wall markers are checked first and take priority over the metadata. |
| Nature.com | `access=Yes` plus a rendered `download-pdf` control gates its href | `nature` | Verified route | 2026-07-20 | Nature publishes `citation_pdf_url` even on paywalled pages, so it is not used as the entitlement signal. Article-in-Press pages can serve `_reference.pdf` while their citation meta points elsewhere. |
| Open-access sources | Unpaywall and Europe PMC direct HTTP sources | None | No adapter needed for direct HTTP | Not applicable | These sources run before browser handoff. A blocked HTTP fetch can still need a browser route; the Europe PMC row covers that separate path. |
| Oxford Academic (OUP) | Rendered Silverchair `article-pdfLink` for journal `article-pdf` and chapter `chapter-ag-pdf` routes | `oup` | Human-assisted | 2026-09-20 | Adapter 0.1.2 downloaded and adopted a 36-page chapter after institutional navigation, explicit Opens, and recovery from a stranded sign-in probe. First page and page count were checked. Validation parked the file for identity review because its front matter contains both book and chapter DOIs; ready/import is unproven. Login walls and PDF metadata alone remain insufficient. The isolated probe used a direct DOI route, not the institution's resolver. |
| PLOS ONE | Exact `journals.plos.org/plosone/article/file` route with an article DOI and `type=printable` | None (direct PDF) | Verified working | 2026-09-20 | A fresh isolated Chrome probe downloaded once, adopted, and validated the correct five-page article in 4.36 seconds. No Open action or PDF click occurred during that probe; the URL had been inspected in the same browser session beforehand. Supplements, extra or duplicate query parameters, and other PLOS routes remain excluded. Zotero import was disabled. |
| ProQuest | OpenURL handler and entitled docview PDF control | `proquest` | Verified working | 2026-09-19 | An isolated Chrome probe reached `ready` with a visually verified 31-page PDF after one Open action. Appending `accountid` unlocks the institutional route before the provider's federated-login fallback. A separate controlled repair test succeeded but recorded one duplicate download; two subsequent probes downloaded once, including one with no Open. Ebook Central is excluded from this article adapter and remains assisted: two entitled books checked on 2026-09-19 offer partial PDF downloads or a time-limited loan, not a full-book PDF. |
| Psychiatry Online | Silverchair `data-article-access='full'` state plus a rendered `#downloadPdfUrl` anchor, read directly | `psychiatryonline` | Verified working | 2026-09-20 | A fresh isolated probe reached the institution's journal-homepage destination and reported drift. One explicit publisher retry reached `ready` with the correct 10-page PDF; the article tab was also focused for inspection. No manual PDF click was used. This proves operator-assisted recovery; the institutional route still needs correction. Denied pages may still render `downloadPdfUrl`, so the full-access marker is checked first, and the PDF href must identify the requested DOI. |
| PubMed Central | Page-authored PDF URL and exact DOI evidence, through the generic acquisition planner | None | Verified working | 2026-09-19 | An isolated Chrome probe downloaded, adopted, and validated the correct 31-page author manuscript after one explicit Open. Generic execution now accepts the live state reached after authentication return; it still requires the daemon's current drive authorization. No PDF control was clicked. |
| SAGE Journals | Rendered `section.format--pdf_epub` panel gates a derived `/doi/pdf/<doi>?download=true` endpoint | `sage` | Verified route | 2026-07-27 | SAGE stopped rendering the earlier `a#downloadPdfUrl` anchor in July 2026; the adapter was rewritten to key on the semantic PDF/EPUB panel instead of a viewer-only eReader href. |
| ScienceDirect | Chrome: rendered View PDF control opens the provider viewer. Chrome downloads a signed viewer link once under the job binding; if that returns HTML or fails, choose **Send this PDF**, then the viewer’s **Download** button. Firefox remains human-assisted | `sciencedirect` | Unverified | 2026-09-20: viewer and manual guidance verified | A fresh isolated direct-provider probe reached the correct eight-page viewer without an Open action, then retained a manual-download task and displayed the matching popup guidance. No file was downloaded or adopted. Earlier signed-URL re-fetches and a fresh complete article href requested through Chrome downloads returned non-PDF content. The manual task releases browser capacity and grants no download authority until Send PDF binds the current document. The adapter supports the captured access-bar and content-actions layouts, binds the page DOI, and scopes controls away from recommended articles. Adapter 0.8.2 also recognizes the captured paired access-bar sign-in layout and refuses disabled PDF controls, including after planning. A purchase control beside institutional access is sign-in evidence, not proof of no entitlement. |
| SpringerLink | Article-header `a[data-test='pdf-link']` anchor to `/content/pdf/`, read directly | `springer` | Verified working | 2026-09-20 | A fresh isolated Chrome probe reached `ready` with the correct 16-page open-access article after one explicit Open and no manual PDF click. Adapter 0.1.2 scopes the download to `.app-masthead__access-container`; the identical sticky-banner link previously made the plan ambiguous. The access panel remains sign-in pending. Subscription access and full-book acquisition are not established by this article result. |
| Taylor & Francis Online | Rendered `.downloadPDFLink a.show-pdf` anchor to `/doi/pdf/`, gated on an Open Access or full-access badge | `tandfonline` | Verified route | 2026-08-06 | Journal platform only — distinct from `taylorfrancis.com` books, whose `citation_pdf_url` can be a preview only. `no_entitlement` runs first since Access Denial pages carry no download control at all. |
| Thieme E-Journals | Rendered `#pdfLink` anchor, read directly, gated on the platform's full-text page state | `thieme` | Verified route | 2026-07-20 | Verified against public full-text pages; the abstract-only route stays unknown/assisted since `citation_pdf_url` and `#pdfLink` also appear there. |
| Wiley Online Library | `citation_pdf_url` is a viewer wrapper; the file is `/doi/pdfdirect/<doi>?download=true` | `wiley` | Verified working | 2026-09-20 | A fresh isolated probe fell back from an unavailable Figshare file to institutional access. One explicit Open reused the existing SSO session; papio then downloaded, adopted, and validated the correct 10-page PDF in 30.53 seconds without a manual PDF click. A preceding direct publisher retry returned HTML, so the institutional route mattered. Zotero import was disabled. |

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
