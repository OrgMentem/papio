// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Options page: source and library-resolver host permission grant/revoke. The
// button click is the user gesture chrome.permissions.request requires.
// Selecting the daemon's `delegated` access mode never grants a Chrome permission
// by itself — that only happens here, explicitly.

import { chromeBackend, type StoreShape } from "./state";
import { renderPapio } from "./dom";
import { adapters, type AdapterSpec } from "./adapters/types";

export interface Source {
  label: string;
  origin: string;
}

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

function render(list: HTMLUListElement, sources: Source[]): void {
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

    const controls = document.createElement("div");
    const status = document.createElement("span");
    status.className = "status";
    const button = document.createElement("button");
    button.disabled = true;
    controls.append(status, document.createTextNode(" "), button);

    item.append(meta, controls);
    list.append(item);

    let granted = false;
    const paint = (next: boolean): void => {
      granted = next;
      status.classList.toggle("granted", granted);
      status.classList.toggle("revoked", !granted);
      status.textContent = granted ? "granted" : "not granted";
      button.textContent = granted ? "Revoke" : "Grant";
      button.disabled = false;
    };

    void chrome.permissions.contains({ origins: [source.origin] }).then(paint);

    button.addEventListener("click", () => {
      // permissions.request must be invoked directly in the trusted click
      // callback. Awaiting contains() first loses Chrome's user gesture.
      const wasGranted = granted;
      button.disabled = true;
      const change = wasGranted
        ? chrome.permissions.remove({ origins: [source.origin] })
        : chrome.permissions.request({ origins: [source.origin] });
      void change.then(
        (ok) => paint(ok ? !wasGranted : wasGranted),
        () => paint(wasGranted),
      );
    });
  }
}

// Bulk grant/revoke for every provider source. permissions.request accepts all
// origins in one call, so a single click yields one Firefox doorhanger listing
// them all — the gesture must reach request() with no await before it.
function wireProviderBulk(list: HTMLUListElement, sources: Source[]): void {
  const origins = sources.map((source) => source.origin);
  const grantAll = document.getElementById("grant-all");
  const revokeAll = document.getElementById("revoke-all");
  if (grantAll instanceof HTMLButtonElement) {
    grantAll.addEventListener("click", () => {
      void chrome.permissions.request({ origins }).then(
        () => render(list, sources),
        () => {},
      );
    });
  }
  if (revokeAll instanceof HTMLButtonElement) {
    revokeAll.addEventListener("click", () => {
      void chrome.permissions.remove({ origins }).then(
        () => render(list, sources),
        () => {},
      );
    });
  }
}

const TERMS_CONSENT_KEY = "papio_terms_consent_v1";

async function renderTermsConsent(): Promise<void> {
  const statusEl = document.getElementById("terms-consent-status");
  if (!statusEl) return;
  let consent: string | undefined;
  try {
    const got = await chrome.storage.local.get(TERMS_CONSENT_KEY);
    const v = got[TERMS_CONSENT_KEY];
    consent = v === "accept" || v === "manual" ? v : undefined;
  } catch {
    consent = undefined;
  }
  const text =
    consent === "accept"
      ? "On — papio accepts publisher terms automatically"
      : consent === "manual"
        ? "Off — you accept terms yourself"
        : "Ask on first download";
  renderPapio(statusEl, text);
}

function wireTermsConsent(): void {
  const on = document.getElementById("terms-consent-on");
  const off = document.getElementById("terms-consent-off");
  if (on instanceof HTMLButtonElement) {
    on.addEventListener("click", () => {
      void chrome.storage.local.set({ [TERMS_CONSENT_KEY]: "accept" }).then(renderTermsConsent);
    });
  }
  if (off instanceof HTMLButtonElement) {
    off.addEventListener("click", () => {
      void chrome.storage.local.set({ [TERMS_CONSENT_KEY]: "manual" }).then(renderTermsConsent);
    });
  }
}

const WORK_WINDOW_KEY = "papio_work_window_v1";

async function renderWorkWindow(): Promise<void> {
  const statusEl = document.getElementById("work-window-status");
  if (!statusEl) return;
  let enabled = true;
  try {
    const got = await chrome.storage.local.get(WORK_WINDOW_KEY);
    enabled = got[WORK_WINDOW_KEY] !== false;
  } catch {
    enabled = true;
  }
  const text = enabled
    ? "On — papio tabs stay in a minimized background window"
    : "Off — papio tabs open in your current window";
  renderPapio(statusEl, text);
}

function wireWorkWindow(): void {
  const on = document.getElementById("work-window-on");
  const off = document.getElementById("work-window-off");
  if (on instanceof HTMLButtonElement) {
    on.addEventListener("click", () => {
      void chrome.storage.local.set({ [WORK_WINDOW_KEY]: true }).then(renderWorkWindow);
    });
  }
  if (off instanceof HTMLButtonElement) {
    off.addEventListener("click", () => {
      void chrome.storage.local.set({ [WORK_WINDOW_KEY]: false }).then(renderWorkWindow);
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

// Match an origin against the static host_permissions wildcards so the
// configured-resolver section lists only custom domains that still need a grant;
// cloud Ex Libris resolvers are already covered by those wildcards.
function coveredByManifest(origin: string): boolean {
  let host: string;
  try {
    host = new URL(origin).host;
  } catch {
    return false;
  }
  return (chrome.runtime.getManifest().host_permissions ?? []).some((pattern: string) => {
    const m = /^https:\/\/(\*\.)?([^/*]+)\/\*$/.exec(pattern);
    if (!m) return false;
    return m[1] ? host === m[2] || host.endsWith(`.${m[2]}`) : host === m[2];
  });
}

// Render the user's configured resolver origins (from the daemon, via hello_ack)
// that aren't already covered by a static wildcard. Each is grantable exactly
// like a provider source, so institution identity stays in config, not code.
async function renderConfiguredResolvers(): Promise<void> {
  const list = document.getElementById("configured-resolvers");
  if (!(list instanceof HTMLUListElement)) return;
  const store: StoreShape = await chromeBackend(chrome.storage).load();
  const custom = (store.resolverOrigins ?? []).filter((origin) => !coveredByManifest(origin));
  const section = document.getElementById("configured-resolvers-section");
  if (section instanceof HTMLElement) section.hidden = custom.length === 0;
  render(
    list,
    custom.map((origin) => ({ label: origin.replace(/^https:\/\//, ""), origin: `${origin}/*` })),
  );
}

const sourceList = document.getElementById("sources");
if (sourceList instanceof HTMLUListElement) {
  render(sourceList, PROVIDER_SOURCES);
  wireProviderBulk(sourceList, PROVIDER_SOURCES);
}
const libraryResolverList = document.getElementById("library-resolvers");
if (libraryResolverList instanceof HTMLUListElement) {
  render(libraryResolverList, LIBRARY_RESOLVERS);
}
void renderConfiguredResolvers();
wireTermsConsent();
void renderTermsConsent();
wireWorkWindow();
void renderWorkWindow();
void renderDaemonFooter();
