// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Browser tab and window shapes the bridge reads, as structural subsets of
// the chrome.tabs/chrome.windows/chrome.tabGroups types, so bridge code and
// its test fakes share one definition without depending on a browser global.

export interface TabInfo {
  incognito?: boolean | undefined;
  cookieStoreId?: string | undefined;
  id?: number | undefined;
  url?: string | undefined;
  pendingUrl?: string | undefined;
  status?: string | undefined;
  /** Page title when available; used only for local IdP failure-page
   * heuristics and never sent over the bridge. */
  title?: string | undefined;
  /** Chrome sets this on a tab opened by another tab (e.g. a provider's
   * "download" that opens the PDF in a new viewer tab). Correlates the viewer
   * tab back to the tracked handoff tab that spawned it. */
  windowId?: number | undefined;
  /** Chrome's group membership id; -1 means the tab is not grouped. */
  groupId?: number | undefined;
  openerTabId?: number | undefined;
  /** Chrome marks the keepalive resolver tab pinned; papio's broker tabs never
   * are. Lets the idle-close check keep a keepalive-pinned work window alive. */
  pinned?: boolean | undefined;
  /** Whether the tab is the selected tab in its window. Orphan cleanup never
   * closes a tab the user is actively looking at. */
  active?: boolean | undefined;
  /** When the tab was last active, in ms since the epoch; a tab never made
   * active reports its creation time. The positive signal that an unledgered
   * tab in papio's container has gone cold. */
  lastAccessed?: number | undefined;
}

export interface TabChangeInfo {
  url?: string | undefined;
  status?: string | undefined;
  /** Chrome fires a title-only update when a document's title resolves after
   * the load completes. Needed because some IdP failure pages are classifiable
   * only by title (see onTabUpdated). */
  title?: string | undefined;
}

export interface WindowInfo {
  id?: number | undefined;
  /** "minimized" | "normal" | ... — used only to avoid un-maximizing a normal
   * window when surfacing. */
  state?: string | undefined;
  /** Populated by windows.create when the window is created with a URL. */
  tabs?: TabInfo[] | undefined;
  /** Reported by windows.create. Read only by the toast, which must know when
   * the browser ignored `focused: false` (macOS Firefox, bugzilla 1271047). */
  focused?: boolean | undefined;
}

export interface TabGroupInfo {
  id: number;
  collapsed: boolean;
  title?: string | undefined;
  /** Groups are scoped to a browser window, so this must agree with every tab
   * moved into the group. */
  windowId?: number | undefined;
}
