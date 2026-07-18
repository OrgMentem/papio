// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Options page: source and library-resolver host permission grant/revoke. The
// button click is the user gesture chrome.permissions.request requires.
// Selecting the daemon's `maximal` access mode never grants a Chrome permission
// by itself — that only happens here, explicitly.

interface Source {
  label: string;
  origin: string;
}

// Must mirror manifest.json optional_host_permissions exactly.
const SOURCES: Source[] = [
  { label: "JSTOR", origin: "https://www.jstor.org/*" },
  { label: "ProQuest", origin: "https://www.proquest.com/*" },
  { label: "EBSCO", origin: "https://research.ebsco.com/*" },
  { label: "Springer Nature Link", origin: "https://link.springer.com/*" },
  { label: "ScienceDirect (Elsevier)", origin: "https://www.sciencedirect.com/*" },
  { label: "ACM Digital Library", origin: "https://dl.acm.org/*" },
  { label: "Wiley Online Library", origin: "https://onlinelibrary.wiley.com/*" },
  { label: "Taylor & Francis Online", origin: "https://www.tandfonline.com/*" },
  { label: "SAGE Journals", origin: "https://journals.sagepub.com/*" },
  { label: "APA PsycNet", origin: "https://psycnet.apa.org/*" },
];

// Must mirror manifest.json host_permissions exactly.
const LIBRARY_RESOLVERS: Source[] = [
  { label: "Ex Libris Alma", origin: "https://*.alma.exlibrisgroup.com/*" },
  { label: "Ex Libris Primo", origin: "https://*.primo.exlibrisgroup.com/*" },
  { label: "Example Institute OneSearch", origin: "https://onesearch.library.example-institute.edu/*" },
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
  statusEl.textContent =
    consent === "accept"
      ? "On — papio accepts publisher terms automatically"
      : consent === "manual"
        ? "Off — you accept terms yourself"
        : "Ask on first download";
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
  statusEl.textContent = enabled
    ? "On — papio tabs stay in a minimized background window"
    : "Off — papio tabs open in your current window";
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

const sourceList = document.getElementById("sources");
if (sourceList instanceof HTMLUListElement) {
  render(sourceList, SOURCES);
  wireProviderBulk(sourceList, SOURCES);
}
const libraryResolverList = document.getElementById("library-resolvers");
if (libraryResolverList instanceof HTMLUListElement) {
  render(libraryResolverList, LIBRARY_RESOLVERS);
}
wireTermsConsent();
void renderTermsConsent();
wireWorkWindow();
void renderWorkWindow();
