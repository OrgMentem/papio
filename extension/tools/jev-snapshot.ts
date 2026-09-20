// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development experiment only. Captured DOM is not proof of live visibility.
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { Window } from "happy-dom";

const compact = (value: string): string => value.replace(/\s+/g, " ").trim();
const redact = (value: string): string => compact(value)
  .replace(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi, "[email]")
  .replace(/https?:\/\/[^\s<>"']+/g, (url) => {
    try { const parsed = new URL(url); return parsed.origin + parsed.pathname; } catch { return "[url]"; }
  });

export function captureSnapshot(html: string, goal: string, origin: string) {
  const url = new URL(origin);
  if (url.protocol !== "https:" || url.username || url.password) throw new Error("Expected a public HTTPS origin");
  const window = new Window({ url: url.origin + url.pathname, settings: {
    disableJavaScriptFileLoading: true, disableJavaScriptEvaluation: true,
    disableCSSFileLoading: true, disableIframePageLoading: true,
  } });
  window.document.write(html);
  const doc = window.document;
  for (const node of doc.querySelectorAll("script,style,iframe,input,textarea,select")) node.remove();
  const root = doc.body;
  const nodes = Array.from(root.querySelectorAll("*"))
    .filter(node => node.matches("a,button,[role='button'],[role='link'],.button,.btn") || node.tagName.toLowerCase().endsWith("-button"))
    .filter(node => !node.closest("[hidden],[aria-hidden='true']") &&
      !/display\s*:\s*none|visibility\s*:\s*hidden/i.test(node.getAttribute("style") ?? ""))
    .filter(node => !(node.matches(".button,.btn") && node.querySelector("a,button,[role='button']")));
  const found = nodes.flatMap((node, index) => {
    const label = redact(node.getAttribute("aria-label") ?? node.getAttribute("title") ?? node.textContent ?? "").slice(0, 180);
    return label === "" ? [] : [{ id: String(index + 1), role: node.tagName.toLowerCase() === "a" ? "link" : "button", label,
      disabled: node.hasAttribute("disabled") || node.getAttribute("aria-disabled") === "true" }];
  });
  // Offline screening only: captures include offscreen references and menus.
  // Restrict to task-related affordances and report the reduction explicitly.
  // Live native observations are collected independently, without this filter.
  const relevant = found.filter(control => /pdf|download|full.?text|article|access|log.?in|sign.?in|institution|accept|agree|terms|purchase|buy|subscribe|supplement|preview/i.test(control.label));
  const controls = relevant.slice(0, 80);
  return {
    goal,
    page: { url: url.origin + url.pathname, title: redact(doc.title || doc.querySelector("meta[name='citation_title']")?.getAttribute("content") || ""),
      text: redact(root.textContent ?? "").slice(0, 3000) },
    controls,
    provenance: { kind: "sanitized-capture", source_sha256: createHash("sha256").update(html).digest("hex"), visibility_verified: false,
      discovered_controls: found.length, omitted_controls: found.length - controls.length, truncated_relevant_controls: relevant.length - controls.length },
  };
}

if (import.meta.main) {
  const [input, output, goal] = process.argv.slice(2);
  if (!input || !output || !goal) throw new Error("usage: jev-snapshot.ts <capture.html> <snapshot.json> <goal>");
  const html = readFileSync(input, "utf8");
  const origin = /^<!-- papio-fixture[^\n]* origin="([^"]+)"/.exec(html)?.[1];
  if (!origin) throw new Error("Canonical sanitized capture header required");
  writeFileSync(output, JSON.stringify(captureSnapshot(html, goal, origin), null, 2) + "\n", { mode: 0o600, flag: "wx" });
}
