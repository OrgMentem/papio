// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Owned-surface lifecycle for the papio bridge: the birth-record ledger of
// tabs papio created, the one close primitive and the daemon-authorized close
// transaction built on it, restart classification, the owned-surface
// reconcile pass and unledgered container sweep, and keepalive ownership.
//
// Bridge constructs one SurfaceLifecycle and delegates to it. Everything the
// lifecycle needs from Bridge is named in SurfaceLifecycleContext; nothing
// here imports background.ts.

import type { TabGroupInfo, TabInfo, WindowInfo } from "./browser-types";
import type { NativeRequestResult } from "./correlation";
import { isAuthenticationURL, type KeepaliveOwnership } from "./keepalive";
import {
  isSurfaceBirthRecord,
  migrateTabLedger,
  originDigestOf,
  type SurfaceBirthRecord,
} from "./ledger";
import { MATERIALIZE_PAGE_PATH } from "./page-paths";
import {
  findByJob,
  findByTab,
  MATERIALIZATION_ID_PATTERN,
  patchJob,
  type StoreShape,
} from "./state";

/** How long a parked handoff surface the operator has never engaged may sit
 * before papio retires it. See surfaceIsCold for the measurement this comes
 * from; 3x the measured p99 operator-return latency. */
const PARKED_SURFACE_COLD_MS = 30 * 60_000;
/** Non-URL free-text marker for a one-use federated-login mint's birth
 * record (Slice 2b): excluded from cross-job reuse pools the same way the
 * legacy raw-URL ledger's `privateURL` flag was. */
export const PRIVATE_SURFACE_PURPOSE = "federated-login";
/** Birth-record purpose for a tab a provider or resolver opened from a papio
 * surface (a `target=_blank` full-text link, a "View PDF" window.open). papio
 * did not create it, but it opened inside papio's container for papio's
 * paper, so it follows that paper's lifecycle: it closes when the paper is
 * filed, ends, or is driven again. */
export const PROVIDER_CHILD_PURPOSE = "provider-child";
/** Bound on how many same-epoch ledger records classifyRestart() probes
 * with tabs.get while re-proving an update-class restart. */
const RESTART_LIVENESS_SCAN_LIMIT = 25;
/** The disposition reasons a close may assert (claim-observation protocol
 * design §2.3): idle scaffold never engaged, settled after artifact win, an
 * authentication claim's abandonment, a binding whose daemon handoff is no
 * longer active, or a handoff this browser has parked. Exported so tests bind
 * to this list rather than re-declaring it: three hand-maintained copies in
 * background.test.ts all silently omitted `handoff_parked`. */
export type SurfaceCloseDisposition =
  | "scaffold_idle"
  | "materialization_settled"
  | "claim_abandoned"
  | "job_inactive"
  /** The job still has an open handoff action - papio is still asking the
   * operator for something - but this browser has parked it and drives
   * nothing through this surface. Distinct from job_inactive, which asserts
   * the opposite about the job and is refused for a parked ask. */
  | "handoff_parked"
  /** This binding owns more than one tab and this is not the one papio
   * drives. Every other disposition speaks about the binding, so a duplicate
   * could only be retired by asserting scaffold_idle - which a navigated
   * claim structurally fails - and the duplicates therefore survived. The
   * daemon does not take this on trust: it compares the named tab against
   * the tab it believes drives the claim, so the id travels with it. */
  | "surface_superseded";
function isSurfaceCloseDisposition(
  value: string | undefined,
): value is SurfaceCloseDisposition {
  return (
    value === "scaffold_idle" ||
    value === "materialization_settled" ||
    value === "claim_abandoned" ||
    value === "job_inactive" ||
    // Omitting handoff_parked here silently DOWNGRADED a replayed tombstone
    // (replayPendingCloseTombstones' fallback) to scaffold_idle — the one
    // disposition a navigated claim can never satisfy. A worker death between
    // tombstone persistence and tabs.remove therefore converted the correct
    // close into a permanently refused one.
    value === "handoff_parked" ||
    value === "surface_superseded"
  );
}
/** Why a surface was ceded. Fixed call-site names, never page-derived text:
 * ceding is terminal and erases the job binding, so the record's own account
 * of which site decided it is the only evidence that survives. */
export type CedeReason =
  /** A close attempt found the tab pinned: an operator act on this tab. */
  | "pinned_at_close"
  /** The reconcile pass found the tab pinned, or outside papio's container. */
  | "pinned_or_moved_out"
  /** Scaffold rediscovery found a pinned duplicate. */
  | "duplicate_operator_owned";
/** Cession reasons earlier builds recorded for acts that no longer cede.
 * Activating a papio tab, or touching it while papio closes it, now only
 * defers the close (operator decision, 2026-09-23): the tab papio opened for
 * a paper does not become the operator's by being looked at. */
const VOIDED_CESSION_REASONS: Readonly<Record<string, true>> = {
  operator_activated: true,
  touched_mid_close: true,
};
/** Whether the operator took this surface over. A record ceded only for a
 * reason in VOIDED_CESSION_REASONS is papio's again. */
export function surfaceIsCeded(entry: SurfaceBirthRecord): boolean {
  return (
    entry.ceded === true &&
    (entry.ceded_reason === undefined ||
      VOIDED_CESSION_REASONS[entry.ceded_reason] !== true)
  );
}
/** Whether a closed surface was showing the paper itself, so closing it is
 * worth offering back. Scaffolds never were: a sign-in, capture, keepalive or
 * one-use login tab carries no paper, and neither does an extension page or
 * a blank tab whose navigation became a download. */
function surfaceShowedPaper(purpose: string, url: string): boolean {
  if (
    purpose === "session-signin" ||
    purpose === "capture" ||
    purpose === "keepalive" ||
    purpose === "claim-scaffold" ||
    purpose === PRIVATE_SURFACE_PURPOSE
  )
    return false;
  try {
    const protocol = new URL(url).protocol;
    if (protocol !== "https:" && protocol !== "http:") return false;
  } catch {
    return false;
  }
  return !isAuthenticationURL(url);
}
/** A close transaction's outcome. `removedURL` is set only when papio itself
 * removed the tab, never when the tab was already gone, so a caller that
 * offers to reopen what papio closed can never offer a tab the operator
 * closed. Worker memory only: it is a route URL, which is never persisted. */
interface SurfaceCloseResult {
  closed: boolean;
  removedURL?: string;
}
/** True only when a `tabs.get` rejection PROVES the tab is gone.
 *
 * Every other rejection — an invalidated extension context, a torn-down
 * window mid-call, a browser shutting down — means "unknown", and unknown must
 * never be spent as evidence. Absence is what authorizes reporting a claim's
 * surface dead and deleting its ledger record, and that record is the only
 * proof the surface ever existed: mistaking a transient failure for absence
 * frees an institution's sign-in slot out from under a live tab and destroys
 * the evidence needed to notice. Chrome and Firefox word the same condition
 * differently, and papio ships both.
 */
export function isTabAbsenceRejection(reason: unknown): boolean {
  const message =
    reason instanceof Error
      ? reason.message
      : typeof reason === "string"
        ? reason
        : "";
  return (
    /no tab with id/iu.test(message) || /invalid tab id/iu.test(message)
  );
}

/** Durable managed-tab ledger storage; see BridgeDeps.tabLedger. */
export interface SurfaceLedgerStorage {
  load(): Promise<unknown>;
  save(entries: Record<string, SurfaceBirthRecord>): Promise<void>;
}

/** Browser-session epoch storage; see BridgeDeps.epoch. */
export interface BrowserEpochStorage {
  getSession(): Promise<string | undefined>;
  setSession(value: string): Promise<void>;
  getLocal(): Promise<string | undefined>;
  setLocal(value: string): Promise<void>;
}

/** The browser seams the lifecycle uses: a structural subset of BridgeDeps.
 * The lifecycle holds the same deps object Bridge holds and reads each seam
 * at call time, so replacing a seam on that object is seen by both. */
export interface SurfaceLifecycleDeps {
  manifestVersion: string;
  randomUUID(): string;
  now(): number;
  runtimeGetURL?: (path: string) => string;
  tabs: {
    get(tabID: number): Promise<TabInfo>;
    remove(tabID: number): Promise<void>;
    query?(query: { groupId?: number }): Promise<TabInfo[]>;
  };
  windows?: {
    get(windowID: number): Promise<WindowInfo>;
  };
  tabLedger?: SurfaceLedgerStorage;
  epoch?: BrowserEpochStorage;
}

/** The Bridge state and behaviour the lifecycle depends on, and nothing
 * else. Getters are read at every use: Bridge replaces the store and its
 * barrier promises over its lifetime. */
export interface SurfaceLifecycleContext {
  /** The current managed-state snapshot. */
  store(): StoreShape;
  update(fn: (store: StoreShape) => StoreShape): Promise<void>;
  /** Managed-state hydration barrier. */
  ready(): Promise<void>;
  /** Slice 2b `surfaceReady` barrier. */
  surfaceReady(): Promise<void>;
  /** The daemon browser-holder-generation fence Bridge last observed. */
  holderGeneration(): number | undefined;
  setHolderGeneration(generation: number): void;
  /** Worker-memory authentication-claim grants, by job. */
  readonly claimGrants: ReadonlyMap<
    string,
    { readonly authenticationClaimID: string; readonly gateOccurrenceID: string }
  >;
  /** The viewer tab each job is adopting from. */
  readonly adoptedViewerTabs: ReadonlyMap<string, number>;
  /** Tabs a finished download keeps open until the daemon acknowledges. */
  readonly completedDownloadTabs: ReadonlyMap<string, number>;
  /** Jobs papio filed in this worker lifetime. */
  readonly filedJobs: ReadonlySet<string>;
  /** Offer a filed paper back after papio closed a tab that showed it. */
  noteFiledPaperClosed(jobID: string, url: string): void;
  knownHandoffGroup(
    groupID: number,
    windowID: number | undefined,
  ): Promise<TabGroupInfo | undefined>;
  focusManagedTab(tabID: number): Promise<void>;
  liveDocumentEpoch(tabID: number): Promise<string | undefined>;
  requestCorrelated(
    kind: "surface_close_request",
    payload: Record<string, unknown>,
  ): Promise<NativeRequestResult>;
  /** Report a vanished claim surface from its durable record alone. */
  enqueueRestartRecoveredObservation(
    entry: {
      job_id: string;
      authentication_claim_id: string;
      binding_id: string;
      browser_holder_generation: number;
      gate_occurrence_id: string;
    },
    eventKind: "owner_closed",
  ): void;
}

export class SurfaceLifecycle {
  /** Serializes every managed-tab ledger load/mutate/save transaction. */
  private tabLedgerChain: Promise<void> = Promise.resolve();
  /** Lazily-loaded durable ledger of broker tabs papio created, migrated to
   * URL-free birth certificates on first touch (Slice 2b). */
  tabLedgerCache: Record<string, SurfaceBirthRecord> | undefined;
  /** Bumped synchronously (before any async work) by the onActivated/
   * onUpdated listeners whenever Chrome reports a tab change — activation,
   * pin, or navigation alike. closeOwnedTab captures this per-tab counter
   * at entry and compares it again immediately before tabs.remove, with no
   * intervening await, so a touch that happens (and even reverts) during a
   * close attempt is never invisible to a single before/after tabs.get. A
   * touch defers the close; the next reconcile pass asks again. */
  private readonly tabTouchEpoch = new Map<number, number>();
  /** Pre-cutover ledger entries retained for one-time manual review because
   * their provenance could not be re-verified at migration (no jobID to
   * correlate against). Recomputed by the same migration pass every worker
   * start; surfaced through orphanTabStatus(). */
  private legacyLedgerReview: string[] = [];
  /** This worker lifetime's browser-session epoch (Slice 2b), resolved by
   * classifyRestart(). Undefined until bootstrapSurfaceLifecycle() runs. */
  browserEpoch: string | undefined;
  /** How this worker came up, classified once at startup by
   * bootstrapSurfaceLifecycle through classifyRestart(). */
  restartClass: "worker" | "update" | "browser" | undefined;
  /** Coalescing trigger state for scheduleCloseTombstoneReplay: a replay
   * already in flight absorbs a concurrent trigger as one more full pass
   * instead of racing it — same shape as Bridge's
   * outboxDrainRunning/outboxDrainRerunRequested. */
  private closeTombstoneReplayRunning = false;
  private closeTombstoneReplayRerunRequested = false;
  /** Serializes adoption scans, group folding, tombstone replay, and
   * terminal reconciliation (Slice 2b) — never the effect governor, which
   * only irreversible provider navigation, page mutation, and download
   * initiation acquire. */
  private lifecycleChain: Promise<void> = Promise.resolve();

  constructor(
    private readonly deps: SurfaceLifecycleDeps,
    private readonly ctx: SurfaceLifecycleContext,
  ) {}

  /** The authoritative get is followed immediately by remove in this turn.
   * A failed fresh-link materialization is the one surface exception: the
   * private one-use tab never bound to a live job, so preserving it would let
   * a sibling open a duplicate institutional login.
   *
   * A PDF or article is closed like any other owned surface. papio does not
   * keep a tab for a paper it has filed or given up on (operator decision,
   * 2026-09-23). What still stops a close is the operator: a tab they pinned
   * or moved out of papio's container is theirs, and the tab they are looking
   * at right now waits for the next pass. */
  async closeOwnedTab(
    tabID: number,
    reason: string,
  ): Promise<boolean> {
    const entry = this.tabLedgerCache?.[String(tabID)];
    const materializationCleanup = reason === "materialization-reconcile";
    const rollbackPrivate =
      reason === "fresh-materialization-rollback" &&
      entry?.purpose === PRIVATE_SURFACE_PURPOSE;
    // An unledgered tab in papio's container (retireUnledgeredContainerTabs)
    // is closable only while it STAYS unledgered: a record written meanwhile
    // hands it to the ordinary lifecycle instead.
    const unledgeredContainer = reason === "unledgered-container";
    if (
      !materializationCleanup &&
      ((unledgeredContainer ? entry !== undefined : entry === undefined) ||
        findByTab(this.ctx.store(), tabID) !== undefined)
    )
      return false;
    // Captured before the first await below: any onActivated/onUpdated
    // listener that fires while the fresh tabs.get is in flight bumps this
    // tab's touch epoch synchronously (bindListeners), so a transient
    // activate-then-revert invisible to the fresh get's active/pinned
    // fields still shows up as a mismatch at the final compare.
    const epochAtStart = this.tabTouchEpoch.get(tabID) ?? 0;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(tabID);
    } catch {
      return false;
    }
    if (materializationCleanup) {
      const base = this.deps.runtimeGetURL?.(MATERIALIZE_PAGE_PATH);
      if (
        base === undefined ||
        typeof tab.url !== "string" ||
        (await this.operatorIsViewing(tab))
      )
        return false;
      try {
        const expected = new URL(base);
        const actual = new URL(tab.url);
        const bindingID = actual.hash.startsWith("#")
          ? actual.hash.slice(1)
          : "";
        if (
          actual.origin !== expected.origin ||
          actual.pathname !== expected.pathname ||
          actual.search !== "" ||
          !MATERIALIZATION_ID_PATTERN.test(bindingID)
        )
          return false;
      } catch {
        return false;
      }
    }
    const inWorkWindow =
      tab.windowId !== undefined && tab.windowId === this.ctx.store().workWindowID;
    const inPapioGroup =
      tab.groupId !== undefined && tab.groupId === this.ctx.store().handoffGroupID;
    // An unrecorded tab is papio's only by GROUP membership. The work window
    // is adopted from wherever papio's tabs live, which is the operator's own
    // window whenever the group sits there, so window membership alone would
    // hand the operator's tabs to this sweep. Measured live 2026-09-23: it
    // closed the operator's own reading tab.
    if (
      !rollbackPrivate &&
      !materializationCleanup &&
      (tab.pinned === true ||
        (unledgeredContainer ? !inPapioGroup : !inWorkWindow && !inPapioGroup) ||
        (await this.operatorIsViewing(tab)))
    )
      return false;
    // Final recheck immediately before remove, no intervening await: a
    // touch epoch bumped since entry — even one the fresh get above cannot
    // see because it already reverted — means the tab changed under this
    // close attempt. Defer: the record and any tombstone stay, so the next
    // reconcile pass asks again once the tab has settled. A job that took
    // the tab back during the awaits above (a re-offer reusing it) keeps it:
    // removing a tracked tab is what onTabRemoved reads as a cancellation.
    if (
      (this.tabTouchEpoch.get(tabID) ?? 0) !== epochAtStart ||
      (!materializationCleanup && findByTab(this.ctx.store(), tabID) !== undefined) ||
      (unledgeredContainer && this.tabLedgerCache?.[String(tabID)] !== undefined)
    )
      return false;
    await this.deps.tabs.remove(tabID).catch(() => undefined);
    return true;
  }

  /** Whether the operator is looking at this tab right now: it is the
   * selected tab of the window that has focus. Chrome reports `active` for
   * the selected tab of EVERY window, the minimized work window included, so
   * `active` alone does not mean anyone is looking. A focus papio cannot read
   * counts as looking: the close waits for a later pass rather than taking a
   * tab out from under the operator. */
  private async operatorIsViewing(tab: TabInfo): Promise<boolean> {
    if (tab.active !== true) return false;
    const windows = this.deps.windows;
    if (windows === undefined || tab.windowId === undefined) return true;
    try {
      const win = await windows.get(tab.windowId);
      if (win.state === "minimized") return false;
      return win.focused !== false;
    } catch {
      return true;
    }
  }


  /** Detach a surface from automation without removing it: the operator
   * took it over, so it is ceded permanently, its pending tombstone cleared so
   * nothing (a replay, a later reconcile pass) acts on it again. Keep the
   * job identity only for a claim-owned surface: if the drive later detaches
   * its tab, the operator's physical close must still retire the claim.
   * A no-op when the ledger no longer has a matching record for `tabID`, or
   * when `bindingID` names a different binding.
   *
   * `reason` is durable so a later investigation can identify which call
   * ceded the surface without inferring it from the page. */
  async cedeOwnedTab(
    tabID: number,
    bindingID: string | undefined,
    reason: CedeReason,
  ): Promise<void> {
    await this.runTabLedgerTransaction((ledger) => {
      const current = ledger[String(tabID)];
      if (
        current === undefined ||
        (bindingID !== undefined && current.binding_id !== bindingID)
      )
        return { value: undefined, changed: false };
      const next: SurfaceBirthRecord = {
        ...current,
        ceded: true,
        ceded_reason: reason,
      };
      delete next.pending_close;
      if (next.claim === undefined) delete next.job_id;
      ledger[String(tabID)] = next;
      return { value: undefined, changed: true };
    });
  }
  /** Retire every other surface papio owns for this job now that `keepTabID`
   * is the one it drives. A redrive, re-offer, fresh link or publisher retry
   * opened its tab beside the previous attempt's and nothing retired the old
   * one: measured live 2026-09-23 as two, two, two and four tabs on single
   * papers.
   *
   * Callers first point the job at `keepTabID`, so closeOwnedTab's
   * tracked-tab guard no longer shields the old surfaces; every other guard
   * still runs. The closes are fired, not awaited: callers ride the serialized
   * inbound chain, and the close transaction's daemon answer can only arrive
   * through that same chain. `skipBindingID` leaves a binding the caller
   * already retires itself (a scaffold's own duplicates) to that caller.
   *
   * `disposition` is `surface_superseded` while the paper is driven from
   * `keepTabID`; removeJobWithOffer passes its own disposition and -1 when
   * the paper keeps no surface at all. The viewer papio is adopting from is
   * spared either way: closeAfterAdoption retires it once the ack lands. */
  async retireSupersededSurfaces(
    jobID: string,
    keepTabID: number,
    skipBindingID?: string,
    disposition: SurfaceCloseDisposition = "surface_superseded",
  ): Promise<void> {
    const ledger = await this.snapshotTabLedger();
    const adopting = this.ctx.adoptedViewerTabs.get(jobID);
    for (const [key, entry] of Object.entries(ledger)) {
      const tabID = Number(key);
      if (
        !Number.isInteger(tabID) ||
        tabID < 0 ||
        tabID === keepTabID ||
        tabID === adopting ||
        entry.job_id !== jobID ||
        entry.purpose === "keepalive" ||
        entry.binding_id === skipBindingID ||
        surfaceIsCeded(entry) ||
        entry.browser_epoch !== this.browserEpoch ||
        findByTab(this.ctx.store(), tabID) !== undefined
      )
        continue;
      void this.closeOwnedSurface(tabID, disposition);
    }
  }
  private async saveTabLedger(
    ledger: Record<string, SurfaceBirthRecord>,
    required = false,
  ): Promise<void> {
    const snapshot = { ...ledger };
    try {
      await this.deps.tabLedger?.save(snapshot);
    } catch {
      if (required) throw new Error("keepalive birth ledger unavailable");
      // Best-effort durability: a failed write only degrades future cleanup.
    }
  }
  /** Load, mutate, and persist the managed-tab ledger as one serialized
   * transaction. The first load of a worker lifetime runs the raw storage
   * contents through migrateTabLedger (Slice 2b) — idempotent on an
   * already-migrated ledger — so every caller sees URL-free birth
   * certificates regardless of which one happens to touch the ledger
   * first; the migration itself is persisted once, immediately. Every
   * cache and storage value is a fresh snapshot so a later mutation cannot
   * rewrite an earlier save's object in place. */
  private runTabLedgerTransaction<T>(
    transaction: (
      ledger: Record<string, SurfaceBirthRecord>,
    ) =>
      Promise<{ value: T; changed: boolean }> | { value: T; changed: boolean },
    required = false,
  ): Promise<T> {
    const operation = this.tabLedgerChain.then(async () => {
      let cached = this.tabLedgerCache;
      if (cached === undefined) {
        let raw: unknown = {};
        try {
          raw = (await this.deps.tabLedger?.load()) ?? {};
        } catch {
          if (required) throw new Error("keepalive birth ledger unavailable");
          raw = {};
        }
        const migrated = await migrateTabLedger(
          raw,
          () => this.deps.randomUUID(),
          () => this.deps.now(),
        );
        cached = migrated.ledger;
        this.legacyLedgerReview = migrated.review;
        this.tabLedgerCache = { ...cached };
        await this.saveTabLedger(this.tabLedgerCache, required);
      }

      const ledger = { ...cached };
      const result = await transaction(ledger);
      if (result.changed) await this.saveTabLedger(ledger, required);
      this.tabLedgerCache = { ...ledger };
      return result.value;
    });
    this.tabLedgerChain = operation.then(
      () => undefined,
      () => undefined,
    );
    return operation;
  }
  async snapshotTabLedger(required = false): Promise<
    Record<string, SurfaceBirthRecord>
  > {
    if (this.deps.tabLedger === undefined) return {};
    return this.runTabLedgerTransaction((ledger) => ({
      value: { ...ledger },
      changed: false,
    }), required);
  }

  /** Record a broker tab papio CREATED as a URL-free birth certificate
   * (Slice 2b). Reused tabs are deliberately never ledgered: a URL-matched
   * reuse can be the user's own tab, and the ledger exists to authorize
   * closing — papio must never earn that authority over a tab it did not
   * open. `privateSurface` marks a one-use federated-login mint, isolated
   * from cross-job reuse the same way the legacy `privateLedgerURL` flag
   * isolated it. */
  async ledgerManagedTab(
    tabID: number,
    purpose: string,
    privateSurface = false,
    jobID?: string,
    bindingID?: string,
  ): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(tabID);
    } catch {
      return;
    }
    const originDigest =
      typeof tab.url === "string" ? await originDigestOf(tab.url) : undefined;
    // Captured before the transaction's await boundary, from the grant that
    // is live exactly now: a worker restart erases claimGrants, and an owner
    // that closes afterwards can only report owner_closed from this record.
    const claim = jobID === undefined ? undefined : this.durableClaimIdentity(jobID);
    await this.runTabLedgerTransaction(async (ledger) => {
      const key = String(tabID);
      const existing = ledger[key];
      if (existing !== undefined) {
        // Additive only: an existing record is never re-dated or re-bound
        // (that is the reuse guard above), but a surface that was ledgered
        // before its claim was granted still needs the identity to survive
        // a restart.
        if (claim === undefined || existing.claim !== undefined)
          return { value: undefined, changed: false };
        existing.claim = claim;
        return { value: undefined, changed: true };
      }
      ledger[key] = {
        binding_id: bindingID ?? this.deps.randomUUID(),
        tab_hint: tabID,
        purpose: privateSurface ? PRIVATE_SURFACE_PURPOSE : purpose,
        browser_epoch: this.browserEpoch ?? "unknown",
        extension_generation: this.deps.manifestVersion,
        created_at: this.deps.now(),
        ...(originDigest === undefined ? {} : { origin_digest: originDigest }),
        ...(jobID === undefined ? {} : { job_id: jobID }),
        ...(claim === undefined ? {} : { claim }),
      };
      return { value: undefined, changed: true };
    });
  }

  /** Give a tab a provider or resolver opened from a papio surface a birth
   * record under the opener's paper. Primo opens full text with
   * `target=_blank`, and ScienceDirect's "View PDF" and getPdf are
   * window.open children; Chrome puts each into the opener's group, and
   * without a record no lifecycle path could ever judge them (measured live
   * 2026-09-23: the ledger held two records while papio's group held about
   * twenty-four tabs). The record carries no claim: the child is not the
   * claim's owner, and closing it must never report owner_closed. Never
   * throws, and a no-op for a tab already ledgered or an opener papio does
   * not own. */
  async ledgerProviderChild(
    tabID: number,
    openerTabID: number,
  ): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    if (this.tabLedgerCache?.[String(tabID)] !== undefined) return;
    // Most tabs with an opener are the operator's own; with the ledger loaded
    // they are refused here without queueing a ledger transaction.
    if (
      this.tabLedgerCache !== undefined &&
      this.tabLedgerCache[String(openerTabID)] === undefined &&
      findByTab(this.ctx.store(), openerTabID) === undefined
    )
      return;
    try {
      const ledger = await this.snapshotTabLedger();
      if (ledger[String(tabID)] !== undefined) return;
      const opener = ledger[String(openerTabID)];
      const jobID =
        findByTab(this.ctx.store(), openerTabID)?.job_id ??
        (opener !== undefined &&
        opener.purpose !== "keepalive" &&
        !surfaceIsCeded(opener) &&
        opener.browser_epoch === this.browserEpoch
          ? opener.job_id
          : undefined);
      if (jobID === undefined) return;
      await this.recordProviderChild(tabID, jobID);
    } catch {
      // Unrecorded is the state this tab was already in; the container sweep
      // in reconcileOwnedTabs still covers it.
    }
  }

  /** Write the claim-free provider-child record. Additive: an existing
   * record for the tab is never rebound. */
  async recordProviderChild(tabID: number, jobID: string): Promise<void> {
    await this.runTabLedgerTransaction((ledger) => {
      const key = String(tabID);
      if (ledger[key] !== undefined) return { value: undefined, changed: false };
      ledger[key] = {
        binding_id: this.deps.randomUUID(),
        tab_hint: tabID,
        purpose: PROVIDER_CHILD_PURPOSE,
        browser_epoch: this.browserEpoch ?? "unknown",
        extension_generation: this.deps.manifestVersion,
        created_at: this.deps.now(),
        job_id: jobID,
      };
      return { value: undefined, changed: true };
    });
  }

  /** The durable mirror of a live worker-memory claim grant. Undefined when
   * this job holds no grant, or when no holder generation has been observed
   * yet — an observation carrying a guessed generation is worse than one
   * that is never sent, because the daemon would apply it under the wrong
   * holder. */
  private durableClaimIdentity(
    jobID: string,
  ): SurfaceBirthRecord["claim"] | undefined {
    const grant = this.ctx.claimGrants.get(jobID);
    if (grant === undefined) return undefined;
    const generation = this.ctx.holderGeneration();
    if (generation === undefined) return undefined;
    return {
      authentication_claim_id: grant.authenticationClaimID,
      gate_occurrence_id: grant.gateOccurrenceID,
      browser_holder_generation: generation,
    };
  }
  /** Mirror a just-granted claim onto the surface it governs.
   *
   * `ledgerManagedTab` captures the identity at BIRTH, and it was the only
   * writer — but a grant almost always post-dates its surface. The consult
   * follows the open (`navigate_existing`/`focus_owner` act on a tab that
   * already exists), and the daemon-driven materialization pipeline claims,
   * binds and routes a tab that is already born. So the durable mirror was
   * written only on the `open_new` ordering, and every other surface kept its
   * claim identity in worker memory alone. MV3 sleeps the worker ~30s after
   * the last event, and a human signing in takes minutes, so by the time the
   * tab closed the grant was gone and `onTabRemoved` had nothing to report
   * from: measured on the operator's own machine 2026-08-21, one entry lease
   * had ever been reserved, zero `claim_abandoned` close authorizations had
   * ever been issued, and `claim_observation_journal` held zero rows across
   * weeks of real sign-ins. The additive branch in `ledgerManagedTab` existed
   * for exactly this and nothing ever invoked it.
   */
  async persistClaimIdentity(
    jobID: string,
    tabID: number,
  ): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    const claim = this.durableClaimIdentity(jobID);
    if (claim === undefined || tabID < 0) return;
    // Strictly additive against an EXISTING record: this must never mint a
    // birth certificate, which only `ledgerManagedTab` may do at the moment
    // papio actually creates the surface. A tab papio did not create has no
    // record here and must not acquire one.
    await this.runTabLedgerTransaction(async (ledger) => {
      const existing = ledger[String(tabID)];
      if (existing === undefined || existing.claim !== undefined)
        return { value: undefined, changed: false };
      existing.claim = claim;
      return { value: undefined, changed: true };
    });
  }

  async forgetLedgeredTab(tabID: number): Promise<void> {
    if (this.deps.tabLedger === undefined) return;
    await this.runTabLedgerTransaction(async (ledger) => {
      const key = String(tabID);
      if (ledger[key] === undefined)
        return { value: undefined, changed: false };
      delete ledger[key];
      return { value: undefined, changed: true };
    });
  }

  /** A live papio-created sign-in tab for this origin. The jobless sign-in
   * fallback used to skip ledger reuse entirely (candidate gathering is
   * jobId-scoped), so repeated fallbacks minted repeated sign-in tabs
   * (surface-lifecycle plan, Slice 0). A tab qualifies while its current
   * document is still at the requested origin or on an authentication page
   * — either way the sign-in surface already exists. A tab tracked by any
   * job is never a candidate: sign-in must not steal a job's surface. */
  async findLedgeredSignInTab(url: string): Promise<number | undefined> {
    const requestedDigest = await originDigestOf(url);
    if (requestedDigest === undefined) return undefined;
    const ledger = await this.snapshotTabLedger();
    for (const key of Object.keys(ledger)) {
      const tabID = Number(key);
      if (!Number.isInteger(tabID) || tabID < 0) continue;
      const entry = ledger[key];
      if (entry === undefined || entry.origin_digest !== requestedDigest)
        continue;
      if (findByTab(this.ctx.store(), tabID) !== undefined) continue;
      let tab: TabInfo;
      try {
        tab = await this.deps.tabs.get(tabID);
      } catch {
        continue;
      }
      if (tab.id !== tabID || typeof tab.url !== "string") continue;
      const liveDigest = await originDigestOf(tab.url);
      if (liveDigest !== requestedDigest && !isAuthenticationURL(tab.url))
        continue;
      return tabID;
    }
    return undefined;
  }

  /** Classify ledgered, untracked tabs without taking lifecycle action. Tabs
   * in papio surfaces and tabs the operator can review are returned
   * separately. A tab whose live origin no longer digests to its birth
   * certificate's `origin_digest` stays ledgered — ordinary resolver→SSO→
   * provider redirects must not erase ownership evidence (surface-lifecycle
   * plan, Slice 0) — but it is surfaced NOWHERE: a navigated papio tab is
   * indistinguishable from a recycled tab id naming a foreign tab by digest
   * alone. Tracked, active, and pinned (keepalive) tabs are never
   * candidates. */
  private async classifyLedgeredTabs(): Promise<{
    auto: number[];
    ask: number[];
  }> {
    await this.ctx.ready();
    await this.ctx.surfaceReady();
    if (this.deps.tabLedger === undefined) return { auto: [], ask: [] };
    const tracked = new Set<number>();
    for (const job of this.ctx.store().activeJobs)
      if (job.tab_id >= 0) tracked.add(job.tab_id);
    for (const id of this.ctx.completedDownloadTabs.values()) tracked.add(id);
    return this.runTabLedgerTransaction(async (ledger) => {
      const auto = new Set<number>();
      const ask = new Set<number>();
      let changed = false;
      for (const key of Object.keys(ledger)) {
        const tabID = Number(key);
        if (!Number.isInteger(tabID) || tabID < 0) {
          delete ledger[key];
          changed = true;
          continue;
        }
        if (tracked.has(tabID)) continue;
        const entry = ledger[key];
        if (entry?.purpose === "keepalive") continue; // Its manager owns pause/reload/teardown.
        if (entry === undefined) {
          delete ledger[key];
          changed = true;
          continue;
        }
        let tab: TabInfo;
        try {
          tab = await this.deps.tabs.get(tabID);
        } catch (e) {
          if (!isTabAbsenceRejection(e)) {
            // Unknown, not gone. Spending a transient rejection as proof of
            // death would free the institution's slot out from under a live
            // tab AND delete the only record of it, so nothing could notice.
            // Leaving the entry intact costs one more pass; it is re-evaluated
            // on the next reconcile.
            continue;
          }
          // The surface is GONE and this record is the only proof it existed.
          // Deleting it in silence stranded the claim behind it: a tab closed
          // with no listener alive (an extension reload, a browser crash)
          // leaves a claim whose institutional effect permit has settled,
          // which reconcile deliberately never expires - so the institution's
          // sign-in slot stayed held, with no deadline, by a paper that has no
          // page. Measured live 2026-08-20: papio's own tab group was closed
          // during an extension reload and the library stayed occupied.
          //
          // Report the same restart-recovered owner_closed that onTabRemoved
          // reports from this record. The enqueue only touches the observation
          // outbox and schedules its drain, so it is safe inside this ledger
          // transaction.
          // Deliberately NOT gated on browser_epoch equality, unlike every
          // live-tab path below. Epoch equality exists to protect TAB-ID
          // AUTHORITY: after a browser restart a stale id may name someone
          // else's tab. Absence removes that hazard entirely - there is no
          // tab to misidentify - and what is left is the record's own
          // self-identifying claim material (plan line 151). The gate made
          // this report unreachable in the exact case it exists for:
          // epochStillLive re-proves an epoch by resolving SOME ledgered
          // tab_hint, so an operator who closes ALL of papio's tabs and then
          // reloads has no live record left to prove with, the reload is
          // classified as a browser restart, and every record it should have
          // reported became prior-epoch. Measured live 2026-08-21: reported
          // nothing, and the library stayed held.
          if (
            entry.ceded !== true &&
            entry.job_id !== undefined &&
            entry.binding_id !== undefined &&
            entry.claim !== undefined
          ) {
            this.ctx.enqueueRestartRecoveredObservation(
              {
                job_id: entry.job_id,
                authentication_claim_id: entry.claim.authentication_claim_id,
                binding_id: entry.binding_id,
                browser_holder_generation:
                  entry.claim.browser_holder_generation,
                gate_occurrence_id: entry.claim.gate_occurrence_id,
              },
              "owner_closed",
            );
          }
          delete ledger[key];
          changed = true;
          continue;
        }
        const navigated =
          entry.origin_digest === undefined ||
          typeof tab.url !== "string" ||
          (await originDigestOf(tab.url)) !== entry.origin_digest;
        if (tab.active === true || tab.pinned === true) continue;
        if (navigated) continue;
        let ownedSurface =
          tab.windowId !== undefined &&
          tab.windowId === this.ctx.store().workWindowID;
        if (!ownedSurface && tab.groupId !== undefined && tab.groupId >= 0) {
          ownedSurface =
            (await this.ctx.knownHandoffGroup(tab.groupId, tab.windowId)) !==
            undefined;
        }
        (ownedSurface ? auto : ask).add(tabID);
      }
      return {
        value: {
          auto: [...auto].sort((a, b) => a - b),
          ask: [...ask].sort((a, b) => a - b),
        },
        changed,
      };
    });
  }

  /** Popup card contents: the strays papio will not touch on its own, plus
   * pre-cutover ledger entries the Slice 2a migration could not re-verify
   * (no jobID to correlate against). */
  async orphanTabStatus(): Promise<{ count: number; tab_ids: number[] }> {
    const { auto, ask } = await this.classifyLedgeredTabs();
    const classified = new Set([...auto, ...ask]);
    const ledger = await this.snapshotTabLedger();
    const legacyCount = this.legacyLedgerReview.filter(
      (key) => ledger[key] !== undefined && !classified.has(Number(key)),
    ).length;
    return { count: ask.length + legacyCount, tab_ids: ask };
  }

  /** Has this surface outlived any plausible operator return?
   *
   * Measured on the operator's own store 2026-08-21, across all 674 recorded
   * returns from an authentication wall: p50 1.2s, p90 5.5s, p99 603s, and 671
   * of 674 inside thirty minutes. A return that is going to happen happens
   * fast, so the threshold sits 3x beyond the measured p99 and still covers
   * 99.6% of them.
   *
   * created_at is the durable birth timestamp, which is the point: an MV3
   * worker death wipes tabTouchEpoch, so age is the only engagement signal
   * that survives a restart. This is a floor on retirement and never
   * authority to remove - every guard below still runs, twice.
   */
  private surfaceIsCold(entry: SurfaceBirthRecord): boolean {
    return this.deps.now() - entry.created_at >= PARKED_SURFACE_COLD_MS;
  }

  /** Reconcile modern, same-browser-epoch birth records that no live job still
   * points at. This is the restart/update repair half of job_inactive: future
   * cancel/job-removal frames close through removeJobWithOffer, but a surface
   * already orphaned needs the same disposition applied after the fact.
   *
   * Safe against a tab being born under it because ledgerManagedTab runs
   * AFTER recordManagedTab (openManagedTab), so a birth record never exists
   * while its job is still pointing at -1.
   *
   * Never infer from age, title, or URL. The opaque daemon binding plus the
   * same browser epoch proves the record; a fresh tabs.get plus papio
   * work-window/group membership proves the physical surface. Pre-v2 records,
   * browser-restart epochs, surfaces a live job still points at, and ceded
   * tabs remain review-only. A surface the operator pinned or moved out of
   * papio's container is ceded, never closed; the one they are looking at is
   * left for a later pass. A PDF or article is closed like anything else:
   * papio keeps no tab for a paper it has filed (operator decision,
   * 2026-09-23), and records an older build marked `content` are swept here. */
  async reconcileOwnedTabs(): Promise<{ closed: number }> {
    await this.classifyLedgeredTabs();
    const ledger = await this.snapshotTabLedger();
    let closed = 0;
    for (const [key, entry] of Object.entries(ledger)) {
      const tabID = Number(key);
      const owner =
        entry.job_id === undefined
          ? undefined
          : findByJob(this.ctx.store(), entry.job_id);
      if (
        !Number.isInteger(tabID) ||
        tabID < 0 ||
        surfaceIsCeded(entry) ||
        entry.job_id === undefined ||
        entry.browser_epoch !== this.browserEpoch ||
        // The question is whether anything still POINTS AT this surface, not
        // whether the paper that opened it still exists. Those differ exactly
        // where the pile came from: the handoff-drive timeout deliberately
        // detaches a legacy job from its tab (tab_id: -1) and then asks to
        // close it, so the job is alive and tabless while the tab is orphaned.
        // Testing job existence skipped every one of those forever - and since
        // the timeout's own close attempt happens once, nothing ever retried
        // it. Measured live 2026-08-21: eighteen such tabs, none reachable by
        // any close path.
        //
        // A job pointing at THIS tab is still a live surface - UNLESS it has
        // parked with it. A fresh-link park deliberately keeps its tab (see
        // registerHandoffDrive's timeout) on the reasoning that detaching
        // leaves the paper with no reusable URL and no way back to the
        // operator's page. The first half is true and the second is no longer:
        // engagement mints a fresh route, so the preserved page is a spent
        // single-use link with no residual value - while the tab it occupies
        // is real, and twelve of them were live on the operator's screen.
        //
        // Cold only, and cold is measured, not guessed: the surface must have
        // outlived PARKED_SURFACE_COLD_MS, so a park whose page the operator
        // may still act on (a live provider challenge is the case that
        // matters) is never taken out from under them.
        (owner?.tab_id === tabID &&
          !(owner.parked_with_tab === true && this.surfaceIsCold(entry))) ||
        // A provider's child of a paper still being driven is part of that
        // drive (the viewer it is about to adopt, the page it reads); it goes
        // with the paper, or once the paper is tabless and the child cold.
        (entry.purpose === PROVIDER_CHILD_PURPOSE &&
          owner !== undefined &&
          (owner.tab_id >= 0 || !this.surfaceIsCold(entry)))
      )
        continue;
      let tab: TabInfo;
      try {
        tab = await this.deps.tabs.get(tabID);
      } catch {
        // Unreachable in practice: classifyLedgeredTabs above already prunes
        // and REPORTS a vanished record. Kept as a plain guard so a future
        // caller ordering cannot crash this loop.
        continue;
      }
      const inWorkWindow =
        tab.windowId !== undefined && tab.windowId === this.ctx.store().workWindowID;
      const inPapioGroup =
        tab.groupId !== undefined &&
        tab.groupId >= 0 &&
        (await this.ctx.knownHandoffGroup(tab.groupId, tab.windowId)) !== undefined;
      if (tab.pinned === true || (!inWorkWindow && !inPapioGroup)) {
        // Pinning a tab, or moving it out of papio's container, is an operator
        // act on the surface itself: positive takeover.
        await this.cedeOwnedTab(
          tabID,
          entry.binding_id,
          "pinned_or_moved_out",
        );
        continue;
      }
      if (await this.operatorIsViewing(tab)) {
        // The operator is looking at it right now. Deferred, never ceded:
        // onTabActivated retires it the moment another tab takes the
        // foreground, and the next pass covers a window switch.
        continue;
      }
      // Two different facts, two different dispositions. The paper being GONE
      // is job_inactive. The paper being alive but tabless is a parked ask:
      // asserting job_inactive for it is simply false, and the daemon rightly
      // refused it on every pass ("the binding still has an active browser
      // handoff") - which is how a parked ask kept a tab for days.
      if (owner?.tab_id === tabID) {
        // closeOwnedTab refuses a tab any live job still tracks, so a cold
        // park has to be detached first - the same detach-then-close order the
        // legacy timeout and the terminal-cleanup sites use. parked_with_tab
        // deliberately STAYS set: the paper is still waiting for the operator,
        // it just no longer holds a surface while it waits, and a re-offer
        // must not silently re-drive it. Engagement (`papio actions open`, the
        // inbox) clears the marker and mints a fresh route when the operator
        // is actually ready.
        await this.ctx.update((s) => patchJob(s, owner.job_id, { tab_id: -1 }));
      }
      // A live paper driving ANOTHER tab has superseded this one: a redrive,
      // re-offer or retry that opened elsewhere. That is the fact the daemon
      // can verify against the tab, so it is the one asserted.
      const result = await this.closeOwnedSurface(
        tabID,
        owner === undefined
          ? "job_inactive"
          : owner.tab_id >= 0 && owner.tab_id !== tabID
            ? "surface_superseded"
            : "handoff_parked",
      );
      if (result.closed) closed += 1;
    }
    closed += await this.retireUnledgeredContainerTabs();
    return { closed };
  }

  /** When each unledgered tab in papio's container was first seen by this
   * worker. Tab id and time only, never a URL. A worker that slept forgets,
   * which only delays a close; `lastAccessed` is the signal that survives. */
  private readonly unledgeredSeenAt = new Map<number, number>();

  /** Close cold tabs in papio's own group or work window that papio has no
   * record of: provider and resolver children opened before papio ledgered
   * them, blank tabs a download-only navigation left, tabs restored into
   * the group after a restart. Operator decision 2026-09-23: papio's group is
   * papio's. Measured live that day: about twenty-four tabs in the group
   * against two ledger records, so no record-driven path could reach them.
   *
   * Browser-local, because there is no binding to ask the daemon about. A
   * tab is closed only when it is not pinned, no live job tracks it, the
   * operator is not looking at it, and it is cold: last active at least
   * PARKED_SURFACE_COLD_MS ago, or seen unledgered by two passes that far
   * apart. A newborn child is warm by both measures, so the gap before
   * onUpdated ledgers it can never close it. No toast: papio does not know
   * that such a tab showed a paper it filed. */
  private async retireUnledgeredContainerTabs(): Promise<number> {
    if (this.deps.tabs.query === undefined) return 0;
    const tabs = await this.deps.tabs.query({}).catch(() => []);
    const ledger = await this.snapshotTabLedger();
    const now = this.deps.now();
    const pinnedToJobs = new Set<number>([
      ...this.ctx.adoptedViewerTabs.values(),
      ...this.ctx.completedDownloadTabs.values(),
    ]);
    const present = new Set<number>();
    let closed = 0;
    for (const tab of tabs) {
      const tabID = tab.id;
      if (
        tabID === undefined ||
        tab.pinned === true ||
        ledger[String(tabID)] !== undefined ||
        findByTab(this.ctx.store(), tabID) !== undefined ||
        pinnedToJobs.has(tabID)
      )
        continue;
      // Group membership only, never the work window: see closeOwnedTab's
      // unledgered-container gate for why the window is not evidence.
      const inPapioGroup =
        tab.groupId !== undefined &&
        tab.groupId >= 0 &&
        (await this.ctx.knownHandoffGroup(tab.groupId, tab.windowId)) !== undefined;
      if (!inPapioGroup) continue;
      present.add(tabID);
      const firstSeen = this.unledgeredSeenAt.get(tabID);
      if (firstSeen === undefined) this.unledgeredSeenAt.set(tabID, now);
      // lastAccessed is authoritative when the browser reports it: a tab
      // someone used a minute ago is warm however long this worker has
      // known it. Only without it do two passes that far apart decide.
      const cold =
        typeof tab.lastAccessed === "number"
          ? now - tab.lastAccessed >= PARKED_SURFACE_COLD_MS
          : firstSeen !== undefined && now - firstSeen >= PARKED_SURFACE_COLD_MS;
      if (!cold) continue;
      // closeOwnedTab re-reads the tab and applies the same gates as every
      // other close: pinned, outside the container, in front of the
      // operator, touched, or taken by a job meanwhile all keep it.
      if (await this.closeOwnedTab(tabID, "unledgered-container")) {
        closed += 1;
        this.unledgeredSeenAt.delete(tabID);
      }
    }
    for (const tabID of this.unledgeredSeenAt.keys())
      if (!present.has(tabID)) this.unledgeredSeenAt.delete(tabID);
    return closed;
  }

  /** Operator-initiated review focuses one bounded orphan surface; the
   * operator closes the reviewed tab through browser UI. */
  async cleanupOrphanTabs(): Promise<{ closed: number; focused: number }> {
    const { tab_ids } = await this.orphanTabStatus();
    const tabID = tab_ids[0];
    if (tabID === undefined) return { closed: 0, focused: 0 };
    try {
      await this.ctx.focusManagedTab(tabID);
      return { closed: 0, focused: 1 };
    } catch {
      return { closed: 0, focused: 0 };
    }
  }

  async inLifecycleChain<T>(work: () => Promise<T>): Promise<T> {
    const queued = this.lifecycleChain.then(work);
    this.lifecycleChain = queued.then(
      () => undefined,
      () => undefined,
    );
    return queued;
  }

  /** Slice 2b restart classification, run once at startup. SW restart:
   * chrome.storage.session still holds the epoch, so every tab id in the
   * ledger is still authoritative. Update: session was wiped, but the
   * durable local epoch's own tabs still resolve live, so the browser
   * process itself never died. Browser restart: neither holds — every tab
   * id's authority is gone, so a fresh epoch is minted that no existing
   * record can accidentally re-prove against; every record becomes
   * review-only until it is touched by a fresh open or close. */
  async classifyRestart(): Promise<"worker" | "update" | "browser"> {
    const epoch = this.deps.epoch;
    if (epoch === undefined) {
      this.browserEpoch = this.deps.randomUUID();
      return "browser";
    }
    const sessionEpoch = await epoch.getSession().catch(() => undefined);
    if (sessionEpoch !== undefined && sessionEpoch.length > 0) {
      this.browserEpoch = sessionEpoch;
      return "worker";
    }
    const localEpoch = await epoch.getLocal().catch(() => undefined);
    if (
      localEpoch !== undefined &&
      localEpoch.length > 0 &&
      (await this.epochStillLive(localEpoch))
    ) {
      this.browserEpoch = localEpoch;
      await epoch.setSession(localEpoch).catch(() => undefined);
      return "update";
    }
    const fresh = this.deps.randomUUID();
    this.browserEpoch = fresh;
    await epoch.setLocal(fresh).catch(() => undefined);
    await epoch.setSession(fresh).catch(() => undefined);
    return "browser";
  }

  /** "Any birth-record tab_hint resolves via tabs.get" — the bounded
   * liveness re-proof backing the update-vs-browser-restart distinction.
   * Reads storage directly: this runs before the ledger migration populates
   * the transaction cache. */
  private async epochStillLive(localEpoch: string): Promise<boolean> {
    if (this.deps.tabLedger === undefined) return false;
    let raw: unknown;
    try {
      raw = await this.deps.tabLedger.load();
    } catch {
      return false;
    }
    if (typeof raw !== "object" || raw === null) return false;
    const candidates = Object.values(raw as Record<string, unknown>)
      .filter(isSurfaceBirthRecord)
      .filter((record) => record.browser_epoch === localEpoch)
      .slice(0, RESTART_LIVENESS_SCAN_LIMIT);
    for (const record of candidates) {
      try {
        if (record.purpose === "keepalive" &&
            (record.keepalive?.document_id === undefined ||
             record.keepalive.document_id !== await this.ctx.liveDocumentEpoch(record.tab_hint))) continue;
        const tab = await this.deps.tabs.get(record.tab_hint);
        if (tab.id === record.tab_hint) return true;
      } catch {
        // One dead tab is not proof the browser restarted; keep scanning.
      }
    }
    return false;
  }

  /** A ledger record proves live ownership only when it was minted under
   * the CURRENT browser epoch (a "pre-v2" or stale epoch never re-proves —
   * the restart-class invariant above) and its tab_hint still resolves
   * live. Only such tabs may seed group/window adoption. */
  async ownedMemberTab(
    tabID: number,
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<TabInfo | undefined> {
    const record = ledger[String(tabID)];
    if (record === undefined || record.browser_epoch !== this.browserEpoch)
      return undefined;
    if (record.purpose === "keepalive" && this.restartClass !== "worker" &&
        (record.keepalive?.document_id === undefined ||
         record.keepalive.document_id !== await this.ctx.liveDocumentEpoch(tabID))) return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      return tab.id === tabID ? tab : undefined;
    } catch {
      return undefined;
    }
  }

  async groupHasOwnedMember(
    group: TabGroupInfo,
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<boolean> {
    const tabs = this.deps.tabs;
    if (tabs.query === undefined) return false;
    let members: TabInfo[];
    try {
      members = await tabs.query({ groupId: group.id });
    } catch {
      return false;
    }
    for (const tab of members) {
      if (tab.id === undefined) continue;
      if ((await this.ownedMemberTab(tab.id, ledger)) !== undefined)
        return true;
    }
    return false;
  }

  /** Rediscover the dedicated work window through an owned member instead
   * of trusting the persisted id: a stale id can 404 (openWorkWindowTab's
   * own fallback already handles that) or, worse, collide with a window
   * Chrome reassigned the same numeric id to after a restart. */
  async adoptWorkWindowFromOwnedMembers(
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<void> {
    if (this.deps.tabs.query === undefined) return;
    const owned: TabInfo[] = [];
    for (const key of Object.keys(ledger)) {
      const tabID = Number(key);
      if (!Number.isInteger(tabID) || tabID < 0) continue;
      const tab = await this.ownedMemberTab(tabID, ledger);
      if (tab !== undefined) owned.push(tab);
    }
    const current = this.ctx.store().workWindowID;
    if (current !== undefined && owned.some((tab) => tab.windowId === current))
      return;
    const adopted = owned.find((tab) => tab.windowId !== undefined)?.windowId;
    if (adopted !== current) {
      await this.ctx.update((s) => {
        const next = { ...s };
        if (adopted === undefined) delete next.workWindowID;
        else next.workWindowID = adopted;
        return next;
      });
    }
  }

  /** The generic Slice 2b close transaction: request a one-use daemon
   * authorization for the tab's binding, persist its tombstone before
   * touching the tab, then re-verify liveness before running the removal
   * through the existing closeOwnedTab primitive. Positive evidence closes;
   * every other outcome retains the surface. */
  async closeOwnedSurface(
    tabID: number,
    disposition: SurfaceCloseDisposition,
    gateOccurrenceID?: string,
  ): Promise<{ closed: boolean }> {
    await this.ctx.surfaceReady();
    const ledger = await this.snapshotTabLedger();
    const record = ledger[String(tabID)];
    if (record === undefined || surfaceIsCeded(record))
      return { closed: false };
    const result = await this.inLifecycleChain(() =>
      this.closeAuthorizedRecord(tabID, record, disposition, gateOccurrenceID),
    );
    // Whatever path closed it - adoption, a terminal cancel, a later pass
    // once the operator looked away - a tab that showed a paper papio just
    // filed is offered back once.
    if (
      result.removedURL !== undefined &&
      record.job_id !== undefined &&
      this.ctx.filedJobs.has(record.job_id) &&
      surfaceShowedPaper(record.purpose, result.removedURL)
    )
      this.ctx.noteFiledPaperClosed(record.job_id, result.removedURL);
    return { closed: result.closed };
  }

  /** §2.3: request a one-use close authorization for `bindingID` under
   * `disposition`. Shared by closeAuthorizedRecord's tab-tombstone dance
   * and the owner_closed reducer counterpart (a tab already gone has
   * nothing to tombstone, only the daemon to inform). */
  async requestCloseAuthorization(
    bindingID: string,
    disposition: SurfaceCloseDisposition,
    gateOccurrenceID: string | undefined,
    // Only surface_superseded names a tab, and it must: it is the one
    // disposition the daemon settles by comparing this surface to the tab it
    // believes drives the claim. The owner_closed counterpart has no tab left
    // to name and never asserts it.
    surfaceTabID?: number,
  ): Promise<
    | {
        authorized: true;
        authorizationID: string;
        nonce: string;
        generation: number;
      }
    | { authorized: false; unclaimed?: true }
  > {
    const generation = this.ctx.holderGeneration();
    if (generation === undefined) return { authorized: false };
    if (disposition === "surface_superseded" && surfaceTabID === undefined)
      return { authorized: false };
    const result = await this.ctx.requestCorrelated("surface_close_request", {
      binding_id: bindingID,
      browser_holder_generation: generation,
      disposition,
      ...(disposition === "claim_abandoned" && gateOccurrenceID !== undefined
        ? { gate_occurrence_id: gateOccurrenceID }
        : {}),
      ...(disposition === "surface_superseded" && surfaceTabID !== undefined
        ? { surface_tab_id: surfaceTabID }
        : {}),
    });
    if (result.kind !== "response" || result.payload === undefined)
      return { authorized: false };
    // The daemon distinguishing "I have no stake in this surface" from "I am
    // withholding it". Only a refusal binds the extension; an unclaimed
    // binding falls back to the browser-local authority that owns ordinary
    // handoff tabs, still subject to every guard below.
    if (result.payload["outcome"] === "unclaimed")
      return { authorized: false, unclaimed: true };
    if (result.payload["outcome"] !== "authorized") return { authorized: false };
    const authorizationID = result.payload["close_authorization_id"];
    const nonce = result.payload["nonce"];
    const responseGeneration = result.payload["browser_holder_generation"];
    if (
      typeof authorizationID !== "string" ||
      typeof nonce !== "string" ||
      typeof responseGeneration !== "number"
    )
      return { authorized: false };
    this.ctx.setHolderGeneration(responseGeneration);
    return {
      authorized: true,
      authorizationID,
      nonce,
      generation: responseGeneration,
    };
  }

  private async closeAuthorizedRecord(
    tabID: number,
    record: SurfaceBirthRecord,
    disposition: SurfaceCloseDisposition,
    gateOccurrenceID: string | undefined,
  ): Promise<SurfaceCloseResult> {
    const authorization = await this.requestCloseAuthorization(
      record.binding_id,
      disposition,
      gateOccurrenceID,
      tabID,
    );
    if (!authorization.authorized) {
      // A binding the daemon has no claim on is an ordinary handoff surface:
      // there is no authorization to tombstone because there is nothing
      // daemon-side to consume, and papio's own guards are the whole of the
      // decision. Retiring it is browser-local, exactly as the handoff-drive
      // timeout has always intended - that intent was simply refused every
      // time before the daemon could say which kind of "no" it meant.
      if (authorization.unclaimed === true)
        return this.retireOwnedSurface(tabID, record.binding_id, "unclaimed");
      return { closed: false };
    }
    const bindingID = record.binding_id;
    const tombstoned = await this.runTabLedgerTransaction((ledger) => {
      const current = ledger[String(tabID)];
      if (current === undefined || current.binding_id !== bindingID)
        return { value: false, changed: false };
      ledger[String(tabID)] = {
        ...current,
        pending_close: {
          authorization_id: authorization.authorizationID,
          nonce: authorization.nonce,
          holder_generation: authorization.generation,
          recorded_at: this.deps.now(),
          disposition,
        },
      };
      return { value: true, changed: true };
    });
    if (!tombstoned) return { closed: false };
    return this.retireOwnedSurface(tabID, bindingID, "authorized");
  }

  /** The one fresh tabs.get the plan requires before the awaited removal,
   * shared by both close authorities: a daemon-authorized claim surface
   * consuming its tombstone, and an unclaimed ordinary handoff surface the
   * daemon has no stake in. The guards are identical because they are the
   * whole of the decision in the unclaimed case, so they must never diverge.
   *
   * A pinned tab is ceded and detached instead of retried: its tombstone is
   * cleared so a later restart never re-requests a closure the operator has
   * already claimed by pinning. A PDF or article is not a reason to keep the
   * tab (operator decision, 2026-09-23). This is the FIRST of two
   * independent freshness checks, not the only one — closeOwnedTab below
   * re-derives the same predicates off its own fresh get, and additionally
   * compares the touch epoch, so a touch landing anywhere between this get
   * and the eventual tabs.remove is still caught even when it is invisible to
   * this particular snapshot. */
  private async retireOwnedSurface(
    tabID: number,
    bindingID: string,
    authority: "authorized" | "unclaimed",
  ): Promise<SurfaceCloseResult> {
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(tabID);
    } catch {
      // Already gone — nothing left to remove or cede.
      return { closed: true };
    }
    if (tab.pinned === true) {
      // Pinning is an operator act on this tab: takeover, ceded permanently.
      await this.cedeOwnedTab(tabID, bindingID, "pinned_at_close");
      return { closed: false };
    }
    if (await this.operatorIsViewing(tab)) {
      // Deferred, not ceded: the tombstone stays replayable, so the surface
      // retires once it is no longer in front of the operator. Ceding here
      // burned the one-use authorization AND detached the binding, which is
      // how an explicitly opened sign-in tab became permanently unretirable
      // (live 2026-08-20).
      return { closed: false };
    }
    const removed = await this.closeOwnedTab(
      tabID,
      authority === "unclaimed" ? "unclaimed-close" : "authorized-close",
    );
    return removed
      ? { closed: true, ...(tab.url === undefined ? {} : { removedURL: tab.url }) }
      : { closed: false };
  }

  /** Startup replay: a failed remove or a worker death between tombstone
   * persistence and removal leaves the tombstone in place. Re-requesting
   * the same binding's authorization is idempotent daemon-side (the same
   * live token comes back), so replay is just re-running the transaction
   * from its authorization step. */
  async replayPendingCloseTombstones(): Promise<void> {
    const ledger = await this.snapshotTabLedger();
    for (const [key, record] of Object.entries(ledger)) {
      const pending = record.pending_close;
      if (pending === undefined) continue;
      const tabID = Number(key);
      if (!Number.isInteger(tabID) || tabID < 0) continue;
      if (record.browser_epoch !== this.browserEpoch) {
        // A tombstone minted under a prior browser epoch carries no live
        // tab-ID authority (classifyRestart's browser-restart case: the
        // numeric id may already have been reused by an unrelated tab).
        // Never authorize or remove against it — cede the record to the
        // standing operator-review path instead of risking a close/reuse
        // collision.
        await this.runTabLedgerTransaction((current) => {
          const existing = current[key];
          if (
            existing === undefined ||
            existing.binding_id !== record.binding_id
          )
            return { value: undefined, changed: false };
          const next: SurfaceBirthRecord = { ...existing, ceded: true };
          delete next.pending_close;
          delete next.job_id;
          current[key] = next;
          return { value: undefined, changed: true };
        });
        continue;
      }
      const disposition = isSurfaceCloseDisposition(pending.disposition)
        ? pending.disposition
        : "scaffold_idle";
      await this.closeAuthorizedRecord(tabID, record, disposition, undefined);
    }
  }

  /** Coalescing trigger for replayPendingCloseTombstones, called from the
   * hello_ack handler every time a fresh ack actually carries
   * browser_holder_generation (always true for a holder ack once every
   * daemon this extension talks to ships that field — a fresh connect, a
   * reconnect, and a pending→holder role promotion are all, from this
   * worker's perspective, just another such ack). bootstrapSurfaceLifecycle
   * cannot itself close this gap: it runs once, before this worker
   * necessarily knows the daemon's live browser_holder_generation, and a
   * race it loses today has no other scheduled retry (P0: a persisted
   * close tombstone can strand forever). Gating on the ack actually
   * carrying a generation — rather than merely negotiating the feature —
   * keeps this a no-storage-touch no-op on every hello_ack that carries
   * nothing new to act on. A replay already in flight absorbs a concurrent
   * trigger as one more full pass instead of racing it, same shape as
   * scheduleObservationOutboxDrain below. Never awaited from the hello_ack
   * handler — inLifecycleChain's queued work must be free to outlive that
   * handler's own turn on the inbound FIFO. */
  scheduleCloseTombstoneReplay(): void {
    if (this.closeTombstoneReplayRunning) {
      this.closeTombstoneReplayRerunRequested = true;
      return;
    }
    this.closeTombstoneReplayRunning = true;
    void this.inLifecycleChain(() => this.replayPendingCloseTombstones())
      .catch((error) =>
        console.error("papio: close-tombstone replay failed", error),
      )
      .finally(() => {
        this.closeTombstoneReplayRunning = false;
        if (this.closeTombstoneReplayRerunRequested) {
          this.closeTombstoneReplayRerunRequested = false;
          this.scheduleCloseTombstoneReplay();
        }
      });
  }

  /** Keepalive uses the same birth ledger as every other owned surface.
   * Elapsed time never revokes ownership of a still-live same-epoch tab. */
  keepaliveOwnership(): KeepaliveOwnership {
    return {
      recover: (origin) => this.recoverKeepalive(origin),
      record: (tabID, origin, reloadAt) => this.recordKeepalive(tabID, origin, reloadAt),
      update: (tabID, paused, reloadAt, leftOrigin) => this.updateKeepalive(tabID, paused, reloadAt, leftOrigin),
      forget: (tabID) => this.forgetKeepalive(tabID),
    };
  }

  private async recoverKeepalive(origin?: string): ReturnType<KeepaliveOwnership["recover"]> {
    await this.ctx.ready();
    await this.ctx.surfaceReady();
    if (this.deps.epoch === undefined || this.browserEpoch === undefined ||
        await this.deps.epoch.getSession().catch(() => undefined) !== this.browserEpoch) {
      throw new Error("browser epoch unavailable");
    }
    const digest = origin === undefined ? undefined : await originDigestOf(origin);
    if (origin !== undefined && digest === undefined) throw new Error("resolver digest unavailable");
    const ledger = await this.snapshotTabLedger(true);
    let recovered: Awaited<ReturnType<KeepaliveOwnership["recover"]>>;
    for (const [key, record] of Object.entries(ledger)) {
      if (record.purpose !== "keepalive") continue;
      const id = record.tab_hint;
      let owned = this.deps.epoch !== undefined && this.browserEpoch !== undefined &&
        record.browser_epoch === this.browserEpoch && String(id) === key &&
        Number.isInteger(id) && id >= 0 &&
        !record.ceded && !record.legacy && !record.content && !record.pending_close;
      if (owned && this.restartClass !== "worker") {
        // A reload can erase session storage. Recycled IDs cannot prove a
        // browser epoch; this record's own document must still be alive.
        owned = record.keepalive?.document_id !== undefined &&
          record.keepalive.document_id === await this.ctx.liveDocumentEpoch(id);
      }
      if (owned) {
        try { owned = (await this.deps.tabs.get(id)).id === id; }
        catch (error) {
          if (!isTabAbsenceRejection(error)) throw error;
          owned = false;
        }
      }
      if (owned && digest !== undefined && record.origin_digest === digest && recovered === undefined) {
        recovered = { tabID: id, paused: record.keepalive!.paused, leftOrigin: record.keepalive!.left_origin, reloadAt: record.keepalive!.reload_at };
        await this.updateKeepalive(id, recovered.paused);
        continue;
      }
      // Off/origin change closes only proven owners. Stale/reused
      // hints lose their record without touching whatever now has that ID.
      if (owned) await this.deps.tabs.remove(id);
      await this.forgetKeepalive(id);
    }
    return recovered;
  }

  private async recordKeepalive(tabID: number, origin: string, reloadAt: number): Promise<boolean> {
    await this.ctx.ready();
    await this.ctx.surfaceReady();
    if (this.deps.tabLedger === undefined || this.deps.epoch === undefined || this.browserEpoch === undefined) return false;
    if (await this.deps.epoch.getSession().catch(() => undefined) !== this.browserEpoch) return false;
    const digest = await originDigestOf(origin);
    if (digest === undefined) return false;
    // The caller just created this blank tab. Persist birth BEFORE any
    // resolver/IdP navigation, so a crash cannot orphan a sign-in surface.
    const tab = await this.deps.tabs.get(tabID);
    // Chrome can return an empty committed URL while its newly created blank
    // is still loading. Only that exact pending blank counts; a navigation
    // away, even from a committed blank, must not acquire ownership.
    const pendingBlank = tab.pendingUrl === "about:blank";
    const committedBlank = tab.url === "about:blank";
    if (tab.id !== tabID ||
        (tab.pendingUrl !== undefined && !pendingBlank) ||
        (!committedBlank && !(tab.url === "" && pendingBlank))) return false;
    const documentID = await this.ctx.liveDocumentEpoch(tabID);
    try {
      return await this.runTabLedgerTransaction((ledger) => {
        if (ledger[String(tabID)] !== undefined) return { value: false, changed: false };
        ledger[String(tabID)] = {
          binding_id: this.deps.randomUUID(), tab_hint: tabID, purpose: "keepalive",
          browser_epoch: this.browserEpoch!, extension_generation: this.deps.manifestVersion,
          created_at: this.deps.now(), origin_digest: digest,
          keepalive: { paused: false, left_origin: false, reload_at: reloadAt,
            ...(documentID === undefined ? {} : { document_id: documentID }) },
        };
        return { value: true, changed: true };
      }, true);
    } catch { return false; }
  }

  private async updateKeepalive(tabID: number, paused: boolean, reloadAt?: number, leftOrigin?: boolean): Promise<void> {
    const documentID = await this.ctx.liveDocumentEpoch(tabID);
    await this.runTabLedgerTransaction((ledger) => {
      const record = ledger[String(tabID)];
      if (record?.purpose !== "keepalive" || record.keepalive === undefined ||
          record.browser_epoch !== this.browserEpoch || record.ceded) return { value: undefined, changed: false };
      ledger[String(tabID)] = { ...record, keepalive: {
        paused, left_origin: leftOrigin ?? record.keepalive.left_origin,
        reload_at: reloadAt ?? record.keepalive.reload_at,
        ...(documentID === undefined ? {} : { document_id: documentID }),
      } };
      return { value: undefined, changed: true };
    });
  }

  private async forgetKeepalive(tabID: number): Promise<void> {
    await this.runTabLedgerTransaction((ledger) => {
      if (ledger[String(tabID)]?.purpose !== "keepalive") return { value: undefined, changed: false };
      delete ledger[String(tabID)];
      return { value: undefined, changed: true };
    });
  }

  /** Synchronous, no async work: called from inside the onUpdated/
   * onActivated listener callbacks themselves, before those callbacks'
   * own async handlers ever run. See tabTouchEpoch. */
  touchTab(tabID: number): void {
    this.tabTouchEpoch.set(tabID, (this.tabTouchEpoch.get(tabID) ?? 0) + 1);
  }

  /** The tab is gone: its touch counter goes with it. */
  forgetTabTouch(tabID: number): void {
    this.tabTouchEpoch.delete(tabID);
  }
}
