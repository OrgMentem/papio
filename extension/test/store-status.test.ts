// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The submission preflights turn a store API payload into one decision: would
// this submission be refused? The two stores refuse for different reasons —
// Chrome locks the item during review, AMO rejects a reused version number —
// so each script gates on its own condition.

import { describe, expect, test } from "bun:test";

import { summarizeItemStatus } from "../scripts/cws-status";
import { parseAmoVersions, summarizeAmoStatus } from "../scripts/amo-status";

describe("summarizeItemStatus (Chrome Web Store)", () => {
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
    expect(status.published?.crxVersion).toBe("0.6.0");
    expect(status.submitted?.crxVersion).toBe("0.7.0");
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
    expect(status.submitted).toBeNull();
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

  test("a payload missing every field reports unknown rather than throwing", () => {
    const status = summarizeItemStatus(null);
    expect(status.reviewOpen).toBe(false);
    expect(status.published).toBeNull();
  });
});

describe("summarizeAmoStatus (Firefox Add-ons)", () => {
  const listing = [
    { version: "0.5.0", channel: "listed", status: "public" },
    { version: "0.3.0", channel: "unlisted", status: "public" },
  ];

  test("a version number already on AMO gates the submission", () => {
    const status = summarizeAmoStatus(listing, "0.5.0");
    expect(status.versionExists).toBe(true);
    expect(status.lines.join("\n")).toContain("already uploaded");
  });

  test("an unlisted upload still consumes the version number", () => {
    expect(summarizeAmoStatus(listing, "0.3.0").versionExists).toBe(true);
  });

  test("an unused version number is submittable", () => {
    const status = summarizeAmoStatus(listing, "0.7.0");
    expect(status.versionExists).toBe(false);
    expect(status.latestListed?.version).toBe("0.5.0");
    expect(status.lines.join("\n")).toContain("0.7.0 is unused on AMO");
  });

  test("a version awaiting review is reported but never gates — AMO reviews per version", () => {
    const status = summarizeAmoStatus(
      [{ version: "0.6.0", channel: "listed", status: "unreviewed" }, ...listing],
      "0.7.0",
    );
    expect(status.reviewOpen).toBe(true);
    expect(status.versionExists).toBe(false);
    expect(status.lines.join("\n")).toContain("awaiting review: 0.6.0 — unreviewed (listed)");
  });

  test("an empty listing reports no versions instead of failing", () => {
    const status = summarizeAmoStatus([], "0.7.0");
    expect(status.latestListed).toBeNull();
    expect(status.lines.join("\n")).toContain("latest listed: none");
  });
});

describe("parseAmoVersions", () => {
  test("reads channel and file status, and drops malformed rows", () => {
    const versions = parseAmoVersions({
      results: [
        { version: "0.5.0", channel: "listed", file: { status: "public" } },
        { channel: "listed", file: { status: "public" } },
        "not an object",
        { version: "0.4.0" },
      ],
    });
    expect(versions).toEqual([
      { version: "0.5.0", channel: "listed", status: "public" },
      { version: "0.4.0", channel: null, status: null },
    ]);
  });

  test("a payload with no results is an empty list", () => {
    expect(parseAmoVersions({})).toEqual([]);
    expect(parseAmoVersions(null)).toEqual([]);
  });
});
