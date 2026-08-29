// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { Window } from "happy-dom";
import { renderPageToast } from "../src/background";
import { capturePage } from "../src/capture";
import {
  TOAST_COPY,
  TOAST_PAGE_ACTION_MESSAGE,
  TOAST_PAGE_DISMISS_MESSAGE,
  TOAST_WINDOW_MS,
  type ToastInjection,
} from "../src/toast-view";

const HOST_ID = "papio-extension-loss-toast-v1";

// The injected function reads `document`, `window`, and `chrome.runtime` at CALL
// time, not at render time — its click handlers run long after `renderPageToast`
// returns. So the fakes must stay installed for the life of each test, and are
// restored between tests rather than at the end of the render.
const priorGlobals = {
  document: globalThis.document,
  window: (globalThis as { window?: unknown }).window,
  chrome: (globalThis as { chrome?: unknown }).chrome,
};
afterEach(() => {
  Object.assign(globalThis, priorGlobals);
});

interface Harness {
  readonly doc: Document;
  readonly sent: Record<string, unknown>[];
  readonly timers: { ms: number; run: () => void; cleared: boolean }[];
  fire(index: number): void;
  host(): HTMLElement | null;
  button(text: string): HTMLElement | undefined;
}

/** Drives the REAL injected function against a real DOM, with the page globals
 * it reaches for. The function is serialized into a page by
 * `scripting.executeScript`, so it uses `document`, `window`, and
 * `chrome.runtime` directly rather than injected deps — the harness therefore
 * installs those globals rather than passing a seam. */
function harness(injection?: Partial<ToastInjection>): Harness {
  const window = new Window();
  const doc = window.document as unknown as Document;
  const sent: Record<string, unknown>[] = [];
  const timers: { ms: number; run: () => void; cleared: boolean }[] = [];
  Object.assign(globalThis, {
    document: doc,
    window: {
      matchMedia: () => ({ matches: false }),
      setTimeout: (run: () => void, ms: number) => {
        timers.push({ ms, run, cleared: false });
        return timers.length - 1;
      },
      clearTimeout: (handle: number) => {
        const timer = timers[handle];
        if (timer !== undefined) timer.cleared = true;
      },
    },
    chrome: {
      runtime: {
        sendMessage: async (message: Record<string, unknown>) => {
          sent.push(message);
        },
      },
    },
  });
  const payload: ToastInjection = {
    kind: "route_lost",
    job_id: "job-1",
    token: "tok-1",
    message: TOAST_COPY.route_lost.message,
    action: TOAST_COPY.route_lost.action,
    window_ms: TOAST_WINDOW_MS,
    action_message: TOAST_PAGE_ACTION_MESSAGE,
    dismiss_message: TOAST_PAGE_DISMISS_MESSAGE,
    ...injection,
  };
  expect(renderPageToast(payload)).toBe(true);
  const host = (): HTMLElement | null => doc.getElementById(HOST_ID);
  return {
    doc,
    sent,
    timers,
    fire: (index) => {
      const timer = timers[index];
      if (timer === undefined || timer.cleared) return;
      timer.run();
    },
    host,
    button: (text) =>
      [...(host()?.shadowRoot?.querySelectorAll("button") ?? [])].find(
        (button) => button.textContent === text,
      ) as HTMLElement | undefined,
  };
}

test("the toast renders inside a shadow root, so a page capture cannot serialize its copy", () => {
  // `document.documentElement.outerHTML` omits shadow roots (WHATWG fragment
  // serialization), and that read is exactly how papio builds adapter fixtures.
  // A toast rendered in the light DOM would commit papio's own sentence into
  // every capture taken while it was up.
  const h = harness();
  expect(h.host()?.shadowRoot).not.toBeNull();
  const serialized = h.doc.documentElement.outerHTML;
  expect(serialized).toContain(HOST_ID);
  expect(serialized).not.toContain(TOAST_COPY.route_lost.message);
});

test("the toast carries the copy and exactly one action", () => {
  const h = harness();
  const root = h.host()?.shadowRoot;
  expect(root?.textContent).toContain(TOAST_COPY.route_lost.message);
  expect([...(root?.querySelectorAll("button") ?? [])]).toHaveLength(2);
  expect(h.button(TOAST_COPY.route_lost.action)).toBeDefined();
  expect(h.button("Dismiss")).toBeDefined();
  // An interruption that names the paper would leak the researcher's reading
  // into a page. The copy table forbids it; this pins it at the DOM.
  expect(root?.textContent ?? "").not.toContain("job-1");
  expect(root?.textContent ?? "").not.toContain("tok-1");
});

test("the toast removes itself BEFORE it reports the action", () => {
  // Ordering, not housekeeping. Taking the action opens a tab, and this page
  // may be captured or classified at any moment afterwards; a toast still in
  // the DOM at that point is in the evidence.
  const h = harness();
  let hostPresentAtSend: boolean | undefined;
  Object.assign(globalThis, {
    chrome: {
      runtime: {
        sendMessage: async (message: Record<string, unknown>) => {
          hostPresentAtSend = h.host() !== null;
          h.sent.push(message);
        },
      },
    },
  });
  h.button(TOAST_COPY.route_lost.action)?.dispatchEvent(
    new (h.doc.defaultView as unknown as { Event: typeof Event }).Event("click"),
  );
  expect(hostPresentAtSend).toBe(false);
  expect(h.sent).toHaveLength(1);
  expect(h.sent[0]).toEqual({
    type: TOAST_PAGE_ACTION_MESSAGE,
    job_id: "job-1",
    token: "tok-1",
  });
});

test("a click and its own expiry cannot both report", () => {
  // The race the researcher creates by clicking at 7.9s. Two reports would ask
  // the daemon to reopen a paper and then tell it the offer lapsed.
  const h = harness();
  const view = h.doc.defaultView as unknown as { Event: typeof Event };
  h.button(TOAST_COPY.route_lost.action)?.dispatchEvent(new view.Event("click"));
  // The expiry timer fires late, as it would in a real page.
  h.fire(0);
  expect(h.sent).toHaveLength(1);
  expect(h.sent[0]?.["type"]).toBe(TOAST_PAGE_ACTION_MESSAGE);
});

test("expiry reports a dismissal and commits nothing", () => {
  const h = harness();
  expect(h.timers[0]?.ms).toBe(TOAST_WINDOW_MS);
  h.fire(0);
  expect(h.host()).toBeNull();
  expect(h.sent).toEqual([
    {
      type: TOAST_PAGE_DISMISS_MESSAGE,
      job_id: "job-1",
      token: "tok-1",
      reason: "expired",
    },
  ]);
});

test("a second loss replaces the first toast rather than stacking", () => {
  // Bound 1 at the DOM. Two hosts would ask the researcher about two losses at
  // once, in a page that is not papio's.
  const h = harness();
  Object.assign(globalThis, {
    document: h.doc,
    window: { matchMedia: () => ({ matches: false }), setTimeout: () => 0, clearTimeout: () => {} },
    chrome: { runtime: { sendMessage: async () => {} } },
  });
  renderPageToast({
    kind: "institution_claim_lost",
    job_id: "job-2",
    token: "tok-2",
    message: TOAST_COPY.institution_claim_lost.message,
    action: TOAST_COPY.institution_claim_lost.action,
    window_ms: TOAST_WINDOW_MS,
    action_message: TOAST_PAGE_ACTION_MESSAGE,
    dismiss_message: TOAST_PAGE_DISMISS_MESSAGE,
  });
  expect(h.doc.querySelectorAll(`#${HOST_ID}`)).toHaveLength(1);
  expect(h.host()?.shadowRoot?.textContent).toContain(
    TOAST_COPY.institution_claim_lost.message,
  );
});

test("two clicks on the action report once", () => {
  // The `settled` flag, isolated. The expiry test above cannot reach it: the
  // first settle clears the timer, so the guard never has to hold. A researcher
  // double-clicking is the path that does reach it, and two reports would mint
  // two fresh routes and open the paper twice.
  const h = harness();
  const view = h.doc.defaultView as unknown as { Event: typeof Event };
  const action = h.button(TOAST_COPY.route_lost.action);
  action?.dispatchEvent(new view.Event("click"));
  action?.dispatchEvent(new view.Event("click"));
  expect(h.sent).toHaveLength(1);
});

test("dismissing after acting reports once", () => {
  // Both buttons are still in the researcher's hands until the host is gone,
  // and a dismissal after an accepted action would tell the daemon the offer
  // lapsed after it had already been taken.
  const h = harness();
  const view = h.doc.defaultView as unknown as { Event: typeof Event };
  h.button(TOAST_COPY.route_lost.action)?.dispatchEvent(new view.Event("click"));
  h.button("Dismiss")?.dispatchEvent(new view.Event("click"));
  expect(h.sent).toHaveLength(1);
  expect(h.sent[0]?.["type"]).toBe(TOAST_PAGE_ACTION_MESSAGE);
});

test("a queued expiry that beats clearTimeout still reports once", () => {
  // The genuine race: the 8s callback is already queued when the researcher
  // clicks, so `clearTimeout` cannot unqueue it. Only the flag stops it.
  const h = harness();
  const view = h.doc.defaultView as unknown as { Event: typeof Event };
  h.button(TOAST_COPY.route_lost.action)?.dispatchEvent(new view.Event("click"));
  h.timers[0]?.run();
  expect(h.sent).toHaveLength(1);
  expect(h.sent[0]?.["type"]).toBe(TOAST_PAGE_ACTION_MESSAGE);
});

test("a capture taken while a toast is up contains no trace of papio", () => {
  // The fixture-integrity case. A capture is evidence about a PROVIDER's page,
  // and it can be taken at any moment — including inside the eight seconds a
  // loss toast is on screen. The shadow root already keeps the copy out of
  // `outerHTML`; this pins that the host element is gone too, and that the LIVE
  // toast survives the capture rather than being cancelled by it.
  const h = harness();
  const view = h.doc.defaultView as unknown as Window & typeof globalThis;
  Object.assign(globalThis, {
    document: h.doc,
    location: { origin: "https://journals.example.edu", pathname: "/article/1" },
  });
  const captured = capturePage();
  expect(captured.html).not.toContain(HOST_ID);
  expect(captured.html).not.toContain(TOAST_COPY.route_lost.message);
  // The researcher's offer is still there.
  expect(h.host()).not.toBeNull();
  expect(h.button(TOAST_COPY.route_lost.action)).toBeDefined();
  expect(view).toBeDefined();
});
