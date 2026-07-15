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
   * selected control in its open shadow root). */
  method: "href" | "click";
  shadowSelector?: string;
  /** Wait for this fixture-backed in-page gate before reclassification. */
  postClickWaitFor?: string;
  /** After the first click, wait for and click this one fixture-backed control
   * (for provider-owned download modals; never terms/consent controls). */
  followupSelector?: string;
  /** Shared bounded wait for post-click gate/follow-up insertion. */
  postClickTimeoutMs?: number;
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
    version: "0.1.0",
    hosts: ["proquest.com"],
    classify: [{ kind: "article", all: ["a[id^='downloadPDFLink_']", "h1"] }],
    download: {
      selector: "a[id^='downloadPDFLink_']",
      requireKind: "article",
      method: "href",
    },
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
      method: "click",
      shadowSelector: "#button-element",
      postClickWaitFor: "mfe-download-pharos-modal.terms-and-conditions[open]",
      postClickTimeoutMs: 3000,
    },
  },
  {
    // Verified live 2026-07-14 against a Example University-authenticated EBSCOhost record
    // and its provider-owned download-format modal (fixtures/ebsco/success.html).
    id: "ebsco",
    version: "0.1.0",
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
    ],
    download: {
      selector: "button[data-auto='card-call-to-action-download-button']",
      requireKind: "article",
      method: "click",
      followupSelector: "button[data-auto='bulk-download-modal-download-button']",
      postClickTimeoutMs: 5000,
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
];
