// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Durable-ish tab/job correlation for the MV3 bridge. The service worker may be
// stopped at any time, so the small amount of state that must survive a restart
// lives in chrome.storage (session preferred, local fallback). Everything here
// is pure over an injected StateBackend so it is unit-testable without chrome.
//
// Privacy invariant: nothing stored here may carry an identity-provider URL,
// host, title, query, or fragment. Only broker-owned resolver metadata
// (provider_hosts) plus timing/status fields are persisted.

export type JobStatus = "offered" | "accepted" | "auth_pending" | "awaiting_download";

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
}

export interface StoreShape {
  activeJobs: ActiveJob[];
}

/** Async key/value seam. The real implementation wraps chrome.storage; tests
 * inject an in-memory fake. */
export interface StateBackend {
  load(): Promise<StoreShape>;
  save(store: StoreShape): Promise<void>;
}

export function emptyStore(): StoreShape {
  return { activeJobs: [] };
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
  return { activeJobs };
}

export function removeJob(store: StoreShape, jobID: string): StoreShape {
  return { activeJobs: store.activeJobs.filter((j) => j.job_id !== jobID) };
}

/** Return a new store with the named job patched. No-op if the job is gone. */
export function patchJob(
  store: StoreShape,
  jobID: string,
  patch: Partial<Omit<ActiveJob, "job_id">>,
): StoreShape {
  return {
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
