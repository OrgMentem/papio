// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Report the Chrome Web Store item's review state before a submission.
//
//   bun run scripts/cws-status.ts          # print the item's status
//   bun run scripts/cws-status.ts --gate   # ...and exit 3 if a review is open
//
// Exists because a submission attempted while the item is review-locked fails
// with an unhelpful generic message ("Your submission does not meet the
// requirements to be published in the store") after the upload has already
// gone through, which reads like a listing defect and is not one. The v2
// publishers.items.fetchStatus endpoint answers the question directly:
// `submittedItemRevisionStatus.state == PENDING_REVIEW` means a review is
// open, so a publish will be refused.
//
// Requires the same credentials as submit-chrome.sh, plus CWS_PUBLISHER_ID
// (fetchStatus is v2-only; the retiring v1 API has no equivalent).

import { record } from "./json";

export type ItemRevision = {
  /** ItemState: PENDING_REVIEW, STAGED, PUBLISHED, PUBLISHED_TO_TESTERS, REJECTED, CANCELLED. */
  state: string | null;
  crxVersion: string | null;
};

export type ItemStatus = {
  /** A submitted revision is awaiting Google's review, so publishing is refused. */
  reviewOpen: boolean;
  published: ItemRevision | null;
  submitted: ItemRevision | null;
  /** One line per fact worth printing, in report order. */
  lines: string[];
};

function parseRevision(value: unknown): ItemRevision | null {
  const revision = record(value);
  if (revision === null) return null;
  const channels = Array.isArray(revision.distributionChannels) ? revision.distributionChannels : [];
  let crxVersion: string | null = null;
  for (const entry of channels) {
    const channel = record(entry);
    if (channel !== null && typeof channel.crxVersion === "string") {
      crxVersion = channel.crxVersion;
      break;
    }
  }
  return { state: typeof revision.state === "string" ? revision.state : null, crxVersion };
}

/** Turn a fetchStatus payload into the report and the one decision that gates a submission. */
export function summarizeItemStatus(payload: unknown): ItemStatus {
  const status = record(payload) ?? {};
  const published = parseRevision(status.publishedItemRevisionStatus);
  const submitted = parseRevision(status.submittedItemRevisionStatus);
  const reviewOpen = submitted?.state === "PENDING_REVIEW";
  const lines: string[] = [];

  const describe = (revision: ItemRevision): string =>
    revision.crxVersion === null
      ? (revision.state ?? "unknown state")
      : `${revision.state ?? "unknown state"} (${revision.crxVersion})`;

  lines.push(published === null ? "live: nothing published yet" : `live: ${describe(published)}`);
  lines.push(
    submitted === null
      ? "submitted: nothing pending since the last publish"
      : `submitted: ${describe(submitted)}`,
  );
  if (status.takenDown === true) {
    lines.push("TAKEN DOWN for a policy violation — check the dashboard");
  }
  if (status.warned === true) {
    lines.push("WARNED for a policy violation — check the dashboard");
  }
  if (reviewOpen) {
    lines.push(
      "a review is open: the Web Store locks the item, so an upload or publish will be refused",
    );
  }

  return { reviewOpen, published, submitted, lines };
}

async function accessToken(env: Record<string, string | undefined>): Promise<string> {
  const response = await fetch("https://oauth2.googleapis.com/token", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      client_id: env.CWS_CLIENT_ID ?? "",
      client_secret: env.CWS_CLIENT_SECRET ?? "",
      refresh_token: env.CWS_REFRESH_TOKEN ?? "",
      grant_type: "refresh_token",
    }),
  });
  const body = record(await response.json()) ?? {};
  if (typeof body.access_token !== "string") {
    const detail = typeof body.error_description === "string" ? body.error_description : response.status;
    throw new Error(`OAuth token refresh failed: ${detail}`);
  }
  return body.access_token;
}

export async function fetchItemStatus(env: Record<string, string | undefined>): Promise<unknown> {
  const token = await accessToken(env);
  const url = `https://chromewebstore.googleapis.com/v2/publishers/${env.CWS_PUBLISHER_ID}/items/${env.CWS_EXTENSION_ID}:fetchStatus`;
  const response = await fetch(url, { headers: { Authorization: `Bearer ${token}` } });
  const text = await response.text();
  if (!response.ok) throw new Error(`fetchStatus ${response.status}: ${text}`);
  return JSON.parse(text);
}

if (import.meta.main) {
  const gate = process.argv.slice(2).includes("--gate");
  const missing = [
    "CWS_CLIENT_ID",
    "CWS_CLIENT_SECRET",
    "CWS_REFRESH_TOKEN",
    "CWS_EXTENSION_ID",
    "CWS_PUBLISHER_ID",
  ].filter((name) => !process.env[name]);
  if (missing.length > 0) {
    console.error(`cws-status: missing ${missing.join(", ")} (see docs/chrome-web-store-listing.md)`);
    process.exit(2);
  }
  let status: ItemStatus;
  try {
    status = summarizeItemStatus(await fetchItemStatus(process.env));
  } catch (error) {
    console.error(`cws-status: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
  for (const line of status.lines) console.log(`    ${line}`);
  if (gate && status.reviewOpen) process.exit(3);
}
