// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Automatic observed-fixture capture is tested against the narrow Chrome seams;
// no test injects into a browser or writes a real download.

import { expect, test } from "bun:test";

import { observeUnknown, type ObserveChromeApi } from "../src/observe";
import type { PageCapture } from "../src/capture";
import type { ActiveJob } from "../src/state";

const RATE_KEY = "papio_observed_capture_rate_v1";
const CLEAN_HTML = `<html><body><main class="article">Known structure</main><script>secret</script></body></html>`;

function jobFor(host: string, tab = 17): ActiveJob {
  return {
    job_id: `job-${host}`,
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
  const downloads: Array<{ filename: string; url: string }> = [];
  const api: ObserveChromeApi = {
    scripting: {
      executeScript: async ({ target }) => {
        injections.push(target);
        return [{ result: page }];
      },
    },
    downloads: {
      download: async (options) => {
        downloads.push({ filename: options.filename, url: options.url });
        return downloads.length;
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
  };
  return {
    api,
    downloads,
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

test("unknown tracked provider page captures one sanitized observed fixture", async () => {
  const host = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(host));

  await observeUnknown(fake.api, jobFor(host), host, fixedNow("2026-07-15T10:11:12.000Z"));

  expect(fake.injections).toEqual([{ tabId: 17 }]);
  expect(fake.downloads).toHaveLength(1);
  expect(fake.downloads[0]?.filename).toBe(
    "papio-fixtures/observed/sciencedirect-com/2026-07-15T10-11-12.000Z.html",
  );
  const html = decodeURIComponent(fake.downloads[0]?.url.split(",")[1] ?? "");
  expect(html).toContain('scenario="observed"');
  expect(html).toContain('<script></script>');
  expect(fake.stored[RATE_KEY]).toEqual({
    total: [new Date("2026-07-15T10:11:12.000Z").getTime()],
    byHost: { "sciencedirect-com": [new Date("2026-07-15T10:11:12.000Z").getTime()] },
  });
});

test("persisted per-host and daily observation quotas prevent later captures", async () => {
  const firstHost = "www.sciencedirect.com";
  const fake = fakeChrome(pageFor(firstHost));
  const firstTime = "2026-07-15T10:00:00.000Z";
  await observeUnknown(fake.api, jobFor(firstHost), firstHost, fixedNow(firstTime));
  await observeUnknown(fake.api, jobFor(firstHost), firstHost, fixedNow("2026-07-15T10:30:00.000Z"));
  expect(fake.downloads).toHaveLength(1);

  const otherHosts = ["www.jstor.org", "www.springer.com", "www.wiley.com", "www.tandfonline.com"];
  for (let index = 0; index < otherHosts.length; index += 1) {
    const host = otherHosts[index]!;
    fake.setPage(pageFor(host));
    await observeUnknown(fake.api, jobFor(host, 20 + index), host, fixedNow(`2026-07-15T${11 + index}:00:00.000Z`));
  }
  expect(fake.downloads).toHaveLength(5);

  const sixthHost = "www.sagepub.com";
  fake.setPage(pageFor(sixthHost));
  await observeUnknown(fake.api, jobFor(sixthHost, 30), sixthHost, fixedNow("2026-07-15T16:00:00.000Z"));
  expect(fake.downloads).toHaveLength(5);
});

test("untracked and non-provider pages are never injected or downloaded", async () => {
  const providerHost = "www.jstor.org";
  const fake = fakeChrome(pageFor(providerHost));

  await observeUnknown(fake.api, undefined, providerHost, fixedNow("2026-07-15T10:00:00.000Z"));
  await observeUnknown(fake.api, jobFor(providerHost), "login.example.edu", fixedNow("2026-07-15T10:01:00.000Z"));

  expect(fake.injections).toHaveLength(0);
  expect(fake.downloads).toHaveLength(0);
  expect(fake.stored[RATE_KEY]).toBeUndefined();
});

test("a residual leak refuses the observed download", async () => {
  // A valid long provider label reaches the fixture header unchanged. The
  // existing residualLeak guard detects it there and refuses to write.
  const host = "abcdefghijklmnopqrstuvwxyzabcdef.com";
  const fake = fakeChrome(pageFor(host));
  const warn = console.warn;
  console.warn = () => undefined;
  try {
    await observeUnknown(fake.api, jobFor(host), host, fixedNow("2026-07-15T10:00:00.000Z"));
  } finally {
    console.warn = warn;
  }

  expect(fake.injections).toHaveLength(1);
  expect(fake.downloads).toHaveLength(0);
});
