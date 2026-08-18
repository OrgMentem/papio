// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The `storage.local` birth-certificate ledger: a URL-free successor to the
// raw-URL managed-tab ledger (Slice 2a of dev/active/surface-lifecycle-plan.md).
// No route URLs, titles, DOIs, hosts, or entity material are ever recorded
// here (ADR-0022 Decision 1) — only an opaque binding identity and enough
// bookkeeping to authorize adoption and closure in later slices. This module
// is intentionally standalone: it imports nothing from background.ts/state.ts,
// only web-platform APIs, so it can be wired into the Bridge in Slice 2b
// without dragging that module's surface along.

/**
 * A birth certificate for one extension-owned surface (tab), persisted in
 * `storage.local`. Replaces the pre-v2 raw-URL ledger entry.
 */
export interface SurfaceBirthRecord {
  /** Opaque identity of the daemon-side binding this surface represents.
   * Daemon-issued once Slice 4 lands; until that cutover it is minted
   * locally (a fresh UUID per record via the caller-supplied `mintBinding`)
   * and carries no meaning beyond distinguishing one record from another. */
  binding_id: string;
  /** Last known tab ID. Advisory only: never trusted without a fresh
   * `tabs.get` re-proof, and never authoritative across a browser restart —
   * see `browser_epoch`. */
  tab_hint: number;
  /** Why this surface exists (e.g. `"legacy"` for entries migrated from the
   * pre-v2 raw-URL ledger). */
  purpose: string;
  /** Browser-session epoch this record was created under. A browser restart
   * invalidates every prior epoch's tab-ID authority, so `tab_hint` from a
   * stale epoch must be re-proven live before use. `"pre-v2"` is the
   * migration sentinel: entries carried over from the raw-URL ledger never
   * had an epoch at all, so their tab-ID hints are untrusted until the
   * surface re-proves itself live. */
  browser_epoch: string;
  /** Extension build generation this record was created under. */
  extension_generation: string;
  /** Creation timestamp (ms since epoch, from the injectable clock). */
  created_at: number;
  /** SHA-256 hex digest of the surface's origin (scheme + host + port),
   * lowercased — never the raw URL itself. Absent when no origin was known
   * or digestible (fail closed to no-digest, never a raw fallback). */
  origin_digest?: string;
  /** Daemon job this surface currently serves, when known. */
  job_id?: string;
  /** Set once the surface is ceded to the operator: the binding is
   * detached, the record is retained for accounting, and it no longer
   * authorizes automation. */
  ceded?: boolean;
  /** True only for entries migrated from the pre-v2 raw-URL ledger whose
   * provenance cannot be re-verified (e.g. no jobID to correlate against).
   * Marks a record listed for one-time manual review rather than
   * auto-adopted. */
  legacy?: true;
  /** Pending close-transaction tombstone (Slice 2b), persisted before
   * `tabs.remove` so a failed remove or worker death can be reconciled with
   * the daemon at startup instead of silently losing the record. */
  pending_close?: {
    authorization_id: string;
    nonce: string;
    holder_generation: number;
    recorded_at: number;
  };
}

/** The pre-v2 raw-URL managed-tab ledger entry shape (see
 * `ManagedTabLedgerEntry` in background.ts). Only the fields this module
 * consumes are declared; the raw-URL `url` field is read once to compute an
 * `origin_digest` and is never retained. */
interface LegacyTabLedgerEntry {
  openedAt?: number;
  url?: string;
  jobID?: string;
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Defensive structural check for whatever `storage.local` actually holds
 * (foreign extension write, a stale schema from a prior version, or a
 * hand-edited value) before trusting it as a migrated `SurfaceBirthRecord`.
 * Strict: every required field is type-checked, every present optional
 * field is type-checked, and a `url` property — the one thing this record
 * shape must never carry — is an automatic rejection.
 */
export function isSurfaceBirthRecord(value: unknown): value is SurfaceBirthRecord {
  if (!isPlainRecord(value)) return false;
  if ("url" in value) return false;
  if (typeof value.binding_id !== "string" || value.binding_id.length === 0) return false;
  if (typeof value.tab_hint !== "number" || !Number.isFinite(value.tab_hint)) return false;
  if (typeof value.purpose !== "string" || value.purpose.length === 0) return false;
  if (typeof value.browser_epoch !== "string" || value.browser_epoch.length === 0) return false;
  if (
    typeof value.extension_generation !== "string" ||
    value.extension_generation.length === 0
  )
    return false;
  if (typeof value.created_at !== "number" || !Number.isFinite(value.created_at)) return false;
  if (value.origin_digest !== undefined && typeof value.origin_digest !== "string") return false;
  if (value.job_id !== undefined && typeof value.job_id !== "string") return false;
  if (value.ceded !== undefined && typeof value.ceded !== "boolean") return false;
  if (value.legacy !== undefined && value.legacy !== true) return false;
  if (value.pending_close !== undefined) {
    if (!isPlainRecord(value.pending_close)) return false;
    const pending = value.pending_close;
    if (typeof pending.authorization_id !== "string") return false;
    if (typeof pending.nonce !== "string") return false;
    if (typeof pending.holder_generation !== "number") return false;
    if (typeof pending.recorded_at !== "number") return false;
  }
  return true;
}

/**
 * SHA-256 hex digest of a URL's origin (scheme + host + port), lowercased —
 * mirrors the digest style `federatedLoginClaimKey`/`termsAuthorityDigest`
 * use in background.ts. Fails closed to `undefined` (never a raw-URL
 * fallback) when the URL cannot be parsed or `crypto.subtle` is
 * unavailable in the current context.
 */
export async function originDigestOf(rawURL: string): Promise<string | undefined> {
  let origin: string;
  try {
    origin = new URL(rawURL).origin.toLowerCase();
  } catch {
    return undefined;
  }
  const subtle = globalThis.crypto?.subtle;
  if (subtle === undefined) return undefined;
  try {
    const digest = await subtle.digest("SHA-256", new TextEncoder().encode(origin));
    return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join(
      "",
    );
  } catch {
    return undefined;
  }
}

/** Migrate one pre-v2 ledger entry keyed by tab ID. Returns `undefined` for
 * anything unrecognizable (fail closed: never throw, never guess). */
async function migrateLegacyEntry(
  key: string,
  value: unknown,
  mintBinding: () => string,
  now: () => number,
): Promise<{ record: SurfaceBirthRecord; needsReview: boolean } | undefined> {
  if (!/^-?\d+$/.test(key)) return undefined;
  if (!isPlainRecord(value)) return undefined;
  const entry = value as LegacyTabLedgerEntry;
  if ("url" in entry && typeof entry.url !== "string") return undefined;
  const hasOpenedAt = typeof entry.openedAt === "number" && Number.isFinite(entry.openedAt);
  const hasURL = typeof entry.url === "string" && entry.url.length > 0;
  if (!hasOpenedAt && !hasURL) return undefined;
  const jobID = typeof entry.jobID === "string" && entry.jobID.length > 0 ? entry.jobID : undefined;
  const originDigest = hasURL ? await originDigestOf(entry.url as string) : undefined;
  const needsReview = jobID === undefined;
  const record: SurfaceBirthRecord = {
    binding_id: mintBinding(),
    tab_hint: Number(key),
    purpose: "legacy",
    browser_epoch: "pre-v2",
    extension_generation: "pre-v2",
    created_at: hasOpenedAt ? (entry.openedAt as number) : now(),
    ...(originDigest === undefined ? {} : { origin_digest: originDigest }),
    ...(jobID === undefined ? {} : { job_id: jobID }),
    ...(needsReview ? { legacy: true as const } : {}),
  };
  return { record, needsReview };
}

/**
 * Migrate a `storage.local` tab ledger to the URL-free birth-certificate
 * shape. Accepts either the pre-v2 raw-URL ledger (keyed by `String(tabID)`,
 * entries like `{ openedAt, url, jobID? }`) or an already-migrated ledger
 * (idempotent pass-through, structurally validated) — the two may also be
 * mixed within one input during a partial migration. The raw `url` of a
 * legacy entry is read only to derive `origin_digest`, then dropped: no
 * output record ever carries a URL. Entries without a `jobID` are retained
 * with `legacy: true` and their original tab-key listed in `review` for
 * manual reconciliation. Malformed or unrecognizable entries are dropped
 * silently — this never throws, and unrecognizable `raw` yields an empty
 * ledger with an empty review list.
 */
export async function migrateTabLedger(
  raw: unknown,
  mintBinding: () => string,
  now: () => number,
): Promise<{ ledger: Record<string, SurfaceBirthRecord>; review: string[] }> {
  const ledger: Record<string, SurfaceBirthRecord> = {};
  const review: string[] = [];
  if (!isPlainRecord(raw)) return { ledger, review };
  for (const [key, value] of Object.entries(raw)) {
    if (isSurfaceBirthRecord(value)) {
      ledger[key] = value;
      continue;
    }
    const migrated = await migrateLegacyEntry(key, value, mintBinding, now);
    if (migrated === undefined) continue;
    ledger[key] = migrated.record;
    if (migrated.needsReview) review.push(key);
  }
  return { ledger, review };
}
