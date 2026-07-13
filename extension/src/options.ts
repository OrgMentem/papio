// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Options page: per-source optional-host permission grant/revoke. The button
// click is the user gesture chrome.permissions.request requires. Selecting the
// daemon's `maximal` access mode never grants a Chrome permission by itself —
// that only happens here, explicitly.

interface Source {
  label: string;
  origin: string;
}

// Must mirror manifest.json optional_host_permissions exactly.
const SOURCES: Source[] = [
  { label: "JSTOR", origin: "https://www.jstor.org/*" },
  { label: "ProQuest", origin: "https://www.proquest.com/*" },
  { label: "EBSCO", origin: "https://research.ebsco.com/*" },
];

function render(list: HTMLUListElement): void {
  list.replaceChildren();
  for (const source of SOURCES) {
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
    controls.append(status, document.createTextNode(" "), button);

    item.append(meta, controls);
    list.append(item);

    const paint = (granted: boolean): void => {
      status.classList.toggle("granted", granted);
      status.classList.toggle("revoked", !granted);
      status.textContent = granted ? "granted" : "not granted";
      button.textContent = granted ? "Revoke" : "Grant";
    };

    void chrome.permissions.contains({ origins: [source.origin] }).then(paint);

    button.addEventListener("click", () => {
      void chrome.permissions.contains({ origins: [source.origin] }).then(async (granted) => {
        const ok = granted
          ? await chrome.permissions.remove({ origins: [source.origin] })
          : await chrome.permissions.request({ origins: [source.origin] });
        if (ok) paint(!granted);
      });
    });
  }
}

const list = document.getElementById("sources");
if (list instanceof HTMLUListElement) {
  render(list);
}
