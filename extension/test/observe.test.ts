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
  expect(fake.stored[RATE_KEY]).toEqual({
    total: [new Date(capturedAt).getTime()],
    byHost: { "sciencedirect-com": [new Date(capturedAt).getTime()] },
  });
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

test("persisted per-host and daily observation quotas prevent later captures", async () => {
  const firstHost = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(firstHost));
  const firstTime = "2026-07-15T10:00:00.000Z";
  await observeUnknown(fake.api, jobFor(firstHost), firstHost, verifiedHosts(firstHost), fixedNow(firstTime));
  await observeUnknown(
    fake.api,
    jobFor(firstHost),
    firstHost,
    verifiedHosts(firstHost),
    fixedNow("2026-07-15T10:30:00.000Z"),
  );
  expect(fake.sent).toHaveLength(1);

  const otherHosts = ["www.jstor.org", "www.springer.com", "www.wiley.com", "www.tandfonline.com"];
  for (let index = 0; index < otherHosts.length; index += 1) {
    const host = otherHosts[index]!;
    fake.setPage(pageFor(host));
    await observeUnknown(
      fake.api,
      jobFor(host, 20 + index),
      host,
      verifiedHosts(host),
      fixedNow(`2026-07-15T${11 + index}:00:00.000Z`),
    );
  }
  expect(fake.sent).toHaveLength(5);

  const sixthHost = "www.sagepub.com";
  fake.setPage(pageFor(sixthHost));
  await observeUnknown(
    fake.api,
    jobFor(sixthHost, 30),
    sixthHost,
    verifiedHosts(sixthHost),
    fixedNow("2026-07-15T16:00:00.000Z"),
  );
  expect(fake.sent).toHaveLength(5);
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
