/**
 * Which handoff tabs papio arms for a job's signed PDF viewer, and Chrome's
 * session rules that make that viewer response download instead of render.
 * Firefox arms the same tabs for the stream capture in
 * viewer-stream-capture.ts.
 *
 * A signed viewer URL is not reusable: ScienceDirect answered a second request
 * with HTML (2026-09-20, and again through Chrome downloads on 2026-09-23), and
 * Silverchair answered HTTP 400. The PDF exists only in the one response the
 * browser already received. `Content-Disposition: attachment` on THAT response
 * turns the navigation into a download of the same bytes, with no second
 * request, and Chrome's own download then runs through papio's filename
 * steering and the daemon's adoption and identity checks.
 *
 * `chrome.pageCapture` cannot do this: blink's FrameSerializer writes a PDF
 * plugin document as its `<embed>` markup, so MHTML never holds the PDF.
 *
 * The URL conditions follow `requiresNativeViewerDownload` without its
 * credential-length bound. DNR compiles `regexFilter` under a 2 KB RE2 budget,
 * and the credential-parameter alternation exceeds it with or without the
 * counted repetition, so each parameter is its own `urlFilter` rule. The PDF
 * content-type condition keeps a login or ticket redirect from matching, and
 * `tabIds` keeps every rule inside papio's own handoff tabs. That condition
 * needs Chrome 128; an older Chrome rejects the rules and papio keeps its
 * previous behaviour.
 */
import { CREDENTIAL_PARAMS } from "./deliver";
import { findByTab, type ActiveJob, type StoreShape } from "./state";

export const SIGNED_VIEWER_HOST = "pdf.sciencedirectassets.com";

const FIRST_RULE_ID = 7301;
const PARAMS = [...CREDENTIAL_PARAMS];

/** Every session rule id this module owns; removal always names all of them. */
export const VIEWER_DOWNLOAD_RULE_IDS: readonly number[] = Array.from(
  { length: PARAMS.length + 1 },
  (_, index) => FIRST_RULE_ID + index,
);

export interface ViewerDownloadRule {
  id: number;
  priority: number;
  action: {
    type: "modifyHeaders";
    responseHeaders: { header: string; operation: "set"; value: string }[];
  };
  condition: {
    tabIds: number[];
    resourceTypes: ("main_frame" | "sub_frame")[];
    responseHeaders: { header: string; values: string[] }[];
    requestDomains?: string[];
    urlFilter?: string;
  };
}

/** The rules for `tabIds`. An empty list yields no rules: DNR rejects an empty
 * `tabIds`, and a rule without one would reach every tab in the browser. */
export function viewerDownloadRules(tabIds: readonly number[]): ViewerDownloadRule[] {
  if (tabIds.length === 0) return [];
  const action: ViewerDownloadRule["action"] = {
    type: "modifyHeaders",
    responseHeaders: [{ header: "content-disposition", operation: "set", value: 'attachment; filename="paper.pdf"' }],
  };
  const condition: ViewerDownloadRule["condition"] = {
    tabIds: [...tabIds],
    resourceTypes: ["main_frame", "sub_frame"],
    responseHeaders: [{ header: "content-type", values: ["application/pdf*", "application/x-pdf*"] }],
  };
  return [
    { id: FIRST_RULE_ID, priority: 1, action, condition: { ...condition, requestDomains: [SIGNED_VIEWER_HOST] } },
    // `^` is DNR's separator class: it matches `?` or `&`, never `_`, so
    // `^token=` does not also match `access_token=`.
    ...PARAMS.map((param, index) => ({
      id: FIRST_RULE_ID + 1 + index, priority: 1, action, condition: { ...condition, urlFilter: `^${param}=` },
    })),
  ];
}

/** True when a download's URL has the shape these rules act on, so papio can
 * bind the download to the tab whose navigation produced it. */
export function matchesViewerDownloadRule(value: string): boolean {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return false;
  }
  const host = url.hostname.toLowerCase();
  if (host === SIGNED_VIEWER_HOST || host.endsWith(`.${SIGNED_VIEWER_HOST}`)) return true;
  for (const name of url.searchParams.keys()) {
    if (CREDENTIAL_PARAMS.has(name.toLowerCase())) return true;
  }
  return false;
}

/** Chrome's session-rules seam; see BridgeDeps.declarativeNetRequest. */
export interface ViewerRuleSessionRules {
  updateSessionRules(options: {
    removeRuleIds?: number[];
    addRules?: ViewerDownloadRule[];
  }): Promise<void>;
}

/** The browser seams the rule sync uses: a structural subset of BridgeDeps,
 * held by reference and read at call time. */
export interface ViewerRuleDeps {
  declarativeNetRequest?: ViewerRuleSessionRules;
  permissions: {
    contains(perm: { origins: string[] }): Promise<boolean>;
  };
}

/** The Bridge state and behaviour the rule sync depends on. */
export interface ViewerRuleContext {
  isFirefox(): boolean;
  /** Firefox: whether the webRequest stream capture is available. */
  viewerCaptureAvailable(): boolean;
  /** The current managed-state snapshot. Its `viewerChildTabs` holds the
   * armed child tabs, so they survive a suspended worker or event page. */
  store(): StoreShape;
  /** Record (or, with no job, forget) an armed child tab in managed state. */
  setViewerChildTab(tabID: number, jobID: string | undefined): void;
  hasDelegatedAuthority(job: ActiveJob): boolean;
  /** Jobs the article agent drives; it binds its own navigation downloads. */
  readonly agentLoops: ReadonlyMap<string, object>;
  /** Each job's tracked browser downloads. */
  readonly downloads: ReadonlyMap<string, { readonly ids: ReadonlySet<number> }>;
  /** Tabs a finished download keeps open until the daemon acknowledges. */
  readonly completedDownloadTabs: ReadonlyMap<string, number>;
  /** Whether the job may still take its one signed-viewer download. */
  viewerAttemptAllowed(jobID: string): boolean;
  reportNativeViewerDownloadRequired(
    jobID: string,
    url: string,
    tabID: number,
    spent?: { detail: string; downloadID?: number },
  ): Promise<void>;
}

/** Which tabs are armed for a signed viewer, and keeping Chrome's session
 * rules in line with them. On Chrome that needs the declarativeNetRequest
 * seam; on Firefox, the stream capture. Without either, nothing is armed. */
export class ViewerRuleSync {
  /** Armed tabs that have since closed. Neither browser reuses a tab id within
   * a session, so a closed id stays disarmed until the job lets go. */
  private readonly viewerRuleClosedTabs = new Set<number>();
  /** Sorted tab ids the installed rules cover; undefined until this worker
   * first writes them, so a new worker replaces its predecessor's rules. */
  private viewerRuleTabsApplied: string | undefined;
  /** "unsupported" once Chrome rejects the rules (before Chrome 128, which
   * lacks the response-header condition); the refetch fallback then applies. */
  private viewerRuleSupport: "unknown" | "active" | "unsupported" = "unknown";
  private viewerRuleChain: Promise<void> = Promise.resolve();
  /** Recent navigation URL -> tab id, so a download a viewer rule produced
   * binds to the tab that navigated to it (Chrome's DownloadItem has no tab).
   * Memory-only and bounded; never persisted or sent. */
  private readonly recentNavigationTabs = new Map<string, number>();

  constructor(
    private readonly deps: ViewerRuleDeps,
    private readonly ctx: ViewerRuleContext,
  ) {}

  /** Arming has an effect: Chrome rules that have not been rejected, or
   * Firefox's stream capture. */
  private armingActive(): boolean {
    return this.ctx.isFirefox()
      ? this.ctx.viewerCaptureAvailable()
      : this.deps.declarativeNetRequest !== undefined && this.viewerRuleSupport !== "unsupported";
  }

  /** Tab id -> job id for every armed tab: a delegated job's handoff tab, and
   * tabs it opened, while that job could still take its one signed-viewer
   * download. Agent-driven jobs are left out because the agent binds its own
   * navigation downloads. A tab two jobs claim belongs to neither. */
  private viewerRuleTabs(): Map<number, string> {
    const armed = new Map<number, string>();
    if (!this.armingActive()) return armed;
    const shared = new Set<number>();
    const arm = (tabID: number, jobID: string): void => {
      if (this.viewerRuleClosedTabs.has(tabID)) return;
      const owner = armed.get(tabID);
      if (owner !== undefined && owner !== jobID) shared.add(tabID);
      armed.set(tabID, jobID);
    };
    for (const job of this.ctx.store().activeJobs) {
      if (
        job.tab_id < 0 || !this.ctx.hasDelegatedAuthority(job) || this.ctx.agentLoops.has(job.job_id) ||
        (job.status !== "accepted" && job.status !== "awaiting_download" && job.status !== "auth_pending") ||
        (this.ctx.downloads.get(job.job_id)?.ids.size ?? 0) > 0 || this.ctx.completedDownloadTabs.has(job.job_id) ||
        !this.ctx.viewerAttemptAllowed(job.job_id)
      )
        continue;
      arm(job.tab_id, job.job_id);
      for (const [child, owner] of Object.entries(this.ctx.store().viewerChildTabs ?? {}))
        if (owner === job.job_id) arm(Number(child), owner);
    }
    for (const tabID of shared) armed.delete(tabID);
    return armed;
  }

  /** The job whose armed tab this is, if any. */
  armedJob(tabID: number): string | undefined {
    return this.viewerRuleTabs().get(tabID);
  }

  /** Bring Chrome's session rules in line with `viewerRuleTabs`. Serialized,
   * and a no-op while the armed set is unchanged. A rejection (Chrome before
   * 128 has no response-header condition) marks the rules unsupported, which
   * restores the refetch fallback and removes anything left installed. */
  syncViewerDownloadRules(): Promise<void> {
    const dnr = this.deps.declarativeNetRequest;
    if (dnr === undefined || this.ctx.isFirefox()) return Promise.resolve();
    this.viewerRuleChain = this.viewerRuleChain.then(async () => {
      const tabs = [...this.viewerRuleTabs().keys()].sort((a, b) => a - b);
      const key = tabs.join(",");
      if (key === this.viewerRuleTabsApplied) return;
      try {
        await dnr.updateSessionRules({
          removeRuleIds: [...VIEWER_DOWNLOAD_RULE_IDS],
          ...(tabs.length > 0 ? { addRules: viewerDownloadRules(tabs) } : {}),
        });
        this.viewerRuleTabsApplied = key;
        if (tabs.length > 0) this.viewerRuleSupport = "active";
      } catch (error) {
        this.viewerRuleTabsApplied = undefined;
        if (tabs.length === 0) return;
        console.error("papio: signed-viewer download rules unavailable; keeping the refetch fallback", error);
        this.viewerRuleSupport = "unsupported";
        void this.syncViewerDownloadRules();
      }
    });
    return this.viewerRuleChain;
  }

  /** A tab an armed tab opened (View PDF with target=_blank) is armed too.
   * Chrome calls this on the tab's first update, which it sends as the
   * navigation starts and before the response the rule acts on. Firefox
   * also calls it from tabs.onCreated, because there the child's response
   * can arrive before its first update. */
  noteViewerRuleChild(tabID: number, openerTabID: number | undefined): void {
    if (
      openerTabID === undefined || this.ctx.store().viewerChildTabs?.[String(tabID)] !== undefined ||
      !this.armingActive() || findByTab(this.ctx.store(), tabID) !== undefined
    )
      return;
    const owner = this.viewerRuleTabs().get(openerTabID);
    if (owner === undefined) return;
    this.ctx.setViewerChildTab(tabID, owner);
    void this.syncViewerDownloadRules();
  }

  noteViewerRuleTabRemoved(tabID: number): void {
    if (!this.armingActive()) return;
    if (this.ctx.store().viewerChildTabs?.[String(tabID)] !== undefined) {
      this.ctx.setViewerChildTab(tabID, undefined);
    } else {
      if (findByTab(this.ctx.store(), tabID) === undefined) return;
      this.viewerRuleClosedTabs.add(tabID);
    }
    void this.syncViewerDownloadRules();
  }

  /** Remember which tab navigated to a URL so the download a viewer rule
   * produces can be bound to that tab. Recorded whether or not this worker
   * has written the rules yet: they outlive a sleeping worker, and the
   * navigation event is what wakes it. Memory-only and bounded. */
  noteNavigationTab(tabID: number, url: string | undefined): void {
    if (url === undefined || this.deps.declarativeNetRequest === undefined || this.ctx.isFirefox()) return;
    this.recentNavigationTabs.delete(url);
    this.recentNavigationTabs.set(url, tabID);
    if (this.recentNavigationTabs.size > 64) {
      const oldest = this.recentNavigationTabs.keys().next().value;
      if (oldest !== undefined) this.recentNavigationTabs.delete(oldest);
    }
  }

  /** The armed job whose tab navigated to this download's URL, when the
   * download has the shape a viewer rule acts on. Chrome's DownloadItem
   * carries no tab id, so the navigation URL is the exact link; host
   * correlation cannot tell two ScienceDirect jobs apart. */
  viewerRuleBinding(item: {
    url?: string | undefined;
    finalUrl?: string | undefined;
    byExtensionId?: string | undefined;
  }): { jobID: string; tabID: number; url: string } | undefined {
    const url = item.finalUrl ?? item.url;
    if (item.byExtensionId !== undefined || url === undefined || !matchesViewerDownloadRule(url)) return undefined;
    const armed = this.viewerRuleTabs();
    for (const candidate of [item.url, item.finalUrl]) {
      const tabID = candidate === undefined ? undefined : this.recentNavigationTabs.get(candidate);
      const jobID = tabID === undefined ? undefined : armed.get(tabID);
      if (tabID !== undefined && jobID !== undefined) return { jobID, tabID, url };
    }
    return undefined;
  }

  /** A signed viewer rendered although its tab could be armed. Its one
   * response is spent and a refetch returns HTML, so ask for the viewer's
   * Download button and name why. False only where arming is unavailable, so
   * the caller keeps its fallback. */
  async reportViewerRuleMiss(jobID: string, url: string, tabID: number): Promise<boolean> {
    if (this.ctx.isFirefox() ? !this.ctx.viewerCaptureAvailable() : this.viewerRuleSupport !== "active") return false;
    let permitted = false;
    try {
      permitted = await this.deps.permissions.contains({ origins: [`https://${new URL(url).hostname}/*`] });
    } catch {
      permitted = false;
    }
    await this.ctx.reportNativeViewerDownloadRequired(jobID, url, tabID, {
      detail: !permitted
        ? "viewer host permission missing"
        : this.ctx.isFirefox()
          ? "the signed viewer rendered outside papio's capture"
          : "the signed viewer rendered outside papio's download rule",
    });
    return true;
  }
}
