// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Report the AMO listing's version and review state before a submission.
//
//   bun run scripts/amo-status.ts          # print the listing's versions
//   bun run scripts/amo-status.ts --gate   # ...and exit 3 if this version was
//                                          #    already uploaded
//
// AMO and the Chrome Web Store block on different things. AMO reviews per
// version, so a version sitting in the queue does NOT lock the add-on — a new
// version can be uploaded alongside it. What AMO rejects outright is a version
// NUMBER that has ever been uploaded, on either channel ("Version X already
// exists"), including unlisted self-distribution builds that never appeared on
// the public listing. So the gate here is the duplicate version; an open review
// is reported for judgement, not enforced.
//
// Credentials are the same account-wide AMO API key submit-firefox.sh uses:
//   WEB_EXT_API_KEY     JWT issuer
//   WEB_EXT_API_SECRET  JWT secret

import { createHmac, randomBytes } from "node:crypto";

import { record } from "./json";

export type AmoVersion = {
  version: string;
  /** "listed" or "unlisted"; unlisted versions still consume the version number. */
  channel: string | null;
  /** AMO file status: public = approved, unreviewed = awaiting review, disabled = rejected/disabled. */
  status: string | null;
};

export type AmoStatus = {
  /** This version number was already uploaded, so AMO will reject the submission. */
  versionExists: boolean;
  /** Some version is still in AMO's review queue — worth knowing, but not blocking. */
  reviewOpen: boolean;
  latestListed: AmoVersion | null;
  lines: string[];
};

/** Pull the version rows out of an AMO version-list payload, dropping anything malformed. */
export function parseAmoVersions(payload: unknown): AmoVersion[] {
  const body = record(payload);
  const results = body !== null && Array.isArray(body.results) ? body.results : [];
  const versions: AmoVersion[] = [];
  for (const entry of results) {
    const row = record(entry);
    if (row === null || typeof row.version !== "string") continue;
    const file = record(row.file);
    versions.push({
      version: row.version,
      channel: typeof row.channel === "string" ? row.channel : null,
      status: file !== null && typeof file.status === "string" ? file.status : null,
    });
  }
  return versions;
}

/** Turn the version list into the report and the one decision that gates a submission. */
export function summarizeAmoStatus(versions: AmoVersion[], manifestVersion: string): AmoStatus {
  const versionExists = versions.some((entry) => entry.version === manifestVersion);
  const awaiting = versions.filter((entry) => entry.status === "unreviewed");
  const latestListed = versions.find((entry) => entry.channel !== "unlisted") ?? null;
  const lines: string[] = [];

  const describe = (entry: AmoVersion): string =>
    `${entry.version} — ${entry.status ?? "unknown status"} (${entry.channel ?? "unknown channel"})`;

  lines.push(latestListed === null ? "latest listed: none" : `latest listed: ${describe(latestListed)}`);
  if (awaiting.length === 0) {
    lines.push("awaiting review: nothing in the queue");
  } else {
    for (const entry of awaiting) lines.push(`awaiting review: ${describe(entry)}`);
  }
  lines.push(
    versionExists
      ? `${manifestVersion} was already uploaded — AMO rejects a duplicate version number on every channel`
      : `${manifestVersion} is unused on AMO`,
  );

  return { versionExists, reviewOpen: awaiting.length > 0, latestListed, lines };
}

export async function fetchAmoVersions(
  env: Record<string, string | undefined>,
  addonID: string,
): Promise<unknown> {
  const issued = Math.floor(Date.now() / 1000);
  const encode = (value: object): string => Buffer.from(JSON.stringify(value)).toString("base64url");
  // AMO refuses a token whose lifetime exceeds five minutes; this one outlives
  // a single request and nothing else.
  const claims = `${encode({ alg: "HS256", typ: "JWT" })}.${encode({
    iss: env.WEB_EXT_API_KEY ?? "",
    jti: randomBytes(16).toString("hex"),
    iat: issued,
    exp: issued + 60,
  })}`;
  const signature = createHmac("sha256", env.WEB_EXT_API_SECRET ?? "").update(claims).digest("base64url");

  const url = `https://addons.mozilla.org/api/v5/addons/addon/${addonID}/versions/?filter=all_with_unlisted&page_size=50`;
  const response = await fetch(url, { headers: { Authorization: `JWT ${claims}.${signature}` } });
  const text = await response.text();
  // 404 = the add-on has no versions AMO will admit to yet (first submission).
  if (response.status === 404) return { results: [] };
  if (!response.ok) throw new Error(`AMO versions ${response.status}: ${text}`);
  return JSON.parse(text);
}

if (import.meta.main) {
  const gate = process.argv.slice(2).includes("--gate");
  const missing = ["WEB_EXT_API_KEY", "WEB_EXT_API_SECRET"].filter((name) => !process.env[name]);
  if (missing.length > 0) {
    console.error(`amo-status: missing ${missing.join(", ")} (see docs/amo-listing.md)`);
    process.exit(2);
  }
  // manifest.json is the version source of truth; the gecko id is injected
  // into firefox/manifest.json by build.ts, so read the built manifest when a
  // build exists and fall back to the id build.ts writes.
  const manifest = record(await Bun.file(new URL("../manifest.json", import.meta.url)).json()) ?? {};
  const version = typeof manifest.version === "string" ? manifest.version : "";
  const built = Bun.file(new URL("../firefox/manifest.json", import.meta.url));
  const gecko = (await built.exists())
    ? record(record(record(await built.json())?.browser_specific_settings)?.gecko)
    : null;
  const addonID = typeof gecko?.id === "string" ? gecko.id : "papio@orgmentem.com";

  let status: AmoStatus;
  try {
    status = summarizeAmoStatus(parseAmoVersions(await fetchAmoVersions(process.env, addonID)), version);
  } catch (error) {
    console.error(`amo-status: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
  for (const line of status.lines) console.log(`    ${line}`);
  if (gate && status.versionExists) process.exit(3);
}
