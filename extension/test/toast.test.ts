// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { test, expect } from "bun:test";
import { Window } from "happy-dom";
import { readFileSync } from "node:fs";

import {
  TOAST_ACTION_MESSAGE,
  TOAST_DISMISS_MESSAGE,
  TOAST_PENDING_MESSAGE,
  parseToastPayload,
  runToastPage,
} from "../src/toast";
import { TOAST_WINDOW_MS } from "../src/toast-view";

interface SentMessage {
  readonly type?: unknown;
  readonly job_id?: unknown;
  readonly reason?: unknown;
}

interface Harness {
  readonly doc: Document;
  readonly sent: SentMessage[];
  readonly closes: { count: number };
  readonly timers: { ms: number; run: () => void; cleared: boolean }[];
  fire: (index?: number) => void;
  /** Brings the window forward, the way the researcher's first click does. */
  focus: () => void;
  deps: Parameters<typeof runToastPage>[0];
}

/** Loads the real shipped page so a markup change (a renamed container, a lost
 * `role`) breaks these tests instead of passing against a fixture. */
function harness(reply: unknown | Error): Harness {
  const window = new Window();
  window.document.write(readFileSync("src/toast.html", "utf8"));
  const doc = window.document as unknown as Document;
  const sent: SentMessage[] = [];
  const closes = { count: 0 };
  const timers: { ms: number; run: () => void; cleared: boolean }[] = [];
  const focusListeners: (() => void)[] = [];
  const h: Harness = {
    doc,
    sent,
    closes,
    timers,
    fire: (index = 0) => {
      const timer = timers[index];
      if (timer !== undefined && !timer.cleared) timer.run();
    },
    focus: () => {
      for (const listener of focusListeners) listener();
    },
    deps: {
      doc,
      sendMessage: async (message) => {
        sent.push(message as SentMessage);
        if ((message as SentMessage).type !== TOAST_PENDING_MESSAGE) return undefined;
        if (reply instanceof Error) throw reply;
        return reply;
      },
      closeWindow: () => {
        closes.count += 1;
      },
      setTimer: (run, ms) => {
        timers.push({ ms, run, cleared: false });
        return timers.length - 1;
      },
      clearTimer: (handle) => {
        const timer = timers[handle];
        if (timer !== undefined) timer.cleared = true;
      },
      onFocus: (run) => {
        focusListeners.push(run);
      },
    },
  };
  return h;
}

test("the page asks the producer for the pending toast and renders it", async () => {
  const h = harness({ ok: true, toast: { kind: "institution_claim_lost", job_id: "job-1" } });
  await runToastPage(h.deps);

  expect(h.sent[0]?.type).toBe(TOAST_PENDING_MESSAGE);
  expect(h.doc.getElementById("toast-action")?.textContent).toBe("Open a new sign-in tab");
  expect(h.closes.count).toBe(0);
});

test("an empty or malformed reply closes the window instead of showing a blank toast", async () => {
  // Three ways the producer can fail to supply a toast. All three must close.
  // A window that opens and shows nothing is a worse interruption than none,
  // and it is the failure a researcher cannot diagnose.
  for (const reply of [
    undefined,
    {},
    { ok: false, error: { code: "x", message: "y" } },
    { ok: true },
    { ok: true, toast: { kind: "route_lost" } },
    { ok: true, toast: { kind: "made_up", job_id: "j" } },
    { ok: true, toast: { kind: "route_lost", job_id: "" } },
  ]) {
    const h = harness(reply);
    await runToastPage(h.deps);
    expect(h.closes.count, JSON.stringify(reply)).toBe(1);
    expect(h.doc.getElementById("toast-action")).toBeNull();
  }
});

test("a failed request closes the window rather than hanging open", async () => {
  const h = harness(new Error("worker asleep"));
  await runToastPage(h.deps);
  expect(h.closes.count).toBe(1);
});

test("expiry reports that the offer lapsed and commits nothing", async () => {
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-7" } });
  await runToastPage(h.deps);
  expect(h.timers[0]?.ms).toBe(TOAST_WINDOW_MS);

  h.fire();
  const last = h.sent.at(-1);
  expect(last?.type).toBe(TOAST_DISMISS_MESSAGE);
  expect(last?.reason).toBe("expired");
  // The distinction that matters: expiry must never send the action message.
  // If it did, walking away from the toast would silently reopen a tab.
  expect(h.sent.some((message) => message.type === TOAST_ACTION_MESSAGE)).toBe(false);
  expect(h.closes.count).toBe(1);
});

test("the action carries the job id the producer supplied", async () => {
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job_b08c0e5e" } });
  await runToastPage(h.deps);
  (h.doc.getElementById("toast-action") as HTMLButtonElement).click();

  const action = h.sent.find((message) => message.type === TOAST_ACTION_MESSAGE);
  expect(action?.job_id).toBe("job_b08c0e5e");
  expect(h.closes.count).toBe(1);
});

test("clicking the action at the last moment cannot race its own expiry", async () => {
  // The reason both handlers clear the timer first. A click at 7.9s with a
  // live timer sends `action` and then `expired` for the same job, and the
  // producer has no way to tell which one the researcher meant — so it would
  // either reopen a tab the researcher dismissed, or drop the reopen they
  // asked for. This is the whole point of the ordering.
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-9" } });
  await runToastPage(h.deps);
  (h.doc.getElementById("toast-action") as HTMLButtonElement).click();
  h.fire();

  expect(h.sent.filter((message) => message.type === TOAST_ACTION_MESSAGE)).toHaveLength(1);
  expect(h.sent.some((message) => message.type === TOAST_DISMISS_MESSAGE)).toBe(false);
  expect(h.timers[0]?.cleared).toBe(true);
});

test("dismissing says so, and never reopens anything", async () => {
  const h = harness({ ok: true, toast: { kind: "institution_claim_lost", job_id: "job-3" } });
  await runToastPage(h.deps);
  (h.doc.getElementById("toast-dismiss") as HTMLButtonElement).click();

  const last = h.sent.at(-1);
  expect(last?.type).toBe(TOAST_DISMISS_MESSAGE);
  expect(last?.reason).toBe("dismissed");
  expect(h.sent.some((message) => message.type === TOAST_ACTION_MESSAGE)).toBe(false);
});

test("a second click cannot submit the action twice", async () => {
  // The window closes on the first click, but `window.close()` is not
  // instantaneous in a real browser, and a double-click is one gesture to a
  // researcher. Two reopen requests for one paper would open two tabs.
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-4" } });
  await runToastPage(h.deps);
  const action = h.doc.getElementById("toast-action") as HTMLButtonElement;
  action.click();
  action.click();

  expect(h.sent.filter((message) => message.type === TOAST_ACTION_MESSAGE)).toHaveLength(1);
});

test("the shipped page declares the container the driver looks for", () => {
  // The page path and the container id are the two things that make this
  // surface render at all, and a unit test cannot see a wrong page path (the
  // fake resolves any string). Pinning the id here at least fails the rename.
  const markup = readFileSync("src/toast.html", "utf8");
  expect(markup).toContain('id="toast"');
  expect(markup).toContain('aria-describedby="toast-message"');
  expect(markup).toContain('src="./toast.js"');
});

test("parseToastPayload keeps the kind vocabulary closed", () => {
  expect(parseToastPayload({ ok: true, toast: { kind: "route_lost", job_id: "j" } })).toEqual({ kind: "route_lost", job_id: "j" });
  expect(parseToastPayload({ ok: true, toast: { kind: "institution_claim_lost", job_id: "j" } })).toEqual({
    kind: "institution_claim_lost",
    job_id: "j",
  });
  // A kind the renderer has no copy for would render an empty toast.
  expect(parseToastPayload({ ok: true, toast: { kind: "surface_lost", job_id: "j" } })).toBeUndefined();
});

test("being brought forward restarts the clock, because macOS spends the first click activating the window", async () => {
  // Measured against a real unfocused Chrome popup: on macOS the first click on
  // an unfocused window activates it and does not reach the button underneath.
  // A researcher who notices the toast at 7s would otherwise lose the offer to
  // expiry before their second click lands.
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-focus" } });
  await runToastPage(h.deps);

  h.focus();

  // The original timer is cancelled and a fresh full window replaces it.
  expect(h.timers[0]?.cleared).toBe(true);
  expect(h.timers[1]?.ms).toBe(TOAST_WINDOW_MS);
  // The original firing must now do nothing: the offer is still live.
  h.fire(0);
  expect(h.sent.some((m) => m.reason === "expired")).toBe(false);
  expect(h.closes.count).toBe(0);

  // And the replacement still expires, so the surface stays bounded.
  h.fire(1);
  expect(h.sent.at(-1)).toMatchObject({ reason: "expired", job_id: "job-focus" });
  expect(h.closes.count).toBe(1);
});

test("the window is re-armed once, so a window cycled in and out of the foreground cannot live forever", async () => {
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-once" } });
  await runToastPage(h.deps);

  h.focus();
  h.focus();
  h.focus();

  // One replacement, not three: an offer that never lapses is a decision papio
  // is still holding.
  expect(h.timers.length).toBe(2);
  h.fire(1);
  expect(h.sent.at(-1)).toMatchObject({ reason: "expired" });
});

test("a click after the re-arm clears the replacement timer, not the dead one", async () => {
  const h = harness({ ok: true, toast: { kind: "route_lost", job_id: "job-clear" } });
  await runToastPage(h.deps);
  h.focus();

  (h.doc.getElementById("toast-action") as HTMLButtonElement).click();

  // The live timer is the one the click must silence; leaving it armed would
  // send `expired` after the action, and the producer could not tell which one
  // told the truth.
  expect(h.timers[1]?.cleared).toBe(true);
  h.fire(1);
  expect(h.sent.filter((m) => m.reason === "expired")).toHaveLength(0);
});
