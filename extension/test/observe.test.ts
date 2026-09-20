// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Automatic observed captures exercise the narrow browser seams. They never
// write to Downloads: the only observable output is a compressed native frame.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { gunzipSync } from "node:zlib";


import {
  observeUnknown,
  type ObservationCaptureContext,
  type ObservationCaptureDiagnostic,
  type ObserveChromeApi,
} from "../src/observe";
import type { PageCapture } from "../src/capture";
import type { PageCapturePayload } from "../src/protocol";
import type { ActiveJob } from "../src/state";
import { adapters } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { fixturePath, parseHTML } from "./harness";

const RATE_KEY = "papio_observed_capture_rate_v1";
const CLEAN_HTML = `<html><body><main class="article">Known structure</main><script>secret</script></body></html>`;

function jobFor(host: string, tab = 17): ActiveJob {
  return {
    job_id: `job_capture_${tab}`,
    tab_id: tab,
    offered_at: 0,
    expires_at: 2_000_000_000_000,
    status: "awaiting_download",
    provider_hosts: [host],
  };
}

function pageFor(host: string): PageCapture {
  return { html: CLEAN_HTML, origin: `https://${host}`, path: "/article/123" };
}

function fakeChrome(initialPage: PageCapture) {
  let page = initialPage;
  const stored: Record<string, unknown> = {};
  const injections: Array<{ tabId: number }> = [];
  const sent: Array<{ payload: PageCapturePayload; jobID: string }> = [];
  const api: ObserveChromeApi = {
    scripting: {
      executeScript: async ({ target }) => {
        injections.push(target);
        return [{ result: page }];
      },
    },
    storage: {
      local: {
        get: async (key) => ({ [key]: stored[key] }),
        set: async (items) => {
          Object.assign(stored, items);
        },
      },
    },
    sendPageCapture: async (payload, jobID) => {
      sent.push({ payload, jobID });
      return true;
    },
  };
  return {
    api,
    sent,
    injections,
    stored,
    setPage(next: PageCapture) {
      page = next;
    },
  };
}

function fixedNow(iso: string): () => Date {
  return () => new Date(iso);
}

function verifiedHosts(...hosts: string[]): ObservationCaptureContext {
  return { verifiedHosts: hosts };
}

function gunzipBase64(body: string): string {
  const compressed = Uint8Array.from(atob(body), (character) => character.charCodeAt(0));
  return new TextDecoder().decode(gunzipSync(compressed));
}

test("unknown tracked provider page emits one sanitized observed page_capture frame", async () => {
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));
  const capturedAt = "2026-07-15T10:11:12.000Z";

  const captured = await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host), fixedNow(capturedAt));

  expect(fake.injections).toEqual([{ tabId: 17 }]);
  expect(fake.sent).toHaveLength(1);
  expect(captured).toBe(true);
  const emitted = fake.sent[0];
  expect(emitted?.jobID).toBe("job_capture_17");
  expect(emitted?.payload).toMatchObject({
    host,
    scenario: "observed",
    encoding: "gzip+base64",
    bytes: new TextEncoder().encode(gunzipBase64(emitted?.payload.body ?? "")).byteLength,
  });
  const sanitized = gunzipBase64(emitted?.payload.body ?? "");
  expect(sanitized).toContain('scenario="observed"');
  expect(sanitized).toContain("<script></script>");
  const shapeKey = "sciencedirect-com|-@-";
  const storedState = fake.stored[RATE_KEY] as {
    total: number[];
    byShape: Record<string, number[]>;
    digests: Record<string, string[]>;
  };
  expect(storedState.total).toEqual([new Date(capturedAt).getTime()]);
  expect(storedState.byShape).toEqual({ [shapeKey]: [new Date(capturedAt).getTime()] });
  // The digest is a dedupe key, not asserted for a specific value here — just
  // that a single sanitized-page fingerprint was recorded for the shape.
  expect(storedState.digests[shapeKey]).toHaveLength(1);
  expect(typeof storedState.digests[shapeKey]?.[0]).toBe("string");
});

test("ScienceDirect unknown emits a sanitized drift frame with its actual adapter identity", async () => {
  const spec = adapters.find((adapter) => adapter.id === "sciencedirect");
  if (!spec) throw new Error("sciencedirect adapter missing");
  const host = "www.sciencedirect.com";
  const path = "/science/article/pii/S2666557326000194";
  // This committed synthetic refusal fixture is a regression input, not live
  // independent evidence. Use the production planner before capturing it.
  const html = readFileSync(fixturePath("sciencedirect", "drift"), "utf8")
    .replace("</body>", "<script>capture-secret</script></body>");
  const planned = planExecution(parseHTML(html, `https://${host}${path}`), spec, {}, {});
  expect(planned.verdict.kind).toBe("unknown");

  const fake = fakeChrome({ html, origin: `https://${host}`, path });
  const captured = await observeUnknown(
    fake.api,
    jobFor("resolver.example.edu"),
    host,
    { verifiedHosts: spec.hosts, adapterID: spec.id, adapterVersion: spec.version },
    fixedNow("2026-07-15T10:11:12.000Z"),
  );

  expect(captured).toBe(true);
  expect(fake.sent).toHaveLength(1);
  expect(fake.sent[0]?.jobID).toBe("job_capture_17");
  expect(fake.sent[0]?.payload).toMatchObject({
    host,
    scenario: "drift",
    adapter_id: spec.id,
    adapter_version: spec.version,
  });
  const sanitized = gunzipBase64(fake.sent[0]!.payload.body);
  expect(sanitized.split("\n")[0]).toBe(
    `<!-- papio-fixture provider="sciencedirect" scenario="drift" origin="https://${host}${path}" captured="2026-07-15T10:11:12.000Z" -->`,
  );
  expect(sanitized).toContain('class="PDFViewer"');
  expect(sanitized).toContain("<script></script>");
  expect(sanitized).not.toContain("capture-secret");
});

test("a registry-only adapter miss produces a canonical drift fixture", async () => {
  const offerHost = "resolver.example.edu";
  const adapterHost = "journals.sagepub.com";
  const fake = fakeChrome(pageFor(adapterHost));

  await observeUnknown(
    fake.api,
    jobFor(offerHost),
    adapterHost,
    {
      verifiedHosts: [offerHost, "sagepub.com"],
      adapterID: "sage",
      adapterVersion: "1.2.3",
    },
    fixedNow("2026-07-15T10:11:12.000Z"),
  );

  expect(fake.injections).toEqual([{ tabId: 17 }]);
  expect(fake.sent).toHaveLength(1);
  expect(fake.sent[0]?.payload).toMatchObject({
    host: adapterHost,
    scenario: "drift",
    adapter_id: "sage",
    adapter_version: "1.2.3",
  });
  const fixture = gunzipBase64(fake.sent[0]?.payload.body ?? "");
  expect(fixture.split("\n")[0]).toBe(
    '<!-- papio-fixture provider="sage" scenario="drift" origin="https://journals.sagepub.com/article/123" captured="2026-07-15T10:11:12.000Z" -->',
  );
});

test("persisted per-shape and daily observation quotas prevent later captures", async () => {
  const firstHost = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(firstHost));
  const firstTime = "2026-07-15T00:00:00.000Z";
  await observeUnknown(fake.api, jobFor(firstHost), firstHost, verifiedHosts(firstHost), fixedNow(firstTime));
  await observeUnknown(
    fake.api,
    jobFor(firstHost),
    firstHost,
    verifiedHosts(firstHost),
    fixedNow("2026-07-15T00:05:00.000Z"),
  );
  expect(fake.sent).toHaveLength(1);

  // Fill the raised daily budget (20) with distinct-shape hosts, all inside
  // one day, so the next capture is refused by the daily ceiling itself and
  // not by the (now per-shape, not per-host) hourly limit.
  const otherHosts = Array.from({ length: 19 }, (_, index) => `provider${index}.org`);
  for (let index = 0; index < otherHosts.length; index += 1) {
    const host = otherHosts[index]!;
    fake.setPage(pageFor(host));
    await observeUnknown(
      fake.api,
      jobFor(host, 20 + index),
      host,
      verifiedHosts(host),
      () => new Date(new Date(firstTime).getTime() + (10 + index * 5) * 60 * 1000),
    );
  }
  expect(fake.sent).toHaveLength(20);

  const overflowHost = "www.sagepub.com";
  fake.setPage(pageFor(overflowHost));
  await observeUnknown(
    fake.api,
    jobFor(overflowHost, 99),
    overflowHost,
    verifiedHosts(overflowHost),
    fixedNow("2026-07-15T02:00:00.000Z"),
  );
  expect(fake.sent).toHaveLength(20);
});

test("untracked and unverified pages are never injected or emitted", async () => {
  const providerHost = "www.jstor.org";
  const fake = fakeChrome(pageFor(providerHost));

  const untracked = await observeUnknown(
    fake.api,
    undefined,
    providerHost,
    verifiedHosts(providerHost),
    fixedNow("2026-07-15T10:00:00.000Z"),
  );
  const unverified = await observeUnknown(
    fake.api,
    jobFor(providerHost),
    "login.example.edu",
    verifiedHosts(providerHost),
    fixedNow("2026-07-15T10:01:00.000Z"),
  );

  expect(fake.injections).toHaveLength(0);
  expect(fake.sent).toHaveLength(0);
  expect(untracked).toBe(false);
  expect(unverified).toBe(false);
  expect(fake.stored[RATE_KEY]).toBeUndefined();
});

test("a residual leak refuses the observed bridge frame", async () => {
  // A valid long provider label reaches the fixture header unchanged. The
  // existing residualLeak guard detects it there and refuses emission.
  const host = "abcdefghijklmnopqrstuvwxyzabcdef.com";
  const fake = fakeChrome(pageFor(host));
  const warn = console.warn;
  console.warn = () => undefined;
  try {
    expect(
      await observeUnknown(
        fake.api,
        jobFor(host),
        host,
        verifiedHosts(host),
        fixedNow("2026-07-15T10:00:00.000Z"),
      ),
    ).toBe(false);
  } finally {
    console.warn = warn;
  }

  expect(fake.injections).toHaveLength(1);
  expect(fake.sent).toHaveLength(0);
});

test("different adapter versions on the same host both capture within the same hour", async () => {
  // Old code keyed the hourly limit on bare host, so a version bump on a
  // still-broken host would be silently throttled by the previous attempt's
  // slot. Shape-keying separates them.
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));

  const first = await observeUnknown(
    fake.api,
    jobFor(host),
    host,
    { verifiedHosts: [host], adapterID: "elsevier", adapterVersion: "1.0.0" },
    fixedNow("2026-07-15T10:00:00.000Z"),
  );
  const second = await observeUnknown(
    fake.api,
    jobFor(host),
    host,
    { verifiedHosts: [host], adapterID: "elsevier", adapterVersion: "1.0.1" },
    fixedNow("2026-07-15T10:05:00.000Z"),
  );

  expect(first).toBe(true);
  expect(second).toBe(true);
  expect(fake.sent).toHaveLength(2);
});

test("an identical sanitized page for the same shape is captured once and the repeat costs no daily budget", async () => {
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));
  const context = verifiedHosts(host);

  // Second attempt lands past the 1-hour per-shape window, so only the
  // digest dedupe — not the hourly shape limit — can be refusing it.
  const first = await observeUnknown(fake.api, jobFor(host), host, context, fixedNow("2026-07-15T10:00:00.000Z"));
  const second = await observeUnknown(fake.api, jobFor(host), host, context, fixedNow("2026-07-15T11:30:00.000Z"));

  expect(first).toBe(true);
  expect(second).toBe(false);
  // A duplicate shape still costs the executeScript injection: the digest is
  // only knowable after the page is captured and sanitized.
  expect(fake.injections).toHaveLength(2);
  expect(fake.sent).toHaveLength(1);
  const stored = fake.stored[RATE_KEY] as { total: number[] };
  expect(stored.total).toHaveLength(1);
});

test("a genuinely different page for the same shape is still refused by the hourly shape limit", async () => {
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));
  const context = verifiedHosts(host);

  const first = await observeUnknown(fake.api, jobFor(host), host, context, fixedNow("2026-07-15T10:00:00.000Z"));
  fake.setPage({
    html: `<html><body><main class="article">Different structure now</main><script>secret</script></body></html>`,
    origin: `https://${host}`,
    path: "/article/456",
  });
  const second = await observeUnknown(fake.api, jobFor(host), host, context, fixedNow("2026-07-15T10:30:00.000Z"));

  expect(first).toBe(true);
  expect(second).toBe(false);
  // Refused before injection: the hourly shape limit, not digest dedupe, is
  // what is blocking this one.
  expect(fake.injections).toHaveLength(1);
  expect(fake.sent).toHaveLength(1);
});

test("legacy byHost persisted state loads as empty instead of throwing", async () => {
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));
  fake.stored[RATE_KEY] = {
    total: [new Date("2026-07-15T09:00:00.000Z").getTime()],
    byHost: { "sciencedirect-com": [new Date("2026-07-15T09:00:00.000Z").getTime()] },
  };

  const captured = await observeUnknown(
    fake.api,
    jobFor(host),
    host,
    verifiedHosts(host),
    fixedNow("2026-07-15T09:05:00.000Z"),
  );

  expect(captured).toBe(true);
  expect(fake.sent).toHaveLength(1);
});

test("capture diagnostics distinguish sent, hourly quota and duplicate without changing reservations", async () => {
  const host = "ebooks.iospress.nl";
  const fake = fakeChrome(pageFor(host));
  const context = { verifiedHosts: [host], adapterID: "iospress", adapterVersion: "0.1.0" };
  const diagnostics: ObservationCaptureDiagnostic[] = [];
  for (const time of ["10:00", "10:05", "11:30"]) {
    await observeUnknown(fake.api, jobFor(host), host, context,
      fixedNow(`2026-09-20T${time}:00.000Z`), (diagnostic) => diagnostics.push(diagnostic));
  }
  expect(diagnostics).toEqual([
    { reason: "sent" },
    { reason: "shape_limit", retryAfterMs: 55 * 60 * 1000 },
    { reason: "duplicate" },
  ]);
  expect(fake.sent).toHaveLength(1);
  expect((fake.stored[RATE_KEY] as { total: number[] }).total).toHaveLength(1);
});

test("daily diagnostic waits until enough unsorted reservations expire", async () => {
  const host = "example.org";
  const fake = fakeChrome(pageFor(host));
  const now = Date.parse("2026-09-20T10:00:00Z");
  // Twenty-one reservations: expiring just the oldest would still leave 20.
  fake.stored[RATE_KEY] = {
    total: Array.from({ length: 21 }, (_, index) => now - (index + 1) * 60_000),
    byShape: {}, digests: {},
  };
  const diagnostics: ObservationCaptureDiagnostic[] = [];
  expect(await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host),
    () => new Date(now), (diagnostic) => diagnostics.push(diagnostic))).toBe(false);
  expect(diagnostics).toEqual([{ reason: "daily_limit", retryAfterMs: 24 * 60 * 60_000 - 20 * 60_000 }]);
  expect(fake.injections).toHaveLength(0);
});

test("unexpected capture failure remains diagnosable and does not block the next observation", async () => {
  const host = "example.org";
  const fake = fakeChrome(pageFor(host));
  const diagnostics: ObservationCaptureDiagnostic[] = [];
  expect(await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host),
    () => { throw new Error("private-clock-error"); }, (diagnostic) => diagnostics.push(diagnostic))).toBe(false);
  expect(diagnostics).toEqual([{ reason: "capture_failed" }]);
  expect(await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host),
    fixedNow("2026-09-20T10:00:00Z"))).toBe(true);
});

for (const reason of ["storage_unavailable", "injection_failed", "origin_changed", "invalid_page", "sanitizer_refused", "encoding_refused", "transport_unavailable"] as const) {
  test(`capture diagnostic reports ${reason} without leaking page or exception content`, async () => {
    const host = reason === "sanitizer_refused" ? "abcdefghijklmnopqrstuvwxyzabcdef.com" : "example.org";
    const fake = fakeChrome(pageFor(host));
    if (reason === "storage_unavailable") fake.api.storage.local.get = async () => { throw new Error("private-storage-error"); };
    if (reason === "injection_failed") fake.api.scripting.executeScript = async () => { throw new Error("private-page-error"); };
    if (reason === "origin_changed") fake.setPage(pageFor("private-other.example"));
    if (reason === "invalid_page") fake.api.scripting.executeScript = async () => [];
    if (reason === "encoding_refused") fake.setPage({ ...pageFor(host), html: `<main>${"ordinary text ".repeat(180_000)}</main>` });
    if (reason === "transport_unavailable") fake.api.sendPageCapture = async () => false;
    const diagnostics: ObservationCaptureDiagnostic[] = [];
    expect(await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host),
      fixedNow("2026-09-20T10:00:00Z"), (diagnostic) => diagnostics.push(diagnostic))).toBe(false);
    expect(diagnostics).toEqual([{ reason }]);
    expect(fake.sent).toHaveLength(0);
    if (reason === "transport_unavailable") {
      // Sending failed after reservation; a second attempt must still meet the quota.
      await observeUnknown(fake.api, jobFor(host), host, verifiedHosts(host),
        fixedNow("2026-09-20T10:01:00Z"), (diagnostic) => diagnostics.push(diagnostic));
      expect(diagnostics[1]).toEqual({ reason: "shape_limit", retryAfterMs: 59 * 60_000 });
    }
  });
}
