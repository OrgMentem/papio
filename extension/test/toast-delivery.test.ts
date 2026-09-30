// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import type { TabInfo, WindowInfo } from "../src/browser-types";
import { TOAST_PAGE_PATH } from "../src/page-paths";
import { ToastDelivery } from "../src/toast-delivery";

const TOAST_URL = `chrome-extension://papio/${TOAST_PAGE_PATH}`;

/** ToastDelivery on the window route (no in-page query seam), with a window
 * fake whose toast tab can be left loading so a replacement races it. */
function harness() {
  const live = new Map<number, TabInfo[]>();
  const removed: number[] = [];
  const opened: string[] = [];
  const minted: string[] = [];
  let nextID = 1;
  let loading = false;
  const delivery = new ToastDelivery(
    {
      randomUUID: () => "uuid",
      setTimeout: () => {},
      runtimeGetURL: (path) => `chrome-extension://papio/${path}`,
      tabs: {
        create: async ({ url }) => {
          opened.push(url);
          return { url };
        },
      },
      windows: {
        create: async ({ url }): Promise<WindowInfo> => {
          const id = nextID++;
          // A window just created is still loading: Chrome reports the page as
          // the tab's pendingUrl while its committed url is still empty.
          live.set(id, [loading ? { url: "", pendingUrl: url } : { url }]);
          return { id, focused: false };
        },
        get: async (id) => {
          const tabs = live.get(id);
          if (tabs === undefined) throw new Error("No window with id");
          return { id, tabs };
        },
        update: async () => ({}),
        remove: async (id) => {
          removed.push(id);
          live.delete(id);
          return {};
        },
      },
      scripting: { executeScript: async () => [] },
      settings: { getInPageToast: async () => false },
      permissions: { contains: async () => false },
    },
    {
      store: () => ({ activeJobs: [] }) as never,
      tabLedgerCache: () => undefined,
      papioSurfaceLikelyFocused: () => false,
      openBrokerTab: async () => undefined,
      requestFreshHandoffLink: async (jobID) => {
        minted.push(jobID);
        return { ok: true, url: `https://resolver.example/${jobID}` };
      },
    },
  );
  return {
    delivery,
    live,
    removed,
    opened,
    minted,
    setLoading(value: boolean) {
      loading = value;
    },
  };
}

test("a stale toast's action or dismissal leaves the live offer and its window intact", async () => {
  const h = harness();
  expect(await h.delivery.raiseToast({ kind: "route_lost", job_id: "job-a" })).toBe(true);
  expect(await h.delivery.raiseToast({ kind: "route_lost", job_id: "job-b" })).toBe(true);
  expect(h.removed).toEqual([1]);

  // A window for job-a that outlived its replacement reports late.
  expect(await h.delivery.toastAction("job-a")).toBe(false);
  h.delivery.toastDismiss("job-a");
  expect(h.delivery.toastPending()).toEqual({ kind: "route_lost", job_id: "job-b" });
  expect(h.minted).toEqual([]);

  // The live window is still tracked, so the next replacement retires it
  // instead of stacking a second toast beside it.
  expect(await h.delivery.raiseToast({ kind: "route_lost", job_id: "job-c" })).toBe(true);
  expect(h.removed).toEqual([1, 2]);
  expect(await h.delivery.toastAction("job-c")).toBe(true);
  expect(h.minted).toEqual(["job-c"]);
  expect(h.opened).toEqual(["https://resolver.example/job-c"]);
});

test("a replacement retires a toast window that is still loading the toast page", async () => {
  const h = harness();
  h.setLoading(true);
  expect(await h.delivery.raiseToast({ kind: "route_lost", job_id: "job-a" })).toBe(true);
  expect(await h.delivery.raiseToast({ kind: "route_lost", job_id: "job-b" })).toBe(true);
  expect(h.removed).toEqual([1]);
  expect([...h.live.keys()]).toEqual([2]);
});
