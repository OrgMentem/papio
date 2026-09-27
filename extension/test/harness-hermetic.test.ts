// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// I16: every happy-dom Window in the suite must stay hermetic. happy-dom
// 20.10.6 loads stylesheets, scripts, and iframes over the real network by
// default, and `<link rel=preload>` bypasses the loader flags entirely. The
// suite preload (test/preload-hermetic.ts, via extension/bunfig.toml) turns
// the loaders off in the shared defaults and refuses whatever still reaches
// the fetch layer. These tests parse a hostile document against a live local
// server and require zero hits — once through parseHTML, once through a raw
// `new Window` with default settings (the ~40 direct constructions in the
// suite) — with the preload attempt dying at the refusing interceptor.
import { expect, test } from "bun:test";

import { Window } from "happy-dom";

import { drainRefusedRequests, parseHTML } from "./harness";

const HOSTILE_DOC = (origin: string): string =>
  "<html><head>" +
  `<link rel="stylesheet" href="${origin}/site.css">` +
  `<link rel="preload" href="${origin}/early.json" as="fetch" crossorigin>` +
  `<script src="${origin}/late.js"></script>` +
  "</head><body><h1>Article</h1>" +
  `<iframe src="${origin}/frame.html"></iframe>` +
  "</body></html>";

/** A live local server that counts every request it receives. */
function countingServer(): { origin: string; hits: () => number; stop: () => void } {
  let hits = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch() {
      hits += 1;
      return new Response("must never be fetched", { status: 200 });
    },
  });
  return {
    origin: `http://127.0.0.1:${server.port}`,
    hits: () => hits,
    stop: () => server.stop(),
  };
}

test("parseHTML makes zero network requests, refusing preloads at the interceptor", async () => {
  const server = countingServer();
  try {
    drainRefusedRequests();
    const doc = parseHTML(HOSTILE_DOC(server.origin), `${server.origin}/article`);
    expect(doc.querySelector("h1")?.textContent).toBe("Article");
    // Real delay on purpose: the test observes the ABSENCE of async network
    // activity, so there is no promise or event to await instead. Any load
    // happy-dom still schedules reaches a local server within milliseconds.
    await Bun.sleep(500);
    expect(server.hits()).toBe(0);
    // The preload bypasses happy-dom's loader flags, so it must reach the
    // refusing interceptor rather than the network.
    expect(drainRefusedRequests()).toEqual([`${server.origin}/early.json`]);
  } finally {
    server.stop();
  }
});

// Real delays below are deliberate: each test observes the ABSENCE of async
// network activity, so there is no promise or event to await instead. Any
// load happy-dom still schedules reaches a local server within milliseconds.
test("a raw new Window with default settings makes zero network requests", async () => {
  const server = countingServer();
  try {
    drainRefusedRequests();
    // Default settings on purpose: this is what the ~40 direct `new Window`
    // constructions in the suite get. The suite preload must keep them
    // hermetic too. The URL looks like a real publisher origin, as committed
    // fixtures do.
    const window = new Window({ url: `${server.origin}/article` });
    try {
      window.document.write(HOSTILE_DOC(server.origin));
      expect(window.document.querySelector("h1")?.textContent).toBe("Article");
      await Bun.sleep(500);
      expect(server.hits()).toBe(0);
      expect(drainRefusedRequests()).toEqual([`${server.origin}/early.json`]);
    } finally {
      await window.happyDOM.abort();
      await window.happyDOM.close();
    }
  } finally {
    server.stop();
  }
});
