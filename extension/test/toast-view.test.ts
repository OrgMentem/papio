// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { test, expect } from "bun:test";
import { Window } from "happy-dom";

import { TOAST_COPY, TOAST_WINDOW_MS, renderToast, toastKindForLoss } from "../src/toast-view";

function toastDocument(): { doc: Document; container: HTMLElement } {
  const window = new Window();
  const doc = window.document as unknown as Document;
  const container = doc.createElement("div");
  doc.body.append(container);
  return { doc, container };
}

test("the toast offers exactly one action and one dismissal", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });

  // Decision 1's density rule applied to the seventh surface: a toast that
  // grows a second action becomes a dialog, and a dialog belongs in the inbox.
  const buttons = container.querySelectorAll("button");
  expect(buttons).toHaveLength(2);
  expect(doc.getElementById("toast-action")?.textContent).toBe("Reopen now");
  expect(doc.getElementById("toast-dismiss")?.textContent).toBe("Dismiss");
});

test("the institutional loss never offers to reopen", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "institution_claim_lost", job_id: "job-2" });

  // `owner_closed` abandons the claim, retires the lease, and consumes the
  // one-use close authorization, so no reversal exists. A button saying
  // "Reopen" or "Undo" here would promise one. This is the assertion that
  // fails if someone unifies the two copy entries to save a line.
  const label = doc.getElementById("toast-action")?.textContent ?? "";
  expect(label).toBe("Open a new sign-in tab");
  expect(label.toLowerCase()).not.toContain("reopen");
  expect(label.toLowerCase()).not.toContain("undo");
});

test("no rendered text carries the job id", () => {
  const { doc, container } = toastDocument();
  const jobID = "job_b08c0e5e6e632d13ab2b607ccd";
  renderToast(doc, container, { kind: "route_lost", job_id: jobID });

  // Bound 3, and the same rule Decision 1 already holds the host-page
  // acknowledgement to. The id travels in the extension's own message; a page
  // never sees it. Checked on the whole subtree, not just the message node,
  // because a helpful `data-job-id` on the button would leak it just as far.
  expect(container.textContent ?? "").not.toContain(jobID);
  expect(container.outerHTML).not.toContain(jobID);
});

test("no copy entry names an identifier, provider, or URL", () => {
  // The closed copy table is the whole vocabulary of this surface. A future
  // entry that interpolates a title or a provider name fails here rather than
  // in review.
  for (const [kind, copy] of Object.entries(TOAST_COPY)) {
    expect(copy.message, kind).not.toMatch(/https?:|\bdoi\b|10\.\d{4}|job[_-]/i);
    expect(copy.action, kind).not.toMatch(/https?:|\bdoi\b|10\.\d{4}|job[_-]/i);
    // Brevity, measured directly. A sentence count was the wrong proxy:
    // `route_lost` needs its second sentence, because ADR-0023 Decision 3
    // requires copy that does not overpromise, and "It will try again on its
    // own" is what makes the offer a shortcut rather than the only recovery.
    // A body past this bound is a report, and reports belong in Activity.
    expect(copy.message.length, kind).toBeLessThanOrEqual(110);
    expect(copy.action.length, kind).toBeLessThanOrEqual(28);
  }
});

test("re-rendering replaces the toast instead of stacking it", () => {
  const { doc, container } = toastDocument();
  renderToast(doc, container, { kind: "route_lost", job_id: "job-1" });
  renderToast(doc, container, { kind: "institution_claim_lost", job_id: "job-2" });

  // Bound 1. Two losses in quick succession is the normal case when a window
  // closes with several papio tabs in it, and an appending renderer would
  // build a list nobody asked for.
  expect(container.querySelectorAll("button")).toHaveLength(2);
  expect(container.querySelectorAll(".toast-message")).toHaveLength(1);
  expect(doc.getElementById("toast-action")?.textContent).toBe("Open a new sign-in tab");
  expect(container.dataset.kind).toBe("institution_claim_lost");
});

test("a loss that lost nothing raises no toast", () => {
  // The two branches where the tab closing cost the researcher nothing: the
  // download keeps its own correlation, and an awaiting_download park is
  // adopted by the daemon's poll scan. A toast for either is a false alarm,
  // and false alarms are how a proactive surface gets turned off.
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: true, awaitingDownload: false }),
  ).toBeUndefined();
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: false, awaitingDownload: true }),
  ).toBeUndefined();
});

test("an institutional claim loss outranks a delivery still in flight", () => {
  // Both true is reachable: a paper can hold an institutional claim and have a
  // download correlated when its window closes. The claim is the loss with no
  // recovery, so it must win — the opposite order would report nothing at all
  // and strand the library's sign-in slot silently, which is the exact failure
  // the surface-lifecycle work exists to prevent.
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: true, deliveryInFlight: true, awaitingDownload: true }),
  ).toBe("institution_claim_lost");
});

test("an ordinary route loss offers a reopen", () => {
  expect(
    toastKindForLoss({ institutionalClaimAbandoned: false, deliveryInFlight: false, awaitingDownload: false }),
  ).toBe("route_lost");
});

test("the toast window outlasts the inbox undo window", () => {
  // Not a magic number check: the inbox already commits a dismissal after six
  // seconds, and this surface asks for an action rather than deferring one, so
  // it must not be the tighter of the two.
  expect(TOAST_WINDOW_MS).toBeGreaterThan(6000);
});

test("the window is wide enough for the longest copy to wrap to two lines", () => {
  // A measurement, kept as an assertion because the first size was a guess and
  // clipped. At 420px — the popup's width — the institutional message wraps to
  // four lines and needs 106px of inner height, while windows.create's height
  // includes the platform frame, so 108 outer left ~80 inner and hid the
  // button. Measured in a real browser at 520px: both messages wrap to two
  // lines and need 65px inner.
  //
  // This pins the INPUT to that measurement, which is what a future copy change
  // would break: a longer sentence re-wraps and the window no longer fits it.
  // The per-message bound below is what the 520px measurement allows at this
  // font, leaving the two controls their room.
  for (const [kind, copy] of Object.entries(TOAST_COPY)) {
    const longestWord = Math.max(...copy.message.split(" ").map((word) => word.length));
    expect(longestWord, `${kind} has an unwrappable word`).toBeLessThanOrEqual(14);
    // Message plus action label share one 520px row. Both together past this
    // and the two-line measurement no longer holds.
    expect(copy.message.length + copy.action.length, kind).toBeLessThanOrEqual(100);
  }
});
