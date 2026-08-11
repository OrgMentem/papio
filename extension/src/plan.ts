// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// INJECTION CONSTRAINT: planExecution and planGeneric are serialized verbatim
// by chrome.scripting.executeScript({func}). Their bodies must not reference
// imports, module helpers, or closure state. Keep every helper nested below,
// and pass every policy/configuration value as an argument.

import type {
  AdapterSpec,
  DownloadRule,
  PageKind,
  PageVerdict,
  WorkEvidenceContract,
} from "./adapters/types";

export interface ExpectedWork {
  title?: string;
  doi?: string;
  year?: number;
}

export interface ExpectedWorkEvidence {
  /** Normalized request identity retained even when page metadata is absent. */
  requested_doi: string | null;
  requested_title: string | null;
  /** Exact normalized identifier and packaged source that supplied it. */
  doi: { normalized: string; fingerprint: string; selector?: string; attribute?: string; pattern?: string | null } | null;
  /** Positive title evidence is retained as a local revalidation binding. */
  title: { fingerprint: string; selector?: string; attribute?: string; pattern?: string | null } | null;
}

export interface PlanTargetWorkBinding {
  kind: "doi" | "opaque";
  selector: string;
  fingerprint: string;
  attribute: string | null;
  normalized: string | null;
  pattern: string | null;
}

export interface PlanEffectTarget {
  selector: string;
  shadow_selector: string | null;
  fingerprint: string | null;
  /** Exact packaged work binding for this page-side effect. */
  work_binding?: PlanTargetWorkBinding | null;
  /** Static packaged labels used to resolve a terms control inside selector. */
  text_any?: string[];
  control_selector?: string | null;
  control_fingerprint?: string | null;
  /** Follow-up/terms targets must be newly present after the preceding effect. */
  must_appear_after_effect?: boolean;
}

export interface PlanEffectGraph {
  primary_target: PlanEffectTarget | null;
  followup_target: PlanEffectTarget | null;
  terms_target: PlanEffectTarget | null;
  api: { endpoint: string; result_field: string; result_origin: string } | null;
  consequence: "none" | "download" | "navigation" | "modal";
  route: { origin: string; pathname: string } | null;
}

export interface PlanRevalidationLimits {
  target_cardinality: 1;
  max_selector_length: number;
  max_wait_ms: number;
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
  /** Complete local authority binding for every page-side effect. */
  expected_work: ExpectedWorkEvidence;
  effect_graph: PlanEffectGraph;
  route_origin: string | null;
  revalidation: PlanRevalidationLimits;
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
  const root: Document = doc ?? document;
  const pageHref = root.location?.href ?? "https://fixture.local/";
  const normalizeDOI = (raw: string): string => {
    let normalized = raw.trim().toLowerCase();
    for (let pass = 0; pass < 2; pass += 1) {
      normalized = normalized.replace(/^doi:\s*/i, "");
      normalized = normalized.replace(/^https?:\/\/(?:dx\.)?doi\.org\//i, "");
    }
    return normalized;
  };
  const evidence: string[] = [];
  const isPublicHost = (rawHost: string): boolean => {
    const host = rawHost.toLowerCase().replace(/^\[/, "").replace(/\]$/, "").replace(/\.$/, "");
    if (
      host === "localhost" ||
      host.endsWith(".localhost") ||
      host.endsWith(".local") ||
      host.endsWith(".internal") ||
      host.includes(":")
    ) {
      return false;
    }
    // URL normalizes the common alternate IPv4 spellings before this check.
    if (/^(?:\d{1,3}\.){3}\d{1,3}$/.test(host)) return false;
    return host.length > 0;
  };
  const safeURL = (raw: string): URL | null => {
    try {
      const parsed = new URL(raw.trim(), pageHref);
      if (parsed.protocol !== "https:" || parsed.username !== "" || parsed.password !== "") return null;
      if (!isPublicHost(parsed.hostname)) return null;
      return parsed;
    } catch {
      return null;
    }
  };
  const page = (() => {
    try {
      const parsed = new URL(pageHref);
      return parsed.protocol === "https:" && parsed.username === "" && parsed.password === "" ? parsed : null;
    } catch {
      return null;
    }
  })();
  if (page === null) return { evidence, candidates: [] };
  // Generic candidates are page-derived and have no packaged provider route
  // contract. Exact origin is therefore the entire authority boundary:
  // sibling subdomains and registrable-domain matches are not authority.
  const sameAllowedOrigin = (candidate: URL): boolean => candidate.origin === page.origin;
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
  const expectedDOI = typeof expected.doi === "string" ? normalizeDOI(expected.doi) : "";
  const jsonRecords: { element: Element; urls: string[] }[] = [];
  const directDOIs = (record: Record<string, unknown>): string[] => {
    const result: string[] = [];
    for (const key of ["identifier", "doi"]) {
      const item = record[key];
      const values = Array.isArray(item) ? item : [item];
      for (const value of values) {
        if (typeof value === "string") result.push(value);
        else if (value !== null && typeof value === "object") {
          const nested = value as Record<string, unknown>;
          for (const nestedKey of ["value", "propertyID", "name"]) {
            if (typeof nested[nestedKey] === "string") result.push(nested[nestedKey] as string);
          }
        }
      }
    }
    return result;
  };
  const recordURLs = (value: unknown, isRoot: boolean): string[] => {
    if (value === null || typeof value !== "object") return [];
    if (Array.isArray(value)) return value.slice(0, 32).flatMap((item) => recordURLs(item, false));
    const record = value as Record<string, unknown>;
    if (!isRoot && directDOIs(record).length > 0) return [];
    const urls: string[] = [];
    for (const [key, item] of Object.entries(record)) {
      if (key === "contentUrl" || key === "associatedMedia") {
        if (typeof item === "string") urls.push(item);
        else urls.push(...recordURLs(item, false));
      } else if (item !== null && typeof item === "object") {
        urls.push(...recordURLs(item, false));
      }
    }
    return urls;
  };
  const visitJSON = (value: unknown, element: Element, depth: number): void => {
    if (depth > 6 || value === null || typeof value !== "object") return;
    if (Array.isArray(value)) {
      for (const item of value.slice(0, 32)) visitJSON(item, element, depth + 1);
      return;
    }
    const record = value as Record<string, unknown>;
    if (directDOIs(record).some((doi) => expectedDOI.length > 0 && normalizeDOI(doi) === expectedDOI)) {
      jsonRecords.push({ element, urls: recordURLs(record, true) });
    }
    for (const item of Object.values(record)) {
      if (item !== null && typeof item === "object") visitJSON(item, element, depth + 1);
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
      visitJSON(JSON.parse(script.textContent ?? ""), script, 0);
    } catch {
      evidence.push("e0:json-ld-invalid");
    }
  }
  const citationPDFURLs = valuesFor('meta[name="citation_pdf_url"]', "content");
  const alternatePDFURLs = valuesFor('link[rel~="alternate"][type="application/pdf"]', "href");
  const jsonURLs = jsonRecords.flatMap((record) => record.urls);
  evidence.push(jsonURLs.length === 0 ? "e0:jsonld-content-url=missing" : "e0:jsonld-content-url=present");
  evidence.push(alternatePDFURLs.length === 0 ? "e0:alternate-pdf=missing" : "e0:alternate-pdf=present");
  const hasCitationDOI = citationDOIs.some((value) => expectedDOI.length > 0 && normalizeDOI(value) === expectedDOI);
  const hasJSONDOI = jsonRecords.length > 0;
  if (!hasCitationDOI && !hasJSONDOI) {
    evidence.push("e0:citation-doi=missing");
    return { evidence, candidates: [] };
  }
  evidence.push("e0:citation-doi=present");
  if (
    (citationDOIs.length > 0 && !hasCitationDOI) ||
    (citationDOIs.some((value) => normalizeDOI(value) !== expectedDOI) && !hasCitationDOI && !hasJSONDOI)
  ) {
    evidence.push("e0:citation-doi=mismatch");
    return { evidence, candidates: [] };
  }
  evidence.push("e0:citation-doi=exact");

  const declaredCandidates: string[] = [];
  const declaredURLs = [...citationPDFURLs, ...jsonURLs, ...alternatePDFURLs];
  const declaredSeen = new Set<string>();
  for (const raw of declaredURLs) {
    const parsed = safeURL(raw);
    if (parsed === null || !sameAllowedOrigin(parsed) || parsed.href === page.href) continue;
    if (declaredSeen.has(parsed.href)) continue;
    declaredSeen.add(parsed.href);
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
  const anchorSeen = new Set<Element>();
  try {
    for (const region of Array.from(root.querySelectorAll("article, [role='article'], main"))) {
      for (const anchor of Array.from(region.querySelectorAll("a[href]"))) {
        if (!anchorSeen.has(anchor)) {
          anchorSeen.add(anchor);
          articleAnchors.push(anchor);
        }
      }
    }
  } catch {
    // Invalid selectors are impossible here, but a hostile DOM must stay E0-only.
  }
  const articleURLs: string[] = [];
  const articleSeen = new Set<string>();
  for (const anchor of articleAnchors) {
    const parsed = safeURL(anchor.getAttribute("href") ?? "");
    if (parsed === null || !routeShape(parsed.pathname) || !sameAllowedOrigin(parsed)) continue;
    if (declaredSeen.has(parsed.href) || articleSeen.has(parsed.href)) continue;
    articleSeen.add(parsed.href);
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
  const boundedMs = doc === null ? Math.max(0, Math.min(spec.settleTimeoutMs ?? 0, 15000)) : 0;
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

  const normalizeDOI = (raw: string): string => {
    let normalized = raw.trim().toLowerCase();
    for (let pass = 0; pass < 2; pass += 1) {
      normalized = normalized.replace(/^doi:\s*/i, "");
      normalized = normalized.replace(/^https?:\/\/(?:dx\.)?doi\.org\//i, "");
    }
    return normalized;
  };
  const normalizeTitle = (raw: string): string => raw.trim().toLowerCase().replace(/\s+/g, " ");
  const workEvidenceFor = (
    contract: WorkEvidenceContract | undefined,
    requestedDOI: string | null,
    requestedTitle: string | null,
  ): {
    evidence: { normalized: string; fingerprint: string; selector: string; attribute: string; pattern: string | null } | null;
    title: { fingerprint: string; selector: string; attribute: string; pattern: string | null } | null;
  } | AssistedPlan => {
    if (requestedDOI === null && requestedTitle === null) return { evidence: null, title: null };
    if (
      contract === undefined ||
      contract.selector.length === 0 ||
      contract.selector.length > 512 ||
      contract.attribute.length === 0
    ) return { assisted: "declared work evidence is missing" };
    if (contract.kind !== "doi" && contract.kind !== "title") return { assisted: "declared work evidence kind is invalid" };
    if (
      (contract.kind === "doi" && requestedDOI === null) ||
      (contract.kind === "title" && requestedTitle === null)
    ) return { assisted: "declared work evidence does not bind the requested identity" };
    let matches: Element[];
    try {
      matches = Array.from(root.querySelectorAll(contract.selector));
    } catch {
      return { assisted: "declared work evidence selector is invalid" };
    }
    if (matches.length !== 1 || matches[0] === undefined) {
      return { assisted: matches.length === 0 ? "declared work evidence is missing" : "declared work evidence is ambiguous" };
    }
    const element = matches[0];
    const raw = element.getAttribute(contract.attribute)?.trim() ?? "";
    if (raw === "") return { assisted: "declared work evidence is empty" };
    let extracted = raw;
    if (contract.pattern !== undefined) {
      if (contract.pattern.length === 0 || contract.pattern.length > 512) return { assisted: "declared work evidence pattern is invalid" };
      let match: RegExpMatchArray | null;
      try {
        match = raw.match(new RegExp(contract.pattern));
      } catch {
        return { assisted: "declared work evidence pattern is invalid" };
      }
      if (!match || typeof match[1] !== "string") return { assisted: "declared work evidence does not match" };
      extracted = match[1];
    }
    if (contract.kind === "doi") {
      const normalized = normalizeDOI(extracted);
      if (normalized === "" || normalized !== requestedDOI) return { assisted: "declared work evidence does not match the requested work" };
      const source = { selector: contract.selector, attribute: contract.attribute, pattern: contract.pattern ?? null };
      return { evidence: { normalized, fingerprint: fingerprint(element), ...source }, title: null };
    }
    if (normalizeTitle(extracted) !== requestedTitle) {
      return { assisted: "declared work evidence does not match the requested work" };
    }
    const source = { selector: contract.selector, attribute: contract.attribute, pattern: contract.pattern ?? null };
    return { evidence: null, title: { fingerprint: fingerprint(element), ...source } };
  };

  const fingerprint = (element: Element): string => {
    const names = ["id", "class", "href", "name", "content", "type", "role", "aria-label", "data-doi", "data-qa"];
    const values: string[] = [element.tagName.toLowerCase()];
    for (const name of names) values.push(name + "=" + (element.getAttribute(name) ?? ""));
    let cursor: Element | null = element;
    while (cursor !== null && cursor.parentElement !== null) {
      let index = 0;
      for (const sibling of Array.from(cursor.parentElement.children)) {
        if (sibling === cursor) break;
        index += 1;
      }
      values.push(`p${index}`);
      cursor = cursor.parentElement;
    }
    return values.join("|");
  };
  const targetFor = (rule: DownloadRule): PlanTargetReference | AssistedPlan => {
    if (rule.selector.length === 0 || rule.selector.length > 512) {
      return { assisted: "declared action selector exceeds the revalidation limit" };
    }
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
    if (rule.method === "meta") {
      const declaredName = rule.metaName ?? "citation_pdf_url";
      const metaCtor = (root.defaultView as { HTMLMetaElement?: typeof HTMLMetaElement } | null)?.HTMLMetaElement;
      if (
        element.tagName.toUpperCase() !== "META" ||
        (metaCtor !== undefined && !(element instanceof metaCtor)) ||
        element.getAttribute("name") !== declaredName ||
        element.getAttribute("content")?.trim() === ""
      ) {
        return { assisted: "declared meta action target is not the exact HTML meta element" };
      }
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
        const allowed = u.origin === page.origin ||
          (Array.isArray(rule.allowedDestinations) && rule.allowedDestinations.some((destination) => {
            if (destination.origin !== u.origin || destination.pathPrefix.length === 0) return false;
            return u.pathname.startsWith(destination.pathPrefix);
          }));
        if (!allowed) return null;
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
    const page = (() => {
      try {
        return new URL(pageHref);
      } catch {
        return null;
      }
    })();
    const requestedDOI =
      expected.doi === undefined || normalizeDOI(expected.doi) === "" ? null : normalizeDOI(expected.doi);
    const requestedTitle =
      expected.title === undefined || normalizeTitle(expected.title) === "" ? null : normalizeTitle(expected.title);
    const expectedWork: ExpectedWorkEvidence = {
      requested_doi: requestedDOI,
      requested_title: requestedTitle,
      doi: null,
      title: null,
    };
    if (classified.verdict.kind === "terms" && requestedDOI === null && requestedTitle === null) {
      return { assisted: "terms effect has no requested work binding" };
    }
    if (
      (classified.verdict.kind === "article" || classified.verdict.kind === "terms") &&
      (requestedDOI !== null || requestedTitle !== null)
    ) {
      const evidence = workEvidenceFor(spec.workEvidence, requestedDOI, requestedTitle);
      if ("assisted" in evidence) return evidence;
      expectedWork.doi = evidence.evidence;
      expectedWork.title = evidence.title;
    }
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
      expected_work: expectedWork,
      effect_graph: {
        primary_target: null,
        followup_target: null,
        terms_target: null,
        api: null,
        consequence: "none",
        route: page === null ? null : { origin: page.origin, pathname: page.pathname },
      },
      route_origin: page?.origin ?? null,
      revalidation: {
        target_cardinality: 1,
        max_selector_length: 512,
        max_wait_ms: boundedMs,
      },
    };

    const termsTarget = (requirePresent: boolean): PlanEffectTarget | null => {
      const terms = spec.termsAccept;
      if (terms === undefined || terms.modalSelector.length === 0 || terms.modalSelector.length > 512) return null;
      let modals: Element[];
      try {
        modals = Array.from(root.querySelectorAll(terms.modalSelector));
      } catch {
        return null;
      }
      if (modals.length === 0 && !requirePresent) {
        const controlSelector = terms.control ?? null;
        if ((controlSelector !== null && (controlSelector.length === 0 || controlSelector.length > 512)) ||
            (controlSelector === null && terms.textAny.length === 0)) return null;
        return {
          selector: terms.modalSelector,
          shadow_selector: null,
          fingerprint: null,
          work_binding: null,
          ...(controlSelector === null ? { text_any: [...terms.textAny] } : {}),
          control_selector: controlSelector,
          control_fingerprint: null,
          must_appear_after_effect: true,
        };
      }
      if (modals.length !== 1 || modals[0] === undefined) return null;
      const modal = modals[0];
      const actionable = (element: Element): boolean => {
        const tag = element.tagName.toLowerCase();
        return (
          tag === "button" ||
          tag === "a" ||
          (tag === "input" && element.getAttribute("type")?.toLowerCase() === "submit") ||
          element.getAttribute("role") === "button" ||
          tag.endsWith("-button")
        );
      };
      const label = (element: Element): string => {
        const tag = element.tagName.toLowerCase();
        const submit = tag === "input" && element.getAttribute("type")?.toLowerCase() === "submit";
        const value = submit ? element.getAttribute("value") ?? "" : "";
        const formContext =
          submit && value.trim() === ""
            ? ((element.closest("form") as HTMLElement | null)?.innerText ?? "")
            : "";
        return `${(element as HTMLElement).innerText ?? ""} ${element.getAttribute("aria-label") ?? ""} ${value} ${formContext}`
          .toLowerCase();
      };
      const controls: Element[] = [];
      const seen = new Set<Element>();
      const walk = (owner: ParentNode): void => {
        for (const element of Array.from(owner.querySelectorAll("*"))) {
          if (seen.has(element)) continue;
          seen.add(element);
          if (actionable(element)) controls.push(element);
          const shadow = (element as HTMLElement & { shadowRoot?: ShadowRoot | null }).shadowRoot;
          if (shadow !== null && shadow !== undefined) walk(shadow);
        }
      };
      const controlSelector = terms.control ?? null;
      if (controlSelector !== null) {
        if (controlSelector.length === 0 || controlSelector.length > 512) return null;
        let selected: Element[];
        try {
          selected = modal.matches(controlSelector)
            ? [modal]
            : Array.from(modal.querySelectorAll(controlSelector));
        } catch {
          return null;
        }
        if (selected.length !== 1 || selected[0] === undefined || !actionable(selected[0])) return null;
        controls.push(selected[0]);
      } else {
        walk(modal);
        const needles = terms.textAny.map((needle) => needle.toLowerCase());
        const matching = controls.filter((control) => needles.some((needle) => needle.length > 0 && label(control).includes(needle)));
        if (matching.length !== 1 || matching[0] === undefined) return null;
        controls.length = 0;
        controls.push(matching[0]);
      }
      const control = controls[0];
      if (control === undefined) return null;
      return {
        selector: terms.modalSelector,
        shadow_selector: null,
        fingerprint: fingerprint(modal),
        work_binding: null,
        ...(terms.control === undefined ? { text_any: [...terms.textAny] } : {}),
        control_selector: controlSelector,
        control_fingerprint: fingerprint(control),
      };
    };
    const workBindingFor = (
      rule: DownloadRule,
      element: Element,
    ): PlanTargetWorkBinding | AssistedPlan => {
      const contract = rule.workTarget;
      if (contract === undefined) {
        if (requestedDOI !== null || requestedTitle !== null) {
          return { assisted: "declared article effect has no requested-work target contract" };
        }
        return {
          kind: "opaque",
          selector: rule.selector,
          fingerprint: fingerprint(element),
          attribute: null,
          normalized: null,
          pattern: null,
        };
      }
      const bindingSelector = contract.selector?.trim() || rule.selector;
      if (bindingSelector.length === 0 || bindingSelector.length > 512) {
        return { assisted: "declared target identity selector exceeds the revalidation limit" };
      }
      let bindingElement: Element | null = element;
      if (bindingSelector !== rule.selector) {
        let matches: Element[];
        try {
          matches = Array.from(root.querySelectorAll(bindingSelector));
        } catch {
          return { assisted: "declared target identity selector is invalid" };
        }
        if (matches.length !== 1 || matches[0] === undefined) {
          return { assisted: "declared target identity evidence is missing or ambiguous" };
        }
        bindingElement = matches[0];
      }
      if (contract.kind !== "doi" && contract.kind !== "opaque") {
        return { assisted: "declared article effect has an invalid target contract" };
      }
      if (contract.kind === "opaque") {
        if (contract.attribute !== undefined || contract.pattern !== undefined) {
          return { assisted: "opaque target contract cannot carry DOI extraction fields" };
        }
        return {
          kind: "opaque",
          selector: bindingSelector,
          fingerprint: fingerprint(bindingElement),
          attribute: null,
          normalized: null,
          pattern: null,
        };
      }
      const attribute = contract.attribute?.trim() ?? "";
      if (attribute.length === 0 || requestedDOI === null) {
        return { assisted: "DOI target contract lacks a requested DOI or attribute" };
      }
      const raw = bindingElement.getAttribute(attribute)?.trim() ?? "";
      if (raw.length === 0) return { assisted: "declared target DOI evidence is missing" };
      let normalized = "";
      if (contract.pattern !== undefined) {
        if (contract.pattern.length === 0 || contract.pattern.length > 512) {
          return { assisted: "declared target DOI pattern exceeds the revalidation limit" };
        }
        let match: RegExpMatchArray | null;
        try {
          match = raw.match(new RegExp(contract.pattern));
        } catch {
          return { assisted: "declared target DOI pattern is invalid" };
        }
        if (!match || typeof match[1] !== "string") {
          return { assisted: "declared target DOI evidence does not match" };
        }
        normalized = normalizeDOI(match[1]);
      } else {
        normalized = normalizeDOI(raw);
      }
      if (normalized === "" || normalized !== requestedDOI) {
        return { assisted: "declared action target does not match the requested work" };
      }
      return {
        kind: "doi",
        selector: bindingSelector,
        fingerprint: fingerprint(bindingElement),
        attribute,
        normalized,
        pattern: contract.pattern ?? null,
      };
    };
    const download = spec.download;
    if (classified.verdict.kind !== "article" || download === undefined || download.requireKind !== "article") {
      if (classified.verdict.kind === "terms" && base.effect_graph !== undefined) {
        const terms = termsTarget(true);
        if (terms === null) return { assisted: "declared terms accept target is missing, ambiguous, or unreadable" };
        base.effect_graph.terms_target = terms;
        base.effect_graph.consequence = "download";
      }
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
    const workBinding = workBindingFor(download, element);
    if ("assisted" in workBinding) return workBinding;
    const url = resolveURL(download, element) ?? null;
    if ((download.method === "href" || download.method === "meta" || download.method === "url") && url === null) {
      return { assisted: "declared action URL is not a distinct HTTPS URL" };
    }
    const followupMatches =
      download.followupSelector === undefined
        ? []
        : (() => {
            try {
              return Array.from(root.querySelectorAll(download.followupSelector));
            } catch {
              return [];
            }
          })();
    if (followupMatches.length > 0) {
      return { assisted: "declared follow-up target already exists before the primary effect" };
    }
    const effectGraph = base.effect_graph!;
    effectGraph.primary_target = {
      selector: target.selector,
      shadow_selector: target.shadow_selector,
      fingerprint: target.fingerprint,
      work_binding: workBinding,
    };
    effectGraph.followup_target =
      download.followupSelector === undefined
        ? null
        : {
            selector: download.followupSelector,
            shadow_selector: null,
            fingerprint: null,
            work_binding: null,
            must_appear_after_effect: true,
          };
    const terms = termsTarget(false);
    if (spec.termsAccept !== undefined && terms === null) {
      return { assisted: "declared terms accept target is missing, ambiguous, or unreadable" };
    }
    effectGraph.terms_target = terms;
    effectGraph.api =
      download.method === "api" && url !== null
        ? {
            endpoint: url,
            result_field: download.jsonField ?? "",
            result_origin: new URL(url).origin,
          }
        : null;
    effectGraph.consequence = download.method === "click" ? "modal" : "download";
    effectGraph.route =
      url === null
        ? effectGraph.route
        : (() => {
            const resolved = new URL(url);
            return { origin: resolved.origin, pathname: resolved.pathname };
          })();
    return {
      ...base,
      target_ref: target,
      method: download.method,
      url,
      required_consequence: "download",
    };
  };
  const selectorsReady = (): PageKind | null => {
    for (const rule of spec.classify) {
      const hasAll = Array.isArray(rule.all) && rule.all.length > 0;
      const hasAny = Array.isArray(rule.any) && rule.any.length > 0;
      const hasText = Array.isArray(rule.textAny) && rule.textAny.length > 0;
      if (!hasAll && !hasAny && !hasText) continue;
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
            // An invalid alternative does not make the rule ready.
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
      if (!allReady || !anyReady || !textReady) continue;
      if (rule.kind === "article" && spec.download !== undefined && spec.download.requireKind === "article") {
        const action = targetFor(spec.download);
        if ("assisted" in action) continue;
        if (spec.download.followupSelector !== undefined) {
          let existing: Element[] = [];
          try {
            existing = Array.from(root.querySelectorAll(spec.download.followupSelector));
          } catch {
            continue;
          }
          if (existing.length !== 0) continue;
        }
      }
      return rule.kind;
    }
    return null;
  };
  if (boundedMs === 0 || root.documentElement === null) return buildPlan();

  return new Promise<PlanResult>((resolve) => {
    let settled = false;
    let observer: MutationObserver | null = null;
    let timer: number | undefined;
    let readyTimer: number | undefined;
    const finish = (): void => {
      if (settled) return;
      settled = true;
      observer?.disconnect();
      if (timer !== undefined) clearTimeout(timer);
      if (readyTimer !== undefined) clearTimeout(readyTimer);
      resolve(buildPlan());
    };
    const settleWindowMs = Math.min(50, boundedMs);
    const scheduleWhenReady = (): void => {
      const readyKind = selectorsReady();
      if (readyKind === null) {
        if (readyTimer !== undefined) {
          clearTimeout(readyTimer);
          readyTimer = undefined;
        }
        return;
      }
      // Article readiness is provisional for the full bounded settle period:
      // earlier login/terms rules may hydrate after the article controls.
      if (readyKind === "article") {
        if (readyTimer !== undefined) {
          clearTimeout(readyTimer);
          readyTimer = undefined;
        }
        return;
      }
      if (readyTimer === undefined) {
        readyTimer = setTimeout(finish, settleWindowMs) as unknown as number;
      }
    };
    const Observer =
      root.defaultView?.MutationObserver ??
      (typeof MutationObserver === "undefined" ? undefined : MutationObserver);
    if (Observer !== undefined) {
      observer = new Observer(scheduleWhenReady);
      observer.observe(root.documentElement as Element, { childList: true, subtree: true, attributes: true });
    }
    timer = setTimeout(finish, boundedMs) as unknown as number;
    scheduleWhenReady();
  });
}
