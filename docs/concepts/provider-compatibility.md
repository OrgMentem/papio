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
| Annual Reviews | Open-access "Download PDF" control inside a JS-backed POST form; the control has no href, so it is clicked directly | `annualreviews` | Verified route | 2026-07-20 | Verified against a public open-access article. A `click` adapter — see Browser limitation below. |
| APA PsycNet | Stable `#pdf` anchor renders once the full article has loaded; denied records show a "Get Access" control instead | `psycnet` | Verified route | 2026-07-20 | Verified live in a fresh browser against a public full-text article and a denied record; `doi.apa.org` DOI landings route into the same application. |
| BMJ Journals | `citation_access=all` plus a rendered `article-pdf-download` anchor gates the file | `bmj` | Verified route | 2026-07-20 | Restricted to explicit `citation_access=all` pages — closed articles stay assisted even when they publish PDF-shaped citation metadata. |
| Cambridge Core | Action-bar `buttonSavePDFOptions` control plus a rendered `aop-cambridge-core/content/view` anchor | `cambridge` | Verified route | 2026-07-20 | Denied pages still publish `citation_pdf_url`, so the adapter requires the rendered PDF action instead; scoped to journals, not Cambridge's separate books PDF service. |
| ClinicalKey | Rendered `pdf-download-link` anchor to `/service/content/pdf/watermarked/<pii>.pdf` | `clinicalkey` | Verified route | 2026-08-06 | Only `clinicalkey.com.au` is covered; `clinicalkey.com` needs its own capture. Uses the largest settle timeout in the registry (15s) after a live resolver hop was observed still rendering a loading shell 10 seconds in. |
| EBSCOhost | The PDF viewer's `opid`/`recordId` build a call to EBSCO's researcher-edge-aggregator API, which returns the PDF URL as JSON | `ebsco` | Verified route | 2026-07-14 | Entitlement is inferred from the rendered PDF viewer (a `canvas`), since the record page's own download button is absent there; the constructed API URL is not independently confirmed to return a file. |
| Emerald Insight | Rendered PDF anchor — the current platform's `a.article-pdfLink`, or the legacy `a.intent_pdf_link` — read directly by href | `emerald` | Verified route | 2026-08-06 | Emerald migrated its delivery anchor; both selectors are kept until a current page is confirmed to no longer serve the legacy one. Entitlement is proved by the paywall rule not matching first, not by an Open Access badge. |
| Ex Libris Alma (View It) | No download route — recognizes the resolver's empty-results terminal state so an unresolved holding classifies `no_entitlement` instead of `unknown` | `exlibris-primo` | No route | Not applicable | Alma pages with a real holding forward elsewhere and are not evidenced here; those stay assisted. |
| Ex Libris Primo | `/discovery/sourceRecord` delivery anchor (`a.anchor-tag-style`) read directly by href | `primo` | Verified route | 2026-08-03 | Covers hosted Primo instances (`<inst>.primo.exlibrisgroup.com`); a custom-domain discovery front needs its own capture. |
| HAL (open repository) | `citation_pdf_url` meta on a public repository record, fetched directly | `hal` | Verified route | 2026-07-20 | An open repository — no login required. Records without a deposited file omit the meta tag and stay assisted rather than being misclassified. |
| Hogrefe eContent | Article pages expose `citation_journal_title` metadata alongside a provider-owned `a[href^='/doi/pdf/']` anchor, read directly by href | `hogrefe` | Verified route | 2026-08-08 | Abstract and login shells lack the required article-metadata + PDF-anchor pair and stay assisted rather than being misclassified. |
| Informit | SAML terms-consent form gates a rendered `a.pdf-button[href^='/doi/pdf/']` control, clicked directly | `informit` | Human-assisted | 2026-08-03 | Terms consent is auto-accepted only with recorded consent; otherwise the human clicks through the SAML consent form. A `click` adapter — see Browser limitation below. |
| JAMA Network | Page script checks `data-article-url` before downloading, so the adapter clicks the `#pdf-link` control directly (this older JAMA control has no href) | `jamanetwork` | Verified route | 2026-07-20 | Gated on a full-access marker plus a free/open-access class, since sign-in and purchase controls also appear on free pages. A `click` adapter — see Browser limitation below. |
| JSTOR | `mfe-download-pharos-modal` terms-consent step gates a derived `/stable/pdf/<id>.pdf?acceptTC=1` endpoint | `jstor` | Human-assisted | 2026-08-03 | The viewer's own PDF control opens via `window.open`, which carries no user gesture and is blocked by Chrome's popup blocker, so the adapter fetches the direct endpoint instead. `acceptTC=1` is JSTOR's own terms acceptance, so the fetch runs only with recorded consent. |
| LWW / Wolters Kluwer Journals | `wkhealth_pdf_url` meta holds the exact PDF URL, fetched directly even when no anchor is rendered | `lww` | Verified route | 2026-07-20 | Requires the rendered full-text container, not just the metas, so abstract/paywall pages with PDF-shaped metadata stay assisted. |
| MDPI | `citation_pdf_url` meta plus a rendered `a.UD_ArticlePDF` anchor, read directly by href | `mdpi` | Verified route | 2026-08-04 | Verified against a public, fully open-access MDPI article. |
| MIT Press Direct | Silverchair `article-pdfLink` anchor read directly by href | `mitpress` | Verified route | 2026-07-20 | Paywalled abstract routes still publish `citation_pdf_url`; the rendered purchase-wall markers are checked first and take priority over the metadata. |
| Nature.com | `access=Yes` plus a rendered `download-pdf` control gates its href | `nature` | Verified route | 2026-07-20 | Nature publishes `citation_pdf_url` even on paywalled pages, so it is not used as the entitlement signal. Article-in-Press pages can serve `_reference.pdf` while their citation meta points elsewhere. |
| Open-access sources | Unpaywall and Europe PMC direct sources | None | No adapter needed | Not applicable | These sources run before browser handoff. |
| Oxford Academic (OUP) | Silverchair `article-pdfLink` anchor read directly by href | `oup` | Verified route | 2026-07-20 | Denied pages redirect to `article-abstract` and render a `js-no-access-jumplink` control, checked first so a metadata-only paywall page cannot look entitled. |
| ProQuest | OpenURL handler; requires the `accountid` parameter | `proquest` | Verified working | 2026-07-18 | Appending `accountid` unlocks the institutional route before the provider's federated-login fallback. |
| Psychiatry Online | Silverchair `data-article-access='full'` state plus a rendered `#downloadPdfUrl` anchor, read directly | `psychiatryonline` | Verified route | 2026-07-20 | Denied pages may still render `downloadPdfUrl`, so the full-access marker is checked first. |
| SAGE Journals | Rendered `section.format--pdf_epub` panel gates a derived `/doi/pdf/<doi>?download=true` endpoint | `sage` | Verified route | 2026-07-27 | SAGE stopped rendering the earlier `a#downloadPdfUrl` anchor in July 2026; the adapter was rewritten to key on the semantic PDF/EPUB panel instead of a viewer-only eReader href. |
| ScienceDirect | `citation_pdf_url` meta (the Highwire/Google-Scholar standard) fetched directly, gated on recorded terms consent | `sciencedirect` | Unverified | Not live-verified | Live observations now include the primary access-bar path, viewer adoption, and provider outcome, but no retained validated artifact reached `ready`; page capture or provider outcome alone is insufficient to call the exact route canary-qualified or **Verified working**. The `no_entitlement` rule remains live-verified (2026-08-06, from a real institutional handoff). |
| SpringerLink | Rendered `a[data-test='pdf-link']` anchor to `/content/pdf/`, read directly | `springer` | Verified route | 2026-07-14 | Verified live against both entitled and no-entitlement article states. |
| Taylor & Francis Online | Rendered `.downloadPDFLink a.show-pdf` anchor to `/doi/pdf/`, gated on an Open Access or full-access badge | `tandfonline` | Verified route | 2026-08-06 | Journal platform only — distinct from `taylorfrancis.com` books, whose `citation_pdf_url` can be a preview only. `no_entitlement` runs first since Access Denial pages carry no download control at all. |
| Thieme E-Journals | Rendered `#pdfLink` anchor, read directly, gated on the platform's full-text page state | `thieme` | Verified route | 2026-07-20 | Verified against public full-text pages; the abstract-only route stays unknown/assisted since `citation_pdf_url` and `#pdfLink` also appear there. |
| Wiley Online Library | `citation_pdf_url` is a viewer wrapper; the file is `/doi/pdfdirect/<doi>?download=true` | `wiley` | Verified working | 2026-07-17 | The adapter builds the direct endpoint from the DOI rather than downloading the wrapper. |

## Browser limitation

Firefox does not expose Chrome's `downloads.onDeterminingFilename` hook, so
*papio* cannot correlate a download back to its job by tab or provider host
there. The three `click`-method adapters — `informit`, `jamanetwork`, and
`annualreviews` — stay human-assisted in Firefox by design: the adapter does
not invoke the page's download control at all, and the human clicks it and
then uses **Send PDF to papio** to adopt the file. Direct `href`, `url`,
`meta`, and `api` adapters carry their own job-scoped filename and are
unaffected; they remain subject to their individual route status above.

## Reporting a broken provider

When a previously working route changes, keep the job ID and run
`papio adapter diagnose <job-id>`. Report the provider, route, and diagnostic
output without credentials, cookies, or page contents.
