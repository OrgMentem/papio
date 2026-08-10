// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// INJECTION CONSTRAINT: planExecution and planGeneric are serialized verbatim
// by chrome.scripting.executeScript({func}). Their bodies must not reference
// imports, module helpers, or closure state. Keep every helper nested below,
// and pass every policy/configuration value as an argument.

import type { AdapterSpec, DownloadRule, PageVerdict } from "./adapters/types";

export interface ExpectedWork {
  title?: string;
  doi?: string;
  year?: number;
}

export interface PlanPolicy {
  access_mode?: "assisted" | "delegated" | "conservative";
  terms_consent?: "accept" | "decline";
}

export interface PlanTargetReference {
  selector: string;
  shadow_selector: string | null;
  fingerprint: string;
}

export interface Plan {
  adapter_id: string;
  adapter_version: string;
  verdict: PageVerdict;
  /** Static rule label, never page-derived text. */
  decisive_rule: string | null;
  target_ref: PlanTargetReference | null;
  method: DownloadRule["method"] | null;
  /** A direct URL or declared API endpoint derivable before the action. */
  url: string | null;
  required_consequence: "none" | "download";
  access_mode: PlanPolicy["access_mode"];
  terms_consent: PlanPolicy["terms_consent"] | null;
}

export interface AssistedPlan {
  assisted: string;
}

export type PlanResult = Plan | AssistedPlan;

export interface GenericCandidate {
  strategy_id: "generic-citation-pdf/1" | "generic-article-pdf-link/1";
  strategy_version: "1";
  url: string;
}

export interface GenericPlan {
  /** E0 observations are static labels; page text and raw URLs stay local. */
  evidence: string[];
  candidates: GenericCandidate[];
}

export function planGeneric(
  doc: Document,
  expected: ExpectedWork,
  policy: PlanPolicy,
): GenericPlan;
export function planGeneric(
  doc: null,
  expected: ExpectedWork,
  policy: PlanPolicy,
): Promise<GenericPlan>;
export function planGeneric(
  doc: Document | null,
  expected: ExpectedWork,
  policy: PlanPolicy,
): GenericPlan | Promise<GenericPlan> {
  // Keep this function self-contained: it is serialized into a provider tab.
  const root: Document = doc ?? document;
  const pageHref = root.location?.href ?? "https://fixture.local/";
  const evidence: string[] = [];

  const normalizeDOI = (raw: string): string => {
    let value = raw.trim().toLowerCase();
    value = value.replace(/^doi:\s*/, "");
    value = value.replace(/^(?:https?:\/\/)?(?:dx\.)?doi\.org\//, "");
    return value;
  };
  const registrableHost = (host: string): string => {
    const labels = host.toLowerCase().split(".").filter(Boolean);
    if (labels.length <= 2) return labels.join(".");
    const suffix = labels.slice(-2).join(".");
    const commonTwoLevel: Record<string, true> = {
      "co.uk": true,
      "org.uk": true,
      "ac.uk": true,
      "gov.uk": true,
      "com.au": true,
      "net.au": true,
      "org.au": true,
      "co.nz": true,
      "com.br": true,
      "co.jp": true,
    };
    return commonTwoLevel[suffix] === true && labels.length >= 3 ? labels.slice(-3).join(".") : suffix;
  };
  const safeURL = (raw: string): URL | null => {
    try {
      const parsed = new URL(raw.trim(), pageHref);
      if (parsed.protocol !== "https:" || parsed.username !== "" || parsed.password !== "") return null;
      return parsed;
    } catch {
      return null;
    }
  };
  const page = safeURL(pageHref);
  if (page === null) return { evidence, candidates: [] };
  const sameAllowedHost = (candidate: URL, declaredOrigin: string | null): boolean => {
    const pageHost = registrableHost(page.hostname);
    if (registrableHost(candidate.hostname) === pageHost) return true;
    if (declaredOrigin === null) return false;
    try {
      const declared = new URL(declaredOrigin);
      return registrableHost(candidate.hostname) === registrableHost(declared.hostname);
    } catch {
      return false;
    }
  };
  const valuesFor = (selector: string, attribute: string): string[] => {
    let nodes: Element[];
    try {
      nodes = Array.from(root.querySelectorAll(selector));
    } catch {
      return [];
    }
    return nodes
      .map((node) => node.getAttribute(attribute)?.trim() ?? "")
      .filter((value) => value.length > 0);
  };
  const citationDOIs = valuesFor('meta[name="citation_doi"]', "content");
  const citationTitles = valuesFor('meta[name="citation_title"]', "content");
  const citationYears = valuesFor('meta[name="citation_year"]', "content");
  evidence.push(citationTitles.length === 0 ? "e0:citation-title=missing" : "e0:citation-title=present");
  evidence.push(citationYears.length === 0 ? "e0:citation-year=missing" : "e0:citation-year=present");
  const jsonValues: { dois: string[]; urls: string[] } = { dois: [], urls: [] };
  const visitJSON = (value: unknown, depth: number): void => {
    if (depth > 6 || value === null || typeof value !== "object") return;
    if (Array.isArray(value)) {
      for (const item of value.slice(0, 32)) visitJSON(item, depth + 1);
      return;
    }
    const record = value as Record<string, unknown>;
    for (const [key, item] of Object.entries(record)) {
      if (typeof item === "string") {
        if (key === "contentUrl" || key === "associatedMedia") jsonValues.urls.push(item);
        if (key === "identifier" || key === "doi") jsonValues.dois.push(item);
      } else {
        visitJSON(item, depth + 1);
      }
    }
  };
  let jsonScripts: Element[] = [];
  try {
    jsonScripts = Array.from(root.querySelectorAll('script[type="application/ld+json"]'));
  } catch {
    jsonScripts = [];
  }
  for (const script of jsonScripts) {
    try {
      visitJSON(JSON.parse(script.textContent ?? ""), 0);
    } catch {
      evidence.push("e0:json-ld-invalid");
    }
  }
  const alternatePDFURLs = valuesFor('link[rel~="alternate"][type="application/pdf"]', "href");
  evidence.push(jsonValues.urls.length === 0 ? "e0:jsonld-content-url=missing" : "e0:jsonld-content-url=present");
  evidence.push(alternatePDFURLs.length === 0 ? "e0:alternate-pdf=missing" : "e0:alternate-pdf=present");
  const exposedDOI = [...citationDOIs, ...jsonValues.dois].find((value) => normalizeDOI(value).length > 0);
  if (exposedDOI === undefined) {
    evidence.push("e0:citation-doi=missing");
    return { evidence, candidates: [] };
  }
  evidence.push("e0:citation-doi=present");
  const expectedDOI = typeof expected.doi === "string" ? normalizeDOI(expected.doi) : "";
  if (expectedDOI.length === 0 || normalizeDOI(exposedDOI) !== expectedDOI) {
    evidence.push("e0:citation-doi=mismatch");
    return { evidence, candidates: [] };
  }
  evidence.push("e0:citation-doi=exact");

  const declaredURLs = [
    ...valuesFor('meta[name="citation_pdf_url"]', "content"),
    ...jsonValues.urls,
    ...alternatePDFURLs,
  ];
  const citationPDFOrigins: string[] = [];
  for (const raw of valuesFor('meta[name="citation_pdf_url"]', "content")) {
    const parsed = safeURL(raw);
    if (parsed !== null) citationPDFOrigins.push(parsed.origin);
  }
  const declaredOrigin = citationPDFOrigins[0] ?? null;
  const declaredCandidates: string[] = [];
  for (const raw of declaredURLs) {
    const parsed = safeURL(raw);
    if (parsed === null || !sameAllowedHost(parsed, declaredOrigin)) continue;
    if (parsed.href === page.href) continue;
    declaredCandidates.push(parsed.href);
  }
  if (declaredCandidates.length === 1) {
    evidence.push("e0:citation-pdf=unique");
  } else if (declaredCandidates.length > 1) {
    evidence.push("e0:citation-pdf=ambiguous");
  } else {
    evidence.push("e0:citation-pdf=missing");
  }
  const candidates: GenericCandidate[] = [];
  if (declaredCandidates.length === 1) {
    candidates.push({
      strategy_id: "generic-citation-pdf/1",
      strategy_version: "1",
      url: declaredCandidates[0]!,
    });
  }

  const routeShape = (pathname: string): boolean => {
    const path = pathname.toLowerCase();
    return path.endsWith(".pdf") || /\/(?:pdf|download|full[-_]?text)(?:\/|$)/.test(path);
  };
  const articleAnchors: Element[] = [];
  try {
    for (const region of Array.from(root.querySelectorAll("article, [role='article'], main"))) {
      for (const anchor of Array.from(region.querySelectorAll("a[href]"))) articleAnchors.push(anchor);
    }
  } catch {
    // Invalid selectors are impossible here, but a hostile DOM must stay E0-only.
  }
  const articleURLs: string[] = [];
  for (const anchor of articleAnchors) {
    const parsed = safeURL(anchor.getAttribute("href") ?? "");
    if (parsed === null || !routeShape(parsed.pathname) || !sameAllowedHost(parsed, declaredOrigin)) continue;
    articleURLs.push(parsed.href);
  }
  if (articleURLs.length === 1) {
    evidence.push("e0:article-pdf-link=unique");
    candidates.push({
      strategy_id: "generic-article-pdf-link/1",
      strategy_version: "1",
      url: articleURLs[0]!,
    });
  } else if (articleURLs.length > 1) {
    evidence.push("e0:article-pdf-link=ambiguous");
  } else {
    evidence.push("e0:article-pdf-link=missing");
  }
  if (policy.access_mode !== "delegated") return { evidence, candidates: [] };
  return { evidence, candidates };
}

export function planExecution(
  doc: Document,
  spec: AdapterSpec,
  expected: ExpectedWork,
  policy: PlanPolicy,
): PlanResult;
export function planExecution(
  doc: null,
  spec: AdapterSpec,
  expected: ExpectedWork,
  policy: PlanPolicy,
): Promise<PlanResult>;
export function planExecution(
  doc: Document | null,
  spec: AdapterSpec,
  expected: ExpectedWork,
  policy: PlanPolicy,
): PlanResult | Promise<PlanResult> {
  // Do not move helpers outside this function. Chrome serializes only this
  // function, not the module imports or any lexical closure around it.
  const root: Document = doc ?? document;
  const pageHref = root.location?.href ?? "https://fixture.local/";

  const classify = (): { verdict: PageVerdict; decisiveRule: string | null } => {
    const evidence: string[] = [];
    const adapter_id = spec.id;
    const adapter_version = spec.version;

    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      const hasText = Array.isArray(rule.textAny) && rule.textAny.length > 0;
      if (!hasAll && !hasAny && !hasText) continue;

      if (hasAll) {
        let ok = true;
        for (const selector of rule.all as string[]) {
          if (root.querySelector(selector) === null) {
            ok = false;
            break;
          }
        }
        if (!ok) continue;
      }
      if (hasAny) {
        let ok = false;
        for (const selector of rule.any as string[]) {
          if (root.querySelector(selector) !== null) {
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

      const decisiveRule = "rule:" + rule.kind + " matched";
      evidence.push(decisiveRule);
      if (rule.kind === "article") {
        const expectedTitle = expected.title;
        if (expectedTitle !== undefined && expectedTitle.length > 0) {
          const parts: string[] = [];
          const h1 = root.querySelector("h1");
          if (h1 && h1.textContent) parts.push(h1.textContent);
          const titleMeta = root.querySelector('meta[name="citation_title"]');
          const metaContent = titleMeta ? titleMeta.getAttribute("content") : null;
          if (metaContent) parts.push(metaContent);
          if (root.title) parts.push(root.title);
          const haystack = parts.join(" ").toLowerCase();
          const tokens = expectedTitle
            .toLowerCase()
            .split(/[^a-z0-9]+/)
            .filter((token) => token.length > 3);
          let present = 0;
          for (const token of tokens) if (haystack.indexOf(token) !== -1) present++;
          const ratio = tokens.length === 0 ? 1 : present / tokens.length;
          if (ratio < 0.6) {
            evidence.push("title-token-check failed");
            return {
              verdict: { kind: "wrong_work", adapter_id, adapter_version, evidence },
              decisiveRule,
            };
          }
          evidence.push("title-token-check passed");
        }
      }
      return {
        verdict: { kind: rule.kind, adapter_id, adapter_version, evidence },
        decisiveRule,
      };
    }

    evidence.push("no rule matched");
    return {
      verdict: { kind: "unknown", adapter_id, adapter_version, evidence },
      decisiveRule: null,
    };
  };

  const fingerprint = (element: Element): string => {
    const names = ["id", "class", "href", "name", "type", "role", "aria-label", "data-doi", "data-qa"];
    const values: string[] = [element.tagName.toLowerCase()];
    for (const name of names) values.push(name + "=" + (element.getAttribute(name) ?? ""));
    return values.join("|");
  };

  const targetFor = (rule: DownloadRule): PlanTargetReference | AssistedPlan => {
    let matches: Element[];
    try {
      matches = Array.from(root.querySelectorAll(rule.selector));
    } catch {
      return { assisted: "declared action selector is invalid" };
    }
    if (matches.length !== 1) {
      return {
        assisted:
          matches.length === 0
            ? "declared action target is missing"
            : "declared action target is not unique",
      };
    }
    const element = matches[0];
    if (element === undefined) return { assisted: "declared action target is missing" };

    if (rule.method === "href" && element.tagName.toUpperCase() !== "A") {
      return { assisted: "href action target is not an anchor" };
    }
    let shadowFingerprint = "";
    if (rule.method === "click" && rule.shadowSelector !== undefined) {
      const shadow = (element as HTMLElement & { shadowRoot?: ShadowRoot | null }).shadowRoot;
      if (shadow === null || shadow === undefined) return { assisted: "declared shadow action target is missing" };
      let shadowMatches: Element[];
      try {
        shadowMatches = Array.from(shadow.querySelectorAll(rule.shadowSelector));
      } catch {
        return { assisted: "declared shadow action selector is invalid" };
      }
      if (shadowMatches.length !== 1) {
        return {
          assisted:
            shadowMatches.length === 0
              ? "declared shadow action target is missing"
              : "declared shadow action target is not unique",
        };
      }
      const shadowTarget = shadowMatches[0];
      if (shadowTarget === undefined) return { assisted: "declared shadow action target is missing" };
      shadowFingerprint = ">>" + fingerprint(shadowTarget);
    }
    return {
      selector: rule.selector,
      shadow_selector: rule.shadowSelector ?? null,
      fingerprint: fingerprint(element) + shadowFingerprint,
    };
  };

  const resolveURL = (rule: DownloadRule, target: Element): string | null => {
    const raw = rule.method === "meta" ? target.getAttribute("content") : target.getAttribute("href");
    if (rule.method === "href" || rule.method === "meta") {
      const trimmed = raw?.trim() ?? "";
      if (trimmed.length === 0) return null;
      try {
        const u = new URL(trimmed, pageHref);
        if (u.protocol !== "https:" || u.username !== "" || u.password !== "") return null;
        const page = new URL(pageHref);
        const isSelf = u.origin === page.origin && u.pathname === page.pathname && u.search === page.search;
        return isSelf ? null : u.href;
      } catch {
        return null;
      }
    }
    if (rule.method !== "url" && rule.method !== "api") return null;
    if (!rule.urlTemplate) return null;
    let built = rule.urlTemplate;
    if (rule.idPattern) {
      let match: RegExpMatchArray | null;
      try {
        match = pageHref.match(new RegExp(rule.idPattern));
      } catch {
        return null;
      }
      if (!match) return null;
      built = built.replace(/\{(\d+|id)\}/g, (_whole: string, key: string) => match[key === "id" ? 1 : Number(key)] ?? "");
    }
    try {
      const u = new URL(built, pageHref);
      return u.protocol === "https:" ? u.href : null;
    } catch {
      return null;
    }
  };

  const buildPlan = (): PlanResult => {
    const classified = classify();
    const base: Plan = {
      adapter_id: spec.id,
      adapter_version: spec.version,
      verdict: classified.verdict,
      decisive_rule: classified.decisiveRule,
      target_ref: null,
      method: null,
      url: null,
      required_consequence: "none",
      access_mode: policy.access_mode,
      terms_consent: policy.terms_consent ?? null,
    };

    const download = spec.download;
    if (classified.verdict.kind !== "article" || download === undefined || download.requireKind !== "article") {
      return base;
    }

    const target = targetFor(download);
    if ("assisted" in target) return target;
    const element = (() => {
      try {
        return root.querySelector(download.selector);
      } catch {
        return null;
      }
    })();
    if (element === null) return { assisted: "declared action target disappeared" };
    const url = resolveURL(download, element);
    if ((download.method === "href" || download.method === "meta" || download.method === "url") && url === null) {
      return { assisted: "declared action URL is not a distinct HTTPS URL" };
    }
    return {
      ...base,
      target_ref: target,
      method: download.method,
      url,
      required_consequence: "download",
    };
  };

  if (doc !== null) return buildPlan();
  const boundedMs = Math.max(0, Math.min(spec.settleTimeoutMs ?? 0, 15000));
  if (boundedMs === 0 || root.documentElement === null) return Promise.resolve(buildPlan());

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
  if (selectorsReady()) return Promise.resolve(buildPlan());

  return new Promise<PlanResult>((resolve) => {
    let settled = false;
    let observer: MutationObserver | null = null;
    let timer: number | undefined;
    const finish = (): void => {
      if (settled) return;
      settled = true;
      observer?.disconnect();
      if (timer !== undefined) clearTimeout(timer);
      resolve(buildPlan());
    };
    observer = new MutationObserver(() => {
      if (selectorsReady()) finish();
    });
    observer.observe(root.documentElement as Element, { childList: true, subtree: true, attributes: true });
    timer = setTimeout(finish, boundedMs) as unknown as number;
  });
}
