// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable-ish tab/job correlation for the MV3 bridge. The service worker may be
// stopped at any time, so the small amount of state that must survive a restart
// lives in chrome.storage (session preferred, local fallback). Everything here
// is pure over an injected StateBackend so it is unit-testable without chrome.
//
// Privacy invariant: no identity-provider URL, host, title, query, or fragment
// is persisted. Legacy URL-bearing state is migrated to the URL-free managed
// shape below; handoff URLs remain worker-local compatibility data only.

import type { DeliverySessionEvidence } from "./protocol";
import type { FederatedClaimPhase } from "./federated-claim";

export type JobStatus = "offered" | "queued" | "accepted" | "auth_pending" | "awaiting_download";

/** Browser-managed delivery correlation. The source URL is worker-local and
 * intentionally omitted by the managed-state migration/serializer. */
export type PendingDeliveryStatus = "sending" | "downloaded" | "failed";
export interface PendingDelivery {
  job_id: string;
  /** Present only while this worker is alive; never written to managed state. */
  url?: string;
  initiated_at: number;
  status?: PendingDeliveryStatus;
  error?: string;
  /** Host of the page that requested the delivery, frozen at request time.
   * The download outlives the navigation that started it, so the provenance
   * host cannot be re-read from the tab once the bytes land. */
  page_host?: string;
  /** Session evidence available at the moment the delivery was requested,
   * frozen alongside page_host. */
  session_evidence?: DeliverySessionEvidence;
}

/** Durable, informed user choice for auto-accepting publisher terms &
 * conditions. `undefined` = never asked; `"accept"` = consented to papio
 * agreeing to publisher T&C on their behalf; `"manual"` = will accept manually. */
export type TermsConsent = "accept" | "manual" | undefined;
export const TERMS_CONSENT_KEY = "papio_terms_consent_v1";
/** Durable one-time consent for transmitting sanitized failure page captures
 * from Firefox < 140 to the local papio daemon. Absent/false means off. */
export const PAGE_CAPTURE_CONSENT_KEY = "papio_page_capture_consent_v1";


/** Durable user choice for the dedicated background work window. `false`
 * disables routing and restores legacy in-window tabs; absent means enabled.
 * Retained for backward compatibility; new installs use HANDOFF_SURFACE_KEY. */
export const WORK_WINDOW_KEY = "papio_work_window_v1";

/** Where papio opens handoff tabs.
 * - `in-window`: in the user's current window (legacy visible tabs).
 * - `work-window`: one dedicated minimized background window.
 * - `tab-group`: a collapsed "papio" tab group in the user's current window.
 * Absent = derive from WORK_WINDOW_KEY (true/absent -> work-window, false ->
 * in-window) so existing installs keep their behavior. */
export type HandoffSurface = "in-window" | "work-window" | "tab-group";
export const HANDOFF_SURFACE_KEY = "papio_handoff_surface_v1";

/** Durable ledger of broker tabs papio created (chrome.storage.local):
 * stringified tab id -> opened-at ms. The session store is wiped by an
 * extension reload; this ledger is what lets the next life recognize —
 * and offer to close — tabs it can no longer track. */
export const MANAGED_TAB_LEDGER_KEY = "papio_managed_tabs_v1";

/** Native-daemon compatibility as last reported by the bridge. `undefined`
 * remains valid for state persisted by earlier extension versions. */
export type DaemonConnectionStatus = "connected" | "disconnected" | "daemon_outdated" | "extension_outdated";

/** Durable drive epoch for one daemon-selected provider candidate. The URL is
 * intentionally absent: only the daemon's opaque attempt and public route
 * revision survive a worker restart. */
export interface ProviderDriveEpoch {
  drive_attempt_id: string;
  ordinal: number;
  strategy: "direct" | "generic";
  route_revision?: string;
  revision?: string;
  strategy_id?: string;
  in_flight_download_id?: number;
  attempt_count: number;
}

/** Deterministic browser filename used while a provider download is being
 * created. It is the only restart-time correlation available before
 * chrome.downloads.download returns its ID; no URL is persisted or searched. */
export function jobDownloadFilename(jobID: string): string {
  return `papio/${jobID}/paper.pdf`;
}
/** Minimal daemon-minted direct-route envelope retained for MV3 restart
 * correlation. It deliberately excludes the resolved URL and query. */
export interface DirectEnvelopeCorrelation {
  allowed_origin: string;
  path_family: string;
  expected_identifier: string;
}


export interface ActiveJob {
  job_id: string;
  tab_id: number;
  offered_at: number;
  expires_at: number;
  status: JobStatus;
  /** Provider hosts from the job offer; needed to recognise return-to-provider
   * navigations locally. Not sensitive — these are the resolver's declared
   * destinations, never an IdP address. */
  provider_hosts: string[];
  /** Access policy retained from the offer for local authority checks.
   * Only the exact `"delegated"` value authorizes autonomous effects.
   * Legacy jobs without this field remain parked until explicit operator
   * engagement. */
  access_mode?: "assisted" | "delegated";
  /** Epoch ms when the tab first left every provider host (auth started). */
  auth_started_ms?: number;
  /** Expected work identity from the job offer, used to build the adapter
   * AdapterContext for declarative classification. Resolver-declared hints
   * only — never an IdP value. */
  expected?: { title?: string; doi?: string };
  /** True when the resolver says this offer needs a warm institutional session.
   * Queued handoffs retain it so a fallback never mints a sign-in request early. */
  requires_auth?: boolean;
  /** Durable direct tuple terminal marker; it carries no URL and suppresses
   * duplicate/late requests for the same daemon-minted attempt. */
  direct_terminal?: boolean;
  /** Durable daemon-minted direct-route attempt correlation. Full URLs and
   * page-derived evidence are never persisted. */
  drive_epoch?: ProviderDriveEpoch;
  /** Direct route envelope needed to classify the browser download after a
   * worker restart. The tuple and download id remain in drive_epoch. */
  direct_envelope?: DirectEnvelopeCorrelation;
  /** Durable generic strategy tuple; candidate URLs remain worker-local. */
  generic_drive_epoch?: ProviderDriveEpoch;
  /** Generic epoch bookkeeping is opaque correlation only. Candidate URLs and
   * page-derived evidence remain worker-local. */
  generic_evaluated?: boolean;
  generic_positive_attempts?: number;
  generic_attempted_strategies?: string[];
  generic_terminal?: boolean;
  /** Cold auth offers wait for explicit inbox/popup engagement before opening
   * a managed tab. */
  engagement_required?: boolean;
  /** Opaque, versioned digest used to coordinate one institution's cold
   * engagement without persisting its entity ID or resolver route. */
  institution_claim_key?: string;
  /** This job was accepted under handoff_link_v1, so no reusable URL exists
   * after engagement. Persisted because feature negotiation can be transiently
   * absent while a drive timeout fires. */
  fresh_handoff?: boolean;
  /** One-download-initiation-per-job latch. Once an adapter has clicked the
   * declared download target, it can never click again for this job. The
   * source-controlled adapter id allows concurrent provider downloads to be
   * correlated without persisting a page URL, referrer, or live host. */
  download_initiated?: boolean;
  adapter_id?: string;
  /** Consecutive `unknown` classification streak, and the epoch-ms of the
   * streak's first observation, for the 2×(≥5s apart) ui_changed debounce. */
  unknown_count?: number;
  last_unknown_ms?: number;
  /** Set when the adapter hit a terms-and-conditions gate while the user has
   * not yet recorded a consent choice, so the popup can surface the one-time
   * informed-consent prompt. Cleared once consent is decided. */
  needs_terms_consent?: boolean;
  /** Exact provider host where a tracked handoff could not be read because
   * the browser lacks effective access. It contains no path, query, or IdP
   * data and lets the report debounce survive an MV3 worker restart. */
  blocked_provider_host?: string;
  /** True when the live broker tab is stopped on a provider security check or
   * redirect-loop dead end. The tab remains open for the operator. */
  challenge_blocked?: boolean;
  /** Registrable provider host where the challenge was observed. Never an IdP
   * host or a URL; retained only with the active job. */
  challenge_host?: string;
  challenge_kind?: "cloudflare" | "redirect_loop";
  challenge_blocked_at?: number;
  /** A re-offered handoff held behind a parked provider has not yet been
   * accepted. It is acknowledged only when that provider is resumed. */
  handoffAckPending?: boolean;
  /** Set when the 3-minute handoff-drive timeout parks this job with its tab
   * intentionally preserved (the tab was sitting on a recognized
   * authentication page, so closing it would destroy a half-completed
   * SSO/2FA form). The governor slot is released at the same moment
   * (parkHandoffForManual), which otherwise leaves `auth_pending` +
   * `tab_id >= 0` indistinguishable from a job still actively driven and
   * mid-timeout. A service-worker restart cannot tell the two apart by
   * inference alone (that ambiguity is exactly what let a restart
   * re-consume this job's already-freed slot and re-arm a fresh timeout
   * indefinitely), so the startup restore scan in background.ts checks
   * this bit instead. Cleared the moment the job is driven again
   * (registerHandoffDrive) or the operator finishes authenticating in this
   * same tab without a fresh drive (the auth_pending -> awaiting_download
   * transition in the tab-update handler) — never left stale, or the job
   * would be skipped by every future restore forever. */
  parked_with_tab?: boolean;
  /** Set when this handoff's classify verdict was "login" and its
   * federated-login claim key (federatedLoginOwners, below — the IdP origin
   * maybeRouteFederatedLogin would navigate to, PLUS the offer's entityID:
   * a shared WAYF/Discovery-Service host serving many institutions exposes
   * only ONE origin, so entityID is the axis that actually distinguishes
   * them) already has a live sibling tab driving that sign-in — so this tab
   * is deliberately left on the provider's login wall instead of opening a
   * second, redundant IdP tab. A distinct marker from parked_with_tab (which
   * it is always set alongside whenever a tab is preserved) so UI copy and
   * the multiple resume paths below can tell "this job's own timeout/
   * challenge park" apart from "this job is only waiting on ANOTHER job's
   * shared institution sign-in". Resumes on: the claim's owner finishing
   * (recordInstitutionalSession, unconditionally — even when this
   * institution's evidence was already warm), the owner's claim retiring
   * for any reason (clearFederatedLoginOwner, which always resumes that
   * claim's own waiters — never leaves one ownerless), or fresh session
   * evidence for the same institution once its claim is no longer live. It
   * stays parked until one of those real events frees it. Cleared the moment
   * job drives again (registerHandoffDrive, via clearParkedMarker) or, if its
   * tab closes while parked, when onTabRemoved demotes it to an ordinary
   * queued drive. */
  waiting_for_session?: boolean;
  /** Opaque versioned SHA-256 digest of the institution entity ID. The raw
   * IdP origin and entityID are never persisted; this key exists only for
   * equality against federatedLoginOwners. */
  waiting_for_session_key?: string | undefined;
  /** Absolute epoch ms used only as a display hint for the inbox waiting
   * overlay. It does not demote a waiting_for_session park; the marker stays
   * set until owner removal, startup owner validation, or fresh session
   * evidence resumes the job. Persisted so the overlay can retain its
   * original timestamp across a service-worker restart and re-park. */
  waiting_deadline?: number | undefined;
}
/** Cross-job record of the one live tab currently driving federated login for
 * an opaque SHA-256 digest of the institution entity ID. Resolver and IdP
 * origins are deliberately excluded because discovery services are shared.
 * Lets multiple papers needing the same institution share one login tab:
 * a job whose login verdict resolves to an existing digest parks as a waiter.
 * Session storage preserves the claim across service-worker restarts;
 * reconciliation drops stale owners and resumes their waiters. The owning
 * tab closing, returning from authentication, or its job ending retires it. */
export interface FederatedLoginOwner {
  jobID: string;
  tabID: number;
  /** "engaging" reserves a cold click before daemon RPC; "auth" is an
   * in-flight federated login after the provider login verdict. */
  phase: FederatedClaimPhase;
}
/** A short, browser-session lease over one provider's queued handoffs. The
 * owner token stays only in the service worker; session storage retains this
 * non-secret recovery record so a restarted worker observes the same park. */
export interface ProviderDrainLease {
  providerKey: string;
  expiresAt: number;
  parkedReason?: "challenge";
}


export interface StoreShape {
  activeJobs: ActiveJob[];
  /** The one operator-initiated PDF delivery currently awaiting daemon
   * adoption. URL is worker-local and is not durable. */
  pendingDelivery?: PendingDelivery;
  /** Resolver-provided offer URLs remain worker-local compatibility data and
   * are always dropped by managed persistence. */
  offerURLs?: Record<string, string>;
  /** Most recent auth return, retained as a display hint. */
  lastAuthReturnedAt?: number;
  /** Release-grade evidence by configured resolver origin. */
  authEvidenceByOrigin?: Record<string, number>;
  /** Per-job authentication attempt counts. */
  authAttempts?: Record<string, number>;
  /** Per-provider drain leases keyed by canonical provider host. */
  providerDrainLeases?: Record<string, ProviderDrainLease>;
  /** Session-scoped work window and tab-group ids. */
  workWindowID?: number;
  handoffGroupID?: number;
  /** Native-daemon connection and capability data from hello_ack. */
  connectionStatus?: DaemonConnectionStatus;
  daemonVersion?: string | null;
  daemonUpdateHint?: boolean;
  daemonFeatures?: string[];
  /** Configured resolver origins, normalized to origins during migration. */
  resolverOrigins?: string[];
  /** Provider hosts currently blocked by browser access. */
  blockedProviderHosts?: string[];
  /** Provider-host security-check cooldowns. */
  challengeCooldowns?: Record<string, number>;
  /** Legacy browser claim map; never promoted by managed-state migration. */
  federatedLoginOwners?: Record<string, FederatedLoginOwner>;
}

/** Async key/value seam. The real implementation wraps chrome.storage; tests
 * inject an in-memory fake. */
export interface StateBackend {
  load(): Promise<StoreShape>;
  save(store: StoreShape): Promise<void>;
}

export function emptyStore(): StoreShape {
  return {
    activeJobs: [],
    connectionStatus: "disconnected",
    daemonVersion: null,
    daemonUpdateHint: false,
    daemonFeatures: [],
  };
}

export function findByJob(store: StoreShape, jobID: string): ActiveJob | undefined {
  return store.activeJobs.find((j) => j.job_id === jobID);
}

export function findByTab(store: StoreShape, tabID: number): ActiveJob | undefined {
  return store.activeJobs.find((j) => j.tab_id === tabID);
}

/** Insert or replace a job (matched by job_id), returning a new store. */
export function upsertJob(store: StoreShape, job: ActiveJob): StoreShape {
  const activeJobs = store.activeJobs.filter((j) => j.job_id !== job.job_id);
  activeJobs.push(job);
  return { ...store, activeJobs };
}

export function removeJob(store: StoreShape, jobID: string): StoreShape {
  return { ...store, activeJobs: store.activeJobs.filter((j) => j.job_id !== jobID) };
}

/** Return a new store with the named job patched. No-op if the job is gone. */
export function patchJob(
  store: StoreShape,
  jobID: string,
  patch: Partial<Omit<ActiveJob, "job_id">>,
): StoreShape {
  return {
    ...store,
    activeJobs: store.activeJobs.map((j) => (j.job_id === jobID ? { ...j, ...patch } : j)),
  };
}
/** Atomically reserve the one download initiation allowed for a job.
 * Callers apply this transform synchronously inside their state transaction;
 * a competing classification therefore observes the monotone latch instead
 * of racing on an older snapshot. */
export function claimJobDownloadInitiated(
  store: StoreShape,
  jobID: string,
): { store: StoreShape; claimed: boolean } {
  const job = findByJob(store, jobID);
  if (job === undefined || job.download_initiated === true) return { store, claimed: false };
  return {
    store: patchJob(store, jobID, { download_initiated: true }),
    claimed: true,
  };
}


export function startPendingDelivery(store: StoreShape, delivery: PendingDelivery): StoreShape {
  return { ...store, pendingDelivery: { ...delivery, status: delivery.status ?? "sending" } };
}

/** Patch only the currently tracked delivery; a late download event from an
 * older job must not overwrite a newer operator action. */
export function updatePendingDelivery(
  store: StoreShape,
  jobID: string,
  patch: Partial<Omit<PendingDelivery, "job_id">>,
): StoreShape {
  const current = store.pendingDelivery;
  if (current === undefined || current.job_id !== jobID) return store;
  return { ...store, pendingDelivery: { ...current, ...patch } };
}

export function clearPendingDelivery(store: StoreShape, jobID?: string): StoreShape {
  if (store.pendingDelivery === undefined || (jobID !== undefined && store.pendingDelivery.job_id !== jobID)) return store;
  const next = { ...store };
  delete next.pendingDelivery;
  return next;
}

/** Version of the durable managed-state shape. The storage key predates this
 * field, so version 1 means the unversioned `papio_state_v1` blob. */
export const MANAGED_STATE_VERSION = 2;
const STORAGE_KEY = "papio_state_v1";
type UnknownRecord = Record<string, unknown>;

function isRecord(value: unknown): value is UnknownRecord {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}
function safeHost(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const host = value.trim().toLowerCase();
  if (host.length === 0 || host.length > 253) return undefined;
  const labels = host.split(".");
  if (labels.some((label) =>
    label.length === 0 ||
    label.length > 63 ||
    !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label)
  )) return undefined;
  return host;
}

function isURLLike(value: string): boolean {
  const trimmed = value.trim();
  return /^(?:https?|ftp|chrome|moz-extension|file|papio):/i.test(trimmed) ||
    /^(?:[a-z][a-z\d+.-]*:)?\/\//i.test(trimmed);
}

function safeOrigin(value: unknown): string | undefined {
  if (typeof value !== "string" || value.trim() === "") return undefined;
  try {
    const parsed = new URL(value);
    if ((parsed.protocol !== "https:" && parsed.protocol !== "http:") ||
        parsed.pathname !== "/" || parsed.search !== "" || parsed.hash !== "") return undefined;
    return parsed.origin;
  } catch {
    return undefined;
  }
}

function forbiddenPersistedKey(key: string): boolean {
  const normalized = key.replace(/[-_]/g, "").toLowerCase();
  const globalTermsAuthority = normalized !== "needstermsconsent" &&
    normalized.includes("terms") &&
    (normalized.includes("consent") || normalized.includes("accept") || normalized.includes("authority"));
  return normalized.includes("url") ||
    globalTermsAuthority ||
    normalized.includes("claim") ||
    normalized.includes("authkey") ||
    normalized.includes("authhash") ||
    normalized.includes("authdigest") ||
    normalized.includes("institutionhash");
}

/** Copy legacy data while dropping URL-bearing and authority-bearing leaves.
 * This is deliberately recursive: old worker versions added fields over time,
 * and an unknown nested URL must never become durable merely because its parent
 * was known. */
function scrubValue(value: unknown, key = ""): unknown {
  if (forbiddenPersistedKey(key)) return undefined;
  if (typeof value === "string") return isURLLike(value) ? undefined : value;
  if (Array.isArray(value)) {
    return value.map((item) => scrubValue(item)).filter((item) => item !== undefined);
  }
  if (!isRecord(value)) return value;
  const out: UnknownRecord = {};
  for (const [childKey, childValue] of Object.entries(value)) {
    const scrubbed = scrubValue(childValue, childKey);
    if (scrubbed !== undefined) out[childKey] = scrubbed;
  }
  return out;
}

function scrubRecord(value: unknown): UnknownRecord | undefined {
  const scrubbed = scrubValue(value);
  return isRecord(scrubbed) ? scrubbed : undefined;
}
const JOB_STATUSES: readonly JobStatus[] = ["offered", "queued", "accepted", "auth_pending", "awaiting_download"];
const DELIVERY_STATUSES: readonly PendingDeliveryStatus[] = ["sending", "downloaded", "failed"];

function validActiveJob(value: unknown): value is ActiveJob {
  if (!isRecord(value) ||
      typeof value.job_id !== "string" ||
      typeof value.tab_id !== "number" ||
      !Number.isInteger(value.tab_id) ||
      !isFiniteNumber(value.offered_at) ||
      !isFiniteNumber(value.expires_at) ||
      typeof value.status !== "string" ||
      !JOB_STATUSES.includes(value.status as JobStatus) ||
      !Array.isArray(value.provider_hosts)) return false;
  const providerHosts: unknown[] = value.provider_hosts;
  return providerHosts.every((host) => safeHost(host) !== undefined);
}
function migratedDriveEpoch(value: unknown): ProviderDriveEpoch | undefined {
  if (!isRecord(value) || typeof value.drive_attempt_id !== "string" || isURLLike(value.drive_attempt_id)) {
    return undefined;
  }
  const ordinal = value.ordinal;
  const strategy = value.strategy;
  const attemptCount = value.attempt_count;
  if (!isFiniteNumber(ordinal) || !Number.isInteger(ordinal) ||
      (strategy !== "direct" && strategy !== "generic") ||
      !isFiniteNumber(attemptCount) || !Number.isInteger(attemptCount)) return undefined;
  const epoch: ProviderDriveEpoch = {
    drive_attempt_id: value.drive_attempt_id,
    ordinal,
    strategy,
    attempt_count: attemptCount,
  };
  if (typeof value.route_revision === "string" && !isURLLike(value.route_revision)) {
    epoch.route_revision = value.route_revision;
  }
  if (typeof value.revision === "string" && !isURLLike(value.revision)) epoch.revision = value.revision;
  if (typeof value.strategy_id === "string" && !isURLLike(value.strategy_id)) epoch.strategy_id = value.strategy_id;
  const inFlightDownloadID = value.in_flight_download_id;
  if (isFiniteNumber(inFlightDownloadID) && Number.isInteger(inFlightDownloadID)) {
    epoch.in_flight_download_id = inFlightDownloadID;
  }
  return epoch;
}
function migratedJob(value: ActiveJob, droppedClaimOwnerJobIDs: ReadonlySet<string>): ActiveJob {
  const scrubbed = scrubRecord(value) ?? {};
  const migrated: ActiveJob = {
    ...scrubbed,
    job_id: value.job_id,
    tab_id: value.tab_id,
    offered_at: value.offered_at,
    expires_at: value.expires_at,
    status: value.status,
    provider_hosts: value.provider_hosts.map(safeHost).filter((host): host is string => host !== undefined),
  };
  const raw = value as unknown as UnknownRecord;
  const droppedWaitAuthority =
    droppedClaimOwnerJobIDs.has(value.job_id) ||
    raw.waiting_for_session === true ||
    raw.waiting_for_session_key !== undefined ||
    raw.institution_claim_key !== undefined;
  delete migrated.institution_claim_key;
  delete migrated.waiting_for_session_key;
  delete migrated.direct_envelope;
  if (isFiniteNumber(value.auth_started_ms)) migrated.auth_started_ms = value.auth_started_ms;
  const expectedRaw = value.expected;
  if (isRecord(expectedRaw)) {
    const expected: NonNullable<ActiveJob["expected"]> = {};
    if (typeof expectedRaw.title === "string" && !isURLLike(expectedRaw.title)) expected.title = expectedRaw.title;
    if (typeof expectedRaw.doi === "string" && !isURLLike(expectedRaw.doi)) expected.doi = expectedRaw.doi;
    if (Object.keys(expected).length > 0) migrated.expected = expected;
  }
  if (typeof value.requires_auth === "boolean") migrated.requires_auth = value.requires_auth;
  if (typeof value.direct_terminal === "boolean") migrated.direct_terminal = value.direct_terminal;
  const driveEpoch = migratedDriveEpoch(value.drive_epoch);
  if (driveEpoch !== undefined) migrated.drive_epoch = driveEpoch;
  const envelopeRaw = value.direct_envelope;
  if (isRecord(envelopeRaw)) {
    const allowedOrigin = safeOrigin(envelopeRaw.allowed_origin);
    const pathFamily = envelopeRaw.path_family;
    const expectedIdentifier = envelopeRaw.expected_identifier;
    if (allowedOrigin !== undefined &&
        typeof pathFamily === "string" && !isURLLike(pathFamily) &&
        typeof expectedIdentifier === "string" && !isURLLike(expectedIdentifier)) {
      migrated.direct_envelope = {
        allowed_origin: allowedOrigin,
        path_family: pathFamily,
        expected_identifier: expectedIdentifier,
      };
    }
  }
  const genericAttemptedStrategies = value.generic_attempted_strategies;
  if (Array.isArray(genericAttemptedStrategies)) {
    const strategies: unknown[] = genericAttemptedStrategies;
    migrated.generic_attempted_strategies = strategies
      .filter((strategy): strategy is string => typeof strategy === "string" && !isURLLike(strategy));
  }
  if (isFiniteNumber(value.generic_positive_attempts)) migrated.generic_positive_attempts = value.generic_positive_attempts;
  if (typeof value.generic_terminal === "boolean") migrated.generic_terminal = value.generic_terminal;
  if (typeof value.engagement_required === "boolean") migrated.engagement_required = value.engagement_required;
  if (typeof value.fresh_handoff === "boolean") migrated.fresh_handoff = value.fresh_handoff;
  if (typeof value.download_initiated === "boolean") migrated.download_initiated = value.download_initiated;
  const unknownCount = value.unknown_count;
  if (isFiniteNumber(unknownCount) && Number.isInteger(unknownCount)) migrated.unknown_count = unknownCount;
  if (isFiniteNumber(value.last_unknown_ms)) migrated.last_unknown_ms = value.last_unknown_ms;
  if (typeof value.needs_terms_consent === "boolean") migrated.needs_terms_consent = value.needs_terms_consent;
  const blockedProviderHost = safeHost(value.blocked_provider_host);
  if (blockedProviderHost !== undefined) migrated.blocked_provider_host = blockedProviderHost;
  if (typeof value.challenge_blocked === "boolean") migrated.challenge_blocked = value.challenge_blocked;
  const challengeHost = safeHost(value.challenge_host);
  if (challengeHost !== undefined) migrated.challenge_host = challengeHost;
  if (value.challenge_kind === "cloudflare" || value.challenge_kind === "redirect_loop") {
    migrated.challenge_kind = value.challenge_kind;
  }
  if (isFiniteNumber(value.challenge_blocked_at)) migrated.challenge_blocked_at = value.challenge_blocked_at;
  if (typeof value.handoffAckPending === "boolean") migrated.handoffAckPending = value.handoffAckPending;
  if (droppedWaitAuthority) {
    // The old claim owner/key is deliberately not retained. Clear every
    // marker that depended on it so startup reconciliation sees a normal
    // schedulable job, while preserving its safe tab/job correlation above.
    const migratedRecord = migrated as unknown as UnknownRecord;
    for (const key of [
      "waiting_for_session",
      "waiting_since",
      "waiting_deadline",
      "waiting_reason",
      "waitingReason",
      "parked_with_tab",
      "parked_at",
      "parked_reason",
    ]) {
      delete migratedRecord[key];
    }
  } else {
    if (typeof value.parked_with_tab === "boolean") migrated.parked_with_tab = value.parked_with_tab;
    if (typeof value.waiting_for_session === "boolean") migrated.waiting_for_session = value.waiting_for_session;
    if (isFiniteNumber(value.waiting_deadline)) migrated.waiting_deadline = value.waiting_deadline;
  }
  return migrated;
}

function migratedPendingDelivery(value: unknown): PendingDelivery | undefined {
  if (!isRecord(value) || typeof value.job_id !== "string" || !isFiniteNumber(value.initiated_at)) return undefined;
  const migrated: PendingDelivery = {
    job_id: value.job_id,
    initiated_at: value.initiated_at,
  };
  if (typeof value.status === "string" && DELIVERY_STATUSES.includes(value.status as PendingDeliveryStatus)) {
    migrated.status = value.status as PendingDeliveryStatus;
  }
  const pageHost = safeHost(value.page_host);
  if (pageHost !== undefined) migrated.page_host = pageHost;
  if (typeof value.error === "string" && !isURLLike(value.error)) migrated.error = value.error;
  if (value.session_evidence === "fresh_auth" || value.session_evidence === "warm" || value.session_evidence === "none") {
    migrated.session_evidence = value.session_evidence;
  }
  return migrated;
}


function migratedOriginMap(value: unknown): Record<string, number> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, number> = {};
  for (const [origin, timestamp] of Object.entries(value)) {
    const normalized = safeOrigin(origin);
    if (normalized !== undefined && isFiniteNumber(timestamp)) out[normalized] = timestamp;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function migratedNumberMap(value: unknown): Record<string, number> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, number> = {};
  for (const [key, numberValue] of Object.entries(value)) {
    if (isFiniteNumber(numberValue)) out[key] = numberValue;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function migratedLeaseMap(value: unknown): Record<string, ProviderDrainLease> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, ProviderDrainLease> = {};
  for (const [key, candidate] of Object.entries(value)) {
    const providerKey = safeHost(key);
    if (!providerKey || !isRecord(candidate) || !isFiniteNumber(candidate.expiresAt)) continue;
    const lease: ProviderDrainLease = { providerKey, expiresAt: candidate.expiresAt };
    if (candidate.parkedReason === "challenge") lease.parkedReason = "challenge";
    out[providerKey] = lease;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function migratedCooldownMap(value: unknown): Record<string, number> | undefined {
  if (!isRecord(value)) return undefined;
  const out: Record<string, number> = {};
  for (const [key, numberValue] of Object.entries(value)) {
    const host = safeHost(key);
    if (host !== undefined && isFiniteNumber(numberValue)) out[host] = numberValue;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

function migratedString(value: unknown): string | null | undefined {
  if (value === null) return null;
  return typeof value === "string" && !isURLLike(value) ? value : undefined;
}

function migratedHosts(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const hostValues: unknown[] = value;
  const hosts = hostValues.map(safeHost).filter((host): host is string => host !== undefined);
  return hosts.length === 0 ? undefined : [...new Set(hosts)];
}
function migratedState(raw: UnknownRecord): StoreShape {
  if (!Array.isArray(raw.activeJobs)) return emptyStore();
  const jobs: unknown[] = raw.activeJobs;
  // A malformed job is not allowed to silently become an empty active job.
  const validJobs = jobs.filter(validActiveJob);
  if (validJobs.length !== jobs.length) return emptyStore();
  const droppedClaimOwnerJobIDs = new Set<string>();
  if (isRecord(raw.federatedLoginOwners)) {
    for (const owner of Object.values(raw.federatedLoginOwners)) {
      if (isRecord(owner) && typeof owner.jobID === "string") {
        droppedClaimOwnerJobIDs.add(owner.jobID);
      }
    }
  }
  const output: StoreShape = {
    ...emptyStore(),
    activeJobs: validJobs.map((job) => migratedJob(job, droppedClaimOwnerJobIDs)),
  };

  const pending = migratedPendingDelivery(raw.pendingDelivery);
  if (pending !== undefined) output.pendingDelivery = pending;
  const lastAuthReturnedAt = raw.lastAuthReturnedAt;
  if (isFiniteNumber(lastAuthReturnedAt)) output.lastAuthReturnedAt = lastAuthReturnedAt;
  const authEvidenceByOrigin = migratedOriginMap(raw.authEvidenceByOrigin);
  if (authEvidenceByOrigin !== undefined) output.authEvidenceByOrigin = authEvidenceByOrigin;
  const authAttempts = migratedNumberMap(raw.authAttempts);
  if (authAttempts !== undefined) output.authAttempts = authAttempts;
  const providerDrainLeases = migratedLeaseMap(raw.providerDrainLeases);
  if (providerDrainLeases !== undefined) output.providerDrainLeases = providerDrainLeases;
  const workWindowID = raw.workWindowID;
  if (isFiniteNumber(workWindowID) && Number.isInteger(workWindowID)) output.workWindowID = workWindowID;
  const handoffGroupID = raw.handoffGroupID;
  if (isFiniteNumber(handoffGroupID) && Number.isInteger(handoffGroupID)) output.handoffGroupID = handoffGroupID;
  if (raw.connectionStatus === "connected" || raw.connectionStatus === "disconnected" ||
      raw.connectionStatus === "daemon_outdated" || raw.connectionStatus === "extension_outdated") {
    output.connectionStatus = raw.connectionStatus;
  }
  const daemonVersion = migratedString(raw.daemonVersion);
  if (daemonVersion !== undefined) output.daemonVersion = daemonVersion;
  if (typeof raw.daemonUpdateHint === "boolean") output.daemonUpdateHint = raw.daemonUpdateHint;
  const daemonFeatures = raw.daemonFeatures;
  if (Array.isArray(daemonFeatures)) {
    const featureValues: unknown[] = daemonFeatures;
    output.daemonFeatures = featureValues.filter(
      (feature): feature is string => typeof feature === "string" && !isURLLike(feature),
    );
  }
  const resolverOrigins = raw.resolverOrigins;
  if (Array.isArray(resolverOrigins)) {
    const originValues: unknown[] = resolverOrigins;
    output.resolverOrigins = originValues
      .map(safeOrigin)
      .filter((origin): origin is string => origin !== undefined);
  }
  const blockedProviderHosts = migratedHosts(raw.blockedProviderHosts);
  if (blockedProviderHosts !== undefined) output.blockedProviderHosts = blockedProviderHosts;
  const challengeCooldowns = migratedCooldownMap(raw.challengeCooldowns);
  if (challengeCooldowns !== undefined) output.challengeCooldowns = challengeCooldowns;
  // Legacy federatedLoginOwners and all per-job claim keys are deterministic
  // browser hashes, not daemon-issued opaque authority IDs. Do not promote.
  delete output.federatedLoginOwners;
  delete output.offerURLs;
  return output;
}

/** Migrate a raw managed-state value without side effects. Unknown/future and
 * malformed versions fail closed to an empty managed state. */
export function migrateManagedState(raw: unknown): StoreShape {
  if (!isRecord(raw) || !Array.isArray(raw.activeJobs)) return emptyStore();
  const version = raw.version;
  if (version !== undefined && version !== 1 && version !== MANAGED_STATE_VERSION) return emptyStore();
  return migratedState(raw);
}

function serializeManagedState(store: StoreShape): UnknownRecord {
  const migrated = migratedState({ ...store, activeJobs: store.activeJobs });
  return { version: MANAGED_STATE_VERSION, ...migrated };
}

/** chrome.storage-backed StateBackend. Prefers session storage (cleared when
 * the browser restarts) and falls back to local when session is unavailable. */
export function chromeBackend(storage: typeof chrome.storage): StateBackend {
  const area: chrome.storage.StorageArea = storage.session ?? storage.local;
  return {
    async load(): Promise<StoreShape> {
      const got: Record<string, unknown> = await area.get(STORAGE_KEY);
      const migrated = migrateManagedState(got[STORAGE_KEY]);
      // Persist the cutover immediately, so a legacy URL-bearing blob is not
      // left behind until some later unrelated state update.
      try {
        await area.set({ [STORAGE_KEY]: serializeManagedState(migrated) });
      } catch {
        // Storage can be temporarily unavailable; keep the sanitized in-memory
        // result and retry on the next normal save.
      }
      return migrated;
    },
    async save(store: StoreShape): Promise<void> {
      await area.set({ [STORAGE_KEY]: serializeManagedState(store) });
    },
  };
}
