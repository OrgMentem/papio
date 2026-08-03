// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable-ish tab/job correlation for the MV3 bridge. The service worker may be
// stopped at any time, so the small amount of state that must survive a restart
// lives in chrome.storage (session preferred, local fallback). Everything here
// is pure over an injected StateBackend so it is unit-testable without chrome.
//
// Privacy invariant: no identity-provider URL, host, title, query, or fragment
// is persisted. Resolver-provided offer URLs are retained only while their
// active jobs exist so a suspended MV3 worker can recover the exact handoff.

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
