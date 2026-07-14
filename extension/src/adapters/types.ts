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
}

export interface AdapterSpec {
  id: string;
  version: string;
  hosts: string[];
  /** Ordered rules; first match wins. */
  classify: ClassifyRule[];
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
 * DOM-only: no chrome.* usage, no network, no mutation.
 *
 * SERIALIZATION CONTRACT: this function must remain self-contained (no imports,
 * helpers, or closures referenced at runtime) so it survives
 * `Function.prototype.toString()` inside chrome.scripting.executeScript. When
 * injected it is called as `interpret(null, spec, ctx)` — the `doc` argument
 * arrives as `null` and we fall back to the page's global `document`; `spec`
 * and `ctx` are the JSON args. Tests pass a real happy-dom Document as `doc`.
 */
export function interpret(doc: Document, spec: AdapterSpec, ctx: AdapterContext): PageVerdict {
  const root: Document = doc ?? document;
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

    // Rule matched.
    evidence.push("rule:" + rule.kind + " matched");

    if (rule.kind === "article") {
      const expectedTitle = ctx.expected.title;
      if (expectedTitle !== undefined && expectedTitle.length > 0) {
        // Gather the page's own title signal: h1, citation_title meta, <title>.
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
}

/**
 * Registered provider adapters. Intentionally empty until the first real
 * provider fixtures are captured and a ProQuest spec is authored against them
 * (Phase 3). The hello frame reports `{ [spec.id]: spec.version }` for every
 * entry here, and the background classifier only ever runs a spec drawn from
 * this registry — on a host both advertised here and granted by the user.
 */
export const adapters: AdapterSpec[] = [];
