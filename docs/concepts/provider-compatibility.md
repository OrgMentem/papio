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

| Provider | Route observed | Adapter | Status | Last verified | Notes |
| --- | --- | --- | --- | --- | --- |
| ProQuest | OpenURL handler; requires the `accountid` parameter | `proquest` | Verified working | 2026-07-18 | Appending `accountid` unlocks the institutional route before the provider's federated-login fallback. |
| Wiley Online Library | `citation_pdf_url` is a viewer wrapper; the file is `/doi/pdfdirect/<doi>?download=true` | `wiley` | Verified working | 2026-07-17 | The adapter builds the direct endpoint from the DOI rather than downloading the wrapper. |
| SAGE Journals | `a#downloadPdfUrl`; `/doi/pdf/<doi>?download=true` is the file | `sage` | Verified route | 2026-07-17 | The live page route and file URL shape are verified; unknown UI still falls back to assisted behavior. |
| JSTOR | Terms-consent step before the PDF route | `jstor` | Human-assisted | 2026-07-14 | Terms consent remains a human action; the adapter does not accept it on your behalf. |
| Open-access sources | Unpaywall and Europe PMC direct sources | None | No adapter needed | Not applicable | These sources run before browser handoff. |

## Browser limitation

Firefox click adapters stay human-assisted by design. A click depends on an
explicit user gesture in that browser; *papio* does not synthesize one. Direct
URL, href, metadata, and API adapters remain subject to their individual route
status and the normal delegated-access safety contract.

## Reporting a broken provider

When a previously working route changes, keep the job ID and run
`papio adapter diagnose <job-id>`. Report the provider, route, and diagnostic
output without credentials, cookies, or page contents.
