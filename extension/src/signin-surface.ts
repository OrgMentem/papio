// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Sign-in surface lifecycle: what papio does with the tab an operator signs
// in on, driven by what the operator and the browser do rather than by a
// clock.
//
// A sign-in surface moves through
//
//   offered   a daemon offer is waiting; no tab exists yet
//   open      a tab exists and nobody has looked at it
//   attended  the operator has looked at it
//   away      the operator left (browser idle or locked)
//   closed    the operator closed the tab
//
// and leaves the table at auth_returned, when the provider drive takes over.
// Nothing here closes a tab. A surface ends on evidence only: the operator
// closed it, the operator took it over, or the daemon retired the action.
// "The operator walked away" is not evidence of any of those, which is the
// defect this module exists to remove: a three-minute drive timer parked the
// paper and the reconcile pass then closed a sign-in the operator was in the
// middle of, because they were in their authenticator app (measured live
// 2026-10-14).
//
// Signals, and what each one means:
//   presence   chrome.idle (active, idle, locked). Gates OPENING a surface and
//              starts the staleness clock; it never closes anything.
//   attention  tabs.onActivated, windows.onFocusChanged, tab.lastAccessed.
//   progress   a top-frame navigation in the sign-in tab. An MFA push approved
//              on a phone navigates the tab with no input on this computer, so
//              progress is independent of presence.
//   completion auth_returned.
//   intent     tabs.onRemoved that papio did not cause, or an address-bar
//              navigation to an unrelated host.
//
// The record is URL-free (ADR-0028 Decision 3) and lives in the session tier:
// loss of that tier means "retain and do not drive", which for this module is
// "no staleness repair and no presence gate", never a close.

import type { TabInfo, WindowInfo } from "./browser-types";
import { isAuthenticationURL, isNonLandingURL } from "./keepalive";
import { isTabAbsenceRejection } from "./surface-lifecycle";

/** How long a sign-in tab can sit untouched, with the operator away, before
 * its sign-in form is presumed stale. When the operator returns to a tab that
 * is still on an authentication host, nothing navigated while they were away,
 * and they were away at least this long, papio re-issues the route in the
 * SAME tab. Nothing is closed and no second tab is opened.
 *
 * 15 minutes sits above the measured operator return latency (674 recorded
 * returns from an authentication wall, 2026-08-21: p50 1.2 s, p90 5.5 s, p99
 * 603 s), so a form the operator is simply slow to finish is left alone, and
 * it is far below the daemon's action lifetime. An identity provider's
 * login-flow state does not outlive that: a Shibboleth "stale request" page
 * is exactly what a long-abandoned form returns. A wrong guess costs one
 * reload. A guess in the other direction costs the operator a failed
 * sign-in, so the bound errs toward reloading. */
export const SIGNIN_STALE_AFTER_MS = 15 * 60_000;

/** chrome.idle reports "idle" after this many seconds without input.
 * Chrome's minimum is 15 and its default 60. A person reading a page
 * without touching the keyboard for a minute is not interrupted by anything
 * papio does on idle: idle only holds NEW tabs back and starts the staleness
 * clock. */
export const SIGNIN_IDLE_DETECTION_SECONDS = 60;

export type PresenceState = "active" | "idle" | "locked";

export type SignInPhase = "offered" | "open" | "attended" | "away" | "closed";

/** One sign-in surface per job. URL-free by construction: ids, phases and
 * timestamps only. */
export interface SignInSurfaceRecord {
  job_id: string;
  /** -1 while no tab exists (offered, closed). */
  tab_id: number;
  /** The daemon binding this surface was born as, when it had one. */
  binding_id?: string;
  phase: SignInPhase;
  /** When the surface got its tab. */
  opened_at: number;
  /** The operator has looked at this surface at least once. Survives the
   * attended/away cycle: it is what makes a later retirement one the
   * operator should be told about. */
  attended: boolean;
  /** When the current absence began. Present only in phase `away`. */
  away_at?: number;
  /** The last top-frame navigation in the tab. Progress, and the staleness
   * test's "nothing happened since the operator left". */
  last_navigation_at: number;
  /** The operator asked for this surface by name (popup Open, `papio actions
   * open`). Overrides the presence gate and the closed marker once. */
  explicit_open?: true;
  /** The last time the operator brought this tab (or its window) forward.
   * Attention since a decision was made is what stops a delayed in-place
   * re-issue: the operator is looking at the form and may be typing. */
  activity_at?: number;
  /** The drive slot was yielded for the human wait (see markYielded). */
  yielded?: true;
}

/** Durable home of the records: chrome.storage.session in production. */
export interface SignInSurfaceStorage {
  load(): Promise<unknown>;
  save(records: Record<string, SignInSurfaceRecord>): Promise<void>;
}

const PHASES: ReadonlySet<string> = new Set([
  "offered",
  "open",
  "attended",
  "away",
  "closed",
]);

function finiteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

/** Defensive shape check for whatever storage actually holds. A record that
 * fails it is dropped, which only means the surface is no longer tracked:
 * tracking never authorizes a close, so dropping is the safe direction. */
export function parseSignInSurfaces(
  raw: unknown,
): Record<string, SignInSurfaceRecord> {
  const out: Record<string, SignInSurfaceRecord> = {};
  if (typeof raw !== "object" || raw === null) return out;
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value !== "object" || value === null) continue;
    const r = value as Record<string, unknown>;
    if (
      r["job_id"] !== key ||
      typeof r["tab_id"] !== "number" ||
      !Number.isInteger(r["tab_id"]) ||
      r["tab_id"] < -1 ||
      typeof r["phase"] !== "string" ||
      !PHASES.has(r["phase"]) ||
      !finiteNumber(r["opened_at"]) ||
      typeof r["attended"] !== "boolean" ||
      !finiteNumber(r["last_navigation_at"]) ||
      (r["away_at"] !== undefined && !finiteNumber(r["away_at"])) ||
      (r["binding_id"] !== undefined && typeof r["binding_id"] !== "string") ||
      (r["explicit_open"] !== undefined && r["explicit_open"] !== true) ||
      (r["activity_at"] !== undefined && !finiteNumber(r["activity_at"])) ||
      (r["yielded"] !== undefined && r["yielded"] !== true)
    )
      continue;
    out[key] = value as SignInSurfaceRecord;
  }
  return out;
}

export interface SignInLifecycleDeps {
  now(): number;
  tabs: {
    get(tabID: number): Promise<TabInfo>;
    query?(query: { active?: boolean; windowId?: number }): Promise<TabInfo[]>;
  };
  /** Absent where the browser has no idle API: presence is then unknown, and
   * unknown never gates anything. */
  idle?: {
    queryState(detectionIntervalSeconds: number): Promise<PresenceState>;
  };
  windows?: {
    getAll?(): Promise<WindowInfo[]>;
  };
  /** Absent keeps the records in worker memory only. */
  signInSurfaces?: SignInSurfaceStorage;
}

export interface SignInLifecycleContext {
  /** The job still exists in the browser's managed state. */
  jobExists(jobID: string): boolean;
  /** Re-issue the route in this surface's own tab. Fire and forget: the
   * implementation awaits a correlated daemon reply, which can only arrive
   * through the inbound chain, so the lifecycle must never wait on it. */
  reissueRoute(jobID: string, tabID: number, fence: SignInReissueFence): void;
  /** Run a held daemon offer now. */
  wakeOffer(jobID: string): void;
}

/** Why an offer may not open a tab yet. */
export type OfferGate = "open" | "closed" | "presence";

/** What the surface looked like when a stale re-issue was decided. The
 * re-issue waits on the effect governor, then on a daemon round trip, and may
 * navigate only if nothing happened to the surface meanwhile: no progress, no
 * operator attention, the same surface still standing. */
export interface SignInReissueFence {
  opened_at: number;
  last_navigation_at: number;
  activity_at: number;
}

export class SignInLifecycle {
  private cache: Record<string, SignInSurfaceRecord> | undefined;
  private chain: Promise<unknown> = Promise.resolve();

  constructor(
    private readonly deps: SignInLifecycleDeps,
    private readonly ctx: SignInLifecycleContext,
  ) {}

  /** Serialize every read-modify-write. Public methods are the only entry
   * points, and none of them calls another public method from inside its
   * critical section. */
  private serialize<T>(work: (records: Record<string, SignInSurfaceRecord>) => Promise<T> | T): Promise<T> {
    const run = this.chain.then(async () => {
      const records = await this.load();
      const before = JSON.stringify(records);
      const result = await work(records);
      if (JSON.stringify(records) !== before) await this.persist(records);
      return result;
    });
    this.chain = run.then(
      () => undefined,
      () => undefined,
    );
    return run;
  }

  private async load(): Promise<Record<string, SignInSurfaceRecord>> {
    if (this.cache !== undefined) return this.cache;
    let raw: unknown;
    try {
      raw = await this.deps.signInSurfaces?.load();
    } catch {
      // Unreadable is not empty, but tracking nothing is the safe direction:
      // this table only ever withholds a tab or reloads one in place.
      raw = undefined;
    }
    this.cache = parseSignInSurfaces(raw);
    return this.cache;
  }

  private async persist(records: Record<string, SignInSurfaceRecord>): Promise<void> {
    try {
      await this.deps.signInSurfaces?.save({ ...records });
    } catch {
      // Best effort. A lost write loses only survival across a worker death.
    }
  }

  /** The browser's own answer, asked fresh every time: a presence remembered
   * in worker memory is exactly the state MV3 loses. */
  async presence(): Promise<PresenceState | undefined> {
    const idle = this.deps.idle;
    if (idle === undefined) return undefined;
    try {
      return await idle.queryState(SIGNIN_IDLE_DETECTION_SECONDS);
    } catch {
      return undefined;
    }
  }

  /** Whether the browser has a window an operator could be looking at.
   * Unknown is yes: only a positive "every window is minimized" holds an
   * offer back. */
  private async hasVisibleWindow(): Promise<boolean> {
    const windows = this.deps.windows;
    if (windows?.getAll === undefined) return true;
    try {
      const all = await windows.getAll();
      return all.length > 0 && all.some((win) => win.state !== "minimized");
    } catch {
      return true;
    }
  }

  async snapshot(): Promise<Record<string, SignInSurfaceRecord>> {
    return this.serialize((records) => ({ ...records }));
  }

  async get(jobID: string): Promise<SignInSurfaceRecord | undefined> {
    return this.serialize((records) => records[jobID]);
  }

  /** Whether `tabID` is a sign-in surface papio must not retire: it has a
   * tab, the operator has not closed or taken it, and the tab has not yet
   * landed on a provider page. A tab that reached the provider with no wall
   * (a warm session) was never a sign-in, and one that returned from sign-in
   * is released at auth_returned; either way it is an ordinary surface. The
   * reconcile pass and the close primitive consult this before any close.
   * An unreadable tab is held: unknown retains. */
  async holdsTab(tabID: number): Promise<boolean> {
    if (tabID < 0) return false;
    return this.serialize(async (records) => {
      const record = Object.values(records).find(
        (entry) =>
          entry.tab_id === tabID &&
          (entry.phase === "open" ||
            entry.phase === "attended" ||
            entry.phase === "away"),
      );
      if (record === undefined) return false;
      try {
        const tab = await this.deps.tabs.get(tabID);
        if (
          typeof tab.url === "string" &&
          /^https?:\/\//u.test(tab.url) &&
          !isNonLandingURL(tab.url)
        ) {
          delete records[record.job_id];
          return false;
        }
      } catch {
        // Unknown retains.
      }
      return true;
    });
  }

  /** May a daemon offer for this job create a tab now? Opening is
   * presence-gated: an offer that arrives while the operator is away, or
   * while every browser window is minimized, waits and opens when they are
   * back. A surface the operator closed stays closed until they ask for it. */
  async offerGate(jobID: string): Promise<OfferGate> {
    const record = await this.get(jobID);
    if (record?.explicit_open === true) return "open";
    if (record?.phase === "closed") return "closed";
    const state = await this.presence();
    if (state !== undefined && state !== "active") return "presence";
    if (state !== undefined && !(await this.hasVisibleWindow())) return "presence";
    return "open";
  }

  /** An offer is waiting for the operator. Durable, so a worker death does
   * not forget it. A closed marker is never downgraded to waiting. */
  async holdOffer(jobID: string): Promise<void> {
    await this.serialize((records) => {
      const existing = records[jobID];
      if (existing !== undefined) return;
      const now = this.deps.now();
      records[jobID] = {
        job_id: jobID,
        tab_id: -1,
        phase: "offered",
        opened_at: now,
        attended: false,
        last_navigation_at: now,
      };
    });
  }

  /** The operator asked for this surface by name. Returns whether a held
   * offer or a closed marker was waiting on that, so the caller can run the
   * offer now. `create` says whether a surface with no record yet may get
   * one: false only lifts a marker that is already there. */
  async markExplicit(
    jobID: string,
    tabless: boolean,
    create = true,
  ): Promise<boolean> {
    return this.serialize((records) => {
      const existing = records[jobID];
      if (existing === undefined && (!tabless || !create)) return false;
      const now = this.deps.now();
      const base: SignInSurfaceRecord = existing ?? {
        job_id: jobID,
        tab_id: -1,
        phase: "offered",
        opened_at: now,
        attended: false,
        last_navigation_at: now,
      };
      const waiting =
        existing !== undefined &&
        (existing.phase === "offered" || existing.phase === "closed");
      records[jobID] = {
        ...base,
        explicit_open: true,
        ...(base.phase === "closed" ? { phase: "offered" as const } : {}),
      };
      // Only an offer or marker that was actually waiting is run by the
      // caller. A fresh record just lets a later offer through the gate; it
      // never starts a pipeline the explicit open did not already own.
      return waiting;
    });
  }

  /** A tab now exists for this surface. */
  async surfaceOpened(
    jobID: string,
    tabID: number,
    bindingID?: string,
  ): Promise<void> {
    await this.serialize((records) => {
      const existing = records[jobID];
      const now = this.deps.now();
      // Same tab re-announced (a restart's re-registration): keep the history.
      if (
        existing !== undefined &&
        existing.tab_id === tabID &&
        existing.phase !== "offered" &&
        existing.phase !== "closed"
      ) {
        if (bindingID !== undefined && existing.binding_id === undefined)
          existing.binding_id = bindingID;
        return;
      }
      records[jobID] = {
        job_id: jobID,
        tab_id: tabID,
        ...(bindingID === undefined ? {} : { binding_id: bindingID }),
        phase: "open",
        opened_at: now,
        attended: false,
        last_navigation_at: now,
      };
    });
  }

  /** The browser's presence changed. Away starts when the operator leaves; a
   * return evaluates staleness in place and wakes any held offer. */
  async presenceChanged(state: PresenceState): Promise<void> {
    const wake: string[] = [];
    const reissue: {
      jobID: string;
      tabID: number;
      fence: SignInReissueFence;
    }[] = [];
    await this.serialize(async (records) => {
      const now = this.deps.now();
      for (const record of Object.values(records)) {
        if (state !== "active") {
          if (
            record.tab_id >= 0 &&
            (record.phase === "open" || record.phase === "attended")
          ) {
            record.phase = "away";
            record.away_at = now;
          }
          continue;
        }
        if (record.phase === "offered") wake.push(record.job_id);
        if (record.phase !== "away") continue;
        const stale = await this.returnedStale(record, now);
        record.phase = record.attended ? "attended" : "open";
        delete record.away_at;
        if (stale) {
          reissue.push({
            jobID: record.job_id,
            tabID: record.tab_id,
            fence: {
              opened_at: record.opened_at,
              last_navigation_at: record.last_navigation_at,
              activity_at: record.activity_at ?? 0,
            },
          });
        }
      }
    });
    for (const jobID of wake) this.ctx.wakeOffer(jobID);
    for (const item of reissue)
      this.ctx.reissueRoute(item.jobID, item.tabID, item.fence);
  }

  /** The operator came back to a surface they had left. Stale means the
   * form is probably dead and nobody moved it: still on an authentication
   * host, no navigation since the absence began, and gone long enough. */
  private async returnedStale(
    record: SignInSurfaceRecord,
    now: number,
  ): Promise<boolean> {
    const awayAt = record.away_at;
    if (awayAt === undefined) return false;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(record.tab_id);
    } catch {
      // Gone, or unreadable. Removal is reported by tabs.onRemoved, and an
      // unreadable tab is not evidence of anything.
      return false;
    }
    this.noteLastAccessed(record, tab, awayAt);
    if (typeof tab.url !== "string" || !isAuthenticationURL(tab.url))
      return false;
    if (record.last_navigation_at > awayAt) return false;
    return now - awayAt >= SIGNIN_STALE_AFTER_MS;
  }

  /** tab.lastAccessed (Chrome 121+) is evidence the operator looked at the
   * tab after the surface opened, including activations this worker slept
   * through. */
  private noteLastAccessed(
    record: SignInSurfaceRecord,
    tab: TabInfo,
    since: number,
  ): void {
    if (typeof tab.lastAccessed === "number" && tab.lastAccessed > Math.min(since, record.opened_at))
      record.attended = true;
  }

  /** The operator brought a tab forward. */
  async tabActivated(tabID: number): Promise<void> {
    await this.serialize((records) => {
      for (const record of Object.values(records)) {
        if (record.tab_id !== tabID) continue;
        record.attended = true;
        record.activity_at = this.deps.now();
        // An away surface stays away until presence says the operator is
        // back: the activation may be the very input that ends the absence,
        // and staleness is judged at that transition.
        if (record.phase === "open") record.phase = "attended";
      }
    });
  }

  /** Whether the surface a re-issue was decided for is still exactly that
   * surface: the same tab, not closed or taken, and untouched since. */
  async reissueFenceHolds(
    jobID: string,
    tabID: number,
    fence: SignInReissueFence,
  ): Promise<boolean> {
    return this.serialize((records) => {
      const record = records[jobID];
      return (
        record !== undefined &&
        record.tab_id === tabID &&
        (record.phase === "open" ||
          record.phase === "attended" ||
          record.phase === "away") &&
        record.opened_at === fence.opened_at &&
        record.last_navigation_at === fence.last_navigation_at &&
        (record.activity_at ?? 0) === fence.activity_at
      );
    });
  }

  /** A browser window gained focus. Its active tab is in front of the
   * operator, and a window that was not on screen may now be: a held offer
   * asks the gate again (and is held again if the answer is still no). */
  async windowFocused(windowID: number): Promise<void> {
    if (windowID < 0) return;
    const held = await this.serialize((records) =>
      Object.values(records)
        .filter((record) => record.phase === "offered")
        .map((record) => record.job_id),
    );
    for (const jobID of held) this.ctx.wakeOffer(jobID);
    const query = this.deps.tabs.query;
    if (query === undefined) return;
    let active: TabInfo[];
    try {
      active = await query.call(this.deps.tabs, { active: true, windowId: windowID });
    } catch {
      return;
    }
    for (const tab of active)
      if (tab.id !== undefined) await this.tabActivated(tab.id);
  }

  /** A top-frame navigation committed in `tabID`: progress. */
  async tabNavigated(tabID: number): Promise<SignInSurfaceRecord | undefined> {
    return this.serialize((records) => {
      for (const record of Object.values(records)) {
        if (record.tab_id !== tabID) continue;
        record.last_navigation_at = this.deps.now();
        return { ...record };
      }
      return undefined;
    });
  }

  /** The tab is gone. `papioCaused` is a removal papio itself performed or
   * authorized; anything else is the operator closing it. Returns what the
   * record said before, or undefined when this tab held no surface. */
  async tabRemoved(
    tabID: number,
    papioCaused: boolean,
  ): Promise<SignInSurfaceRecord | undefined> {
    return this.serialize((records) => {
      for (const record of Object.values(records)) {
        if (record.tab_id !== tabID) continue;
        const before = { ...record };
        if (papioCaused) delete records[record.job_id];
        else {
          records[record.job_id] = {
            job_id: record.job_id,
            tab_id: -1,
            ...(record.binding_id === undefined
              ? {}
              : { binding_id: record.binding_id }),
            phase: "closed",
            opened_at: record.opened_at,
            attended: record.attended,
            last_navigation_at: this.deps.now(),
          };
        }
        return before;
      }
      return undefined;
    });
  }

  /** papio stops tracking this surface. Two different reasons share the one
   * effect: the operator took the tab over (it left sign-in for somewhere of
   * their own), or auth_returned, when the provider drive starts and the
   * sign-in is over. */
  async release(jobID: string): Promise<void> {
    await this.serialize((records) => {
      delete records[jobID];
    });
  }

  /** The surface yielded its provider drive slot for the human wait. Durable,
   * because the startup scan would otherwise re-register the retained tab as
   * a drive after a worker restart and spend the only slot on a person's
   * wait. Ends with the record, at auth_returned. */
  async markYielded(jobID: string): Promise<void> {
    await this.serialize((records) => {
      const record = records[jobID];
      if (record !== undefined && record.tab_id >= 0) record.yielded = true;
    });
  }

  /** The job is gone. Returns whether the operator had used the surface, so
   * the caller can say why it closed. A `closed` marker survives: the
   * operator closing the tab is exactly what removes the job from the
   * browser, and the marker is what stops the daemon's next offer from
   * reopening what they just closed. It ends on an explicit open or on the
   * daemon's own cancel (`release`). */
  async forgetJob(jobID: string): Promise<{ operatorUsed: boolean }> {
    return this.serialize((records) => {
      const record = records[jobID];
      if (record?.phase !== "closed") delete records[jobID];
      return {
        operatorUsed:
          record !== undefined &&
          record.phase !== "closed" &&
          record.tab_id >= 0 &&
          record.attended,
      };
    });
  }

  /** Worker start. Drops records for jobs that no longer exist (except a
   * `closed` marker, which outlives its job by design), proves each tab
   * still exists, and reconciles phase with the presence the browser reports
   * now: the transition that changed it may have fired while no worker was
   * alive to hear it. */
  async restore(): Promise<void> {
    const state = await this.presence();
    await this.serialize(async (records) => {
      for (const record of Object.values(records)) {
        if (record.phase === "closed") continue;
        if (!this.ctx.jobExists(record.job_id)) {
          delete records[record.job_id];
          continue;
        }
        if (record.tab_id < 0) continue;
        try {
          const tab = await this.deps.tabs.get(record.tab_id);
          this.noteLastAccessed(record, tab, record.opened_at);
        } catch (error) {
          if (!isTabAbsenceRejection(error)) continue;
          // Proven gone while no worker listened. Report-wise onRemoved
          // never fired, so the surface is the operator's to reopen.
          records[record.job_id] = {
            job_id: record.job_id,
            tab_id: -1,
            ...(record.binding_id === undefined
              ? {}
              : { binding_id: record.binding_id }),
            phase: "closed",
            opened_at: record.opened_at,
            attended: record.attended,
            last_navigation_at: this.deps.now(),
          };
        }
      }
    });
    if (state !== undefined) await this.presenceChanged(state);
  }
}
