// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Automatic observed captures exercise the narrow browser seams. They never
// write to Downloads: the only observable output is a compressed native frame.
import { expect, test } from "bun:test";
import { gunzipSync } from "node:zlib";


import {
  observeUnknown,
  type ObservationCaptureContext,
  type ObserveChromeApi,
} from "../src/observe";
import type { PageCapture } from "../src/capture";
import type { PageCapturePayload } from "../src/protocol";
import type { ActiveJob } from "../src/state";

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

test("a registry-only adapter host now passes the observed-capture gate", async () => {
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
    adapter_id: "sage",
    adapter_version: "1.2.3",
  });
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
