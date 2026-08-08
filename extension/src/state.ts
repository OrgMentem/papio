// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable-ish tab/job correlation for the MV3 bridge. The service worker may be
// stopped at any time, so the small amount of state that must survive a restart
// lives in chrome.storage (session preferred, local fallback). Everything here
// is pure over an injected StateBackend so it is unit-testable without chrome.
//
// Privacy invariant: no identity-provider URL, host, title, query, or fragment
// is persisted. Resolver-provided offer URLs are retained only while their
// active jobs exist so a suspended MV3 worker can recover the exact handoff.

import type { DeliverySessionEvidence } from "./protocol";

export type JobStatus = "offered" | "queued" | "accepted" | "auth_pending" | "awaiting_download";

/** Browser-managed delivery of an active PDF tab. The URL is intentionally
 * retained only in this narrow record so a suspended worker can finish the
 * exact download; it must never be copied into another persisted field. */
export type PendingDeliveryStatus = "sending" | "downloaded" | "failed";
export interface PendingDelivery {
  job_id: string;
  url: string;
  initiated_at: number;
  status?: PendingDeliveryStatus;
  error?: string;
  /** Host of the page that requested the delivery, frozen at request time.
   * The download outlives the navigation that started it, so the provenance
   * host cannot be re-read from the tab once the bytes land. */
  page_host?: string;
  /** Session evidence available at the moment the delivery was requested,
   * frozen alongside page_host. keepaliveAuthenticated/authReturnedThisWorker/
   * lastAuthReturnedAt are live global state, not scoped to this tab or
   * download — the multi-second download window leaves time for an
   * institutional probe or sign-in to complete elsewhere in the browser, and
   * reading evidence again at completion would credit this delivery with
   * that unrelated session instead of the one that actually existed when the
   * page produced the bytes. */
  session_evidence?: DeliverySessionEvidence;
}

/** Durable, informed user choice for auto-accepting publisher terms &
 * conditions. `undefined` = never asked; `"accept"` = consented to papio
 * agreeing to publisher T&C on their behalf; `"manual"` = will accept manually. */
export type TermsConsent = "accept" | "manual" | undefined;
export const TERMS_CONSENT_KEY = "papio_terms_consent_v1";

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
  /** Epoch ms when the tab first left every provider host (auth started). */
  auth_started_ms?: number;
  /** Expected work identity from the job offer, used to build the adapter
   * AdapterContext for declarative classification. Resolver-declared hints
   * only — never an IdP value. */
  expected?: { title?: string; doi?: string };
  /** True when the resolver says this offer needs a warm institutional session.
   * Queued handoffs retain it so a fallback never mints a sign-in request early. */
  requires_auth?: boolean;
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
   * evidence for the same institution (recordFreshSessionEvidence). Bounded
   * by its own SESSION_WAIT_TIMEOUT_MS governor timer: past it, the marker
   * clears on its own and the job reverts to an ordinary parked_with_tab
   * park — the pre-feature presentation — rather than waiting invisibly
   * forever for an owner who may have simply walked away. Cleared the
   * moment the job drives again (registerHandoffDrive, via
   * clearParkedMarker) or, if its tab closes while parked, when
   * onTabRemoved demotes it to an ordinary queued drive. */
  waiting_for_session?: boolean;
  /** Opaque SHA-256 hex digest of the federated-login claim tuple. The raw
   * IdP origin and entityID are never persisted; this key exists only for
   * equality against federatedLoginOwners. */
  waiting_for_session_key?: string | undefined;
  /** Absolute epoch ms past which a waiting_for_session park demotes on its
   * own (SESSION_WAIT_TIMEOUT_MS after the FIRST park, not each re-park —
   * see below). Persisted, not just a worker-local timer: an MV3 restart
   * mid-wait must re-arm the SAME deadline, not grant a fresh budget merely
   * because the worker happened to sleep. Set once, the first time a job
   * ever parks; reused (never reset) by every subsequent re-park under a
   * new or the same claim, so a job cannot extend its own wait indefinitely
   * by cycling through park/resume/park. Cleared only when the deadline
   * itself is spent (the timeout demotion fires) — a genuinely fresh future
   * park, after that, earns a fresh budget. */
  waiting_deadline?: number | undefined;
}
/** Cross-job record of the one live tab currently driving federated login for
 * an opaque SHA-256 claim digest. The digest is derived from the destination
 * origin and entityID but the raw tuple never enters persisted browser state.
 * Lets three papers needing the same institution share ONE login tab instead
 * of each opening its own: a job whose login verdict resolves to a digest
 * already held here parks (waiting_for_session) instead. Persisted beside
 * parked_with_tab (session storage) so a service-worker restart sees the same
 * claim; reconcileFederatedLoginOwners drops stale owners and resumes their
 * waiters through the ownerless path. Retirement is deliberately narrow: the
 * owning tab closes, navigates off the claimed origin, or its job is removed.
 * Every retirement resumes that claim's own waiters. */
export interface FederatedLoginOwner {
  jobID: string;
  tabID: number;
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
   * adoption. The URL is retained only here and is cleared after ack. */
  pendingDelivery?: PendingDelivery;
  /** Resolver-provided offer URL by active job. This is needed to recreate a
   * broker handoff after a service-worker restart. */
  offerURLs?: Record<string, string>;
  /** Most recent local proof that an institutional login returned to a provider.
   * It is bounded by the bridge before use and lets queued jobs drain after an
   * MV3 restart while the session is still warm. */
  lastAuthReturnedAt?: number;
  /** Epoch ms of the most recent release-grade observation or warm institutional
   * landing, per configured resolver origin. This — not the global
   * lastAuthReturnedAt — is the authorization input for releasing that origin's
   * queued handoffs, and it must survive a service-worker restart: an MV3 worker
   * dies constantly, and losing authority on every restart would park the
   * operator's accepted work behind a probe that may have no inspectable tab.
   * lastAuthReturnedAt stays for display only. */
  authEvidenceByOrigin?: Record<string, number>;
  /** Per-job count of authentication drives that never reached a download,
   * within this browser session. Accumulates across worker restarts and parks
   * (cleared on browser close with the rest of session state); reset once a
   * download proves the session works. Bounds re-driving a job whose warm SSO
   * session cannot complete human authentication. */
  authAttempts?: Record<string, number>;
  /** Per-provider drain leases. These contain only a canonical provider-host
   * key, expiry, and optional park reason — never a resolver or identity URL. */
  providerDrainLeases?: Record<string, ProviderDrainLease>;
  /** Id of papio's dedicated background work window, when work-window mode
   * has created one this browser session. Window ids are session-scoped, never
   * sensitive. Verified live (and recreated) before every reuse. */
  workWindowID?: number;
  /** Id of papio's collapsed "papio" tab group in the user's window, when
   * tab-group mode has created one this browser session. Group ids are
   * session-scoped, never sensitive. Verified live (and recreated) before reuse. */
  handoffGroupID?: number;
  /** Native-daemon connection and capability data, refreshed by hello_ack.
   * Version is null when an older daemon does not report one. */
  connectionStatus?: DaemonConnectionStatus;
  daemonVersion?: string | null;
  /** True when this build shipped with a newer daemon than the one connected. */
  daemonUpdateHint?: boolean;
  daemonFeatures?: string[];
  /** https origins of the daemon's configured OpenURL resolvers, from hello_ack.
   * The popup and options page request a host permission for each so papio can
   * steer that resolver's menu. Not sensitive: these are the user's own library
   * discovery hosts, the same origins already carried in every job offer. */
  resolverOrigins?: string[];
  /** Exact provider hosts currently blocking a tracked handoff because the
   * browser lacks effective access. Kept after a job finishes so the popup and
   * badge describe the standing condition until access changes. */
  blockedProviderHosts?: string[];
  /** Provider registrable-host cooldowns after a security check or redirect
   * loop. Values are epoch milliseconds; no URL or IdP data is retained. */
  challengeCooldowns?: Record<string, number>;
  /** One live login-tab claim per federated-login tuple, keyed by an opaque
   * SHA-256 digest so the persisted map contains neither raw origin nor
   * entityID. See FederatedLoginOwner's doc comment for the full lifecycle. */
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

const STORAGE_KEY = "papio_state_v1";

/** chrome.storage-backed StateBackend. Prefers session storage (cleared when
 * the browser restarts) and falls back to local when session is unavailable. */
export function chromeBackend(storage: typeof chrome.storage): StateBackend {
  const area: chrome.storage.StorageArea = storage.session ?? storage.local;
  return {
    async load(): Promise<StoreShape> {
      const got: Record<string, unknown> = await area.get(STORAGE_KEY);
      const raw: unknown = got[STORAGE_KEY];
      if (raw !== null && typeof raw === "object" && "activeJobs" in raw && Array.isArray(raw.activeJobs)) {
        // Our own persisted blob, already narrowed to carry an activeJobs array.
        return raw as StoreShape;
      }
      return emptyStore();
    },
    async save(store: StoreShape): Promise<void> {
      await area.set({ [STORAGE_KEY]: store });
    },
  };
}
