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

/** Durable, informed user choice for auto-accepting publisher terms &
 * conditions. `undefined` = never asked; `"accept"` = consented to papio
 * agreeing to publisher T&C on their behalf; `"manual"` = will accept manually. */
export type TermsConsent = "accept" | "manual" | undefined;
export const TERMS_CONSENT_KEY = "papio_terms_consent_v1";

/** Durable user choice for the dedicated background work window. `false`
 * disables routing and restores legacy in-window tabs; absent means enabled. */
export const WORK_WINDOW_KEY = "papio_work_window_v1";

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
}

export interface StoreShape {
  activeJobs: ActiveJob[];
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
  /** Id of papio's dedicated background work window, when work-window mode
   * has created one this browser session. Window ids are session-scoped, never
   * sensitive. Verified live (and recreated) before every reuse. */
  workWindowID?: number;
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
