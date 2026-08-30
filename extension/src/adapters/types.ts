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
  attribute: string;
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
  /** Page-derived href/meta destinations require this packaged envelope when
   * they leave the current page origin. */
  allowedDestinations?: DownloadDestinationContract[];
  method: "href" | "click" | "url" | "api" | "meta";
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
  /** Ordered rules; first match wins. */
  classify: ClassifyRule[];
  /** Exact packaged page evidence used to bind expected DOI/title identity. */
  workEvidence?: WorkEvidenceContract;
  /** On live SPA pages only, wait this long for a complete rule's declared
   * selectors to hydrate before classifying. Fixture Documents stay synchronous. */
  settleTimeoutMs?: number;
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
  const classify = (allowDeferred: boolean): PageVerdict => {
    const evidence: string[] = [];
    const adapter_id = spec.id;
    const adapter_version = spec.version;

    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      const hasText = Array.isArray(rule.textAny) && rule.textAny.length > 0;
      // A rule with no conditions never matches: refuse a blanket fallback.
      if (!hasAll && !hasAny && !hasText) continue;
      if (rule.deferUntilDeadline === true && !allowDeferred) continue;

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
  if (doc !== null) return classify(true);
  // The ceiling has to exceed every value a spec may declare, or the field is
  // a lie: it was 5000 while `clinicalkey` declared 8000, so that adapter's
  // extra budget was silently discarded and the provider's Angular content
  // player — which really is slower than five seconds when reached through an
  // institutional resolver hop — kept classifying `unknown`. This is a worst
  // case, not a delay: the MutationObserver below resolves the instant a
  // declared selector appears, so a fast page never spends it. The separate
  // HANDOFF_DRIVE_LIMIT bounds how many stalled provider pages can hold the
  // drive queue at once.
  const boundedMs = Math.max(0, Math.min(spec.settleTimeoutMs ?? 0, 15000));
  if (boundedMs === 0 || root.documentElement === null)
    return Promise.resolve(classify(true));

  const selectorsReady = (): PageKind | null => {
    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      const hasText = Array.isArray(rule.textAny) && rule.textAny.length > 0;
      if (!hasAll && !hasAny && !hasText) continue;
      if (rule.deferUntilDeadline === true) continue;
      let allReady = true;
      if (hasAll) {
        for (const selector of rule.all as string[]) {
          try {
            if (root.querySelector(selector) === null) {
              allReady = false;
              break;
            }
          } catch {
            allReady = false;
            break;
          }
        }
      }
      let anyReady = true;
      if (hasAny) {
        anyReady = false;
        for (const selector of rule.any as string[]) {
          try {
            if (root.querySelector(selector) !== null) {
              anyReady = true;
              break;
            }
          } catch {
            // Invalid alternatives cannot authorize a ready rule.
          }
        }
      }
      let textReady = true;
      if (hasText) {
        textReady = false;
        const bodyText = (root.body?.innerText ?? "").toLowerCase();
        for (const needle of rule.textAny as string[]) {
          if (bodyText.includes(needle.toLowerCase())) {
            textReady = true;
            break;
          }
        }
      }
      if (allReady && anyReady && textReady) return rule.kind;
    }
    return null;
  };
  const deferred = Promise.withResolvers<PageVerdict>();
  let settled = false;
  let observer: MutationObserver | null = null;
  let timer: number | Timer | undefined;
  let readyTimer: number | Timer | undefined;
  const finish = (allowDeferred: boolean): void => {
    if (settled) return;
    settled = true;
    observer?.disconnect();
    clearTimeout(timer);
    clearTimeout(readyTimer);
    deferred.resolve(classify(allowDeferred));
  };
  const settleWindowMs = Math.min(50, boundedMs);
  const scheduleWhenReady = (): void => {
    const readyKind = selectorsReady();
    if (readyKind === null || readyKind === "article") {
      if (readyTimer !== undefined) {
        clearTimeout(readyTimer);
        readyTimer = undefined;
      }
      return;
    }
    if (readyTimer === undefined) {
      readyTimer = setTimeout(
        () => finish(false),
        settleWindowMs,
      );
    }
  };
  observer = new MutationObserver(scheduleWhenReady);
  observer.observe(root.documentElement, {
    childList: true,
    subtree: true,
    attributes: true,
  });
  timer = setTimeout(() => finish(true), boundedMs);
  scheduleWhenReady();
  return deferred.promise;
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
    version: "0.3.0",
    hosts: ["jstor.org"],
    workEvidence: {
      kind: "doi",
      selector:
        "mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']",
      attribute: "data-doi",
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
      idPattern: "^https://www\\.jstor\\.org/stable/(?:pdf/)?(\\d+)",
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
    // declared value was 8000 and silently clamped to 5000 by `interpret`
    // until that ceiling was raised.
    id: "clinicalkey",
    version: "0.2.0",
    hosts: ["clinicalkey.com.au"],
    settleTimeoutMs: 15000,
    classify: [
      {
        kind: "article",
        all: ["a[data-testid='pdf-download-link'][href*='/service/content/pdf/']"],
      },
    ],
    download: {
      selector: "a[data-testid='pdf-download-link'][href*='/service/content/pdf/']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "href",
    },
  },
  {
    // Verified live 2026-07-14 against an institutionally authenticated EBSCOhost record
    // and its provider-owned download-format modal (fixtures/ebsco/success.html).
    id: "ebsco",
    version: "0.2.0",
    hosts: ["research.ebsco.com"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
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
      workTarget: { kind: "opaque" },
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
    // Activate that exact provider-owned control: Elsevier's documented flow
    // opens the PDF in a new browser window, which papio's viewer-adoption path
    // captures. Fetching the bare href directly redirects to Cookie Notice HTML.
    //
    // The no_entitlement rule is separate live evidence (fixtures/
    // sciencedirect/no-entitlement.html, captured 2026-08-26 after a fresh UNE
    // institutional sign-in). The paywall publishes one provider-owned
    // `/getaccess/pii/<pii>/purchase` anchor labelled "Purchase PDF" and no View
    // PDF control. ScienceDirect removed the old `.PurchasePDF` class, so that
    // page used to fall through as `ui_changed` and keep the institution's one
    // sign-in slot occupied. `article` stays first so a transitional page that
    // briefly carries both still trusts the positive entitlement signal.
    id: "sciencedirect",
    version: "0.8.0",
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
          ".accessbar .ViewPDF > a.accessbar-utility-link[href*='/pdf']",
          ".content-details-actions > .content-actions > a.accessbar-utility-link[href*='/pdf']",
        ],
      },
      {
        kind: "no_entitlement",
        all: [
          "meta[name='citation_doi']",
          // `*='/purchase'` for the same reason as above: this href is a live
          // value, so a trailing anchor breaks the moment ScienceDirect adds a
          // query. `[href^='/getaccess/pii/']` is what keeps it tight.
          ".access-options a.accessbar-utility-link[aria-label='Purchase PDF'][href^='/getaccess/pii/'][href*='/purchase']",
        ],
      },
    ],
    download: {
      selector:
        ".accessbar .ViewPDF > a.accessbar-utility-link[href*='/pdf'], .content-details-actions > .content-actions > a.accessbar-utility-link[href*='/pdf']",
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
    // Verified 2026-07-20 against the public OA Annual Reviews article in
    // fixtures/annualreviews/success.html. The platform's PDF action is a
    // JavaScript-backed POST form whose anchor href is "#", so href extraction
    // is impossible: click exactly the fixture-backed Download PDF control.
    // Non-OA pages can render the same form, therefore require the explicit
    // Open Access marker and full-text container as entitlement evidence.
    id: "annualreviews",
    version: "0.1.0",
    hosts: ["annualreviews.org"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    classify: [
      {
        kind: "article",
        all: [
          "meta[name='citation_title']",
          ".article-access.item-meta-data__oa .accesstext",
          "#html_fulltext",
          "form.ft-download-content__form--pdf a[aria-label='Download PDF']",
        ],
      },
    ],
    download: {
      selector: "form.ft-download-content__form--pdf a[aria-label='Download PDF']",
      requireKind: "article",
      workTarget: { kind: "opaque" },
      method: "click",
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
    version: "0.1.0",
    hosts: ["academic.oup.com"],
    workEvidence: { kind: "title", selector: "meta[name='citation_title']", attribute: "content" },
    settleTimeoutMs: 5000,
    classify: [
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
    version: "0.1.0",
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
      workTarget: { kind: "opaque" },
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
    candidate.hosts.some((host) => page.hostname === host || page.hostname.endsWith(`.${host}`))
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
