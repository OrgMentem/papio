// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio provider adapters are DECLARATIVE selector/pattern specs (source-
// controlled, versioned) interpreted by exactly one generic function,
// `interpret`. There is NO free-form injected code and NO "click the likely
// download button" fallback: a page that matches no rule classifies as
// `unknown`, and the extension stays in assisted behaviour.
//
// `interpret` is intentionally self-contained: it references no module import,
// helper, or closure at runtime, so the background service worker can hand it
// verbatim to chrome.scripting.executeScript with the matched spec + ctx as
// JSON args. The same function is unit-tested against happy-dom fixtures.

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
  /** At least one lowercase substring must appear in document.body.innerText
   * (compared lowercased). Static labels only — never page-derived text. */
  textAny?: string[];
}

export interface DownloadRule {
  selector: string;
  requireKind: "article";
  /** `href` extracts an HTTPS anchor and uses chrome.downloads.download.
   * `click` activates the explicitly selected element (or an explicitly
   * selected control in its open shadow root).
   * `url` constructs the direct PDF endpoint from the page URL (idPattern +
   * urlTemplate) and fetches it via chrome.downloads.download — no click, no
   * gesture. The privileged downloads API carries the session cookies, so an
   * entitled endpoint (e.g. JSTOR /stable/pdf/<id>.pdf) is fetched autonomously. */
  method: "href" | "click" | "url" | "api" | "meta";
  shadowSelector?: string;
  /** Wait for this fixture-backed in-page gate before reclassification. */
  postClickWaitFor?: string;
  /** After the first click, wait for and click this one fixture-backed control
   * (for provider-owned download modals; never terms/consent controls). */
  followupSelector?: string;
  /** Shared bounded wait for post-click gate/follow-up insertion. */
  postClickTimeoutMs?: number;
  /** method "url"/"api": regex matched against the page URL; capture groups fill
   * {1},{2},… (and {id} = {1}) in urlTemplate. */
  idPattern?: string;
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
   * URL (default "citation_pdf_url", the Highwire/Google-Scholar standard that
   * Elsevier/ScienceDirect and others expose). The URL is fetched via the
   * privileged downloads API — no click, no gesture — like the "url" method. */
  metaName?: string;
}

export interface AdapterSpec {
  id: string;
  version: string;
  hosts: string[];
  /** Ordered rules; first match wins. */
  classify: ClassifyRule[];
  /** On live SPA pages only, wait this long for a complete rule's declared
   * selectors to hydrate before classifying. Fixture Documents stay synchronous. */
  settleTimeoutMs?: number;
  download?: DownloadRule;
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
   * access (ProQuest: "accountid"). On a `login` verdict, if the offer carries a
   * provider account id, papio appends `?<param>=<id>` to the current URL —
   * fully autonomous, no sign-in. Tried before federatedLogin. */
  accountIdParam?: string;
}

export interface TermsAcceptRule {
  /** The open terms modal container (same selector as the `terms` classify rule). */
  modalSelector: string;
  /** Accessible-text needles identifying the accept-and-download control. */
  textAny: string[];
}

export interface AdapterContext {
  expected: { title?: string; doi?: string; year?: number };
}

export interface PageVerdict {
  kind: PageKind | "wrong_work";
  adapter_id: string;
  adapter_version: string;
  /** Static rule labels only (e.g. `rule:article matched`). NEVER page text. */
  evidence: string[];
}

/**
 * Classify a provider page against a declarative adapter spec. Pure and
 * DOM-only: no chrome.* usage, no network, no mutation. A live SPA invocation
 * may await declared rule selectors via MutationObserver before classification;
 * fixture Documents classify synchronously.
 *
 * SERIALIZATION CONTRACT: this function must remain self-contained (no imports,
 * helpers, or closures referenced at runtime) so it survives
 * `Function.prototype.toString()` inside chrome.scripting.executeScript. When
 * injected it is called as `interpret(null, spec, ctx)` — the `doc` argument
 * arrives as `null` and we fall back to the page's global `document`; `spec`
 * and `ctx` are the JSON args. Tests pass a real happy-dom Document as `doc`.
 */
export function interpret(doc: Document, spec: AdapterSpec, ctx: AdapterContext): PageVerdict;
export function interpret(doc: null, spec: AdapterSpec, ctx: AdapterContext): Promise<PageVerdict>;
export function interpret(
  doc: Document | null,
  spec: AdapterSpec,
  ctx: AdapterContext,
): PageVerdict | Promise<PageVerdict> {
  const root: Document = doc ?? document;
  const classify = (): PageVerdict => {
    const evidence: string[] = [];
    const adapter_id = spec.id;
    const adapter_version = spec.version;

    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      const hasText = Array.isArray(rule.textAny) && rule.textAny.length > 0;
      // A rule with no conditions never matches: refuse a blanket fallback.
      if (!hasAll && !hasAny && !hasText) continue;

      if (hasAll) {
        let ok = true;
        for (const sel of rule.all as string[]) {
          if (root.querySelector(sel) === null) {
            ok = false;
            break;
          }
        }
        if (!ok) continue;
      }
      if (hasAny) {
        let ok = false;
        for (const sel of rule.any as string[]) {
          if (root.querySelector(sel) !== null) {
            ok = true;
            break;
          }
        }
        if (!ok) continue;
      }
      if (hasText) {
        const body = root.body;
        const bodyText = (body && body.innerText ? body.innerText : "").toLowerCase();
        let ok = false;
        for (const needle of rule.textAny as string[]) {
          if (bodyText.indexOf(needle) !== -1) {
            ok = true;
            break;
          }
        }
        if (!ok) continue;
      }

      evidence.push("rule:" + rule.kind + " matched");
      if (rule.kind === "article") {
        const expectedTitle = ctx.expected.title;
        if (expectedTitle !== undefined && expectedTitle.length > 0) {
          const parts: string[] = [];
          const h1 = root.querySelector("h1");
          if (h1 && h1.textContent) parts.push(h1.textContent);
          const meta = root.querySelector('meta[name="citation_title"]');
          const metaContent = meta ? meta.getAttribute("content") : null;
          if (metaContent) parts.push(metaContent);
          if (root.title) parts.push(root.title);
          const haystack = parts.join(" ").toLowerCase();

          const tokens = expectedTitle
            .toLowerCase()
            .split(/[^a-z0-9]+/)
            .filter((t) => t.length > 3);
          let present = 0;
          for (const tok of tokens) {
            if (haystack.indexOf(tok) !== -1) present++;
          }
          const ratio = tokens.length === 0 ? 1 : present / tokens.length;
          if (ratio < 0.6) {
            evidence.push("title-token-check failed");
            return { kind: "wrong_work", adapter_id, adapter_version, evidence };
          }
          evidence.push("title-token-check passed");
        }
      }

      return { kind: rule.kind, adapter_id, adapter_version, evidence };
    }

    evidence.push("no rule matched");
    return { kind: "unknown", adapter_id, adapter_version, evidence };
  };

  // Fixture interpretation is deterministic and synchronous. Only the
  // serialized live invocation waits for React/custom-element hydration.
  if (doc !== null) return classify();
  const boundedMs = Math.max(0, Math.min(spec.settleTimeoutMs ?? 0, 5000));
  if (boundedMs === 0 || root.documentElement === null) return Promise.resolve(classify());

  const selectorsReady = (): boolean => {
    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      if (!hasAll && !hasAny) continue;
      let allReady = true;
      if (hasAll) {
        for (const selector of rule.all as string[]) {
          if (root.querySelector(selector) === null) {
            allReady = false;
            break;
          }
        }
      }
      let anyReady = true;
      if (hasAny) {
        anyReady = false;
        for (const selector of rule.any as string[]) {
          if (root.querySelector(selector) !== null) {
            anyReady = true;
            break;
          }
        }
      }
      if (allReady && anyReady) return true;
    }
    return false;
  };
  if (selectorsReady()) return Promise.resolve(classify());

  return new Promise<void>((resolve) => {
    let settled = false;
    let observer: MutationObserver | null = null;
    let timer: number | Timer | undefined;
    const finish = (): void => {
      if (settled) return;
      settled = true;
      observer?.disconnect();
      clearTimeout(timer);
      resolve();
    };
    observer = new MutationObserver(() => {
      if (selectorsReady()) finish();
    });
    observer.observe(root.documentElement, { childList: true, subtree: true, attributes: true });
    timer = setTimeout(finish, boundedMs);
  }).then(() => classify());
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
    // Verified live 2026-07-14 against Example University-authenticated ProQuest
    // (fixtures/proquest/*.html). The PDF link id is document-scoped
    // (`downloadPDFLink_MSTAR_<docid>`), hence the prefix selector.
    // A docview page without that link (citation-only, HTML-only, or
    // unentitled) stays `unknown`: distinguishing those needs fixtures
    // we do not have yet.
    id: "proquest",
    version: "0.2.0",
    hosts: ["proquest.com"],
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
    // Verified live 2026-07-14 against Example University-authenticated and isolated
    // logged-out JSTOR pages (fixtures/jstor/*.html). Terms is first because
    // JSTOR overlays it on the still article-shaped page. settleTimeoutMs waits
    // for JSTOR's `mfe-*` custom elements to upgrade: the tracked tab's post-SSO
    // landing fires `complete` before they render, so a single-shot classify
    // would see no download button, return unknown, and never retry (SPA: no
    // second complete).
    id: "jstor",
    version: "0.1.0",
    hosts: ["jstor.org"],
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
        all: ["mfe-download-pharos-button[data-qa='download-pdf'][data-doi]"],
      },
    ],
    download: {
      selector: "mfe-download-pharos-button[data-qa='download-pdf'][data-doi]",
      requireKind: "article",
      method: "url",
      idPattern: "/stable/([^?#]+)",
      urlTemplate: "https://www.jstor.org/stable/pdf/{id}.pdf",
      requiresTermsConsent: true,
    },
    termsAccept: {
      modalSelector: "mfe-download-pharos-modal.terms-and-conditions[open]",
      textAny: ["accept and download"],
    },
  },
  {
    // Verified live 2026-07-14 against a Example University-authenticated EBSCOhost record
    // and its provider-owned download-format modal (fixtures/ebsco/success.html).
    id: "ebsco",
    version: "0.2.0",
    hosts: ["research.ebsco.com"],
    settleTimeoutMs: 5000,
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
      method: "api",
      idPattern: "/c/([^/]+)/viewer/pdf/([^/?#]+)",
      urlTemplate:
        "https://research.ebsco.com/api/researcher-edge-aggregator/v1/records/{2}/fulltext/pdf?sourceRecordId={2}&opid={1}&intent=view&lang=en-US",
      jsonField: "url",
    },
  },
  {
    // Verified live 2026-07-14 against entitled and isolated no-entitlement
    // Springer Nature Link article states (fixtures/springer/*.html).
    id: "springer",
    version: "0.1.0",
    hosts: ["link.springer.com"],
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
        kind: "no_entitlement",
        all: [
          "meta[name='citation_title']",
          "[data-test='access-article']",
        ],
      },
    ],
    download: {
      selector: "a[data-test='pdf-link'][href*='/content/pdf/']",
      requireKind: "article",
      method: "href",
    },
  },
  {
    // Verified live 2026-07-16 against an entitled ACM Digital Library article
    // (fixtures/acm/success.html). The download anchor's id + data-doi are
    // stable and its href is the direct entitled PDF (?download=true), fetched
    // via the browser cookie jar. No isolated no-entitlement capture was
    // available at build time, so non-entitled ACM pages classify unknown and
    // stay assisted rather than risk a wrong verdict.
    id: "acm",
    version: "0.1.0",
    hosts: ["dl.acm.org"],
    classify: [
      {
        kind: "article",
        all: ["meta[name='publication_doi']", "a#downloadPdfUrl[data-doi][href*='/doi/pdf/']"],
      },
    ],
    download: {
      selector: "a#downloadPdfUrl[data-doi][href*='/doi/pdf/']",
      requireKind: "article",
      method: "href",
    },
  },
  {
    // STRUCTURAL-ONLY (synthetic citation_pdf_url DOM in sciencedirect.test.ts),
    // NOT yet live-verified.
    // ScienceDirect sits behind Cloudflare, which bot-challenges automated
    // capture, so no entitled DOM could be captured under automation at build
    // time. This adapter follows the citation_pdf_url standard (the
    // Highwire/Google-Scholar meta tag carrying the entitled pdfft URL that
    // Elsevier exposes) and fetches it via the privileged downloads API — no
    // click, no gesture — gated on recorded terms consent, exactly like the
    // JSTOR url / EBSCO api adapters. Confirm live against a real entitled
    // session (a warm human browser does not trip Cloudflare) before trusting
    // it; if ScienceDirect gates the pdfft URL behind an interstitial
    // (isDTMRedir), the meta URL fetch will need a follow-up.
    id: "sciencedirect",
    version: "0.1.0",
    hosts: ["sciencedirect.com"],
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
      method: "meta",
      metaName: "citation_pdf_url",
      requiresTermsConsent: true,
    },
  },
  {
    // Verified live 2026-07-17 against a Example University-authenticated Wiley Online Library
    // article (fixtures/wiley/success.html). The page's citation_pdf_url meta
    // points at /doi/pdf/<doi>, but that path returns an HTML viewer wrapper —
    // the actual file is Wiley's /doi/pdfdirect/<doi>?download=true endpoint
    // (what the viewer's download button builds; confirmed live to return the
    // PDF while /doi/pdf/ returns HTML). So classify on the citation metas but
    // build the direct endpoint from the DOI in the page URL and fetch it
    // through the privileged downloads API with the session cookies. No
    // publisher terms modal, so no consent gate.
    id: "wiley",
    version: "0.2.0",
    hosts: ["onlinelibrary.wiley.com"],
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
      method: "url",
      // Wiley article/abstract/viewer paths all carry the DOI after /doi/[seg/].
      idPattern: "/doi/(?:[a-z]+/)?(10\\.[^?#]+)",
      urlTemplate: "https://onlinelibrary.wiley.com/doi/pdfdirect/{1}?download=true",
    },
  },
  {
    // Classify fixture-verified 2026-07-17 against a live Example University-authenticated SAGE
    // article captured via CDP (fixtures/sage/success.html). SAGE sits behind
    // Cloudflare and emits no Highwire citation_* metas; it exposes
    // publication_doi plus a "Download PDF" anchor (id downloadPdfUrl, same shape
    // as ACM) whose live href is the direct /doi/pdf/<doi>?download=true file.
    // The sanitizer strips the ?download=true query from the captured fixture, so
    // the selector keys on the stable id/path/data-doi and the href method reads
    // the live anchor href at download time.
    // NOTE: end-to-end download NOT yet live-exercised — Example University's resolver routed
    // the SAGE test title (10.1177/0018720814547570) to ProQuest, not sagepub, so
    // the flow never landed on a SAGE page. Adapter fires when the resolver routes
    // a title to journals.sagepub.com; download method mirrors ACM's proven href.
    id: "sage",
    version: "0.1.0",
    hosts: ["journals.sagepub.com"],
    settleTimeoutMs: 5000,
    classify: [
      {
        kind: "article",
        all: ["meta[name='publication_doi']", "a#downloadPdfUrl[data-doi][href*='/doi/pdf/']"],
      },
    ],
    download: {
      selector: "a#downloadPdfUrl[data-doi][href*='/doi/pdf/']",
      requireKind: "article",
      method: "href",
    },
  },
];
