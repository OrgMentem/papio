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

export type ItemRevisionStatus = {
  state?: string;
  distributionChannels?: { deployPercentage?: number; crxVersion?: string }[];
};

export type FetchItemStatusResponse = {
  itemId?: string;
  publishedItemRevisionStatus?: ItemRevisionStatus;
  submittedItemRevisionStatus?: ItemRevisionStatus;
  lastAsyncUploadState?: string;
  takenDown?: boolean;
  warned?: boolean;
};

export type ItemStatus = {
  /** A submitted revision is awaiting Google's review, so publishing is refused. */
  reviewOpen: boolean;
  liveVersion: string | null;
  submittedVersion: string | null;
  /** One line per fact worth printing, in report order. */
  lines: string[];
};

function version(revision: ItemRevisionStatus | undefined): string | null {
  const crx = revision?.distributionChannels?.find((channel) => channel.crxVersion)?.crxVersion;
  return crx ?? null;
}

function describe(revision: ItemRevisionStatus | undefined): string {
  const state = revision?.state ?? "unknown state";
  const crx = version(revision);
  return crx === null ? state : `${state} (${crx})`;
}

/** Turn a fetchStatus payload into the report and the one decision that gates a submission. */
export function summarizeItemStatus(status: FetchItemStatusResponse): ItemStatus {
  const submitted = status.submittedItemRevisionStatus;
  const reviewOpen = submitted?.state === "PENDING_REVIEW";
  const lines: string[] = [];

  lines.push(
    status.publishedItemRevisionStatus
      ? `live: ${describe(status.publishedItemRevisionStatus)}`
      : "live: nothing published yet",
  );
  lines.push(
    submitted
      ? `submitted: ${describe(submitted)}`
      : "submitted: nothing pending since the last publish",
  );
  if (status.takenDown) lines.push("TAKEN DOWN for a policy violation — check the dashboard");
  if (status.warned) lines.push("WARNED for a policy violation — check the dashboard");
  if (reviewOpen) {
    lines.push(
      "a review is open: the Web Store locks the item, so an upload or publish will be refused",
    );
  }

  return { reviewOpen, liveVersion: version(status.publishedItemRevisionStatus), submittedVersion: version(submitted), lines };
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
  const body = (await response.json()) as { access_token?: string; error_description?: string };
  if (!body.access_token) {
    throw new Error(`OAuth token refresh failed: ${body.error_description ?? response.status}`);
  }
  return body.access_token;
}

export async function fetchItemStatus(
  env: Record<string, string | undefined>,
): Promise<FetchItemStatusResponse> {
  const token = await accessToken(env);
  const url = `https://chromewebstore.googleapis.com/v2/publishers/${env.CWS_PUBLISHER_ID}/items/${env.CWS_EXTENSION_ID}:fetchStatus`;
  const response = await fetch(url, { headers: { Authorization: `Bearer ${token}` } });
  const text = await response.text();
  if (!response.ok) throw new Error(`fetchStatus ${response.status}: ${text}`);
  return JSON.parse(text) as FetchItemStatusResponse;
}

if (import.meta.main) {
  const gate = process.argv.slice(2).includes("--gate");
  const missing = ["CWS_CLIENT_ID", "CWS_CLIENT_SECRET", "CWS_REFRESH_TOKEN", "CWS_EXTENSION_ID", "CWS_PUBLISHER_ID"].filter(
    (name) => !process.env[name],
  );
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
