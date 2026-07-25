// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The submission preflight turns a Chrome Web Store fetchStatus payload into
// one decision: is a review open (so a submission would be refused)?

import { describe, expect, test } from "bun:test";

import { summarizeItemStatus } from "../scripts/cws-status";

describe("summarizeItemStatus", () => {
  test("a submitted revision awaiting review gates the submission", () => {
    const status = summarizeItemStatus({
      publishedItemRevisionStatus: {
        state: "PUBLISHED",
        distributionChannels: [{ deployPercentage: 100, crxVersion: "0.6.0" }],
      },
      submittedItemRevisionStatus: {
        state: "PENDING_REVIEW",
        distributionChannels: [{ crxVersion: "0.7.0" }],
      },
    });
    expect(status.reviewOpen).toBe(true);
    expect(status.liveVersion).toBe("0.6.0");
    expect(status.submittedVersion).toBe("0.7.0");
    expect(status.lines.join("\n")).toContain("PENDING_REVIEW (0.7.0)");
  });

  test("nothing pending since the last publish is submittable", () => {
    const status = summarizeItemStatus({
      publishedItemRevisionStatus: {
        state: "PUBLISHED",
        distributionChannels: [{ deployPercentage: 100, crxVersion: "0.6.0" }],
      },
    });
    expect(status.reviewOpen).toBe(false);
    expect(status.submittedVersion).toBeNull();
    expect(status.lines.join("\n")).toContain("nothing pending");
  });

  test("an approved-but-unpublished revision does not gate — only review does", () => {
    const status = summarizeItemStatus({
      publishedItemRevisionStatus: { state: "PUBLISHED", distributionChannels: [{ crxVersion: "0.6.0" }] },
      submittedItemRevisionStatus: { state: "STAGED", distributionChannels: [{ crxVersion: "0.7.0" }] },
    });
    expect(status.reviewOpen).toBe(false);
    expect(status.lines.join("\n")).toContain("STAGED (0.7.0)");
  });

  test("a rejected revision is reported but does not block the next submission", () => {
    const status = summarizeItemStatus({
      publishedItemRevisionStatus: { state: "PUBLISHED", distributionChannels: [{ crxVersion: "0.6.0" }] },
      submittedItemRevisionStatus: { state: "REJECTED", distributionChannels: [{ crxVersion: "0.7.0" }] },
    });
    expect(status.reviewOpen).toBe(false);
    expect(status.lines.join("\n")).toContain("REJECTED (0.7.0)");
  });

  test("policy takedowns and warnings are surfaced", () => {
    const status = summarizeItemStatus({ takenDown: true, warned: true });
    const text = status.lines.join("\n");
    expect(text).toContain("TAKEN DOWN");
    expect(text).toContain("WARNED");
    expect(text).toContain("nothing published yet");
  });
});
