// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Hermetic happy-dom settings: the one refusing fetch interceptor and the
// loader flags, shared by the test preload (test/preload-hermetic.ts) and by
// `parseHTML` in test/harness.ts.
//
// happy-dom 20.10.6 loads stylesheets, scripts and iframes over the real
// network by default, and `<link rel=preload>` reaches its fetch layer even
// with the loader flags off. The flags stop the loaders; the interceptor
// refuses whatever still reaches fetch and records the URL.
//
// `parseHTML` passes these settings explicitly because the adapter tools
// (tools/adapter-try.ts, tools/adapter-repair.ts) import the harness under
// `bun run`, where the test preload does not run.
//
// Do NOT import bun:test here: the preload and the tools run outside the
// test runner.
import type { IFetchInterceptor } from "happy-dom";
import DefaultBrowserSettings from "happy-dom/lib/browser/DefaultBrowserSettings.js";

const refused: string[] = [];

/** Take and clear the log of URLs the interceptor refused. */
export function drainRefusedRequests(): string[] {
  return refused.splice(0);
}

const refusingInterceptor: IFetchInterceptor = {
  beforeAsyncRequest: async ({ request, window }) => {
    refused.push(request.url);
    return new window.Response(null, { status: 599, statusText: "refused: hermetic happy-dom" });
  },
  beforeSyncRequest: ({ request, window }) => {
    refused.push(request.url);
    return {
      status: 599,
      statusText: "refused: hermetic happy-dom",
      ok: false,
      url: request.url,
      redirected: false,
      headers: new window.Headers(),
      body: Buffer.alloc(0),
    };
  },
};

const hermeticFlags = {
  disableJavaScriptFileLoading: true,
  disableCSSFileLoading: true,
  disableIframePageLoading: true,
  handleDisabledFileLoadingAsSuccess: true,
};

/** Window settings that keep a document off the network. */
export const hermeticWindowSettings = { ...hermeticFlags, fetch: { interceptor: refusingInterceptor } };

/**
 * Make the hermetic settings happy-dom's defaults. `BrowserSettingsFactory`
 * spreads `DefaultBrowserSettings` under each window's own settings and merges
 * `fetch` one level deep, so every later `new Window(...)` that does not
 * override a key inherits it. Call before any window exists.
 */
export function installHermeticDefaults(): void {
  Object.assign(DefaultBrowserSettings, hermeticFlags);
  DefaultBrowserSettings.fetch.interceptor = refusingInterceptor;
}
