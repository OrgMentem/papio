// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio provider adapters are DECLARATIVE selector/pattern specs (source-
// controlled, versioned) plus the types that describe them. This file DECLARES
// them only: the single generic function that interprets a spec is
// `planExecution` in extension/src/plan.ts, and the injection constraint lives
// there with it — `planExecution` is intentionally self-contained (it
// references no module import, helper, or closure at runtime), so the
// background service worker can hand it verbatim to
// chrome.scripting.executeScript with the matched spec + args as JSON. That
// same function is unit-tested against happy-dom fixtures.
//
// There is NO free-form injected code and NO "click the likely download
// button" fallback: a page that matches no rule classifies as `unknown`, and
// the extension stays in assisted behaviour.

export type PageKind =
  | "article"
  | "login"
  | "terms"
  | "no_entitlement"
  | "wrong_work_check"
  | "unknown";

export interface ClassifyRule {
  kind: PageKind;
  /** Every CSS selector must match for the rule to fire. */
  all?: string[];
  /** At least one CSS selector must match for the rule to fire. */
  any?: string[];
  /** At least one lowercase substring must appear in textSelector's innerText
   * (or document.body.innerText when omitted), compared lowercased.
   * Static labels only — never page-derived text. */
  textAny?: string[];
  /** Scope textAny to exactly one matching element. Missing or ambiguous
   * matches refuse the rule; omitted retains the document-body text scope. */
  textSelector?: string;
  /** On live pages, this rule may classify only after the full settle budget.
   *
   * A positive marker can paint before the marker that would select an earlier
   * rule. So a deferred rule neither declares readiness nor participates in an
   * early classification triggered by some other rule. Fixture Documents are
   * already complete and evaluate it immediately.
   *
   * Primo is the measured case. Its availability control and source link live
   * 17 KB and 26 containers apart, so a held record can paint the control first.
   * At the deadline the earlier article rule wins if the source link exists;
   * otherwise the availability control names the not-held state. */
  deferUntilDeadline?: boolean;
}

export interface WorkEvidenceContract {
  /** Exact packaged page-side identity evidence for the requested work. */
  kind: "doi" | "title";
  selector: string;
  /** Attribute holding the identity. Omit it to read the element's own text,
   * which is the only evidence some providers expose: ProQuest's docview
   * prints the title in `h1#documentTitle` and carries no citation meta tag
   * at all, so an attribute-only contract left that provider permanently
   * assisted. */
  attribute?: string;
  /** Optional extraction pattern; group 1 is the identity value. */
  pattern?: string;
}

export interface DownloadDestinationContract {
  /** Exact HTTPS origin authorized by the packaged adapter. */
  origin: string;
  /** Explicit path prefix authorized on that origin. */
  pathPrefix: string;
}

export interface DownloadTargetContract {
  /**
   * The packaged adapter's declared relation between the selected effect and
   * the requested work. `doi` reads an exact identifier from the selected
   * element; `opaque` is reserved for provider controls whose identity is
   * intentionally not URL-shaped, and means the selector itself is the
   * provider's work-bound control.
  */
  kind: "doi" | "opaque";
  /** Optional exact element carrying the target identity; defaults to the
   * selected action element. */
  selector?: string;
  /** Attribute carrying the DOI when kind is `doi` (for example `data-doi`,
   * `content`, or `href`). Omitted for opaque provider controls. */
  attribute?: string;
  /** Optional explicit extraction pattern for a DOI-bearing attribute. The
   * first capture group is the DOI; no URL inference is performed otherwise. */
  pattern?: string;
}

export interface RouteIdentityContract {
  /** Exact page element carrying the identifier used to build a URL route. */
  selector: string;
  /** Attribute carrying that identifier. */
  attribute: string;
  /** Optional extraction pattern. Group 1 must equal idPattern group 1. */
  pattern?: string;
}

export interface ProviderViewerRoute {
  /** Exact leading pathname identifying the provider's journal viewer. */
  pathPrefix: string;
  /** Optional viewer-specific extraction/build pair. Omit both to reuse the
   * download rule's idPattern and urlTemplate. */
  idPattern?: string;
  urlTemplate?: string;
}

export interface DownloadRule {
  selector: string;
  requireKind: "article";
  /** Explicitly binds the selected effect to the requested work. */
  workTarget?: DownloadTargetContract;
  /** `href` extracts an HTTPS anchor and uses chrome.downloads.download.
   * `click` activates the explicitly selected element (or an explicitly
   * selected control in its open shadow root).
   * `url` constructs the direct PDF endpoint from the page URL (idPattern +
   * urlTemplate) and fetches it via chrome.downloads.download — no click, no
   * gesture. The privileged downloads API carries the session cookies, so an
   * entitled endpoint (e.g. JSTOR /stable/pdf/<id>.pdf) is fetched
   * autonomously. */
  /** Page-derived href/meta/API destinations require this packaged envelope
   * when they leave the current page origin. */
  allowedDestinations?: DownloadDestinationContract[];
  /** `post` downloads an empty HTML POST form's explicit same-origin PDF
   * action. Forms with fields are refused; this never submits consent,
   * credentials, or other page data and never opens a named viewer window. */
  method: "href" | "click" | "url" | "api" | "meta" | "post";
  shadowSelector?: string;
  /** Wait for this fixture-backed in-page gate before reclassification. */
  postClickWaitFor?: string;
  /** After the first click, wait for and click this one fixture-backed control
   * (for provider-owned download modals; never terms/consent controls). */
  followupSelector?: string;
  /** Shared bounded wait for post-click gate/follow-up insertion. */
  postClickTimeoutMs?: number;
  /** Unambiguous packaged provider viewers and their direct-PDF mapping. */
  viewerRoutes?: ProviderViewerRoute[];
  /** method "url"/"api": regex matched against the page URL; capture groups fill
   * {1},{2},… (and {id} = {1}) in urlTemplate. */
  idPattern?: string;
  /** Bind idPattern group 1 to independent page metadata before building a
   * URL, then revalidate both the route and metadata before the effect. */
  routeIdentity?: RouteIdentityContract;
  /** method "url": the resolved HTTPS PDF endpoint. method "api": an HTTPS
   * endpoint returning JSON whose jsonField holds the PDF URL. */
  urlTemplate?: string;
  /** method "url": fetch the endpoint only when the user has recorded consent to
   * auto-accept publisher terms (the fetch bypasses the terms UI); without
   * consent the gate stays human, prompted once. */
  requiresTermsConsent?: boolean;
  /** method "api": field in the urlTemplate JSON response holding the PDF URL. */
  jsonField?: string;
  /** method "meta": name of the page meta tag whose content is the entitled PDF
   * URL (default "citation_pdf_url", the Highwire/Google-Scholar standard
   * that Elsevier/ScienceDirect and others expose). The URL is fetched via the
   * privileged downloads API — no click, no gesture — like the "url" method. */
  metaName?: string;
}

export interface AdapterSpec {
  id: string;
  version: string;
  hosts: string[];
  /** Separate platforms within a provider domain that this adapter cannot
   * classify. Each exclusion also covers its subdomains. */
  excludedHosts?: string[];
  /** Ordered rules; first match wins. */
  classify: ClassifyRule[];
  /** Exact packaged page evidence used to bind expected DOI/title identity. */
  workEvidence?: WorkEvidenceContract;
  /** On live SPA pages only, wait this long for a complete rule's declared
   * selectors to hydrate before classifying. Fixture Documents stay synchronous. */
  settleTimeoutMs?: number;
  /** Extra bounded render window after the first inconclusive plan. Defaults
   * to 5 seconds; the worker caps provider overrides at 60 seconds. */
  unknownGraceMs?: number;
  download?: DownloadRule;
  /** Minimized work windows under-render some provider SPAs; keep this adapter's
   * handoff window visible without focusing it. */
  requiresVisible?: boolean;
  /** The terms-and-conditions accept control, found by accessible text inside
   * the open modal. Clicked ONLY when the user has recorded informed consent to
   * auto-accept publisher terms; otherwise the terms gate stays human. */
  termsAccept?: TermsAcceptRule;
  /** Provider federated-login entry, used ONLY on a `login` verdict when the
   * job offer carries a `login_entity_id`. `{entityID}` is replaced with the
   * URL-encoded institution entityID; papio navigates the handoff tab there to
   * auto-select the institution (skipping the provider's institution picker),
   * leaving credential entry to the human. Absent = surface the wall as-is. */
  federatedLogin?: string;
  /** Query param this provider's openurl handler needs to unlock institutional
   * access (ProQuest: "accountid"). On a `login` verdict, if the offer carries
   * a provider account id, papio appends `?<param>=<id>` to the current URL —
   * fully autonomous, no sign-in. Tried before federatedLogin. */
  accountIdParam?: string;
}

export interface TermsAcceptRule {
  /** The open terms modal container (same selector as the `terms` classify rule). */
  modalSelector: string;
  /** Optional fixture-backed accept control. When present, click it directly
   * instead of inferring the control from accessible text. */
  control?: string;
  /** Accessible-text needles identifying the accept-and-download control. */
  textAny: string[];
}

export interface PageVerdict {
  kind: PageKind | "wrong_work";
  adapter_id: string;
  adapter_version: string;
  /** Static rule labels only (e.g. `rule:article matched`). NEVER page text. */
  evidence: string[];
}

/** Match a packaged adapter's domain scope, including platform exclusions. */
export function adapterSupportsHost(host: string, spec: AdapterSpec): boolean {
  const normalized = host.toLowerCase();
  const matches = (domain: string): boolean =>
    normalized === domain || normalized.endsWith(`.${domain}`);
  return spec.hosts.some(matches) && !(spec.excludedHosts ?? []).some(matches);
}

/**
 * Registered provider adapters, in plan order. Every spec is fixture-backed:
 * a rule may only reference markers proven by a captured fixture under
 * extension/fixtures/<id>/. States without a fixture (e.g. a real logged-out
 * ProQuest wall — the header embeds a decorative login form on EVERY page, so
 * no safe selector exists without a genuine capture) are deliberately absent
 * and classify as `unknown` -> assisted behaviour. The hello frame reports
 * `{ [spec.id]: spec.version }` for every entry here, and the background
 * classifier only ever runs a spec drawn from this registry — on a host both
 * advertised here and granted by the user.
 */
export const adapters: AdapterSpec[] = [
  {
    // Captured 2026-09-20 on an open-access IOS Press ebooks article. The
    // page owns a POST form and a JS-backed div control; no PDF URL is exposed.
    // Click only that form's control, with the page DOI and license present.
    id: "iospress",
    version: "0.1.0",
    hosts: ["ebooks.iospress.nl"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [{
      kind: "article",
      all: [
        "meta[name='citation_title']",
        "main#contentcolumn .content > .actions > .openaccesslicense a[rel='license'][href^='https://creativecommons.org/licenses/']",
        "main#contentcolumn .content > .actions > form[action='/Download/Pdf'][method='post'][id^='downloadform'] > div.button.getpdf[id^='downloadlink']:not([aria-disabled='true']):not([disabled])",
      ],
    }],
    download: {
      selector: "main#contentcolumn .content > .actions > form[action='/Download/Pdf'][method='post'][id^='downloadform'] > div.button.getpdf[id^='downloadlink']:not([aria-disabled='true']):not([disabled])",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
    },
  },
  {
    id: "chemrxiv",
    version: "0.1.0",
    hosts: ["chemrxiv.org"],
    // This platform omits citation_doi. Its primary self-citation names the
    // exact preprint version; the separate version-of-record link does not.
    workEvidence: {
      kind: "doi",
      selector: ".core-self-citation .doi a[property='sameAs']",
      attribute: "href",
      pattern: "^https://doi\\.org/(10\\.26434/chemrxiv-[^?#]+)(?:[?#].*)?$",
    },
    classify: [{
      kind: "article",
      all: [
        "meta[name='citation_fulltext_world_readable']",
        "meta[name='citation_article_type'][content='preprint']",
        ".core-self-citation .doi a[property='sameAs']",
        ".info-panel__formats a.btn--pdf[href*='/doi/pdf/']",
      ],
    }],
    // The page repeats the PDF link in view options and in a hidden credits
    // section. Only the rendered article toolbar proves this route is usable.
    download: {
      method: "href",
      selector: ".info-panel__formats a.btn--pdf[href*='/doi/pdf/']",
      requireKind: "article",
      workTarget: {
        kind: "doi",
        attribute: "href",
        pattern: "^(?:https://chemrxiv\\.org)?/doi/pdf/(10\\.26434/chemrxiv-[^?#]+)(?:[?#].*)?$",
      },
    },
  },
  {
    // Verified live 2026-07-14 against Example University-authenticated ProQuest
    // (fixtures/proquest/*.html). The PDF link id is document-scoped
    // (`downloadPDFLink_MSTAR_<docid>`), hence the prefix selector.
    // A docview page without that link (citation-only, HTML-only, or
    // unentitled) stays `unknown`: distinguishing those needs fixtures
    // we do not have yet.
    id: "proquest",
    version: "0.3.1",
    // The docview carries no citation meta tag at all, so the printed title in
    // the stable `documentTitle` id is the only identity evidence on the page.
    // Without a declared contract the planner refused every real job — a job
    // always supplies a requested identity — so ProQuest, this resolver's
    // highest-volume destination, was permanently human-assisted while its
    // fixture test still reported the `article` verdict.
    hosts: ["proquest.com"],
    // Ebook Central offers book reading, chapter exports and loans; it does
    // not use this article platform's docview controls or sign-in route.
    excludedHosts: ["ebookcentral.proquest.com"],
    workEvidence: { kind: "title", selector: "h1#documentTitle" },
    classify: [
      // ProQuest's "Find your institution" wall (fixtures/proquest/login-return.html):
      // when the resolver routes here without a ProQuest institutional session,
      // it blocks the article behind an institution-selection form instead of
      // showing the download link. Classify it `login` (ordered first) so papio
      // surfaces it as a human sign-in step rather than staying silently
      // assisted/unknown — Example University routes heavily through ProQuest. After the user
      // authenticates (OpenAthens/Shibboleth → Example University), the re-drive lands on the
      // entitled docview matched by the article rule below.
      { kind: "login", all: ["form#institutionForm", "input#institutionName"] },
      { kind: "article", all: ["a[id^='downloadPDFLink_']", "h1"] },
    ],
    download: {
      selector: "a[id^='downloadPDFLink_']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      allowedDestinations: [{ origin: "https://media.proquest.com", pathPrefix: "/media/" }],
      method: "href",
    },
    // On the login wall, route straight to the institution's Shibboleth login
    // via ProQuest's discovery-service entry with the configured entityID,
    // skipping the "Find your institution" picker. {entityID} is filled from the
    // offer's login_entity_id; the target returns to ProQuest, and papio
    // re-drives the openurl once the session is warm. Verified live 2026-07-17:
    // this DS URL with Example University's entityID routes directly to idp.example.edu login.
    // Preferred over federatedLogin for ProQuest: appending ?accountid=<id>
    // unlocks Example University's institutional access with no sign-in at all (verified live
    // 2026-07-18 — resolves the wall cold, "Access provided by EXAMPLE
    // UNIVERSITY"). federatedLogin stays as a fallback when no account id is set.
    accountIdParam: "accountid",
    federatedLogin:
      "https://shibboleth-sp.prod.proquest.com/Shibboleth.sso/DS?entityID={entityID}&target=https://shibboleth-sp.prod.proquest.com/ONE_SEARCH/PRODWWW",
  },
  {
    // Captured 2026-08-03 from institutionally entitled JSTOR pages: the stable/ viewer
    // (fixtures/jstor/success.html) and the article record page
    // (fixtures/jstor/record.html, stable/45277272). Both render the same
    // primary control (data-qa='download-pdf', data-doi, data-sc='but
    // click:pdf download', variant='primary') but wire it differently: the
    // viewer downloads on click while the record page calls window.open with
    // ?acceptTC=1 — and a programmatic adapter click carries no user gesture,
    // so Chrome's popup blocker eats it (live field report 2026-08-03). The
    // download therefore derives the direct endpoint from the tab URL and
    // fetches it with the privileged downloads API (cookie-authenticated, no
    // popup, no gesture). acceptTC=1 IS JSTOR's terms acceptance — the bare
    // endpoint returns a terms interstitial (verified 2026-07) — so the rule
    // is consent-gated: without recorded auto-accept consent the page stays
    // assisted and the human clicks through the terms modal themselves.
    id: "jstor",
    version: "0.3.1",
    hosts: ["jstor.org"],
    // data-doi is a JSTOR stable ID, not a DOI. The packaged title identifies
    // the work; the primary control independently binds the URL's stable ID.
    workEvidence: {
      kind: "title",
      selector: "meta[property='og:title']",
      attribute: "content",
      pattern: "^(.+) \\| JSTOR$",
    },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "terms",
        all: ["mfe-download-pharos-modal.terms-and-conditions[open]"],
        textAny: ["accept and download"],
      },
      {
        kind: "login",
        all: [".turnaway-access-option-content__title"],
        textAny: ["log in through your school or library", "this is a preview. log in through your library"],
      },
      {
        kind: "article",
        all: [
          "mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']",
        ],
      },
    ],
    download: {
      selector:
        "mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      allowedDestinations: [{ origin: "https://www.jstor.org", pathPrefix: "/stable/pdf/" }],
      method: "url",
      idPattern: "^https://www\\.jstor\\.org/stable/(?:pdf/)?(\\d+)(?:\\.pdf)?(?:[?#]|$)",
      routeIdentity: {
        selector:
          "mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']",
        attribute: "data-doi",
      },
      urlTemplate: "https://www.jstor.org/stable/pdf/{id}.pdf?acceptTC=1",
      requiresTermsConsent: true,
    },
    termsAccept: {
      modalSelector: "mfe-download-pharos-modal.terms-and-conditions[open]",
      textAny: ["accept and download"],
    },
  },
  {
    // Captured 2026-08-03 from the institutionally entitled Informit article record at
    // https://search.informit.org/doi/10.3316/informit.TOKEN
    // (fixtures/informit/success.html). Atypon exposes both reader and PDF
    // anchors, but /doi/pdf can be bot-gated or return a viewer wrapper rather
    // than PDF bytes. Invoke the captured PDF control so the browser's native
    // click/download correlation supplies the evidence, instead of extracting
    // or synthesizing an endpoint.
    id: "informit",
    version: "0.1.0",
    hosts: ["search.informit.org"],
    workEvidence: { kind: "doi", selector: "[data-doi]", attribute: "data-doi" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "terms",
        all: [
          "form.saml__consent__form",
          "form.saml__consent__form input.saml__consent__yes[type='submit']",
        ],
      },
      {
        kind: "article",
        all: [
          "[data-doi]",
          "a[aria-label='View PDF'].main-link[href^='/doi/reader/']",
          "a.pdf-button[href^='/doi/pdf/']",
        ],
      },
    ],
    download: {
      selector: "a.pdf-button[href^='/doi/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
    },
    termsAccept: {
      modalSelector: "form.saml__consent__form",
      control: "input.saml__consent__yes",
      textAny: ["i have read and agree to the terms and conditions"],
    },
  },
  {
    // Captured 2026-08-03 from a institutionally entitled Primo NDE full-display record
    // (fixtures/primo/success.html): Ex Libris Primo's own "Get PDF" delivery
    // anchor for Open Access and held items. The classify key is the
    // language-independent /discovery/sourceRecord href, not the localized
    // label. sanitizeFixture strips the anchor's query string, but method
    // "href" reads the LIVE anchor at download time, so the runtime request
    // carries the full delivery parameters. Hosts cover hosted Primo
    // instances (<inst>.primo.exlibrisgroup.com); custom-domain fronts like
    // custom-domain discovery fronts need their own captured evidence before joining.
    // A record page the library does not hold, measured 2026-08-30 across ten
    // live resolver captures (fixtures/primo/no-entitlement.html is one of
    // them, redacted). Only the source link separates it from a held record:
    // the availability control, its "Get it for me from other libraries"
    // label, and the not-linkable record title are present in BOTH cases, so
    // no positive marker names the negative one.
    //
    // Ordering a positive rule first cannot decide a page that is still
    // painting. In fixtures/primo/success.html the availability control and
    // source path start at bytes 35130 and 52954 - 17824 bytes and 26 closing
    // containers apart. `deferUntilDeadline` keeps the negative rule out of
    // every early classification. At the deadline, the article rule
    // wins when the source link exists; otherwise the availability control
    // names the not-held state.
    //
    // A shell that never paints its availability control matches neither rule
    // and stays `unknown`, which is the truth about it.
    //
    // The budget is the ClinicalKey value and the same reason applies with more
    // force: that entry records a resolver hop eating the render budget, and
    // this adapter IS the resolver. Eight of the ten captures were shells
    // between 1.1 KB and 29.9 KB with no availability control rendered.
    //
    // Human-assisted by design, for now: this adapter declares no
    // workEvidence, so the planner refuses to execute its article effect
    // whenever a job supplies a requested identity — which every real job
    // does. That refusal is correct, not an oversight: the committed capture
    // exposes the title only in Angular-generated class names and in a
    // localized `aria-label` ("Get PDF for <title>, opens in a new window"),
    // and the sanitizer rewrites unstable tokens, so neither is a
    // trustworthy live selector. A stable identity node has to come from a
    // fresh capture before this can bind. TestPrimoStaysAssistedWithoutWorkEvidence
    // pins the refusal so the green fixture test cannot read as a working
    // automated path.
    id: "primo",
    version: "0.3.0",
    hosts: ["primo.exlibrisgroup.com"],
    settleTimeoutMs: 15000,
    classify: [
      {
        kind: "article",
        all: ["a.anchor-tag-style[href*='/discovery/sourceRecord']"],
      },
      {
        kind: "no_entitlement",
        all: ["nde-record-availability .available-at-button"],
        deferUntilDeadline: true,
      },
    ],
    download: {
      selector: "a.anchor-tag-style[href*='/discovery/sourceRecord']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Captured 2026-08-04 from an institutionally entitled ClinicalKey
    // full-text article (fixtures/clinicalkey/success.html). The SPA renders
    // a stable download anchor - a[data-testid="pdf-download-link"] with an
    // href to /service/content/pdf/watermarked/<pii>.pdf - so method "href"
    // rides the site's own watermarked-PDF endpoint with session cookies.
    // Evidence covers the .com.au front; clinicalkey.com joins with its own
    // capture.
    //
    // The settle budget is the largest in the registry because this provider
    // is routinely reached through a resolver hop rather than directly, and
    // the hop eats the render budget: captured live 2026-08-06, the same
    // article arrived from an institutional OpenURL as a 54 KB shell still
    // titled "Page loading" with `.c-cksc-content-player.loading` ten seconds
    // after load, while the direct content URL rendered the full 164 KB
    // article with its download anchor inside the same ten seconds. The
    // declared value was 8000 and silently clamped to 5000 by the interpreter
    // (`planExecution` in extension/src/plan.ts) until that ceiling was raised
    // to 15000.
    //
    // Captured again 2026-09-19 (success-current.html): the main content
    // header's direct title span is stable across both captures. Do not read
    // the whole h1: it includes the RSS and PDF controls, and the outline
    // carries unrelated h1 section headings. The header also owns one PDF
    // link; the sticky toolbar repeats it, so the download selector must be
    // scoped to the header to keep the action target unique.
    id: "clinicalkey",
    version: "0.3.0",
    hosts: ["clinicalkey.com.au"],
    workEvidence: { kind: "title", selector: "header#top h1 > span" },
    settleTimeoutMs: 15000,
    classify: [
      {
        kind: "article",
        all: ["a[data-testid='pdf-download-link'][href*='/service/content/pdf/']"],
      },
    ],
    download: {
      selector: "header#top a[data-testid='pdf-download-link'][href*='/service/content/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified live 2026-07-14 against an institutionally authenticated EBSCOhost record
    // and its provider-owned download-format modal (fixtures/ebsco/success.html).
    id: "ebsco",
    version: "0.3.0",
    hosts: ["research.ebsco.com"],
    // EBSCO adds punctuation to citation titles. Bind execution to the exact
    // DOI supplied by the rendered page; a missing or different DOI refuses.
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    // The current viewer still has no citation metadata ten seconds after
    // load, before rendering the entitled article. Give it the full budget.
    settleTimeoutMs: 15000,
    // A live cold return remained on its captured loading shell for more
    // than 30 seconds, then rendered the requested article without input.
    unknownGraceMs: 45000,
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          "button[data-auto='card-call-to-action-download-button']",
        ],
      },
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_title']",
          "button[data-auto='card-call-to-action']",
        ],
      },
      {
        // Live flow lands on the PDF viewer, where the article renders to
        // canvas; the record-page download button is absent there.
        kind: "article",
        all: ["meta[name='citation_title']", "canvas"],
      },
    ],
    download: {
      // Entitlement is implied on the viewer (the article is rendered); the real
      // gate is the viewer URL, whose opid/recordId build the aggregator call.
      selector: "meta[name='citation_title']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "api",
      idPattern: "^https://research\\.ebsco\\.com/c/([A-Za-z0-9_-]+)/viewer/pdf/([A-Za-z0-9_-]+)(?:[?#]|$)",
      urlTemplate:
        "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/{2}/fulltext/pdf?sourceRecordId={2}&opid={1}&intent=view&lang=en-US",
      jsonField: "url",
      allowedDestinations: [
        { origin: "https://content.ebscohost.com", pathPrefix: "/cds/retrieve" },
      ],
    },
  },
  {
    // Captured Springer Nature Link article states (fixtures/springer/*.html).
    // The access panel offers institutional sign-in; its presence does not
    // establish no entitlement. A rendered PDF control takes precedence.
    id: "springer",
    version: "0.1.2",
    hosts: ["link.springer.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 3000,
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          "a[data-test='pdf-link'][href*='/content/pdf/']",
        ],
      },
      {
        kind: "login",
        all: [
          "meta[name='citation_title']",
          "[data-test='access-article'] a[href]:not([aria-disabled='true']) [data-test='access-via-institution']",
        ],
      },
    ],
    download: {
      // Springer repeats the same PDF link in a sticky banner. Bind the
      // article header's control so the planner has exactly one target.
      selector: ".app-masthead__access-container a[data-test='pdf-link'][href*='/content/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified live 2026-07-23 against ACM Digital Library (fixtures/acm/
    // success.html entitled, no-entitlement.html paywalled). The "PDF/eReader"
    // toolbar control (a.btn--eReader -> /doi/epdf/) is the entitlement signal:
    // it renders only when THIS session can read the PDF (open/free access or
    // an entitled institution). The bottom-of-document a#downloadPdfUrl anchor
    // is NOT an entitlement signal — ACM emits it even on paywalled "Get
    // Access" pages, so keying on it false-positived and fetched an HTML access
    // page instead of the file. Download builds the deterministic
    // /doi/pdf/<doi>?download=true endpoint from the DOI in the page URL, gated
    // on the eReader control; the privileged downloads API returns %PDF via the
    // session cookie jar. No publisher terms gate.
    id: "acm",
    version: "0.2.0",
    hosts: ["dl.acm.org"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: ["meta[name='publication_doi']", "a.btn--eReader[href*='/doi/epdf/']"],
      },
    ],
    download: {
      selector: "a.btn--eReader[href*='/doi/epdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "url",
      idPattern: "/doi/(?:abs/|full/|epdf/|pdf/)?(10\\.[0-9]+/[^?#]+)",
      urlTemplate: "https://dl.acm.org/doi/pdf/{1}?download=true",
    },
  },
  {
    // Verified live 2026-08-09 against a warm, entitled ScienceDirect article
    // (fixtures/sciencedirect/success.html). ScienceDirect no longer publishes
    // citation_pdf_url there; it renders the current article's View PDF anchor
    // under the access bar instead. The page also carries a visible OneTrust
    // cookie banner, which does not hide that structural control from the DOM.
    // Activate that exact provider-owned control. The signed viewer can require
    // manual Send PDF; a fresh full live href also returned non-PDF content
    // through Chrome downloads on 2026-09-20. Do not re-fetch its signed URL.
    //
    // The 2026-09-19 login-return capture pairs Purchase PDF with an enabled
    // institutional RemoteAccessButton. The older no-entitlement fixture has
    // the same prompt: neither capture proves the institution lacks this
    // work. Treat that combination as login, not terminal no_entitlement.
    // Article stays first so an enabled PDF control still wins over a prompt.
    id: "sciencedirect",
    version: "0.8.2",
    hosts: ["sciencedirect.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    // ScienceDirect's access bar hydrates client-side and does not paint at all
    // while the work window is minimized, so the View PDF control never gains an
    // href and `article` cannot match. Measured 2026-08-24 on one entitled
    // article (pii/S0747563216303168, doi 10.1016/j.chb.2016.04.041), same host
    // and same session, varying only the surface:
    //   minimized window, settle 5000  ->  32 KB, no href, aria-disabled=true
    //   visible tab,      settle 5000  -> 262 KB, href=/pdfft, aria-disabled=false
    //   visible tab,      settle 10000 -> 262 KB, href=/pdfft, aria-disabled=false
    // The settle window is not the variable, so raising settleTimeoutMs cannot
    // fix this: nothing arrives late, the SPA never paints. `revealForHydration`
    // reveals the window without focus and reloads, because revealing after the
    // hidden load leaves the unpainted document in place.
    requiresVisible: true,
    // The access bar's own control is not always `/pdfft`. Measured live
    // 2026-08-26 on an entitled open-access Procedia article
    // (pii/S1877042814012683, doi 10.1016/j.sbspro.2014.01.1251): the enabled
    // `.ViewPDF` anchor is `aria-disabled="false"` with
    // href=/science/article/pii/<own-pii>/pdf — no `/pdfft` anywhere on the
    // page for this paper. The only `/pdfft` hrefs belong to three RECOMMENDED
    // sibling articles (S1877042814011513/11525/11537) rendered as
    // `div.buttons > a.anchor-primary`, so a rule matching `/pdfft` loosely
    // would download a different paper. Both shapes are therefore accepted,
    // and both stay scoped to `.accessbar .ViewPDF >` — that scoping, not the
    // path, is what keeps the sibling anchors unreachable.
    //
    // Re-measured live 2026-08-30, same article, scratch profile, no
    // institutional session, page fully painted. The href match had to become
    // `*=` because BOTH earlier predicates miss the real page:
    //   live  href=/science/article/pii/<pii>/pdf?md5=<32 hex>&pid=<pii>-main.pdf
    //   fixture href=/science/article/pii/<pii>/pdfft
    // `sanitizeFixture` strips query strings, so `[href$='/pdf']` matched the
    // sanitized fixture and could never match the live anchor, and the
    // `/pdfft` alternative does not apply to this paper's own route at all.
    // Every fixture-backed test passed while the field classified `unknown`:
    // 20 of the operator's 172 `ui_changed` outcomes were this, on pages that
    // had rendered correctly. A `$=` or `*=` predicate on an href is only ever
    // safe against a value the fixture pipeline preserves; path membership is,
    // a trailing anchor is not.
    //
    // `[href*='/pdf']` is not a widening: each scope below admits exactly one
    // anchor (verified against every fixture and capture on disk), and an
    // anchor with NO href — the unpainted signature above — still cannot
    // match, so `requiresVisible` keeps its meaning.
    //
    // TWO layouts, and only one of them has an access bar. Compared across
    // three sanitized captures from the operator's own session:
    //   fixtures/sciencedirect/open-access.html  div.accessbar > ul > li.ViewPDF > a
    //   fixtures/sciencedirect/success.html      same access-bar shape
    //   fixtures/sciencedirect/subscription.html div.content-details-actions >
    //                                            div.content-actions > a
    // The subscription capture (2026-08-24, pii/S0747563216303168, entitled,
    // `aria-disabled="false"`, href=<pii>/pdfft) contains NO `.accessbar`
    // container and no `.ViewPDF` at all — the enabled control lives in the
    // article's own content-actions region instead. An access-bar-only rule
    // therefore misses every article served that layout, which is the layout
    // an entitled institutional route lands on.
    //
    // Whether that layout is current or superseded is NOT established: it is
    // one observed sample, and the three later captures are all access-bar.
    // Both rules are kept because each is scoped to a single anchor and each
    // fails closed on every other fixture, so carrying a possibly-retired
    // layout costs one selector while dropping it would silently re-park a
    // whole class of paper.
    classify: [
      {
        kind: "article",
        all: ["meta[name='citation_title']"],
        any: [
          ".accessbar .ViewPDF > a.accessbar-utility-link[href*='/pdf']:not([aria-disabled='true']):not([disabled])",
          ".content-details-actions > .content-actions > a.accessbar-utility-link[href*='/pdf']:not([aria-disabled='true']):not([disabled])",
        ],
      },
      {
        kind: "login",
        all: [
          "meta[name='citation_doi']",
          // `*='/purchase'` for the same reason as above: this href is a live
          // value, so a trailing anchor breaks the moment ScienceDirect adds a
          // query. `[href^='/getaccess/pii/']` is what keeps it tight.
          ".access-options a.accessbar-utility-link[aria-label='Purchase PDF'][href^='/getaccess/pii/'][href*='/purchase']",
          ".access-options a.RemoteAccessButton[aria-disabled='false'][href^='https://auth.elsevier.com/ShibAuth/institutionLogin']",
        ],
      },
      {
        // The reduced observed capture keeps the paired access-bar layout.
        // A body sign-in link must not complete this scoped login signal.
        kind: "login",
        all: [
          "meta[name='citation_doi']",
          ".accessbar > ul > li.PurchasePDF > a.accessbar-utility-link[aria-label='Purchase PDF'][href^='/getaccess/pii/'][href*='/purchase']:not([aria-disabled='true']):not([disabled])",
          ".accessbar > ul > li.RemoteAccess > a.RemoteAccessButton[aria-disabled='false'][href^='https://auth.elsevier.com/ShibAuth/institutionLogin']:not([disabled])",
        ],
      },
    ],
    download: {
      selector:
        ".accessbar .ViewPDF > a.accessbar-utility-link[href*='/pdf']:not([aria-disabled='true']):not([disabled]), .content-details-actions > .content-actions > a.accessbar-utility-link[href*='/pdf']:not([aria-disabled='true']):not([disabled])",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
    },
  },
  {
    // Verified live 2026-07-17 against an institutionally authenticated Wiley Online Library
    // article (fixtures/wiley/success.html). The page's citation_pdf_url meta
    // points at /doi/pdf/<doi>, but that path returns an HTML viewer wrapper —
    // the actual file is Wiley's /doi/pdfdirect/<doi>?download=true endpoint
    // (what the viewer's download button builds; confirmed live to return the
    // PDF while /doi/pdf/ returns HTML). The resolver can also land directly
    // on that endpoint; declaring it as a viewer route lets the tracked-tab
    // download path adopt the file before blank PDF-viewer DOM is reported as
    // adapter drift. So classify on the citation metas but build the direct
    // endpoint from the DOI in the page URL and fetch it through the privileged
    // downloads API with the session cookies. No publisher terms modal.
    id: "wiley",
    version: "0.3.0",
    hosts: ["onlinelibrary.wiley.com"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: ["meta[name='citation_pdf_url']", "meta[name='citation_title']"],
      },
    ],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "url",
      viewerRoutes: [
        { pathPrefix: "/doi/epdf/" },
        { pathPrefix: "/doi/pdfdirect/" },
      ],
      // Wiley article/abstract/viewer paths all carry the DOI after /doi/[seg/].
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate: "https://onlinelibrary.wiley.com/doi/pdfdirect/{1}?download=true",
    },
  },
  {
    // Verified live 2026-08-24 against two authenticated Cochrane reviews,
    // CD013850.pub2 (fixtures/cochrane/success.html) and CD000072.pub3.
    // citation_pdf_url and every in-page PDF link name
    // /cdsr/doi/<doi>/pdf/full[/<lang>], which returns a 1.7 KB HTML wrapper
    // whose iframe names the only real file,
    // /cdsr/doi/<doi>/pdf/CDSR/<code>/<code>.pdf — the exact URL Chrome
    // recorded when a human downloaded the same review. So classify on the
    // full-review PDF affordance and build that nested file from the review
    // code in the page URL. pdf-link-abstract names a different document and
    // is never the target. Cochrane ships its institutional access panel on
    // entitled pages too, so sign-in markup is not entitlement evidence and
    // this adapter declares no login rule.
    id: "cochrane",
    version: "0.1.0",
    hosts: ["cochranelibrary.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: ["meta[name='citation_doi']", "a.pdf-link-full"],
      },
    ],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      workTarget: {
        kind: "doi",
        selector: "meta[name='citation_doi']",
        attribute: "content",
      },
      method: "url",
      viewerRoutes: [{
        pathPrefix: "/cdsr/doi/",
        idPattern:
          "^https://www\\.cochranelibrary\\.com/cdsr/doi/(10\\.1002/14651858\\.(CD\\d+)(?:\\.pub\\d+)?)/pdf/full(?:/[a-zA-Z]{2}(?:_[a-zA-Z]{2,4})?)?$",
        urlTemplate:
          "https://www.cochranelibrary.com/cdsr/doi/{1}/pdf/CDSR/{2}/{2}.pdf",
      }],
      // Every review route can carry a language segment, and the resolver hop
      // lands on one: /full/fr was the live landing 2026-08-24. The nested
      // review file is the same document in every case.
      idPattern:
        "^https://www\\.cochranelibrary\\.com/cdsr/doi/(10\\.1002/14651858\\.(CD\\d+)(?:\\.pub\\d+)?)/(?:full|abstract|pdf/full)(?:/[a-zA-Z]{2}(?:_[a-zA-Z]{2,4})?)?(?:[?#]|$)",
      urlTemplate:
        "https://www.cochranelibrary.com/cdsr/doi/{1}/pdf/CDSR/{2}/{2}.pdf",
    },
  },
  {
    // The eReader link is an access affordance, not a PDF. SAGE's documented
    // direct route is derived only after its semantic PDF/EPUB section appears,
    // avoiding generic button styling and a viewer-specific href as evidence.
    id: "sage",
    version: "0.2.0",
    hosts: ["journals.sagepub.com"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: ["meta[name='publication_doi']", "section.format--pdf_epub"],
      },
    ],
    download: {
      selector: "section.format--pdf_epub",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "url",
      viewerRoutes: [
        { pathPrefix: "/doi/epdf/" },
        { pathPrefix: "/doi/epub/" },
      ],
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate: "https://journals.sagepub.com/doi/pdf/{1}?download=true",
    },
  },
  {
    // Verified live 2026-07-20 in a fresh browser against the public full-text
    // page for APA UID 2025-01080-001 and the access wall for 2023-82557-001
    // (fixtures/psycnet/*). PsycNet's Angular shell uses a stable #pdf anchor
    // only after full article content has rendered; denied records replace it
    // with an explicit Get Access control. doi.apa.org is included because APA
    // DOI landings route into the same PsycNet application.
    id: "psycnet",
    version: "0.1.0",
    hosts: ["psycnet.apa.org", "doi.apa.org"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_doi']",
          "a.pdf[aria-label='Get Access']",
        ],
      },
      {
        kind: "article",
        all: [
          "#psycnet_fulltext_article_content",
          "a#pdf[href*='/fulltext/'][href$='.pdf']",
        ],
      },
    ],
    download: {
      selector: "a#pdf[href*='/fulltext/'][href$='.pdf']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified against the public OA captures in fixtures/annualreviews/.
    // The PDF action is an empty POST form with no file href. Download its
    // action directly: its named window target can reuse an old viewer whose
    // opener no longer belongs to this job, and pop-up blocking can stop it.
    // The Open Access class survives when its decorative text span is absent;
    // retain that explicit marker and the full-text container, since non-OA
    // pages can also render the PDF form.
    id: "annualreviews",
    version: "0.2.0",
    hosts: ["annualreviews.org"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          ".article-access.item-meta-data__oa",
          "#html_fulltext",
          "form.ft-download-content__form--pdf a[aria-label='Download PDF']",
        ],
      },
    ],
    download: {
      selector: "form.ft-download-content__form--pdf",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "post",
    },
  },
  {
    // Verified against authentic Taylor & Francis Online publisher captures
    // archived 2025-12-09 (OA article) and 2023-03-31 (Access Denial), plus a
    // live institutionally entitled article captured 2026-08-06
    // (fixtures/tandfonline/institutional.html), stored under
    // fixtures/tandfonline/. The journal platform is distinct from
    // taylorfrancis.com books, whose citation_pdf_url can be only a preview.
    //
    // The access badge is a DISJUNCTION, not the OA badge alone. T&F renders
    // `.access-icon.oa` for open access and `.access-icon.full` for an
    // entitled institutional session, and the earlier spec required `.oa` —
    // so every article papio actually exists to fetch (paywalled, reached
    // through the institution) classified `unknown` while its working
    // `/doi/pdf/` control sat rendered on the page. The badge still has to be
    // present: it is the rendered proof that this session may read the file,
    // which is what separates this rule from clicking whatever looks like a
    // download button. `no_entitlement` stays first — the Access Denial page
    // carries no download control at all, so the two can never both match.
    id: "tandfonline",
    version: "0.2.0",
    hosts: ["tandfonline.com"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "[role='region'][aria-label='Purchase Options']",
          "[data-pb-dropzone='accessDenialDropZone']",
        ],
      },
      {
        kind: "article",
        all: [".downloadPDFLink a.show-pdf[href*='/doi/pdf/']"],
        any: [".accessLogo .access-icon.oa", ".accessLogo .access-icon.full"],
      },
    ],
    download: {
      selector: ".downloadPDFLink a.show-pdf[href*='/doi/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
      viewerRoutes: [{
        pathPrefix: "/doi/epdf/",
        idPattern: "/doi/epdf/(10\\.[^?#]+)",
        urlTemplate: "https://www.tandfonline.com/doi/pdf/{1}?download=true",
      }],
    },
  },
  {
    // Verified against authentic Emerald publisher captures: the legacy
    // Insight platform archived 2025-01-23 (OA PDF control) and 2024-07-13
    // (No License turnaway), plus the current platform captured live
    // 2026-08-06 (fixtures/emerald/institutional.html). Unauthenticated
    // automation is WAF-blocked, so both article shapes come from real
    // sessions.
    //
    // Emerald has MIGRATED article delivery: the legacy anchor
    // `a.intent_pdf_link` -> /insight/content/doi/<doi>/full/pdf is gone from
    // current pages, replaced by `a.article-pdfLink` -> /<journal>/article-pdf/
    // …. Neither shape is a superset of the other, so each gets its own rule
    // rather than a loosened selector that would also match a listing page.
    // `download.selector` is the union of the two controls because an
    // AdapterSpec carries exactly one download rule; whichever anchor the page
    // actually rendered is the one querySelector returns. Neither classify
    // rule keys on the publisher's Open Access badge: entitlement here is
    // proved by the turnaway rule NOT matching first.
    //
    // Drop the legacy rule, its fixture and its half of the download union
    // once a current Emerald page is confirmed to no longer serve
    // intent_pdf_link anywhere.
    id: "emerald",
    version: "0.2.0",
    hosts: ["emerald.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: ["#turnaway-block", ".turnaway__dropdown"],
      },
      {
        kind: "article",
        all: [
          "meta[name='citation_doi']",
          "a.article-pdfLink[data-doctype='contentPdf'][href*='/article-pdf/']",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='dc.Title']",
          "a.intent_pdf_link[href*='/insight/content/doi/'][href*='/full/pdf']",
        ],
      },
    ],
    download: {
      selector:
        "a.article-pdfLink[data-doctype='contentPdf'][href*='/article-pdf/'], a.intent_pdf_link[href*='/insight/content/doi/'][href*='/full/pdf']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against live Cambridge Core journal fixtures for
    // both an OA article and a purchase wall. Denied pages still publish
    // citation_pdf_url, so require the action-bar PDF control and its rendered
    // content-view anchor. citation_journal_title excludes Cambridge book
    // chapters, which use the same PDF service under /core/books/.
    id: "cambridge",
    version: "0.1.0",
    hosts: ["cambridge.org"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_journal_title']",
          "a[data-test-id='buttonGetAccess']",
          "#access-block .access-options",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='citation_journal_title']",
          "[data-test-id='buttonSavePDFOptions']",
          "a[href*='/core/services/aop-cambridge-core/content/view/']",
        ],
      },
    ],
    download: {
      selector: "a[href*='/core/services/aop-cambridge-core/content/view/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against three public Thieme E-Journals full-text
    // pages, including fixtures/thieme/success.html. citation_pdf_url and
    // #pdfLink also appear on abstract-only routes, so they are not sufficient
    // access signals. Require the platform's fullText page state and rendered
    // article body, then read the live relative PDF anchor through the browser
    // cookie jar. The captured abstract route stays unknown/assisted.
    id: "thieme",
    version: "0.1.0",
    hosts: ["thieme-connect.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='page'][content='fullText']",
          "section#htmlfulltext",
          "a#pdfLink[href*='/products/ejournals/pdf/']",
        ],
      },
    ],
    download: {
      selector: "a#pdfLink[href*='/products/ejournals/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against a finalized public Nature Communications
    // article (fixtures/nature/success.html) and the subscription preview for
    // nature14539 (fixtures/nature/no-entitlement.html). Nature publishes
    // citation_pdf_url even on paywalled pages, so it is not an entitlement
    // signal. Require both access=Yes and the rendered download control, then
    // use that control's href: Article-in-Press pages can use _reference.pdf
    // while their citation meta still points at an HTML-canonicalizing .pdf.
    id: "nature",
    version: "0.1.0",
    hosts: ["nature.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "meta[name='access'][content='No']",
          "[data-test='entitlement-box']",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='access'][content='Yes']",
          "meta[name='citation_title']",
          "a[data-test='download-pdf'][data-article-pdf='true']",
        ],
      },
    ],
    download: {
      selector: "a[data-test='download-pdf'][data-article-pdf='true']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified live 2026-07-20 against a public PNAS Nexus article and a
    // subscription-only Child Development article (fixtures/oup/*). Oxford
    // Academic is Silverchair-backed: entitled articles expose a stable
    // article-pdfLink, while denied pages redirect to article-abstract and
    // render the js-no-access-jumplink control. Prefer the rendered action over
    // citation_pdf_url so a metadata-only paywall cannot look entitled.
    id: "oup",
    version: "0.1.2",
    hosts: ["academic.oup.com"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        // A chapter's current-session wall offers institutional sign-in. It
        // does not prove the institution lacks the book, and its PDF metadata
        // must never be mistaken for an entitled file control.
        kind: "login",
        all: [
          "meta[name='citation_title']",
          "#no-access-message.chapter-user-restricted",
          "#unauth .login-box a.js-shibboleth-action[data-action-type='login-discovery']",
        ],
      },
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_title']",
          "a.js-no-access-jumplink",
          "#no-access-message.article-top-info-user-restricted-options",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          // Entitled chapters use chapter-ag-pdf; the same rendered PDF
          // control carries both routes. Metadata also exists behind walls.
          "a.article-pdfLink[href*='/article-pdf/'], a.article-pdfLink[href*='/chapter/'][href*='/chapter-ag-pdf/']",
        ],
      },
    ],
    download: {
      selector: "a.article-pdfLink[href*='/article-pdf/'], a.article-pdfLink[href*='/chapter/'][href*='/chapter-ag-pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified live 2026-07-20 against an OA Quantitative Science Studies
    // article and the subscription-only Long Short-Term Memory article
    // (fixtures/mitpress/*). MIT Press uses Silverchair's stable article PDF
    // action. Paywalled article-abstract routes still publish citation_pdf_url,
    // so require the rendered purchase wall and prioritize it over metadata.
    id: "mitpress",
    version: "0.1.0",
    hosts: ["direct.mit.edu"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_title']",
          ".article-top-info-user-restricted-options",
          "#dvPurchaseButton.ppv-wrap",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          "a.article-pdfLink[href*='/article-pdf/']",
        ],
      },
    ],
    download: {
      selector: "a.article-pdfLink[href*='/article-pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against BMJ's live JATS OA metadata and an authentic
    // 2024 publisher capture of BMJ Open DOI 10.1136/bmjopen-2017-017569
    // (fixtures/bmj/success.html). Current automated HTML fetches hit
    // Cloudflare, but papio runs in the user's non-automated browser. Restrict
    // auto-download to explicit citation_access=all pages: closed BMJ articles
    // stay assisted even if they publish PDF-shaped metadata.
    id: "bmj",
    version: "0.1.0",
    hosts: ["bmj.com"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_public_url']",
          "meta[name='citation_access'][content='all']",
          "meta[name='citation_pdf_url']",
          "a.article-pdf-download[href$='.full.pdf']",
        ],
      },
    ],
    download: {
      selector: "a.article-pdf-download[href$='.full.pdf']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against authentic publisher captures of an entitled
    // STAR*D article and a denied 2024 American Journal of Psychiatry article
    // (fixtures/psychiatryonline/*). Silverchair exposes access state directly;
    // denied pages may still render downloadPdfUrl, so the full-access marker
    // is load-bearing and the no-access rule must run first.
    id: "psychiatryonline",
    version: "0.1.1",
    hosts: ["psychiatryonline.org"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "meta[name='publication_doi']",
          "[data-article-access='no'][data-article-access-type='other']",
        ],
      },
      {
        kind: "article",
        all: [
          "meta[name='publication_doi']",
          "[data-article-access='full'][data-article-access-type='full']",
          "a#downloadPdfUrl[data-doi]",
        ],
      },
    ],
    download: {
      selector: "a#downloadPdfUrl[data-doi]",
      requireKind: "article",
      workTarget: { kind: "doi", attribute: "href", pattern: "^/doi/pdf/(10\\.[^?#]+)(?:[?#]|$)" },
      method: "href",
    },
  },
  {
    // Verified 2026-07-20 against an authentic JAMA Psychiatry publisher
    // capture of DOI 10.1001/archgenpsychiatry.2010.116
    // (fixtures/jamanetwork/success.html). This older JAMA control has no href:
    // site JavaScript checks access using data-article-url before downloading,
    // so click the exact control. Gate on both Free and full-text access;
    // sign-in/purchase controls also appear on free pages and are not verdicts.
    id: "jamanetwork",
    version: "0.1.0",
    hosts: ["jamanetwork.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_doi']",
          ".article-full-text[data-userhasaccess='True']",
          "a#pdf-link.pdfaccess[data-article-url$='.pdf'][data-ajax-url='/Content/CheckPdfAccess']",
        ],
        any: [".meta-access-type.free-access", ".meta-access-type.open-access"],
      },
    ],
    download: {
      selector:
        "a#pdf-link.pdfaccess[data-article-url$='.pdf'][data-ajax-url='/Content/CheckPdfAccess']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
    },
  },
  {
    // Verified 2026-07-20 against an authentic 2024 LWW full-text capture of
    // DOI 10.4103/0972-6748.57865 (fixtures/lww/success.html), corroborated by
    // its current Ovid/PMC OA records. LWW publishes the exact browser PDF URL
    // in wkhealth_pdf_url even when no anchor is rendered. Require the actual
    // full-text container so abstract/paywall pages with metadata stay assisted.
    id: "lww",
    version: "0.1.0",
    hosts: ["journals.lww.com"],
    workEvidence: { kind: "doi", selector: "meta[name='wkhealth_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='wkhealth_doi']",
          "meta[name='wkhealth_pdf_url']",
          "article#ej-article-view .ejp-fulltext-content.js-ejp-fulltext-content",
        ],
      },
    ],
    download: {
      selector: "meta[name='wkhealth_pdf_url']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "meta",
      metaName: "wkhealth_pdf_url",
    },
  },
  {
    // Captured 2026-09-19 from the public Europe PMC full-text article
    // PMC8053968 (fixtures/europepmc/success.html). The visible PDF affordance
    // is a role=button span, not an anchor, and citation_pdf_url points back to
    // the article after fixture query sanitization. Build Europe PMC's measured
    // direct endpoint only when the requested DOI, article route PMCID, and
    // citation_pmcid metadata all bind the same page.
    id: "europepmc",
    version: "0.1.0",
    hosts: ["europepmc.org"],
    settleTimeoutMs: 5_000,
    workEvidence: {
      kind: "doi",
      selector: "meta[name='citation_doi']",
      attribute: "content",
    },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          "meta[name='citation_doi']",
          "meta[name='citation_pmcid']",
          "#open_pdf > span[role='button']",
        ],
      },
    ],
    download: {
      selector: "#open_pdf > span[role='button']",
      requireKind: "article",
      workTarget: {
        kind: "doi",
        selector: "meta[name='citation_doi']",
        attribute: "content",
      },
      method: "url",
      idPattern:
        "^https://europepmc\\.org/article/PMC/([1-9][0-9]*)(?:[?#]|$)",
      routeIdentity: {
        selector: "meta[name='citation_pmcid']",
        attribute: "content",
        pattern: "^PMC([1-9][0-9]*)$",
      },
      urlTemplate: "https://europepmc.org/api/getPdf?pmcid=PMC{id}",
    },
  },
  {
    // Verified 2026-07-20 against the live public HAL record
    // hal-04206682 (fixtures/hal/success.html). HAL exposes the real
    // repository file in the Highwire citation_pdf_url meta
    // (/hal-…/document); the browser download API follows it without a
    // provider login. Records without a deposited file omit this meta and
    // remain assisted/unknown rather than being misclassified.
    id: "hal",
    version: "0.1.0",
    hosts: ["hal.science"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          "meta[name='citation_doi']",
          "meta[name='citation_pdf_url']",
        ],
      },
    ],
    download: {
      selector: "meta[name='citation_pdf_url']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "meta",
      metaName: "citation_pdf_url",
    },
  },
  {
    // Verified 2026-08-04 against the live public MDPI article
    // 10.3390/educsci12060369 (fixtures/mdpi/success.html). Require both the
    // Highwire PDF metadata and MDPI's provider-owned download anchor so an
    // article-shaped metadata shell cannot trigger a download. href reads the
    // live URL, including the version query stripped from the fixture.
    id: "mdpi",
    version: "0.1.0",
    hosts: ["mdpi.com"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_journal_title']",
          "meta[name='citation_doi']",
          "meta[name='citation_pdf_url']",
          "a.UD_ArticlePDF[href*='/pdf']",
        ],
      },
    ],
    download: {
      selector: "a.UD_ArticlePDF[href*='/pdf']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      allowedDestinations: [{ origin: "https://www.mdpi.com", pathPrefix: "/" }],
      method: "href",
    },
  },
  {
    // Captured 2026-08-08 from the public Hogrefe European Journal of
    // Psychology Open article (fixtures/hogrefe/success.html). Hogrefe
    // renders a viewer route for the page's visible PDF control, but the
    // explicit `/doi/pdf/...?...download=true` anchor is the browser-download
    // endpoint. Require the article metadata and provider-owned PDF anchor so
    // abstract or login shells stay assisted.
    id: "hogrefe",
    version: "0.1.0",
    hosts: ["econtent.hogrefe.com"],
    workEvidence: { kind: "doi", selector: "meta[name='publication_doi']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='publication_doi']",
          "meta[name='citation_journal_title']",
          "a[href^='/doi/pdf/']",
          "h1.citation__title",
        ],
      },
    ],
    download: {
      selector: "a[href^='/doi/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Captured Figshare record with an explicit unavailable-file banner. This
    // ends this repository route only; the daemon can still try institutional
    // access. Metadata-only records do not establish a PDF download route.
    id: "figshare",
    version: "0.1.0",
    hosts: ["figshare.com"],
    settleTimeoutMs: 5000,
    classify: [{
      kind: "no_entitlement",
      all: [
        "meta[name='citation_title']",
        "main [data-id='layout-preview'] [data-id='layout-header'] h1",
      ],
      textSelector: "main .jbW3L > h2.rSf-Z",
      textAny: ["file(s) not publicly available"],
    }],
  },
  {
    // The seven captured Alma View It pages all expose this terminal empty-results
    // state. Resolver pages with holdings forward elsewhere, so their success
    // shape is not evidenced here and must remain assisted.
    id: "exlibris-primo",
    version: "0.1.0",
    hosts: ["alma.exlibrisgroup.com"],
    classify: [
      {
        kind: "no_entitlement",
        all: [
          "form[name='uResolverViewItForm']",
          "#repDataLong",
          "#showAllLine",
        ],
        textAny: ["no full text available"],
      },
    ],
  },
];

/** Resolve a packaged provider viewer route to its declared direct PDF
 * endpoint. Only HTTPS pages on a registered host, an explicit viewer prefix,
 * and a successful source-controlled extraction/build pair may qualify. */
export function providerViewerPDFURL(
  value: string,
  specs: readonly AdapterSpec[] = adapters,
): string | undefined {
  let page: URL;
  try {
    page = new URL(value);
  } catch {
    return undefined;
  }
  if (page.protocol !== "https:") return undefined;
  const spec = specs.find((candidate) =>
    adapterSupportsHost(page.hostname, candidate)
  );
  const rule = spec?.download;
  const route = rule?.viewerRoutes?.find((candidate) => page.pathname.startsWith(candidate.pathPrefix));
  const idPattern = route?.idPattern ?? rule?.idPattern;
  const urlTemplate = route?.urlTemplate ?? rule?.urlTemplate;
  if (route === undefined || typeof idPattern !== "string" || typeof urlTemplate !== "string") {
    return undefined;
  }

  let match: RegExpMatchArray | null;
  try {
    match = `${page.origin}${page.pathname}`.match(new RegExp(idPattern));
  } catch {
    return undefined;
  }
  if (match === null) return undefined;
  const built = urlTemplate.replace(
    /\{(\d+|id)\}/g,
    (_, key: string) => match?.[key === "id" ? 1 : Number(key)] ?? "",
  );
  try {
    const target = new URL(built);
    return target.protocol === "https:" ? target.href : undefined;
  } catch {
    return undefined;
  }
}
