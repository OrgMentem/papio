// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Options page: source and library-resolver host permission grant/revoke. The
// button click is the user gesture chrome.permissions.request requires.
// Selecting the daemon's `delegated` access mode never grants a Chrome permission
// by itself — that only happens here, explicitly.

import {
  chromeBackend,
  PAGE_CAPTURE_CONSENT_KEY,
  WORK_WINDOW_KEY,
  HANDOFF_SURFACE_KEY,
  type StoreShape,
} from "./state";
import { renderPapio } from "./dom";
import { adapters, type AdapterSpec } from "./adapters/types";
import { clampKeepaliveInterval } from "./keepalive";

export interface Source {
  label: string;
  origin: string;
}

export const ALL_SITES_ORIGIN = "https://*/*";
const ALL_SITES_SOURCE: Source = {
  label: "All sites (covers every site)",
  origin: ALL_SITES_ORIGIN,
};
const ALL_SITES_SOURCES: readonly Source[] = [ALL_SITES_SOURCE];
const ALL_SITES_PATTERNS: readonly string[] = [ALL_SITES_ORIGIN];

const ADAPTER_LABELS: Readonly<Record<string, string>> = {
  acm: "ACM Digital Library",
  annualreviews: "Annual Reviews",
  bmj: "BMJ",
  cambridge: "Cambridge Core",
  ebsco: "EBSCO",
  emerald: "Emerald Insight",
  hal: "HAL",
  jamanetwork: "JAMA Network",
  jstor: "JSTOR",
  lww: "Lippincott Williams & Wilkins",
  mitpress: "MIT Press Direct",
  nature: "Nature",
  oup: "Oxford Academic",
  proquest: "ProQuest",
  psychiatryonline: "PsychiatryOnline",
  psycnet: "APA PsycNet",
  sage: "SAGE Journals",
  sciencedirect: "ScienceDirect (Elsevier)",
  springer: "Springer Nature Link",
  tandfonline: "Taylor & Francis Online",
  thieme: "Thieme Connect",
  wiley: "Wiley Online Library",
};

function adapterLabel(adapter: AdapterSpec): string {
  const known = ADAPTER_LABELS[adapter.id];
  if (known) return known;
  const words = adapter.id
    .split(/[-_]+/)
    .filter(Boolean)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1));
  return words.join(" ") || adapter.hosts[0] || "Unknown provider";
}

/** Produce host permissions that match the provider and all its subdomains. */
export function providerSourcesFromAdapters(adapterSpecs: readonly AdapterSpec[]): Source[] {
  const sources = new Map<string, Source>();
  for (const adapter of adapterSpecs) {
    for (const host of adapter.hosts) {
      const origin = `https://*.${host.toLowerCase()}/*`;
      if (!sources.has(origin)) sources.set(origin, { label: adapterLabel(adapter), origin });
    }
  }
  return [...sources.values()];
}

export const PROVIDER_SOURCES = providerSourcesFromAdapters(adapters);

// Must mirror manifest.json host_permissions exactly.
const LIBRARY_RESOLVERS: Source[] = [
  { label: "Ex Libris Alma", origin: "https://*.alma.exlibrisgroup.com/*" },
  { label: "Ex Libris Primo", origin: "https://*.primo.exlibrisgroup.com/*" },
];

type PermissionSnapshot = {
  origins: readonly string[];
  allSitesGranted: boolean;
};

function render(
  list: HTMLUListElement,
  sources: readonly Source[],
  permissionSnapshot: PermissionSnapshot,
): void {
  list.replaceChildren();
  for (const source of sources) {
    const item = document.createElement("li");

    const meta = document.createElement("div");
    const label = document.createElement("div");
    label.className = "source-label";
    label.textContent = source.label;
    const host = document.createElement("div");
    host.className = "source-host";
    host.textContent = source.origin;
    meta.append(label, host);

    const specificallyGranted = permissionSnapshot.origins.includes(source.origin);
    const coveredByAllSites =
      source.origin !== ALL_SITES_ORIGIN &&
      !specificallyGranted &&
      coveredByAllSitesGrant(source.origin, permissionSnapshot.allSitesGranted);
    if (coveredByAllSites) {
      const coverage = document.createElement("div");
      coverage.className = "hint";
      coverage.textContent = "Covered by all-sites access. Turn it off above to control this provider separately.";
      meta.append(coverage);
      item.append(meta);
      list.append(item);
      continue;
    }

    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.className = "switch";
    toggle.setAttribute("role", "switch");
    toggle.setAttribute("aria-checked", specificallyGranted ? "true" : "false");
    toggle.setAttribute("aria-label", `Access to ${source.label} (${source.origin})`);

    toggle.addEventListener("click", () => {
      // permissions.request must be invoked directly in the trusted click
      // callback. Awaiting a state read first loses Chrome's user gesture.
      toggle.disabled = true;
      const change = specificallyGranted
        ? chrome.permissions.remove({ origins: [source.origin] })
        : chrome.permissions.request({ origins: [source.origin] });
      const refresh = (): void => {
        void renderPermissionLists().then(() => {
          if (toggle.isConnected) toggle.disabled = false;
        });
      };
      void change.then(refresh, refresh);
    });

    item.append(meta, toggle);
    list.append(item);
  }
}

// Bulk grants keep Firefox's one-click prompt. Revocation includes the broad
// optional origin so its label remains true when that grant is active.
function wireProviderBulk(sources: readonly Source[]): void {
  const origins = sources.map((source) => source.origin);
  const revokeOrigins = [...origins, ALL_SITES_ORIGIN];
  const grantAll = document.getElementById("grant-all");
  const revokeAll = document.getElementById("revoke-all");
  if (grantAll instanceof HTMLButtonElement) {
    grantAll.addEventListener("click", () => {
      // permissions.request must reach Chrome before any await.
      const change = chrome.permissions.request({ origins });
      void change.then(
        () => void renderPermissionLists(),
        () => void renderPermissionLists(),
      );
    });
  }
  if (revokeAll instanceof HTMLButtonElement) {
    revokeAll.addEventListener("click", () => {
      const change = chrome.permissions.remove({ origins: revokeOrigins });
      void change.then(
        () => void renderPermissionLists(true),
        () => void renderPermissionLists(true),
      );
    });
  }
}

const TERMS_CONSENT_KEY = "papio_terms_consent_v1";

/** Reflect the stored consent on the switch; the hint row shows only while the
 * choice is still unset (a two-state switch can't express "ask on first use"). */
async function renderTermsConsent(): Promise<void> {
  const toggle = document.getElementById("terms-consent-toggle");
  if (!(toggle instanceof HTMLButtonElement)) return;
  let consent: "accept" | "manual" | undefined;
  try {
    const got = await chrome.storage.local.get(TERMS_CONSENT_KEY);
    const v = got[TERMS_CONSENT_KEY];
    consent = v === "accept" || v === "manual" ? v : undefined;
  } catch {
    consent = undefined;
  }
  toggle.setAttribute("aria-checked", consent === "accept" ? "true" : "false");
  toggle.disabled = false;
  const hint = document.getElementById("terms-consent-hint");
  if (hint instanceof HTMLElement) hint.hidden = consent !== undefined;
}

function wireTermsConsent(): void {
  const toggle = document.getElementById("terms-consent-toggle");
  if (!(toggle instanceof HTMLButtonElement)) return;
  toggle.addEventListener("click", () => {
    // Turning the switch off is an explicit "manual" choice, not back to unset.
    const next = toggle.getAttribute("aria-checked") === "true" ? "manual" : "accept";

    toggle.disabled = true;
    void chrome.storage.local.set({ [TERMS_CONSENT_KEY]: next }).then(renderTermsConsent, () => {
      toggle.disabled = false;
    });
  });
}
/** Reflect Firefox's pre-140 page-capture consent. Chrome and Firefox 140+
 * ignore this setting in the background, but keeping the choice durable lets
 * an upgrade preserve the operator's decision. */
export async function renderPageCaptureConsent(): Promise<void> {
  const checkbox = document.getElementById("page-capture-consent");
  if (!(checkbox instanceof HTMLElement) || checkbox.tagName !== "INPUT") return;
  const input = checkbox as HTMLInputElement;
  let consent = false;
  try {
    const got = await chrome.storage.local.get(PAGE_CAPTURE_CONSENT_KEY);
    consent = got[PAGE_CAPTURE_CONSENT_KEY] === true;
  } catch {
    consent = false;
  }
  input.checked = consent;
  input.disabled = false;
}

export function wirePageCaptureConsent(): void {
  const checkbox = document.getElementById("page-capture-consent");
  if (!(checkbox instanceof HTMLElement) || checkbox.tagName !== "INPUT") return;
  const input = checkbox as HTMLInputElement;
  input.addEventListener("change", () => {
    input.disabled = true;
    void chrome.storage.local
      .set({ [PAGE_CAPTURE_CONSENT_KEY]: input.checked })
      .then(renderPageCaptureConsent, () => {
        input.disabled = false;
      });
  });
}

export const KEEPALIVE_ENABLED_KEY = "keepalive.enabled";
export const KEEPALIVE_INTERVAL_KEY = "keepalive.interval";

/** Reflect keep-warm preferences from the same storage keys the background
 * manager reads. The input is deliberately clamped before it is displayed. */
export async function renderKeepalive(): Promise<void> {
  const toggle = document.getElementById("keepalive-enabled");
  const inputElement = document.getElementById("keepalive-interval");
  if (!(toggle instanceof HTMLButtonElement) || !(inputElement instanceof HTMLElement) || inputElement.tagName !== "INPUT") {
    return;
  }
  const input = inputElement as HTMLInputElement;
  let values: Record<string, unknown> = {};
  try {
    values = await chrome.storage.local.get([KEEPALIVE_ENABLED_KEY, KEEPALIVE_INTERVAL_KEY]);
  } catch {
    // Use the manager's defaults when storage is temporarily unavailable.
  }
  toggle.setAttribute("aria-checked", values[KEEPALIVE_ENABLED_KEY] === false ? "false" : "true");
  toggle.disabled = false;
  input.value = String(clampKeepaliveInterval(values[KEEPALIVE_INTERVAL_KEY]));
  input.disabled = false;
}

export function wireKeepalive(): void {
  const toggle = document.getElementById("keepalive-enabled");
  const inputElement = document.getElementById("keepalive-interval");
  if (!(toggle instanceof HTMLButtonElement) || !(inputElement instanceof HTMLElement) || inputElement.tagName !== "INPUT") {
    return;
  }
  const input = inputElement as HTMLInputElement;
  toggle.addEventListener("click", () => {
    const enabled = toggle.getAttribute("aria-checked") !== "true";
    toggle.disabled = true;
    void chrome.storage.local
      .set({ [KEEPALIVE_ENABLED_KEY]: enabled })
      .then(renderKeepalive, () => {
        toggle.disabled = false;
      });
  });
  input.addEventListener("change", () => {
    const interval = clampKeepaliveInterval(Number.parseInt(input.value, 10));
    input.value = String(interval);
    input.disabled = true;
    void chrome.storage.local
      .set({ [KEEPALIVE_INTERVAL_KEY]: interval })
      .then(renderKeepalive, () => {
        input.disabled = false;
      });
  });
}

type HandoffSurface = "in-window" | "work-window" | "tab-group";

async function currentHandoffSurface(): Promise<HandoffSurface> {
  try {
    const got = await chrome.storage.local.get([HANDOFF_SURFACE_KEY, WORK_WINDOW_KEY]);
    const v = got[HANDOFF_SURFACE_KEY];
    if (v === "in-window" || v === "work-window" || v === "tab-group") return v;
    return got[WORK_WINDOW_KEY] === false ? "in-window" : "work-window";
  } catch {
    return "work-window";
  }
}

const HANDOFF_SURFACE_BUTTONS: Record<HandoffSurface, string> = {
  "tab-group": "handoff-tab-group",
  "work-window": "handoff-work-window",
  "in-window": "handoff-in-window",
};

async function renderHandoffSurface(): Promise<void> {
  const current = await currentHandoffSurface();
  for (const [surface, id] of Object.entries(HANDOFF_SURFACE_BUTTONS)) {
    const btn = document.getElementById(id);
    if (!(btn instanceof HTMLButtonElement)) continue;
    btn.setAttribute("aria-pressed", surface === current ? "true" : "false");
  }
}

function wireHandoffSurface(): void {
  for (const [surface, id] of Object.entries(HANDOFF_SURFACE_BUTTONS) as [
    HandoffSurface,
    string,
  ][]) {
    const btn = document.getElementById(id);
    if (!(btn instanceof HTMLButtonElement)) continue;
    btn.addEventListener("click", () => {
      // Keep the legacy boolean in sync so an older bridge build still honors it.
      void chrome.storage.local
        .set({ [HANDOFF_SURFACE_KEY]: surface, [WORK_WINDOW_KEY]: surface !== "in-window" })
        .then(renderHandoffSurface);
    });
  }
}

async function renderDaemonFooter(): Promise<void> {
  const footer = document.getElementById("daemon-footer");
  if (!footer) return;

  const extensionVersion = chrome.runtime.getManifest().version;
  let daemon: Pick<StoreShape, "connectionStatus" | "daemonVersion"> = {
    connectionStatus: "disconnected",
    daemonVersion: null,
  };
  try {
    // Share the popup's persisted bridge-state read rather than opening a
    // second native connection from this page.
    daemon = await chromeBackend(chrome.storage).load();
  } catch {
    // A storage failure is indistinguishable from an unavailable daemon here.
  }

  const prefix = `papio extension v${extensionVersion} · `;
  switch (daemon.connectionStatus ?? "disconnected") {
    case "connected":
      renderPapio(
        footer,
        typeof daemon.daemonVersion === "string" && daemon.daemonVersion.length > 0
          ? `${prefix}daemon v${daemon.daemonVersion} (connected)`
          : `${prefix}daemon connected (version unknown)`,
      );
      return;
    case "daemon_outdated":
      renderPapio(
        footer,
        typeof daemon.daemonVersion === "string" && daemon.daemonVersion.length > 0
          ? `${prefix}daemon v${daemon.daemonVersion} (outdated)`
          : `${prefix}daemon connected (outdated)`,
      );
      return;
    case "extension_outdated":
      renderPapio(footer, `${prefix}daemon connected (extension outdated)`);
      return;
    case "disconnected":
      renderPapio(footer, `${prefix}daemon not connected`);
      return;
  }
}

// Keep static and dynamic wildcard coverage on identical match semantics so
// the options page cannot disagree about whether an origin is already usable.
function coveredByPatterns(origin: string, patterns: readonly string[]): boolean {
  let host: string;
  try {
    host = new URL(origin).host;
  } catch {
    return false;
  }
  return patterns.some((pattern) => {
    if (pattern === ALL_SITES_ORIGIN) return true;
    const m = /^https:\/\/(\*\.)?([^/*]+)\/\*$/.exec(pattern);
    if (!m) return false;
    return m[1] ? host === m[2] || host.endsWith(`.${m[2]}`) : host === m[2];
  });
}

function coveredByManifest(origin: string): boolean {
  const patterns = chrome.runtime.getManifest().host_permissions ?? [];
  return coveredByPatterns(origin, patterns);
}

function coveredByAllSitesGrant(origin: string, allSitesGranted: boolean): boolean {
  if (!allSitesGranted) return false;
  return coveredByPatterns(origin, ALL_SITES_PATTERNS);
}

function setProviderPermissionNotice(message: string | null): void {
  const notice = document.getElementById("provider-permission-message");
  if (!(notice instanceof HTMLElement)) return;
  notice.hidden = message === null;
  notice.textContent = message ?? "";
}

// Render the user's configured resolver origins (from the daemon, via hello_ack)
// that aren't already covered by a static wildcard. Each is grantable exactly
// like a provider source, so institution identity stays in config, not code.
async function renderConfiguredResolvers(permissionSnapshot: PermissionSnapshot): Promise<void> {
  const list = document.getElementById("configured-resolvers");
  if (!(list instanceof HTMLUListElement)) return;
  const store: StoreShape = await chromeBackend(chrome.storage).load();
  const custom = (store.resolverOrigins ?? []).filter((origin) => !coveredByManifest(origin));
  const section = document.getElementById("configured-resolvers-section");
  if (section instanceof HTMLElement) section.hidden = custom.length === 0;
  render(
    list,
    custom.map((origin) => ({ label: origin.replace(/^https:\/\//, ""), origin: `${origin}/*` })),
    permissionSnapshot,
  );
}

const allSitesList = document.getElementById("all-sites-access");
const sourceList = document.getElementById("sources");
const libraryResolverList = document.getElementById("library-resolvers");

async function renderPermissionLists(reportAllSitesStillActive = false): Promise<void> {
  let origins: readonly string[];
  try {
    const permissions = await chrome.permissions.getAll();
    origins = permissions.origins ?? [];
  } catch {
    if (reportAllSitesStillActive) {
      setProviderPermissionNotice(
        "papio could not confirm that all-sites access was revoked. Turn it off with the All-sites access control above.",
      );
    }
    return;
  }

  const permissionSnapshot: PermissionSnapshot = {
    origins,
    allSitesGranted: origins.includes(ALL_SITES_ORIGIN),
  };
  if (allSitesList instanceof HTMLUListElement) {
    render(allSitesList, ALL_SITES_SOURCES, permissionSnapshot);
  }
  if (sourceList instanceof HTMLUListElement) {
    render(sourceList, PROVIDER_SOURCES, permissionSnapshot);
  }
  if (libraryResolverList instanceof HTMLUListElement) {
    render(libraryResolverList, LIBRARY_RESOLVERS, permissionSnapshot);
  }
  void renderConfiguredResolvers(permissionSnapshot);
  setProviderPermissionNotice(
    reportAllSitesStillActive && permissionSnapshot.allSitesGranted
      ? "All-sites access is still active. Turn it off with the All-sites access control above to manage providers separately."
      : null,
  );
}

if (sourceList instanceof HTMLUListElement) {
  wireProviderBulk(PROVIDER_SOURCES);
}
void renderPermissionLists();
wireTermsConsent();
void renderTermsConsent();
wirePageCaptureConsent();
void renderPageCaptureConsent();
wireHandoffSurface();
void renderHandoffSurface();
void renderDaemonFooter();
wireKeepalive();
void renderKeepalive();
