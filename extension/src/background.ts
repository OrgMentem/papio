// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio MV3 bridge service worker. Least-privilege handoff between the daemon
// (via the papio-native-host native-messaging host) and ordinary Chrome tabs.
//
// Invariants enforced here, not merely documented:
//   - Every inbound frame is re-parsed with parseBrowserMessage; a ProtocolError
//     drops the connection (fail closed).
//   - Outgoing frames are validated with the same parser before postMessage, so
//     the extension can never emit a malformed or privacy-violating frame.
//   - auth_pending/auth_returned carry timing only. URL/host/title are compared
//     locally and NEVER placed in any outgoing frame or persisted state.
//   - Exactly one broker-owned tab per job; downloads are adopted only when they
//     correlate to that tab, and only when a single candidate is unambiguous.
//
// The class is constructed with an injected BridgeDeps seam so the whole flow is
// unit-testable without a real chrome runtime.

import {
  PAPIO_MARK,
  PAPIO_MARK_SIZE_PX,
  PAPIO_MARK_VIEWBOX,
  TOAST_COPY,
  TOAST_PAGE_ACTION_MESSAGE,
  TOAST_PAGE_DISMISS_MESSAGE,
  TOAST_WINDOW_MS,
  TOAST_WINDOW_SIZE,
  type ToastInjection,
  type ToastKind,
  type ToastPayload,
  toastKindForLoss,
} from "./toast-view";
import {
  TOAST_ACTION_MESSAGE,
  TOAST_DISMISS_MESSAGE,
  TOAST_PENDING_MESSAGE,
} from "./toast";
import {
  BROWSER_PROTOCOL_VERSION,
  EFFECT_PERMIT_FEATURE,
  durablePdfGrabState,
  MAX_BROWSER_MESSAGE_BYTES,
  MsgPageCapture,
  MsgPageCaptureRequestResult,
  parseBrowserMessage,
  parseBrowserMessageWithLegacyInstitutionalNavigation,
  isBareLowercaseHTTPSOrigin,
  isCanonicalKey,
  isDetectorText,
  type ActivityEntryPayload,
  type ArtifactProducerPayload,
  type BrowserMessage,
  type BrowserMessageType,
  type BrowserSessionRole,
  type DeliveryRoute,
  type DeliverySessionEvidence,
  type PageAcquireAckPayload,
  type PageAcquirePayload,
  type PageBulkIdentifier,
  type PageBulkStatusItem,
  type PageBulkSubmitSource,
  type PageCapturePayload,
  type PageCaptureRequestPayload,
  type PageCaptureRequestResultPayload,
  type SurfacePresencePayload,
  type WorkPulseResponsePayload,
  type SurfaceCloseRequestPayload,
  type SurfaceCloseResponsePayload,
  SURFACE_CLOSE_FEATURE,
  INSTITUTIONAL_AUTHENTICATION_CLAIM_FEATURE,
  type AuthenticationClaimResponsePayload,
  type ClaimObservationPayload,
  type ClaimObservationAckPayload,
} from "./protocol";
import {
  isSurfaceBirthRecord,
  migrateTabLedger,
  originDigestOf,
  type SurfaceBirthRecord,
} from "./ledger";
import {
  emptyPageBulkScanStore,
  PAGE_BULK_SCAN_STORAGE_KEY,
  scanDocument,
  withPageBulkSnapshot,
  type DetectedPaper,
  type PageBulkScanStore,
  type PageBulkSnapshot,
  type ScanResult,
} from "./page-scan";
import {
  chunkKeysFor,
  pageBulkPayloadDigest,
  PageBulkCohortRecovery,
  type PageBulkRecoveryCohort,
  type PageBulkRecoverySource,
} from "./page-bulk-recovery";
import {
  chromeBackend,
  claimJobDownloadInitiated,
  clearPendingDelivery,
  emptyStore,
  findByJob,
  findByTab,
  jobDownloadFilename,
  PAGE_CAPTURE_CONSENT_KEY,
  patchJob,
  reduceMaterialization,
  removeJob,
  startPendingDelivery,
  updatePendingDelivery,
  upsertJob,
  type ActiveJob,
  type MaterializationCorrelation,
  type MaterializationEvent,
  type MaterializationPhase,
  type PageIdentity,
  type StateBackend,
  type StoreShape,
  TERMS_CONSENT_KEY,
  type TermsConsent,
  type TermsEffectCorrelation,
  type ProviderDrainLease,
  type ProviderDriveEpoch,
  WORK_WINDOW_KEY,
  HANDOFF_SURFACE_KEY,
  type HandoffSurface,
  MANAGED_TAB_LEDGER_KEY,
  TOOLBAR_COUNT_MODE_KEY,
  type ToolbarCountMode,
  isURLLike,
  ALL_SITES_ORIGIN,
  getInPageToastEnabled,
} from "./state";
import {
  doiFromURL,
  isPDFPage,
  pdfGrabRefusalText,
  pdfSourceURL,
  pageAcquireOrigin,
  carriesSignedCredential,
  sanitizePageHost,
  PDF_GRAB_FEATURE,
  PDF_GRAB_SUGGEST_FEATURE,
} from "./deliver";
import {
  adapters,
  type AdapterSpec,
  type PageVerdict,
  providerViewerPDFURL,
} from "./adapters/types";
import {
  planExecution,
  planGeneric,
  type GenericCandidate,
  type GenericPlan,
  type Plan,
  type PlanResult,
} from "./plan";
import { observeUnknown, type ObserveChromeApi } from "./observe";
import {
  capturePage,
  encodePageCapture,
  residualLeak,
  sanitizeFixture,
  type PageCapture,
  type Provider,
  type Scenario,
} from "./capture";
import {
  chromeKeepaliveAPI,
  initKeepalive,
  isAuthenticationURL,
} from "./keepalive";
import type {
  FreshSessionEvidence,
  KeepaliveManager,
  KeepaliveOriginSnapshot,
  KeepaliveSnapshot,
} from "./keepalive";
import { routeResolverService, type ResolverRoute } from "./resolver";
import { detectAuthFailure } from "./authfail";

export const NATIVE_HOST = "com.orgmentem.papio";
const CHROME_PDF_VIEWER_HOST = "mhjfbmdgcfjbbpaeojofohoefgiehjai";
/** Lowest native daemon that can service this extension. 0.18.0 added the
 * optional `request_id` echo on `page_capture`; this extension always sends it
 * on a requested capture, and a daemon that predates the field rejects the
 * whole frame — which is fatal to the entire native-messaging session, not
 * just that capture. The floor cannot prevent the frame (it drives the popup's
 * "daemon is out of date" line, not emission), but it names the skew instead
 * of leaving the operator with an unexplained disconnect. 0.9.0, the previous
 * floor, renamed the wire access mode to "delegated"; older daemons emit
 * "maximal", which this extension rejects fail-closed. */
export const MIN_DAEMON_VERSION = "0.18.0";

const AUTH_EVIDENCE_TTL_MS = 30 * 60_000;
const QUEUED_HANDOFF_RELEASE_MS = 45_000;
/** At most one papio-created handoff tab may drive an effect at once during
 * stabilization. Descriptor/offer counts remain independent of this cap. */
const HANDOFF_DRIVE_LIMIT = 1;
const HANDOFF_DRIVE_TIMEOUT_MS = 3 * 60_000;
// (custom elements, React roots) upgrade after the tab reports complete and
// after the SSO landing. Re-drive the idempotent classify path on a bounded
// schedule so a slow render still reaches a decisive verdict.
const CLASSIFY_RETRY_MS = 2_500;
const MAX_CLASSIFY_RETRIES = 8;
// A challenge holds only its provider's queue for the same one minute the old
// bounded challenge probe used, then a fresh drain can reclaim it.
const PROVIDER_DRAIN_LEASE_MS = 24 * CLASSIFY_RETRY_MS;
/** Security checks and redirect-loop dead ends cool a provider for ten minutes
 * so an automated re-offer cannot immediately trip the same hardening again. */
const CHALLENGE_COOLDOWN_MS = 10 * 60_000;
/** How many challenge-blocked tabs one keepalive wake may re-probe. A solved
 * check must retire its own ask without waiting for a tab event that may never
 * arrive, and each probe is a scripting call into a live tab — so the sweep is
 * bounded rather than proportional to the backlog. */
const CHALLENGE_RECHECK_LIMIT = 3;
/** How long a Cloudflare-style check must PERSIST before papio turns it into a
 * human ask. Cloudflare's managed challenge resolves itself in a few seconds
 * with no human action, and it publishes "Just a moment..." as a title-only
 * update mid-navigation - which papio read as a wall, parked the drive, and
 * asked the operator. Measured live on one paper: 25 blocks, 2 clears, every
 * block landing 0.4-1.0s after an institutional effect while `auth_returned`
 * came back inside a second. By the time the operator looked, the page had
 * long since resolved to the article with a live download button - the defect
 * that opened this work ("why is that paper not being grabbed, when the pdf
 * button is there, clickable?"). A redirect loop is exempt: it is a dead end,
 * not a stage, and waiting only delays a true refusal. */
const CHALLENGE_CONFIRM_MS = 8_000;
/** A title-only OpenAthens error update can precede its body render. Recheck
 * exactly once, late enough for the bounded DOM marker probe to see it. */
const OPENATHENS_ERROR_RECHECK_MS = 1_500;
const OPENATHENS_LOGIN_HOST = "login.openathens.net";
const OPENATHENS_ERROR_TITLE = "Error | OpenAthens";
const PROVIDER_MULTI_LABEL_SUFFIXES: Record<string, true> = {
  "ac.uk": true,
  "co.uk": true,
  "com.au": true,
  "com.br": true,
  "com.cn": true,
  "com.mx": true,
  "co.jp": true,
  "co.nz": true,
  "edu.au": true,
  "gov.au": true,
  "gov.uk": true,
  "govt.nz": true,
  "net.au": true,
  "ne.jp": true,
  "org.au": true,
  "org.nz": true,
  "or.jp": true,
};

export type ChallengeBlockKind = "cloudflare" | "redirect_loop";
type TermsAcceptResult = "accepted" | "not_dispatched" | "occupied";

/** Canonical provider key: registrable hostname only, never a path or IdP. */
export function registrableProviderHost(host: string): string | undefined {
  const labels = host.toLowerCase().split(".").filter(Boolean);
  if (labels.length < 2 || labels.some((label) => !/^[a-z0-9-]+$/.test(label)))
    return undefined;
  const suffix = labels.slice(-2).join(".");
  const count = PROVIDER_MULTI_LABEL_SUFFIXES[suffix] === true ? 3 : 2;
  return labels.slice(-count).join(".");
}

/** Stable, URL-free identity of the exact packaged terms authority. Length
 * prefixes make the tuple unambiguous without depending on JSON key ordering. */
export async function termsAuthorityDigest(
  spec: AdapterSpec,
): Promise<string | undefined> {
  const rule = spec.termsAccept;
  if (rule === undefined) return undefined;
  const values = [
    spec.id,
    spec.version,
    rule.modalSelector,
    rule.control ?? "",
    ...rule.textAny,
  ];
  const canonical = values
    .map((value) => `${new TextEncoder().encode(value).length}:${value}`)
    .join("|");
  const digest = await globalThis.crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(canonical),
  );
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

// A job whose warm SSO session cannot complete human authentication would
// otherwise be re-driven on every daemon re-offer and worker spin-up forever,
// thrashing the provider (repeat navigations trip bot walls) and burning the
// resolver. Cap authentication drives per browser session; past it the job is
// reported human_auth_required (kept parked daemon-side, non-terminal) and no
// longer opens broker tabs until a fresh launch clears the budget.
const MAX_AUTH_ATTEMPTS = 3;
// The alarm that wakes an idle MV3 worker to re-establish the daemon connection
// so queued offers arrive without a keepalive tab or user activity. One minute
// is Chrome's reliable floor for a packed extension; it bounds delivery latency.
const KEEPALIVE_ALARM = "papio-keepalive";
const KEEPALIVE_ALARM_MINUTES = 1;
/** Chrome can deliver the same keepalive wake twice when start() re-registers
 * the alarm; ignore a second handler inside one period minus slack. */
const KEEPALIVE_ALARM_DEDUPE_MS = KEEPALIVE_ALARM_MINUTES * 60_000 - 5_000;
/** Bound a foreground runtime request without retaining it past the worker's
 * lifetime. Native frames themselves are bounded by the protocol parser. */
const TRIAGE_REQUEST_TIMEOUT_MS = 15_000;
const HELLO_WAIT_TIMEOUT_MS = 5_000;
const TRIAGE_SNAPSHOT_FEATURE = "triage_snapshot_v1";
const TRIAGE_SNAPSHOT_SCHEMA_2_FEATURE = "triage_snapshot_schema_v2";
const TRIAGE_SNAPSHOT_SCHEMA_3_FEATURE = "triage_snapshot_schema_v3";
const TRIAGE_SNAPSHOT_SCHEMA_4_FEATURE = "triage_snapshot_schema_v4";
const TRIAGE_SNAPSHOT_SCHEMA_5_FEATURE = "triage_snapshot_schema_v5";
const TRIAGE_COUNTS_SCHEMA_2_FEATURE = "triage_counts_schema_v2";
const TRIAGE_COUNTS_SCHEMA_3_FEATURE = "triage_counts_schema_v3";
const TRIAGE_MUTATIONS_FEATURE = "triage_mutations_v1";
const SURFACE_PRESENCE_FEATURE = "surface_presence_v1";
const WORK_PULSE_FEATURE = "work_pulse_v1";
const SESSION_EVIDENCE_FEATURE = "session_evidence_v1";
const DELIVERY_CONTEXT_FEATURE = "delivery_context_v1";
const PROVIDER_DIRECT_GET_FEATURE = "provider_direct_get_v1";
const HANDOFF_LINK_FEATURE = "handoff_link_v1";
/** §1 of dev/active/claim-observation-protocol.md: Slice 3 daemon-side
 * authentication-claim arbitration and observation. Aliased, not
 * re-literaled, so this string can never drift from protocol.ts's own
 * validated constant. Until a daemon advertises it, the Slice 0 containment
 * gate below stays closed everywhere: an autonomous `requires_auth` surface
 * cannot be created, and institutional work parks tabless for explicit
 * engagement instead. This is also the permanent degraded-compatibility
 * behavior against an older daemon. */
const AUTHENTICATION_CLAIM_FEATURE = INSTITUTIONAL_AUTHENTICATION_CLAIM_FEATURE;
const PROVIDER_DRIVE_EPOCH_FEATURE = "provider_drive_epoch_v1";
const REVIEW_PREVIEW_FEATURE = "review_preview_v1";
const STATS_FEATURE = "browser_stats_v1";
const ACTIVITY_FEED_FEATURE = "activity_feed_v1";
const ACTIVITY_PAGE_FEATURE = "activity_page_v1";
const PAGE_CAPTURE_FEATURE = "page_capture_v1";
const PAGE_CAPTURE_REQUEST_FEATURE = "page_capture_request_v1";
const PAGE_CAPTURE_TERMS_FEATURE = "page_capture_terms_v1";
/** ADR-0019 Decision 7: page_bulk_status_request/page_bulk_submit_request. */
const PAGE_BULK_ACQUIRE_FEATURE = "page_bulk_acquire_v1";
const PAGE_BULK_COHORT_V2_FEATURE = "page_bulk_cohort_v2";
const INSTITUTIONAL_MATERIALIZATION_FEATURE =
  "institutional_materialization_v1";
const MATERIALIZATION_ID_PATTERN = /^[A-Za-z0-9_-]{8,128}$/u;
const MATERIALIZATION_RFC3339_PATTERN =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/u;
/** A bind refused because another institution sign-in owns the entry lease.
 * The daemon re-offers on its 2s native-host poll, so this browser alarm is
 * the pacing fence that stops each offer from rebuilding a real scaffold. */
const INSTITUTIONAL_RETRY_ALARM_PREFIX = "papio-institutional-bind-retry:";
const INSTITUTIONAL_RETRY_BASE_MS = 15_000;
const INSTITUTIONAL_RETRY_MAX_MS = 5 * 60_000;
const INSTITUTIONAL_RETRY_MAX_ATTEMPTS = 6;
/** A lost claim/bind response is retried as the exact idempotent operation.
 * The daemon owns the durable claim; this budget bounds browser-side wakes. */
const MATERIALIZATION_RETRY_BASE_MS = 1_000;
const MATERIALIZATION_RETRY_MAX_MS = 15_000;
const MATERIALIZATION_RETRY_COOLDOWN_MS = 60_000;
const MATERIALIZATION_MAX_RESPONSE_LOSS_RETRIES = 3;
const MATERIALIZATION_RESPONSE_TYPES: Record<string, true> = {
  institutional_claim_response: true,
  institutional_bind_response: true,
  institutional_route_response: true,
  institutional_navigated_response: true,
  institutional_reconcile_response: true,
};
const PDF_GRAB_CORRELATION_STORAGE_KEY = "papio_pdf_grab_correlations_v1";
const CLAIM_OBSERVATION_OUTBOX_STORAGE_KEY =
  "papio_claim_observation_outbox_v1";
const NAVIGATION_ERROR_MARKER_STORAGE_KEY = "papio_navigation_error_markers_v1";
/** Valid `event_kind` values (protocol.ts's `ClaimObservationPayload`),
 * duplicated here as a static lookup so hydrateClaimObservationOutbox can
 * validate a persisted entry without importing a type as a value. */
const CLAIM_OBSERVATION_EVENT_KINDS: Record<
  ClaimObservationPayload["event_kind"],
  true
> = {
  wall_observed: true,
  login_started: true,
  mfa: true,
  challenge: true,
  auth_returned: true,
  entitled_landing: true,
  owner_closed: true,
  navigation_error: true,
};
/** Defensive container check for the value returned by chrome.storage.session.
 * Entry validation stays per-record so one torn or foreign entry cannot erase
 * valid observations that were persisted beside it. */
function isClaimObservationOutboxRecord(
  value: unknown,
): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isClaimObservationOutboxEntry(
  observationID: string,
  value: unknown,
): value is ClaimObservationOutboxEntry {
  if (typeof value !== "object" || value === null || Array.isArray(value))
    return false;
  const entry = value as Partial<ClaimObservationOutboxEntry>;
  return (
    entry.observation_id === observationID &&
    typeof entry.job_id === "string" &&
    typeof entry.authentication_claim_id === "string" &&
    typeof entry.binding_id === "string" &&
    typeof entry.browser_holder_generation === "number" &&
    Number.isFinite(entry.browser_holder_generation) &&
    typeof entry.gate_occurrence_id === "string" &&
    typeof entry.event_ordinal === "number" &&
    Number.isFinite(entry.event_ordinal) &&
    typeof entry.event_kind === "string" &&
    CLAIM_OBSERVATION_EVENT_KINDS[
      entry.event_kind as ClaimObservationPayload["event_kind"]
    ] === true
  );
}

/** States this worker writes into a live correlation record. Anything else in
 * storage is stale or hostile and must be dropped — repairing it could steer
 * bytes into a grab this session no longer owns. */
const PDF_GRAB_CORRELATION_STATES = new Set<string>([
  "awaiting_viewer",
  "grabbed",
  "identifying",
]);
const PAGE_CAPTURE_DEFAULT_SETTLE_MS = 3_000;
const PAGE_CAPTURE_NAV_TIMEOUT_MS = 30_000;
const TRIAGE_COUNTS_FRESH_MS = 3 * KEEPALIVE_ALARM_MINUTES * 60_000;
const SESSION_EVIDENCE_THROTTLE_MS = 60_000;
/** How long a parked handoff surface the operator has never engaged may sit
 * before papio retires it. See surfaceIsCold for the measurement this comes
 * from; 3x the measured p99 operator-return latency. */
const PARKED_SURFACE_COLD_MS = 30 * 60_000;
/** How often the keepalive wake may re-run the owned-surface repair pass. It
 * only has to be short relative to the 3-minute drive timeout whose refused
 * closes it repairs, and to PARKED_SURFACE_COLD_MS; five minutes repairs a
 * stranded surface within one cold window instead of never, at one ledger walk
 * per five wakes. */
const OWNED_TAB_RECONCILE_INTERVAL_MS = 5 * 60_000;
/** Daemon replies that settle a correlated `requestNative` call. `requestNative`
 * rejects any type outside this set before registering a wait, so wrappers and
 * variables cannot create a request that only fails later by timing out. */
const CORRELATED_RESULT_TYPES: ReadonlySet<BrowserMessageType> = new Set([
  "triage_snapshot_response",
  "triage_counts_response",
  "triage_decide_result",
  "human_action_resolve_result",
  "review_preview_result",
  "stats_response",
  "activity_response",
  "activity_page_response",
  "pdf_grab_status_result",
  "pdf_grab_abandon_result",
  "delivery_reconcile_result",
  "pdf_grab_result",
  "surface_presence_ack",
  "work_pulse_response",
  "surface_close_response",
  // Registered 2026-08-12: the page-bulk bridge (ADR-0019 phase B) landed after
  // this guard and never added its own reply types, so every availability check
  // and v1 submit threw before sending a frame. page_bulk_runs recorded six
  // opens and zero submissions as a result.
  "page_bulk_status_result",
  "page_bulk_submit_result",
  "page_bulk_submit_v2_result",
  "handoff_link_result",
  "provider_drive_epoch_start_result",
  "provider_drive_epoch_result",
  "terms_effect_start_result",
  "terms_effect_result",
  "pdf_grab_suggest_response",
  "pdf_grab_confirm_response",
  "authentication_claim_response",
  "claim_observation_ack",
]);
// the pages under dist/ (see build.ts) and the manifest is the source of truth.
const POPUP_PAGE_PATH = "dist/popup.html";
/** Derived, never hardcoded: extension pages ship beside the declared popup
 * (`dist/` in every manifest — Chrome, generated Firefox, and dev-unpacked),
 * so a root-relative "materialize.html" resolves to nothing and every
 * automatically-owned institutional tab lands on ERR_FILE_NOT_FOUND. Same
 * rule as the authorized page URLs derived in realDeps(); see popup.ts's
 * historyPagePath(). */
const MATERIALIZE_PAGE_PATH = POPUP_PAGE_PATH.replace(
  /[^/]*$/,
  "materialize.html",
);
/** Same derivation rule, same reason: a bare "toast.html" resolves in no
 * deployment, and this surface's whole job is to appear when something already
 * went wrong — a broken page URL here would be silent. */
const TOAST_PAGE_PATH = POPUP_PAGE_PATH.replace(/[^/]*$/, "toast.html");
/** How long a papio surface's focus report suppresses the toast. */
const TOAST_PRESENCE_TTL_MS = 30_000;

/**
 * ADR-0023's seventh surface, delivered into the page the researcher is reading
 * instead of into a small papio window. Runs INSIDE that page via
 * scripting.executeScript, so — like `isBotChallenge` above — it must stay
 * fully self-contained: no outer-scope reference, module import, or shared
 * constant survives serialization, which is why every value it needs arrives in
 * the one `ToastInjection` argument.
 *
 * Three differences from the sixth surface's chip (`popup.ts`
 * renderInPageAcknowledgement), all of them deliberate:
 *
 * - It is INTERACTIVE. The chip is `pointer-events: none` because it is a
 *   receipt; this one carries the single take-back-control action, so it takes
 *   pointer and keyboard input and is an `alertdialog` like the toast page.
 * - It reads NOTHING from the page. It appends one host element and removes it.
 *   No selector runs, no text is collected, and nothing is returned except
 *   whether the host was appended.
 * - It removes itself from the DOM before it reports, so a capture taken after
 *   an action can never contain it. The host id is also what the capture path
 *   strips, so a toast that is still live is excluded from fixture bytes.
 */
export function renderPageToast(injection: ToastInjection): boolean {
  const HOST_ID = "papio-extension-loss-toast-v1";
  const prior = document.getElementById(HOST_ID);
  if (prior !== null) prior.remove();
  const host = document.createElement("div");
  host.id = HOST_ID;
  host.style.cssText = [
    "position:fixed",
    "right:16px",
    "bottom:16px",
    "z-index:2147483647",
    "margin:0",
    "padding:0",
    "border:0",
  ].join(";");
  // Open, not closed: same reason as the chip — isolation is what the shadow
  // root is for, and an open root stays inspectable without weakening it. It
  // also keeps the copy out of `document.documentElement.outerHTML`, which
  // omits shadow roots, so a capture that races the strip still carries an
  // empty host rather than papio's sentence.
  const root = host.attachShadow({ mode: "open" });
  const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  // The card's own palette, plus papio's brand pair for the mark. The brand
  // colours are the same literals every papio page sets as `--color-brand-*`;
  // they cannot be read as custom properties here, because the document these
  // styles land in is the publisher's, not papio's.
  const [ink, border, surface, accentInk, accent, brandInk, brandAccent] = dark
    ? ["#e9ecf1", "#3a4049", "#1c1f26", "#10131a", "#6f9cf0", "#f0edf3", "#ef6a57"]
    : ["#16181d", "#d3d7de", "#ffffff", "#ffffff", "#1c5fd6", "#2b2d42", "#d94f3d"];
  const card = document.createElement("div");
  card.setAttribute("role", "alertdialog");
  card.setAttribute("aria-describedby", "papio-toast-message");
  card.style.cssText = [
    "align-items:center",
    `background:${surface}`,
    `border:1px solid ${border}`,
    "border-radius:10px",
    // `toast.html`'s body is border-box, so the shared width means the box
    // INCLUDING padding and border there. Without this the same constant sizes
    // the content box here, and the injected card renders 30px wider than the
    // window it is supposed to match — measured at 606 against 576.
    "box-sizing:border-box",
    "box-shadow:0 10px 30px rgb(16 22 33 / 22%)",
    `color:${ink}`,
    "display:flex",
    "font:14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif",
    "gap:12px",
    `max-width:min(${injection.max_width_px}px, calc(100vw - 32px))`,
    "padding:12px 14px",
  ].join(";");
  // The same loop `renderPapioMark` runs, over the same geometry, inline because
  // a function reference cannot cross this serialization boundary. Decorative:
  // `aria-hidden`, no title, no label — the sentence beside it names papio.
  const SVG_NS = "http://www.w3.org/2000/svg";
  const mark = document.createElementNS(SVG_NS, "svg");
  mark.setAttribute("viewBox", injection.mark_viewbox);
  mark.setAttribute("aria-hidden", "true");
  mark.style.cssText = `flex:none;width:${injection.mark_size_px}px;height:${injection.mark_size_px}px`;
  for (const shape of injection.mark) {
    const el = document.createElementNS(SVG_NS, shape.tag);
    for (const [name, value] of Object.entries(shape.attrs)) el.setAttribute(name, value);
    const colour = shape.role === "ink" ? brandInk : brandAccent;
    el.setAttribute("stroke", colour);
    el.setAttribute("fill", shape.filled === true ? colour : "none");
    mark.append(el);
  }
  const message = document.createElement("p");
  message.id = "papio-toast-message";
  message.textContent = injection.message;
  message.style.cssText = "margin:0;flex:1 1 auto";
  const action = document.createElement("button");
  action.type = "button";
  action.textContent = injection.action;
  action.style.cssText = [
    "flex:none",
    `background:${accent}`,
    `color:${accentInk}`,
    "border:1px solid transparent",
    "border-radius:7px",
    "cursor:pointer",
    "font:inherit",
    "padding:6px 12px",
  ].join(";");
  const dismiss = document.createElement("button");
  dismiss.type = "button";
  dismiss.textContent = "Dismiss";
  dismiss.setAttribute("aria-label", "Dismiss this message");
  dismiss.style.cssText = [
    "flex:none",
    "background:transparent",
    `color:${ink}`,
    `border:1px solid ${border}`,
    "border-radius:7px",
    "cursor:pointer",
    "font:inherit",
    "padding:6px 10px",
  ].join(";");
  // One settlement only. Both buttons and the expiry race each other, and a
  // second report would either reopen a paper twice or dismiss an offer the
  // researcher just accepted. `timer` is declared before `settle` reads it:
  // the handlers only fire after the assignment below, but a const declared
  // after its closure is a compile error, not a runtime one.
  let settled = false;
  let timer: number | undefined;
  const settle = (type: string, reason?: string): void => {
    if (settled) return;
    settled = true;
    window.clearTimeout(timer);
    // Remove BEFORE reporting: the action opens a tab, and this page may be
    // captured or classified at any moment after that.
    host.remove();
    const payload: Record<string, unknown> = {
      type,
      job_id: injection.job_id,
      token: injection.token,
    };
    if (reason !== undefined) payload["reason"] = reason;
    void chrome.runtime.sendMessage(payload).catch(() => undefined);
  };
  action.addEventListener("click", () => settle(injection.action_message));
  dismiss.addEventListener("click", () =>
    settle(injection.dismiss_message, "dismissed"),
  );
  // Expiry commits nothing, exactly as the window route's does: the recovery
  // stays in the inbox, so the eight seconds are a shortcut, not a deadline.
  timer = window.setTimeout(
    () => settle(injection.dismiss_message, "expired"),
    injection.window_ms,
  );
  card.append(mark, message, action, dismiss);
  root.append(card);
  document.documentElement.append(host);
  return true;
}

/** The handoff group's title. Constant: it is an ownership marker, not a
 * status line. It briefly carried the surfaced paper's own title, which read
 * as information and was not — with more than one tab in the group the label
 * names an arbitrary one of them, and the tab strip already shows every title.
 *
 * `isHandoffGroupTitle` still accepts the old `papio — <paper>` form so a
 * reloaded worker rediscovers a group that an earlier build had renamed,
 * instead of orphaning it and opening a second papio group beside it. */
const HANDOFF_GROUP_TITLE = "papio";
function isHandoffGroupTitle(title: string | undefined): boolean {
  return (
    title === HANDOFF_GROUP_TITLE ||
    title?.startsWith(`${HANDOFF_GROUP_TITLE} — `) === true
  );
}
export type { ToolbarCountMode };

export interface BadgeState {
  connectionStatus: StoreShape["connectionStatus"] | undefined;
  reauthNeeded: boolean;
  /** Papers whose own surface is at a login page: exactly the set a human can
   * act on. */
  authBlockers: number;
  /** Papers with no surface at all, waiting their turn at an institution one
   * sign-in serves. papio owes them nothing from the operator, so they never
   * escalate the badge - they only qualify a tooltip, because reporting them
   * as "waiting on your sign-in" turned an internal queue into a false human
   * ask (the badge read 13 while exactly one paper could proceed). */
  queuedAuth?: number | undefined;
  /** Number of active jobs left on a provider security check/dead-end page. */
  challengeBlocked?: number | undefined;
  blockedHosts: number | readonly string[];
  ungrantedResolvers: number;
  /** Legacy pending inventory, used only when v3 is unavailable or mode=all. */
  triageCount: number | undefined;
  /** Exact daemon-owned actionable turns from counts schema v3. */
  requiredTurnCount?: number | undefined;
  /** True only when the daemon supplied a complete required-turn projection. */
  requiredTurnsComplete?: boolean | undefined;
  /** Counts schema v3 was negotiated for this reading. */
  countsSchemaV3?: boolean | undefined;
  watchHits?: number | undefined;
  retractions?: number | undefined;
  toolbarCountMode?: ToolbarCountMode | undefined;
}

export interface BadgeResult {
  text: string;
  color: string;
  tooltip: string;
}

/** Compute the toolbar badge. Browser-local blockers always outrank counts.
 * Schema v3 turns are used only when negotiated and complete; old daemons
 * honestly fall back to pending inventory. */
export function computeBadge(state: BadgeState): BadgeResult {
  const blockedHostCount =
    typeof state.blockedHosts === "number"
      ? Math.max(0, Math.trunc(state.blockedHosts))
      : state.blockedHosts.length;
  const challengeBlockedCount =
    typeof state.challengeBlocked === "number"
      ? Math.max(0, Math.trunc(state.challengeBlocked))
      : 0;
  const authBlockerCount = Math.max(0, Math.trunc(state.authBlockers));
  const resolverCount = Math.max(0, Math.trunc(state.ungrantedResolvers));
  const pendingCount =
    typeof state.triageCount === "number" && Number.isFinite(state.triageCount)
      ? Math.max(0, Math.trunc(state.triageCount))
      : undefined;
  const requiredCount =
    typeof state.requiredTurnCount === "number" &&
    Number.isFinite(state.requiredTurnCount)
      ? Math.max(0, Math.trunc(state.requiredTurnCount))
      : undefined;
  const mode = state.toolbarCountMode ?? "required";
  const v3Complete =
    state.countsSchemaV3 === true &&
    state.requiredTurnsComplete === true &&
    requiredCount !== undefined;
  const watchHits = Math.max(0, Math.trunc(state.watchHits ?? 0));
  const retractions = Math.max(0, Math.trunc(state.retractions ?? 0));
  const queuedAuthCount = Math.max(0, Math.trunc(state.queuedAuth ?? 0));
  // Named as papio's own work, never as an ask: these papers are waiting for
  // papio to reach them, which is why they qualify every sign-in tooltip
  // instead of being counted into one.
  const queuedClause =
    queuedAuthCount > 0
      ? ` · ${queuedAuthCount} more queued for your library`
      : "";
  const breakdown = (need: number): string =>
    `papio: ${need} need you · ${watchHits} watch hit${watchHits === 1 ? "" : "s"} · ${retractions} retraction notice${retractions === 1 ? "" : "s"}${queuedClause}`;
  if (state.connectionStatus === "session_elsewhere") {
    // Reachable daemon, wrong browser: the remedy is switching the session,
    // not diagnosing the daemon.
    return {
      text: "!",
      color: "#777777",
      tooltip: "papio: another browser holds the papio session",
    };
  }
  if (state.connectionStatus !== "connected") {
    return {
      text: "!",
      color: "#777777",
      tooltip: "papio: daemon disconnected",
    };
  }
  if (state.reauthNeeded) {
    return {
      text: "!",
      color: "#b06000",
      tooltip: `papio: institution sign-in needed${queuedClause}`,
    };
  }
  if (authBlockerCount > 0) {
    return {
      text: String(authBlockerCount),
      color: "#b06000",
      tooltip: `papio: ${authBlockerCount} paper${authBlockerCount === 1 ? "" : "s"} need${authBlockerCount === 1 ? "s" : ""} your institution sign-in${queuedClause}`,
    };
  }
  if (challengeBlockedCount > 0) {
    return {
      text: String(challengeBlockedCount),
      color: "#b06000",
      tooltip: `papio: ${challengeBlockedCount} security check${challengeBlockedCount === 1 ? "" : "s"} need your attention`,
    };
  }
  if (blockedHostCount > 0) {
    const host =
      typeof state.blockedHosts === "number"
        ? undefined
        : state.blockedHosts[0];
    const tooltip =
      blockedHostCount === 1 && typeof host === "string"
        ? `papio: ${host} needs browser access`
        : `papio: ${blockedHostCount} provider hosts need browser access`;
    return { text: String(blockedHostCount), color: "#b06000", tooltip };
  }
  if (resolverCount > 0) {
    return {
      text: String(resolverCount),
      color: "#1a73e8",
      tooltip: `papio: ${resolverCount} library resolver permission${resolverCount === 1 ? "" : "s"} need attention`,
    };
  }
  if (mode === "off")
    return { text: "", color: "#1a73e8", tooltip: "papio: connected" };
  if (mode === "all") {
    if (pendingCount !== undefined && pendingCount > 0) {
      return {
        text: String(pendingCount),
        color: "#1a73e8",
        tooltip: `papio: ${pendingCount} pending item${pendingCount === 1 ? "" : "s"}${queuedClause}`,
      };
    }
    return {
      text: "",
      color: "#1a73e8",
      tooltip:
        (pendingCount === 0
          ? "papio: no pending items"
          : "papio: pending items unavailable") + queuedClause,
    };
  }
  if (state.countsSchemaV3 !== true) {
    if (pendingCount !== undefined && pendingCount > 0) {
      return {
        text: String(pendingCount),
        color: "#1a73e8",
        tooltip: `papio: ${pendingCount} pending item${pendingCount === 1 ? "" : "s"}${queuedClause}`,
      };
    }
    // The disabled-keepalive path: no evidence, no required-turn projection.
    // The queued clause is the whole signal that papio has institutional work
    // in flight, so it must survive here - this is the case the old
    // count-them-as-blockers behaviour was protecting, minus the false ask.
    return {
      text: "",
      color: "#1a73e8",
      tooltip: `papio: connected${queuedClause}`,
    };
  }
  if (!v3Complete) {
    return {
      text: "",
      color: "#1a73e8",
      tooltip: "papio: Many decisions waiting — open inbox",
    };
  }
  if (requiredCount > 0) {
    return {
      text: String(requiredCount),
      color: "#1a73e8",
      tooltip: breakdown(requiredCount),
    };
  }
  return { text: "", color: "#1a73e8", tooltip: breakdown(0) };
}
export interface SessionAuthDemand {
  job_id: string;
  origin: string;
}
export type BridgeSessionState = KeepaliveSnapshot & {
  releasedAuthJobs: number;
  /** Epoch ms of the most recent release; keys once-per-event popup notices. */
  releasedAuthJobsAt: number | null;
  /** Browser-local demand binding; never sent over native messaging. */
  authDemand?: SessionAuthDemand[];
  /** True when this worker computed the complete browser-local demand set. */
  authDemandComplete: true;
};

/** Whether this adapter's SPA must render outside the minimized work window. */
export function needsVisibleWindow(spec: AdapterSpec | undefined): boolean {
  return spec?.requiresVisible === true;
}

export type DrivenPageAssessmentKind = "normal" | "challenge" | "redirect_loop";
export interface DrivenPageAssessment {
  kind: DrivenPageAssessmentKind;
}

/**
 * Cloudflare/Turnstile marker probe. Keep this function self-contained:
 * chrome.scripting serializes it into the provider page's isolated world.
 * Only bounded structural markers and page-authored text are inspected; no
 * page text is returned to the extension.
 *
 * `/cdn-cgi/challenge-platform/` is deliberately NOT a marker. That path serves
 * Cloudflare's JS Detections script, which is injected into ORDINARY responses
 * on JSD-enabled sites, so keying on it classified every page on such a
 * provider as a bot challenge - permanently, since the marker never goes away.
 * Measured on the operator's own browser 2026-08-21, on a fully loaded SAGE
 * article they were reading: that script present, `#challenge-form` absent,
 * turnstile absent, challenge text absent, real article title. The committed
 * fixtures do not show this (their captures predate JSD on those hosts), which
 * is why this was settled against a live tab rather than against a fixture.
 *
 * What distinguishes the interstitial is its own title and text plus a real
 * widget. Verified against the live 403 interstitial the same day: "Just a
 * moment…" in the title, and NO widget markers in the served HTML at all -
 * Cloudflare injects those at runtime.
 */
export function isBotChallenge(doc: Document | null): boolean {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  const structural =
    root.querySelector(
      'script[src*="challenges.cloudflare.com/turnstile/"], ' +
        'input[name="cf-turnstile-response"], ' +
        '#challenge-form, #challenge-running, .cf-turnstile, ' +
        '[id*="cf-chl-"], [class*="cf-chl-"], ' +
        '#captcha-box .main-wrapper[role="main"]',
    ) !== null;
  return (
    structural ||
    /^just a moment/i.test(title) ||
    /are\s+you\s+a\s+robot\??/i.test(title) ||
    /verif(?:y|ying)\s+you\s+are\s+human|checking\s+your\s+browser|needs\s+to\s+review\s+the\s+security\s+of\s+your\s+connection/i.test(
      `${title}\n${text}`,
    )
  );
}

/** OpenAthens and browser redirect-loop dead ends remain human-visible.
 * `openAthensHost` is supplied only after the tracked tab's origin is verified;
 * keeping it explicit prevents an OpenAthens-looking provider page from
 * triggering the origin-specific code/phrase markers. */
export function isRedirectLoopPage(
  doc: Document | null,
  openAthensHost = false,
): boolean {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  const genericLoop =
    /\btoo\s+many\s+redirects\b|\berr_too_many_redirects\b|\bredirect\s+loop\b/i.test(
      `${title}\n${text}`,
    );
  const openAthensLoop =
    openAthensHost &&
    title === "Error | OpenAthens" &&
    /\btoo\s+many\s+redirects\b|\bservice\s+provider\s+redirecting\b|\b(?:GA|OA)-AP-\d{4}-\d{2}\b/i.test(
      text,
    );
  return (!openAthensHost && genericLoop) || openAthensLoop;
}

/**
 * Single bounded assessment injected before adapter interpretation. It keeps
 * challenge/error pages from being mistaken for articles or login forms.
 * Do not reference outer functions: this body is serialized by Chrome.
 */
export function assessDrivenPage(
  doc: Document | null,
  openAthensHost = false,
): DrivenPageAssessment {
  const root: Document = doc ?? document;
  const title = (root.title ?? "").trim().slice(0, 256);
  const text = (root.body?.textContent ?? "").slice(0, 40_000);
  // Same marker set as isBotChallenge, and same deliberate omission of
  // `/cdn-cgi/challenge-platform/` — see that function for the live
  // measurement. These two lists MUST agree: one decides whether to raise the
  // block, the other whether it may be retired, so a marker in only one of
  // them creates an ask that can be raised and never cleared. That is exactly
  // the shape of the defect this fixes.
  const structural =
    root.querySelector(
      'script[src*="challenges.cloudflare.com/turnstile/"], ' +
        'input[name="cf-turnstile-response"], ' +
        '#challenge-form, #challenge-running, .cf-turnstile, ' +
        '[id*="cf-chl-"], [class*="cf-chl-"], ' +
        '#captcha-box .main-wrapper[role="main"]',
    ) !== null;
  if (
    structural ||
    /^just a moment/i.test(title) ||
    /are\s+you\s+a\s+robot\??/i.test(title) ||
    /verif(?:y|ying)\s+you\s+are\s+human|checking\s+your\s+browser|needs\s+to\s+review\s+the\s+security\s+of\s+your\s+connection/i.test(
      `${title}\n${text}`,
    )
  ) {
    return { kind: "challenge" };
  }
  const genericLoop =
    /\btoo\s+many\s+redirects\b|\berr_too_many_redirects\b|\bredirect\s+loop\b/i.test(
      `${title}\n${text}`,
    );
  const openAthensLoop =
    openAthensHost &&
    title === "Error | OpenAthens" &&
    /\btoo\s+many\s+redirects\b|\bservice\s+provider\s+redirecting\b|\b(?:GA|OA)-AP-\d{4}-\d{2}\b/i.test(
      text,
    );
  if ((!openAthensHost && genericLoop) || openAthensLoop)
    return { kind: "redirect_loop" };
  return { kind: "normal" };
}

export interface Listenable<A extends unknown[]> {
  addListener(cb: (...args: A) => void): void;
}

export interface NativePort {
  postMessage(msg: object): void;
  onMessage: Listenable<[unknown]>;
  onDisconnect: Listenable<[]>;
  disconnect(): void;
}

export interface TabInfo {
  id?: number | undefined;
  url?: string | undefined;
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
}
/** Normalize only the fragment component for managed-tab dedupe. Chrome may
 * canonicalize a URL while creating a tab, so use URL.href when possible and
 * retain a conservative string fallback for malformed values. */
export function normalizeManagedTabURL(rawURL: string): string {
  try {
    const url = new URL(rawURL);
    url.hash = "";
    return url.href;
  } catch {
    const fragment = rawURL.indexOf("#");
    return fragment < 0 ? rawURL : rawURL.slice(0, fragment);
  }
}

/** Return the live tab that should be reused for a managed open. A tracked job
 * wins even when its current document has navigated away; otherwise exact URL
 * equality (ignoring only fragments) prevents duplicate browser tabs. */
export function findManagedTab(
  candidates: readonly TabInfo[],
  url: string,
  trackedTabID?: number,
): TabInfo | undefined {
  if (trackedTabID !== undefined) {
    const tracked = candidates.find(
      (candidate) => candidate.id === trackedTabID,
    );
    if (tracked !== undefined) return tracked;
  }
  const normalized = normalizeManagedTabURL(url);
  return candidates.find(
    (candidate) =>
      candidate.id !== undefined &&
      candidate.url !== undefined &&
      normalizeManagedTabURL(candidate.url) === normalized,
  );
}

export type ManagedTabPurpose =
  | "handoff"
  | "inbox-open"
  | "session-signin"
  | "redrive"
  | "reoffer"
  | "capture"
  | "claim-scaffold";
/** Non-URL free-text marker for a one-use federated-login mint's birth
 * record (Slice 2b): excluded from cross-job reuse pools the same way the
 * legacy raw-URL ledger's `privateURL` flag was. */
const PRIVATE_SURFACE_PURPOSE = "federated-login";
/** Bound on how many same-epoch ledger records classifyRestart() probes
 * with tabs.get while re-proving an update-class restart. */
const RESTART_LIVENESS_SCAN_LIMIT = 25;
const BROWSER_EPOCH_LOCAL_KEY = "papio_browser_epoch_v1";
const BROWSER_EPOCH_SESSION_KEY = "papio_browser_epoch_session_v1";
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
  /** The operator activated a tab papio did not focus itself. */
  | "operator_activated"
  /** A close attempt found the tab pinned: an operator act on this tab. */
  | "pinned_at_close"
  /** The reconcile pass found the tab pinned, or outside papio's container. */
  | "pinned_or_moved_out"
  /** A touch landed between the close decision and the removal. */
  | "touched_mid_close"
  /** Scaffold rediscovery found an operator-active or pinned duplicate. */
  | "duplicate_operator_owned";
export interface OpenManagedTabOptions {
  url: string;
  jobId?: string;
  purpose: ManagedTabPurpose;
  /** Legacy in-window visibility for a new handoff; work-window placement
   * still follows its adapter-driven visibility rules. */
  surfaceFallback?: boolean;
  /** Set false when the caller just surfaced the tab (e.g. stale-page
   * recovery) and only needs managed URL reuse/navigation. */
  focusExisting?: boolean;
  /** Synchronous claim binding immediately after Chrome returns a new tab. */
  onTabMaterialized?: (tabID: number) => void;
  /** Persist only a private ownership marker, never the one-use URL. */
  privateLedgerURL?: boolean;
  /** Explicit birth-certificate binding for a self-identifying scaffold
   * (Slice 4): the ledger record must carry the SAME id baked into the
   * scaffold URL's fragment, never an independently minted one. Ignored on
   * a reuse hit — a reused tab's ledger binding is already fixed. */
  bindingID?: string;
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

export interface DownloadItemLike {
  id: number;
  state?: string | undefined;
  filename?: string | undefined;
  fileSize?: number | undefined;
  totalBytes?: number | undefined;
  bytesReceived?: number | undefined;
  referrer?: string | undefined;
  finalUrl?: string | undefined;
  url?: string | undefined;
  mime?: string | undefined;
  /** Present in the test fake and some Chromium builds; absent in stable
   * chrome.downloads.DownloadItem, in which case we fall back to referrer. */
  tabId?: number | undefined;
}
export interface DownloadDeltaLike {
  id: number;
  state?: { current?: string | undefined } | undefined;
  filename?: { current?: string | undefined } | undefined;
  error?: { current?: string | undefined } | undefined;
}

function isCleanNonBrowserMime(mime: string | undefined): boolean {
  if (mime === undefined || mime === "" || mime === "application/pdf")
    return false;
  return (
    /^(?:image|audio|video)\//u.test(mime) ||
    mime === "application/octet-stream" ||
    mime === "application/zip" ||
    mime === "application/x-7z-compressed" ||
    mime === "application/gzip"
  );
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
function isTabAbsenceRejection(reason: unknown): boolean {
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
export interface PdfGrabCorrelation {
  scanID: string;
  tabID: number;
  state: string;
  /** Absent while the grab waits for the researcher's own viewer download:
   * papio started no download of its own, so there is no id yet. */
  downloadID?: number;
  /** The download route — origin and pathname only — the grab was armed for.
   *
   * The awaiting-viewer state has to re-register its steering after MV3 kills
   * the worker, or the researcher's Download click a minute later correlates to
   * nothing. It must not persist the URL to do that: a provider delivery URL
   * carries a bearer-grade signing token in its query, and this record is
   * written to extension storage. A route is what matching actually compares
   * (`sameDownloadRoute` ignores query and fragment by design), so it is
   * exactly as strong a key and carries no credential. */
  route?: string;
  steeringPath: string;
  /** The request id the daemon stored as this grab's `effect_request_id`. The
   * cancellation fence compares against it, so a later worker generation of
   * this same browser needs it to clean up its own interrupted grab. */
  effectRequestID?: string;
  abandonPending?: boolean;
}
/** One durably-queued `claim_observation` frame (Slice 3), keyed by
 * `observation_id` in both the worker-memory map and the persisted outbox.
 * Mirrors `ClaimObservationPayload` minus `request_id` — `requestNative`
 * mints a fresh correlation id per send attempt, but `observation_id` is the
 * daemon's own idempotency key and must survive every retry unchanged.
 * `job_id` is carried separately from the wire payload (claim_observation is
 * JOB_SCOPED: the daemon protocol puts it on the message envelope, not
 * inside `payload` — see `drainObservationOutbox`) but must still be
 * persisted here so a replay after a restart still knows which job's
 * envelope to stamp it onto. */
export interface ClaimObservationOutboxEntry {
  observation_id: string;
  job_id: string;
  authentication_claim_id: string;
  binding_id: string;
  browser_holder_generation: number;
  gate_occurrence_id: string;
  event_ordinal: number;
  event_kind: ClaimObservationPayload["event_kind"];
}

/** Durable evidence of a top-frame webNavigation.onErrorOccurred, keyed by
 * tab id (chrome.storage.session, oracle finding 5): the worker-local
 * `navigationErrors` Map is the only OTHER record of this, so a worker
 * teardown between the error and its document-settle consumption would
 * otherwise lose it completely — the generic auth-wall detector could then
 * mistake the dead end for a human sign-in wall, or the daemon could never
 * learn the route died. `authentication_claim_id`/`gate_occurrence_id`/
 * `browser_holder_generation` are populated only when a claim grant already
 * existed at error time; when absent, bootstrap reconciliation can restore
 * the local exclusion marker but not synthesize a durable claim_observation
 * out of nothing. */
export interface NavigationErrorMarkerEntry {
  tab_id: number;
  binding_id: string;
  at: number;
  job_id?: string;
  authentication_claim_id?: string;
  gate_occurrence_id?: string;
  browser_holder_generation?: number;
}

/** Structured outcome of consultAuthenticationClaim (§2.1.1). The four
 * operational outcomes narrow to what each one actually needs; `refuse`
 * covers feature_disabled/not_eligible/busy/error and every transport/parse
 * failure alike — the caller's response is identical either way (stay
 * tabless, engagement_required). */
type ClaimConsultResult =
  | { kind: "open_new" }
  | { kind: "navigate_existing"; ownerBindingID: string; ownerTabHint?: number }
  | { kind: "focus_owner"; ownerBindingID: string; ownerTabHint?: number }
  | { kind: "park"; dependentCount: number }
  | { kind: "refuse" };

export interface BridgeDeps {
  connectNative(name: string): NativePort;
  manifestVersion: string;
  randomUUID(): string;
  now(): number;
  /** Runtime URL seam for the URL-free materialization scaffold. */
  runtimeGetURL?: (path: string) => string;
  /** Injectable timers so tests control reconnect backoff and queue release. */
  /** Dedicated local cohort recovery seam; never uses the managed state backend. */
  pageBulkRecovery?: PageBulkCohortRecovery;
  setTimeout(fn: () => void | Promise<void>, ms: number): void;
  backend: StateBackend;
  tabs: {
    create(props: {
      url: string;
      active: boolean;
      windowId?: number;
    }): Promise<TabInfo>;
    remove(tabID: number): Promise<void>;
    get(tabID: number): Promise<TabInfo>;
    reload(tabID: number): Promise<unknown>;
    /** Optional: surface a work-window tab on human auth ({active}), or
     * navigate the handoff tab to a federated-login route ({url}). */
    update?(
      tabID: number,
      props: { active?: boolean; url?: string },
    ): Promise<unknown>;
    onUpdated: Listenable<[number, TabChangeInfo, TabInfo]>;
    /** Used only for the singleton inbox tab. */
    sendMessage?(tabID: number, message: object): Promise<unknown>;
    /** `active`+`lastFocusedWindow` finds the page the researcher is actually
     * looking at, which is the only tab the injected toast route may target. */
    query?(query: {
      url?: string;
      groupId?: number;
      active?: boolean;
      lastFocusedWindow?: boolean;
    }): Promise<TabInfo[]>;
    onRemoved: Listenable<[number, { isWindowClosing: boolean }]>;
    /** ADR-0013 privileges the focused tab: an activation with no matching
     * navigation event is still evidence the operator is looking at that
     * origin's resolver page. */
    onActivated: Listenable<[{ tabId: number; windowId: number }]>;
    /** `createProperties.windowId` pins a NEW group to the tab's own window.
     * Without it Chrome picks the window, and a tab whose window is not
     * `normal` (a devtools or app window can be the focused one) is refused
     * with "Tabs can only be moved to and from normal windows" - measured
     * live 2026-08-23, 36 times in one service-worker session. */
    group?(opts: {
      tabIds: number[];
      groupId?: number;
      createProperties?: { windowId?: number };
    }): Promise<number>;
  };
  webNavigation?: {
    onCommitted?: Listenable<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>;
    onHistoryStateUpdated: Listenable<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>;
    onReferenceFragmentUpdated: Listenable<[{ tabId: number; frameId: number; url?: string; documentId?: string }]>;
    /** chrome.webNavigation.onTabReplaced delivers `{tabId, replacedTabId}`
     * (tabId is the new tab). `{addedTabId, removedTabId}` belongs to the
     * *separate* tabs.onReplaced API; reading those names off this event
     * yields undefined and silently disarms every revocation below. */
    onTabReplaced: Listenable<[{ tabId: number; replacedTabId: number }]>;
    /** Live top-frame document epoch, read from the browser instead of from
     * `pageNavSeq`/an in-memory map: MV3 kills the worker at will, and an
     * emptied map must never read as "the page has not changed". */
    getFrame?(details: {
      tabId: number;
      frameId: number;
    }): Promise<{ documentId?: string } | null | undefined>;
    /** chrome.webNavigation.onErrorOccurred — a top-frame navigation that
     * failed to commit (net error, aborted, blocked). No URL is read from
     * this event: it exists only to order navigation-error evidence before
     * generic auth-wall detection (surface-lifecycle-plan.md Slice 1). */
    onErrorOccurred?: Listenable<
      [{ tabId: number; frameId: number; error?: string }]
    >;
  };
  /** Extension-page broadcast channel (runtime.onMessage), distinct from tabs.sendMessage content-script delivery. */
  runtimeSendMessage?(message: object): Promise<unknown>;
  /** chrome.windows seam. When present (and the user setting allows), broker
   * tabs use one dedicated minimized "work window" instead of the user's tab
   * strip, except an adapter whose SPA needs a visible window. A tab otherwise
   * platforms without the API — tabs then open with the legacy visibility rules. */
  windows?: {
    create(props: {
      url: string;
      focused: boolean;
      state?: "minimized" | "normal";
      /** ADR-0023's seventh surface needs a small chrome-less window rather
       * than a browser window. Optional so the work-window caller, which wants
       * a real window, is unchanged. */
      type?: "popup";
      width?: number;
      height?: number;
      top?: number;
      left?: number;
    }): Promise<WindowInfo>;
    get(windowID: number): Promise<WindowInfo>;
    update(
      windowID: number,
      props: {
        focused?: boolean;
        state?: "normal" | "minimized";
        drawAttention?: boolean;
      },
    ): Promise<unknown>;
    /** Closes a window papio itself opened. Only the toast uses it: the work
     * window is the researcher's to close. */
    remove(windowID: number): Promise<unknown>;
  };
  tabGroups?: {
    get(groupID: number): Promise<TabGroupInfo>;
    update(
      groupID: number,
      props: { collapsed?: boolean; title?: string; color?: string },
    ): Promise<unknown>;
    /** Find groups by title. Used to rediscover papio's orphaned handoff group
     * after an extension reload clears the in-memory id but leaves the group. */
    query(props: { title?: string }): Promise<TabGroupInfo[]>;
  };
  downloads: {
    search(query: {
      id?: number;
      filename?: string;
      limit?: number;
    }): Promise<DownloadItemLike[]>;
    /** Start a browser-managed download. The resolver-provided offer URL stays
     * local to the extension/browser and is never put in a native frame. */
    download(options: {
      url: string;
      filename?: string;
      conflictAction: "uniquify";
      saveAs: false;
    }): Promise<number>;
    removeFile(downloadID: number): Promise<void>;
    erase(query: { id: number }): Promise<number[]>;
    onCreated: Listenable<[DownloadItemLike]>;
    onChanged: Listenable<[DownloadDeltaLike]>;
    /** chrome.downloads.onDeterminingFilename — Chrome-only; absent elsewhere.
     * The listener may call suggest() synchronously to relocate a download to
     * a relative path under the browser's Downloads directory. */
    onDeterminingFilename?: Listenable<
      [
        DownloadItemLike,
        (s: { filename: string; conflictAction: "uniquify" }) => void,
      ]
    >;
  };
  /** Registered declarative provider adapters. Injected so hello's
   * adapter_versions map and the classifier are unit-testable. */
  adapterSpecs: AdapterSpec[];
  /** Inject only serializable DOM probes into tracked, granted provider tabs so
   * page inspection cannot escape the host-permission boundary. */
  scripting: {
    executeScript(injection: {
      target: { tabId: number };
      // `never[]` accepts concrete injected signatures without disabling type
      // checking at this serialization boundary.
      func: (...args: never[]) => unknown;
      args?: unknown[];
    }): Promise<{ result?: unknown }[]>;
  };
  /** Browser-local toolbar count mode. */
  toolbarCount?: {
    get(): Promise<ToolbarCountMode>;
  };
  /** The observation path needs durable quota state but must not depend on a
   * browser global, so tests can prove the capture frame reaches the bridge. */
  captureStorage?: ObserveChromeApi["storage"];
  /** Firefox-only runtime identity. Its absence is Chrome (or another
   * Chromium-compatible browser), which keeps existing always-on capture
   * behaviour unchanged. */
  browserInfo?: () => Promise<{ name?: string; version?: string }>;
  /** Durable pre-Firefox-140 consent for page-capture transmission. */
  captureConsent?: {
    get(): Promise<boolean>;
  };
  /** chrome.permissions seam. Adapter execution is gated on an explicit
   * optional-host-permission grant for the provider origin. */
  permissions: {
    contains(perm: { origins: string[] }): Promise<boolean>;
  };
  /** Durable user settings (chrome.storage.local): informed consent for
   * auto-accepting publisher terms, and the background work-window toggle. */
  settings: {
    getTermsConsent(): Promise<TermsConsent>;
    setTermsConsent(value: Exclude<TermsConsent, undefined>): Promise<void>;
    /** Tri-state surface choice. `tab-group` degrades to `work-window` if
     * tabGroups is absent. */
    getHandoffSurface(): Promise<HandoffSurface>;
    /** Opt-in delivery of the loss toast into the researcher's own page.
     * Absent/false = the extension-window route, which needs no host access. */
    getInPageToast(): Promise<boolean>;
  };
  /** Durable managed-tab ledger (chrome.storage.local): URL-free birth
   * certificates (Slice 2b, `./ledger.ts`). `load()` returns raw storage
   * contents — possibly still the legacy raw-URL shape — because migration
   * happens once, lazily, inside the ledger-transaction cache, never here.
   * Optional: absent disables durable orphan ownership tracking. */
  tabLedger?: {
    load(): Promise<unknown>;
    save(entries: Record<string, SurfaceBirthRecord>): Promise<void>;
  };
  /** Browser-session epoch (Slice 2b): a session-scoped mirror
   * (chrome.storage.session, wiped by an update or a restart) paired with a
   * durable copy (chrome.storage.local, survives an update) so
   * classifyRestart() can tell an SW restart (session intact, every tab id
   * still authoritative) from an update (session wiped, but the durable
   * epoch's own tabs still resolve live) from a genuine browser restart
   * (neither holds — no existing record's tab-ID authority survives).
   * Optional: absent classifies every start as a browser restart, the
   * fail-closed default. */
  epoch?: {
    getSession(): Promise<string | undefined>;
    setSession(value: string): Promise<void>;
    getLocal(): Promise<string | undefined>;
    setLocal(value: string): Promise<void>;
  };
  /** Ephemeral scan snapshots (chrome.storage.session): never chrome.storage
   * local/sync, never persisted, never sent to the daemon (ADR-0019
   * Decision 4). Optional so callers that never exercise page-bulk scanning
   * can omit it; a missing dep degrades scanning to "not saved" rather than
   * throwing. */
  pageBulkScans?: {
    get(): Promise<PageBulkScanStore>;
    set(store: PageBulkScanStore): Promise<void>;
  };
  /** Session correlation for PDF-grab terminal pushes across SW restarts. */
  pdfGrabCorrelations?: {
    get(): Promise<Record<string, PdfGrabCorrelation>>;
    set(value: Record<string, PdfGrabCorrelation>): Promise<void>;
  };
  /** Durable claim_observation outbox (chrome.storage.session): survives an
   * MV3 worker restart within the same browser session, dies with it
   * (`dev/active/claim-observation-protocol.md` §2.2, plan's storage-tier
   * design). Replayed before any lease-renewing action after a restart. */
  claimObservationOutbox?: {
    get(): Promise<Record<string, ClaimObservationOutboxEntry>>;
    set(value: Record<string, ClaimObservationOutboxEntry>): Promise<void>;
  };
  /** Durable navigation-error marker (chrome.storage.session), oracle
   * finding 5: the worker-local `navigationErrors` Map alone cannot
   * survive an MV3 worker restart, so a teardown between
   * webNavigation.onErrorOccurred and its document-settle consumption
   * must not silently broaden auth-wall treatment or leak the scaffold.
   * Optional: absent degrades to the pre-existing worker-memory-only
   * behavior. */
  navigationErrorMarkers?: {
    get(): Promise<Record<string, NavigationErrorMarkerEntry>>;
    set(value: Record<string, NavigationErrorMarkerEntry>): Promise<void>;
  };
  /** Toolbar badge for connection health. Kept injectable so bridge logic has
   * no dependency on a particular browser global. */
  action: {
    setBadgeText(details: { text: string }): Promise<void>;
    setBadgeBackgroundColor(details: { color: string }): Promise<void>;
    setTitle?(details: { title: string }): Promise<void>;
  };
  /** chrome.alarms seam. An MV3 service worker sleeps after ~30s idle; a
   * persistent one-shot alarm carries the bind backoff across that boundary. */
  alarms: {
    create(
      name: string,
      info: { periodInMinutes?: number; when?: number },
    ): void;
    get?(name: string): Promise<{ name: string } | undefined>;
    onAlarm: Listenable<[{ name: string }]>;
  };
  /** Development-mode self-reload seam (dev_reload). Optional so unit tests
   * and any host without chrome.management simply never reload. */
  devReload?: {
    /** chrome.management.getSelf().installType. "development" means unpacked. */
    installType(): Promise<string>;
    /** chrome.runtime.reload(). Never returns: the worker dies with the call. */
    reload(): void;
  };
  /** navigator.onLine snapshot. Absent (older harnesses) reads as online.
   * False gates queued-handoff releases and the autonomous-auth surface
   * gate: a wake after sleep must not navigate work into a dead network. */
  online?: () => boolean;
}

interface GenericDownloadAttempt {
  candidates: GenericCandidate[];
  index: number;
  epoch: ProviderDriveEpoch;
}

interface InstitutionalDownloadAttempt {
  claim_id: string;
  binding_id: string;
  effect_ordinal: number;
  institutional_request_id: string;
}
interface DownloadTrack {
  ids: Set<number>;
  ambiguous: boolean;
  directOffer: boolean;
  /** True for a popup-initiated delivery download. This is deliberately
   * worker-local: the durable pendingDelivery record carries the matching
   * provenance and consent evidence across restarts. */
  delivery?: boolean;
  directEpoch?: ProviderDriveEpoch;
  /** Resolved URL is memory-only; this origin/path envelope survives restart
   * through ActiveJob.direct_envelope instead. */
  directURL?: string;
  directAllowedOrigin?: string;
  directPathFamily?: string;
  directExpectedIdentifier?: string;
  route?: DeliveryRoute;
  sessionEvidence?: DeliverySessionEvidence;
  generic?: GenericDownloadAttempt;
  /** Exact institutional effect identity captured only when the browser
   * download belongs to the materialization tab. Ordinary/manual downloads
   * must not inherit a job's lingering materialization correlation. */
  institutional?: InstitutionalDownloadAttempt;
}
/** Generic state is intentionally carried on the persisted job object so the
 * attempt bound survives an MV3 worker restart without widening the wire. */
interface GenericJobState {
  generic_evaluated?: boolean;
  generic_positive_attempts?: number;
  generic_attempted_strategies?: string[];
  /** A non-applied epoch result parks this candidate until a fresh daemon
   * epoch arrives; local retries must never mint candidate two. */
  generic_terminal?: boolean;
  /** A busy/stale start defers this exact identity until its same-tuple
   * daemon re-offer arrives. */
  generic_deferred?: boolean;
}

interface PdfGrabTrack {
  ids: Set<number>;
  tabID: number;
  scanID: string;
  url: string;
  steeringPath: string;
}
interface StalledAuthHandoff {
  url: string;
  providerHosts: string[];
  expected?: { title?: string; doi?: string };
  requiresAuth?: boolean;
  accessMode?: ActiveJob["access_mode"];
}
interface QueuedHandoffDrive {
  jobID: string;
  purpose: ManagedTabPurpose;
  surfaceFallback?: boolean;
  focusExisting?: boolean;
  /** An explicit operator gesture (inbox open, popup retry) queued only for
   * a governor/effect slot. The Slice 0 containment gate never converts an
   * operator request into an engagement park. */
  operator?: boolean;
  /** Claim-resume redrives fence on an operator-active tab (the sibling the
   * operator may be typing credentials into). Other redrives — stale-IdP
   * recovery after papio raised its own tab, operator retries — navigate;
   * distinguishing papio's own focus from the operator's needs the Slice 2
   * cession tokens. */
  fenceOperatorActive?: boolean;
}

interface HandoffDrive {
  tabID: number;
  token: object;
}

interface PendingMaterializationRequest {
  responseType: string;
  requestID: string;
  jobID?: string;
  candidateID?: string;
  claimID?: string;
  bindingID?: string;
  resolve(message: BrowserMessage | undefined): void;
}

type NativeRequestKind = "response" | "transport" | "timeout";

interface NativeRequestResult {
  kind: NativeRequestKind;
  payload?: Record<string, unknown>;
  code?: string;
  message?: string;
}

interface PendingNativeRequest {
  expectedType: BrowserMessageType;
  resolve(result: NativeRequestResult): void;
}

type ClassifyRetryKind = "unknown" | "effect" | "federated_evidence";
interface ClassifyRetry {
  kind: ClassifyRetryKind;
  attempts: number;
}
type ClassificationDisposition = "apply" | "evidence_only";

interface BrokerFailure {
  ok: false;
  error: { code: string; message: string };
}
function failure(code: string, message: string): BrokerFailure {
  return { ok: false, error: { code, message } };
}
const CONNECTION_LOST_RUNTIME_COPY =
  "papio lost its connection to the daemon and is retrying…";
const INTERNAL_RUNTIME_COPY =
  "papio could not complete that request. Please try again.";

function runtimeRejectionCode(reason: unknown): string | undefined {
  if (!isObjectRecord(reason)) return undefined;
  if (typeof reason["code"] === "string") return reason["code"];
  const nested = reason["error"];
  return isObjectRecord(nested) && typeof nested["code"] === "string"
    ? nested["code"]
    : undefined;
}

function runtimeRejectionReply(reason: unknown): {
  ok: false;
  error: "connection_lost" | "internal";
  message: string;
} {
  const code = runtimeRejectionCode(reason);
  if (
    code === "connection_lost" ||
    (reason instanceof Error &&
      /message channel closed|message port closed|receiving end does not exist|daemon.*(?:disconnect|unavailable)/i.test(
        reason.message,
      ))
  ) {
    return {
      ok: false,
      error: "connection_lost",
      message: CONNECTION_LOST_RUNTIME_COPY,
    };
  }
  // A daemon-coded failure already carries actionable remediation (session_busy
  // names `papio browser use`; not_permitted names the missing host grant).
  // Collapsing it into "could not complete that request" discards the only text
  // that tells the researcher what to do, so keep the daemon's own message.
  let supplied: string | undefined;
  if (code !== undefined && isObjectRecord(reason)) {
    const nested = reason["error"];
    const direct =
      typeof reason["message"] === "string" ? reason["message"] : "";
    const inner =
      isObjectRecord(nested) && typeof nested["message"] === "string"
        ? nested["message"]
        : "";
    supplied = direct !== "" ? direct : inner !== "" ? inner : undefined;
  }
  return {
    ok: false,
    error: "internal",
    message: supplied ?? INTERNAL_RUNTIME_COPY,
  };
}

/** Attach both fulfillment and rejection paths before returning true to Chrome. */
export function respondToRuntimePromise(
  promise: Promise<unknown>,
  sendResponse: (response?: unknown) => void,
): void {
  void promise.then(
    (reply) => sendResponse(reply),
    (reason) => sendResponse(runtimeRejectionReply(reason)),
  );
}

export const INBOX_RUNTIME_MESSAGE_TYPES = [
  "papio.page_capture",
  "papio.openInbox",
  "papio.stats",
  "papio.work.pulse",
  "papio.surface.presence",
  "papio.handoff.open",
  "papio.manual.open",
  "papio.delivery.start",
  "papio.delivery.state",
  "papio.session.state",
  "papio.session.probe",
  "papio.session.signin",
  "papio.session.retry",
  "papio.pageBulk.load",
  "papio.pageBulk.scan",
  "papio.pageBulk.rescan",
  "papio.pageBulk.status",
  "papio.pageBulk.submit",
  "papio.pageBulk.grabPdf",
  "papio.pageBulk.grabStatus",
  "papio.triage.waiting",
  "papio.activity",
  "papio.triage.snapshot",
  "papio.triage.counts",
  "papio.triage.decide",
  "papio.action.resolve",
  "papio.delivery.reconcile",
  "papio.preview",
  "papio.grab.suggest",
  "papio.grab.confirm",
] as const;
type InboxRuntimeMessageType = (typeof INBOX_RUNTIME_MESSAGE_TYPES)[number];

function isInboxRuntimeMessageType(
  value: unknown,
): value is InboxRuntimeMessageType {
  return (
    typeof value === "string" &&
    INBOX_RUNTIME_MESSAGE_TYPES.includes(value as InboxRuntimeMessageType)
  );
}

interface BrokerSuccess<T extends Record<string, unknown>> {
  ok: true;
}

type BrokerReply<T extends Record<string, unknown>> =
  BrokerFailure | (BrokerSuccess<T> & T);
type ActivityPageBrokerPayload = {
  feature: boolean;
  entries: ActivityEntryPayload[];
  generated_at?: string;
  has_more?: boolean;
  cursor?: string;
  latest_seq?: number;
  new_count_since?: number;
  gap?: boolean;
};
/** A manual-download open carries a job id and nothing else. The route is
 * minted by the daemon per gesture and never chosen browser-side: the page a
 * hand-fetched paper lives on is behind the institution's sign-in, and the
 * item's canonical link is the paywall for almost all of them. */
interface ManualOpenPayload {
  job_id: string;
}

export interface DeliveryChoice {
  interaction: string;
  job_id: string;
}

export interface DeliveryCandidate {
  job_id: string;
  title: string;
}

export interface DeliveryChoiceOffer {
  interaction: string;
  candidates: DeliveryCandidate[];
}

interface DeliveryStartPayload {
  tab_id: number;
  url: string;
  job_id?: string;
  doi?: string;
  title?: string;
  choice?: DeliveryChoice;
}

type DeliveryState =
  | "sending"
  | "waiting_manual"
  | "downloaded"
  | "failed"
  | "adopted"
  | "idle"
  | "needs_choice";

type DeliveryReply = BrokerReply<{
  state: DeliveryState;
  job_id?: string;
  duplicate?: boolean;
  message?: string;
  choice?: DeliveryChoiceOffer;
}>;
function hostMatches(host: string, providerHosts: string[]): boolean {
  return providerHosts.some((h) => host === h || host.endsWith("." + h));
}

/**
 * True when papio must not fetch this URL itself, and should ask for the PDF
 * viewer's own Download button instead — bytes the browser already holds.
 *
 * Two cases. `pdf.sciencedirectassets.com` is a named host whose viewer URL is
 * not a file at all. The general case is any URL carrying a signed, expiring
 * delivery credential: re-requesting one returns an error page, because the
 * grant belongs to the session that minted it. That was a single hard-coded
 * hostname while the mechanism it guards was fully built, so every other
 * signed-CDN publisher — Silverchair, which serves JAMA and Oxford University
 * Press among others — took the doomed fetch instead of this path.
 */
function requiresNativeViewerDownload(url: string): boolean {
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== "https:") return false;
    if (parsed.hostname === "pdf.sciencedirectassets.com") return true;
  } catch {
    return false;
  }
  return carriesSignedCredential(url);
}

/** Parse a released semver (with an optional leading v) without retaining its
 * prerelease identifier: callers only need to distinguish release from pre-release. */
function parseSemver(
  version: string,
): [number, number, number, boolean] | null {
  const match =
    /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$/.exec(
      version,
    );
  if (match === null) return null;
  const [, major, minor, patch, prerelease] = match;
  return [
    Number(major),
    Number(minor),
    Number(patch),
    prerelease !== undefined,
  ];
}

/** True when a released semver (with an optional leading v) is older than the
 * bridge's compatibility floor. Unparseable daemon banners stay connected: the
 * daemon has already completed the protocol handshake. */
function isSemverLowerThan(
  version: string,
  minimum: string,
  includePrerelease = true,
): boolean {
  const actual = parseSemver(version);
  const floor = parseSemver(minimum);
  if (actual === null || floor === null) return false;
  for (let i = 0; i < 3; i += 1) {
    if (actual[i] !== floor[i]) return actual[i]! < floor[i]!;
  }
  return includePrerelease && actual[3] && !floor[3];
}

/** Whether a stamped extension release has a newer daemon version available.
 * Buildless development bundles deliberately carry the 0.0.0-dev sentinel. */
export function hasDaemonUpdateHint(
  daemonVersion: string | null,
  stampedVersion: string,
): boolean {
  if (
    daemonVersion === null ||
    stampedVersion === "" ||
    stampedVersion === "0.0.0-dev"
  )
    return false;
  return isSemverLowerThan(daemonVersion, stampedVersion, false);
}

/** Capabilities are valid only for the hello exchange on the current port. */
function clearNegotiationState(store: StoreShape): StoreShape {
  return {
    ...store,
    daemonFeatures: [],
    resolverOrigins: [],
  };
}

/** Narrow a job_offer's optional `expected` block to the resolver-declared work
 * hints we persist for classification. Never carries an IdP value. */
function parseExpected(
  raw: unknown,
): { title?: string; doi?: string } | undefined {
  if (raw === null || typeof raw !== "object") return undefined;
  const e = raw as Record<string, unknown>;
  const title = typeof e["title"] === "string" ? e["title"] : undefined;
  const doi = typeof e["doi"] === "string" ? e["doi"] : undefined;
  if (title === undefined && doi === undefined) return undefined;
  return {
    ...(title !== undefined ? { title } : {}),
    ...(doi !== undefined ? { doi } : {}),
  };
}

/** Compare only the stable, non-secret part of a provider download URL.
 * Chrome may normalize a signed query before onDeterminingFilename fires. */
function sameDownloadRoute(a: string, b: string): boolean {
  try {
    const left = new URL(a);
    const right = new URL(b);
    return left.origin === right.origin && left.pathname === right.pathname;
  } catch {
    return false;
  }
}

/** The origin-and-pathname key `sameDownloadRoute` compares, with the query and
 * fragment dropped. A provider delivery URL signs its query, so this is the only
 * part of it that may be written to extension storage. */
function downloadRoute(url: string): string | undefined {
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== "https:" && parsed.protocol !== "http:")
      return undefined;
    return `${parsed.origin}${parsed.pathname}`;
  } catch {
    return undefined;
  }
}

/** True when a persisted value is a route this worker generation may act on: a
 * bare origin and path. Anything carrying a query, a fragment, credentials or a
 * non-web scheme was not written by `downloadRoute`, so it is rejected rather
 * than used as a steering key or re-persisted. */
function isDownloadRoute(value: string): boolean {
  return downloadRoute(value) === value;
}

function directEnvelopePath(
  pathname: string,
  family: string | undefined,
  expectedIdentifier: string | undefined,
): boolean {
  if (
    family === undefined ||
    expectedIdentifier === undefined ||
    !pathname.startsWith("/")
  )
    return false;
  const separator = expectedIdentifier.indexOf(":");
  if (separator <= 0 || separator === expectedIdentifier.length - 1)
    return false;
  const kind = expectedIdentifier.slice(0, separator);
  const identifier = expectedIdentifier.slice(separator + 1);
  if (kind.length === 0 || identifier.length === 0) return false;
  const marker = `{${kind}}`;
  const markerIndex = family.indexOf(marker);
  if (
    markerIndex < 0 ||
    family.indexOf(marker, markerIndex + marker.length) >= 0
  )
    return false;
  const openBraces = family.split("{").length - 1;
  const closeBraces = family.split("}").length - 1;
  if (
    openBraces !== 1 ||
    closeBraces !== 1 ||
    family.indexOf("{") !== markerIndex ||
    family.indexOf("}") !== markerIndex + marker.length - 1 ||
    /[?#\\\u0000\r\n]/u.test(family) ||
    /(?:^|\/)\.{1,2}(?:\/|$)/u.test(family)
  )
    return false;
  if (markerIndex === 0) return false;
  let escaped: string;
  try {
    escaped = identifier
      .split("/")
      .map((part) => {
        if (part === "." || part === "..")
          return [...part].map(() => "%2E").join("");
        return encodeURIComponent(part).replace(
          /[!'()*]/gu,
          (char) =>
            `%${char.charCodeAt(0).toString(16).toUpperCase().padStart(2, "0")}`,
        );
      })
      .join("/");
  } catch {
    return false;
  }
  return (
    pathname ===
    `${family.slice(0, markerIndex)}${escaped}${family.slice(markerIndex + marker.length)}`
  );
}

/** Self-contained resolver for a provider's direct PDF endpoint, injected into
 * the tracked page. It fills {N}/{id} in urlTemplate from idPattern's capture
 * groups against the page URL, and only when the declared entitled download
 * control is present (the same signal the `article` verdict uses). For method
 * "api" the built URL returns JSON carrying the real download URL in jsonField
 * (fetched with the page's session cookies). The resolved URL is handed to
 * chrome.downloads.download; it never crosses native messaging or storage. */
export async function resolveDownloadURL(
  selector: string,
  idPattern: string | null,
  urlTemplate: string | null,
  jsonField: string | null,
): Promise<string | null> {
  if (!urlTemplate) return null;
  if (!document.querySelector(selector)) return null;
  let built = urlTemplate;
  if (idPattern) {
    const m = location.href.match(new RegExp(idPattern));
    if (!m) return null;
    built = built.replace(
      /\{(\d+|id)\}/g,
      (_, k: string) => m[k === "id" ? 1 : Number(k)] ?? "",
    );
  }
  let target = built;
  if (jsonField) {
    try {
      const r = await fetch(built, { credentials: "include" });
      if (!r.ok) return null;
      const data = (await r.json()) as Record<string, unknown>;
      const raw = data[jsonField];
      if (typeof raw !== "string") return null;
      target = raw;
    } catch {
      return null;
    }
  }
  try {
    const u = new URL(target, location.href);
    return u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** Final page-side executor for an already planned adapter effect. It is
 * injected as one function so selector lookup, identity evidence, follow-up
 * appearance, API resolution, and the page mutation share one authority
 * boundary. The background still owns chrome.downloads.download. */
export async function executePlannedPageEffect(
  plan: Plan,
  rule: {
    method: "href" | "click" | "url" | "api" | "meta";
    metaName?: string;
    followupSelector?: string;
    postClickTimeoutMs?: number;
    allowedDestinations?: { origin: string; pathPrefix: string }[];
  },
): Promise<{ ok: boolean; url?: string; why?: string }> {
  // Every refusal below is a decision not to download a paper the classifier
  // has already positively identified, so each one names itself. The caller
  // logs it. Diagnosing this from the outside cost a full session twice.
  const no = (why: string): { ok: false; why: string } => ({ ok: false, why });
  // THE PLAN ARRIVES ACROSS chrome.scripting's SERIALIZATION, WHICH DROPS
  // null-VALUED PROPERTIES. `planExecution` runs in the page and its result is
  // serialized back to the worker, which then serializes it into this
  // injection: a field the planner set to `null` arrives ABSENT. Measured live
  // 2026-08-22 — the planner emits
  // `{selector, shadow_selector: null, fingerprint, work_binding}` and this
  // function received `[fingerprint,selector,work_binding]`.
  //
  // So every authority check here MUST read "absent" and "null" as the same
  // thing. Distinguishing them refuses plans that are perfectly well formed,
  // and it refused every single one: SAGE's target has no shadow selector, so
  // `shadow_selector` was always null, so it was always absent, so this
  // function always answered no. That is why the paper was identified fifteen
  // times and never once downloaded. No unit test could see it — the harness
  // builds plan objects in-process, where the nulls are still there.
  const orNull = <T>(value: T | null | undefined): T | null => value ?? null;
  // The injected function is the final authority boundary. Never infer a
  // missing field from the adapter or from the current DOM.
  if (
    plan === null ||
    typeof plan !== "object" ||
    plan.expected_work === null ||
    typeof plan.expected_work !== "object" ||
    plan.effect_graph === null ||
    typeof plan.effect_graph !== "object" ||
    plan.revalidation === null ||
    typeof plan.revalidation !== "object" ||
    plan.revalidation.target_cardinality !== 1 ||
    typeof plan.revalidation.max_selector_length !== "number" ||
    typeof plan.revalidation.max_wait_ms !== "number"
  )
    return no("the plan is structurally incomplete");
  const graph = plan.effect_graph;
  const primary = graph.primary_target ?? graph.terms_target;
  const expectedWork = plan.expected_work as typeof plan.expected_work & {
    requested_doi?: unknown;
    requested_title?: unknown;
  };
  if (primary === null || primary === undefined)
    return no("the plan names no primary or terms target");
  if (typeof plan.route_origin !== "string")
    return no("the plan carries no route origin");
  if (plan.route_origin !== location.origin)
    return no(
      `route origin ${plan.route_origin} is not this page's ${location.origin}`,
    );
  const primarySelector = primary.selector;
  if (
    typeof primarySelector !== "string" ||
    primarySelector.length === 0 ||
    primarySelector.length > plan.revalidation.max_selector_length ||
    typeof primary.fingerprint !== "string"
  )
    return no("the primary target's selector or fingerprint is unusable");
  const primaryShadowSelector = orNull(primary.shadow_selector);
  if (
    primaryShadowSelector !== null &&
    typeof primaryShadowSelector !== "string"
  )
    return no("the primary target's shadow selector is malformed");
  const normalize = (raw: string): string => {
    let value = raw.trim().toLowerCase();
    for (let pass = 0; pass < 2; pass += 1) {
      value = value.replace(/^doi:\s*/i, "");
      value = value.replace(/^https?:\/\/(?:dx\.)?doi\.org\//i, "");
    }
    return value;
  };
  const fingerprint = (element: Element): string => {
    const names = [
      "id",
      "class",
      "href",
      "name",
      "content",
      "type",
      "role",
      "aria-label",
      "data-doi",
      "data-qa",
    ];
    const values: string[] = [element.tagName.toLowerCase()];
    for (const name of names)
      values.push(name + "=" + (element.getAttribute(name) ?? ""));
    let cursor: Element | null = element;
    while (cursor !== null && cursor.parentElement !== null) {
      let index = 0;
      for (const sibling of Array.from(cursor.parentElement.children)) {
        if (sibling === cursor) break;
        index += 1;
      }
      values.push(`p${index}`);
      cursor = cursor.parentElement;
    }
    return values.join("|");
  };
  const matchesTarget = (
    target: Element,
    ref: { fingerprint: string | null; shadow_selector: string | null },
  ): boolean => {
    if (typeof ref.fingerprint !== "string") return false;
    const [hostFingerprint, shadowFingerprint] = ref.fingerprint.split(">>");
    if (fingerprint(target) !== hostFingerprint) return false;
    const refShadowSelector = orNull(ref.shadow_selector);
    if (refShadowSelector === null) return shadowFingerprint === undefined;
    if (typeof refShadowSelector !== "string") return false;
    const shadow = (target as HTMLElement & { shadowRoot?: ShadowRoot | null })
      .shadowRoot;
    if (shadow === null || shadow === undefined) return false;
    const inner = shadow.querySelector(refShadowSelector);
    return (
      inner !== null &&
      shadowFingerprint !== undefined &&
      fingerprint(inner) === shadowFingerprint
    );
  };
  const findExactlyOne = (selector: string): Element | null => {
    try {
      const found = Array.from(document.querySelectorAll(selector));
      return found.length === 1 ? (found[0] ?? null) : null;
    } catch {
      return null;
    }
  };
  const target = findExactlyOne(primarySelector);
  if (target === null)
    return no(
      `selector ${primarySelector} did not match exactly one element on the live page`,
    );
  if (!matchesTarget(target, primary))
    return no(
      "the live element no longer matches the planned fingerprint (the page moved under the plan)",
    );
  if (
    rule.method === "meta" &&
    (target.tagName.toUpperCase() !== "META" ||
      target.getAttribute("name") !== (rule.metaName ?? "citation_pdf_url"))
  ) {
    return no("the meta target is not the declared meta element");
  }
  // No `in` test here: a null field arrives absent across the injection
  // boundary, so presence cannot be distinguished from "the planner set null".
  // The type checks below are the real contract.
  const requestedDOI = orNull(expectedWork.requested_doi);
  const requestedTitle = orNull(expectedWork.requested_title);
  if (
    (requestedDOI !== null && typeof requestedDOI !== "string") ||
    (requestedTitle !== null && typeof requestedTitle !== "string")
  ) {
    return no("the requested work fields are not strings");
  }
  const workBinding = orNull(
    (
      primary as typeof primary & {
        work_binding?: unknown;
      }
    ).work_binding,
  );
  if (
    plan.verdict.kind === "article" &&
    (requestedDOI !== null || requestedTitle !== null)
  ) {
    if (
      workBinding === null ||
      typeof workBinding !== "object" ||
      Array.isArray(workBinding)
    )
      return no("an article verdict carries no work binding");
    const binding = workBinding as {
      kind?: unknown;
      selector?: unknown;
      fingerprint?: unknown;
      attribute?: unknown;
      normalized?: unknown;
      pattern?: unknown;
    };
    if (
      (binding.kind !== "doi" && binding.kind !== "opaque") ||
      typeof binding.selector !== "string" ||
      binding.selector.length === 0 ||
      typeof binding.fingerprint !== "string" ||
      binding.fingerprint === ""
    )
      return no("the work binding is malformed");
    const bindingTarget = findExactlyOne(binding.selector);
    if (bindingTarget === null)
      return no(
        `the work binding's selector ${String(binding.selector)} did not match exactly one element`,
      );
    if (fingerprint(bindingTarget) !== binding.fingerprint)
      return no(
        "the work binding's element no longer matches its planned fingerprint",
      );
    const bindingAttribute = orNull(binding.attribute);
    const bindingNormalized = orNull(binding.normalized);
    const bindingPattern = orNull(binding.pattern);
    if (binding.kind === "opaque") {
      if (
        bindingAttribute !== null ||
        bindingNormalized !== null ||
        bindingPattern !== null
      )
        return no("an opaque work binding carries extraction fields");
    } else {
      if (
        requestedDOI === null ||
        typeof bindingAttribute !== "string" ||
        bindingAttribute.length === 0 ||
        typeof bindingNormalized !== "string" ||
        bindingNormalized !== normalize(requestedDOI) ||
        (bindingPattern !== null && typeof bindingPattern !== "string")
      )
        return no("the doi work binding is malformed or names another work");
      const raw = bindingTarget.getAttribute(bindingAttribute)?.trim() ?? "";
      if (raw === "") return no("the work binding's attribute is empty");
      let extracted = raw;
      if (bindingPattern !== null) {
        let match: RegExpMatchArray | null;
        try {
          match = raw.match(new RegExp(bindingPattern));
        } catch {
          return no("the work binding's pattern is invalid");
        }
        if (!match || typeof match[1] !== "string")
          return no("the work binding's pattern did not match");
        extracted = match[1];
      }
      if (normalize(extracted) !== bindingNormalized)
        return no("the page's doi is not the requested work");
    }
  } else if (plan.verdict.kind === "article" && workBinding !== null) {
    return no("a work binding is present but no identity was requested");
  }
  const doiEvidence = orNull(
    expectedWork.doi as {
      normalized?: unknown;
      fingerprint?: unknown;
      selector?: unknown;
      attribute?: unknown;
      pattern?: unknown;
    } | null,
  );
  const titleEvidence = orNull(
    expectedWork.title as {
      normalized?: unknown;
      fingerprint?: unknown;
      selector?: unknown;
      attribute?: unknown;
      pattern?: unknown;
    } | null,
  );
  const validatesEvidence = (
    entry: typeof doiEvidence | typeof titleEvidence,
    requested: string,
    kind: "doi" | "title",
  ): boolean => {
    if (
      entry === null ||
      typeof entry !== "object" ||
      typeof entry.fingerprint !== "string" ||
      entry.fingerprint === "" ||
      typeof entry.selector !== "string" ||
      entry.selector.length === 0 ||
      typeof entry.attribute !== "string" ||
      entry.attribute.length === 0 ||
      (orNull(entry.pattern) !== null &&
        typeof orNull(entry.pattern) !== "string")
    )
      return false;
    const source = findExactlyOne(entry.selector);
    if (source === null || fingerprint(source) !== entry.fingerprint)
      return false;
    const raw = source.getAttribute(entry.attribute)?.trim() ?? "";
    if (raw === "") return false;
    let extracted = raw;
    const entryPattern = orNull(entry.pattern);
    if (entryPattern !== null) {
      let match: RegExpMatchArray | null;
      try {
        match = raw.match(new RegExp(String(entryPattern)));
      } catch {
        return false;
      }
      if (!match || typeof match[1] !== "string") return false;
      extracted = match[1];
    }
    if (kind === "doi") {
      return (
        typeof entry.normalized === "string" &&
        entry.normalized === normalize(requested) &&
        normalize(extracted) === entry.normalized
      );
    }
    return (
      extracted.trim().toLowerCase().replace(/\s+/g, " ") ===
      requested.trim().toLowerCase().replace(/\s+/g, " ")
    );
  };
  const evidenceVerdict =
    plan.verdict.kind === "article" || plan.verdict.kind === "terms";
  if (evidenceVerdict) {
    // An adapter declares ONE `workEvidence` contract, so `workEvidenceFor`
    // (plan.ts) emits exactly one of these and null for the other — and it has
    // already refused the plan outright unless that one kind binds an identity
    // the job actually requested, and matches it.
    //
    // This used to demand BOTH: any requested identity had to re-validate here,
    // and `validatesEvidence(null, …)` is false by its first guard. So a
    // DOI-binding adapter driving a job that carried a title as well — which is
    // the normal case, since the resolver fills titles in — refused its own
    // download forever, reporting nothing. Measured live 2026-08-22 on
    // job_012f55be2bbfe0abd0ce456e36: fifteen `entitled_landing` observations,
    // an operator-solved CAPTCHA, and not one download attempt. The guard read
    // as "prove the work" and meant "never act", the same unsatisfiable shape
    // the surface-close dispositions had.
    //
    // What must still hold: the identity the adapter DID bind re-validates on
    // the page under the effect, evidence is never accepted for something the
    // job did not ask for, and an article/terms verdict never acts with no
    // identity bound at all when one was requested.
    if (doiEvidence !== null && titleEvidence !== null)
      return no("the plan carries two work-evidence bindings");
    if (doiEvidence !== null) {
      if (requestedDOI === null)
        return no("doi evidence is bound but no doi was requested");
      if (!validatesEvidence(doiEvidence, requestedDOI, "doi"))
        return no("the page's doi evidence does not re-validate");
    } else if (titleEvidence !== null) {
      if (requestedTitle === null)
        return no("title evidence is bound but no title was requested");
      if (!validatesEvidence(titleEvidence, requestedTitle, "title"))
        return no("the page's title evidence does not re-validate");
    } else if (requestedDOI !== null || requestedTitle !== null) {
      return no("an identity was requested but the plan bound none");
    }
  }
  if (rule.method === "click") {
    const termsTarget =
      graph.primary_target === null && graph.terms_target !== null
        ? graph.terms_target
        : null;
    if (termsTarget !== null) {
      if (requestedDOI === null && requestedTitle === null)
        return { ok: false };
      const planned = termsTarget as typeof termsTarget & {
        text_any?: unknown;
        control_selector?: unknown;
        control_fingerprint?: unknown;
      };
      const needles = Array.isArray(planned.text_any)
        ? planned.text_any
            .filter(
              (value): value is string =>
                typeof value === "string" && value.length > 0,
            )
            .map((value) => value.toLowerCase())
        : [];
      const controlSelector = planned.control_selector;
      const controlFingerprint = planned.control_fingerprint;
      if (
        (typeof controlSelector !== "string" && needles.length === 0) ||
        typeof controlFingerprint !== "string" ||
        controlFingerprint === ""
      )
        return { ok: false };
      let control: Element | null = null;
      if (typeof controlSelector === "string") {
        try {
          const candidates = target.matches(controlSelector)
            ? [target]
            : Array.from(target.querySelectorAll(controlSelector));
          if (candidates.length !== 1) return { ok: false };
          control = candidates[0] ?? null;
        } catch {
          return { ok: false };
        }
      } else {
        const candidates: Element[] = [];
        const walk = (root: ParentNode): void => {
          for (const element of Array.from(root.querySelectorAll("*"))) {
            const tag = element.tagName.toLowerCase();
            const actionable =
              tag === "button" ||
              tag === "a" ||
              element.getAttribute("role") === "button" ||
              tag.endsWith("-button") ||
              (tag === "input" &&
                element.getAttribute("type")?.toLowerCase() === "submit");
            if (actionable) {
              const label =
                `${(element as HTMLElement).innerText ?? ""} ${element.getAttribute("aria-label") ?? ""} ${element.getAttribute("value") ?? ""}`.toLowerCase();
              if (needles.some((needle) => label.includes(needle)))
                candidates.push(element);
            }
            const shadow = (
              element as HTMLElement & { shadowRoot?: ShadowRoot | null }
            ).shadowRoot;
            if (shadow !== null && shadow !== undefined) walk(shadow);
          }
        };
        walk(target);
        if (candidates.length !== 1) return { ok: false };
        control = candidates[0] ?? null;
      }
      if (
        !(control instanceof HTMLElement) ||
        typeof control.click !== "function" ||
        fingerprint(control) !== controlFingerprint
      )
        return { ok: false };
      control.click();
      return { ok: true };
    }
    const followup = graph.followup_target;
    if (followup === null && rule.followupSelector !== undefined)
      return { ok: false };
    let followupSelector: string | null = null;
    if (followup !== null) {
      if (
        typeof followup.selector !== "string" ||
        followup.selector.length === 0 ||
        followup.selector.length > plan.revalidation.max_selector_length ||
        rule.followupSelector !== followup.selector
      )
        return { ok: false };
      followupSelector = followup.selector;
      if (
        followup.must_appear_after_effect === true &&
        findExactlyOne(followupSelector) !== null
      )
        return { ok: false };
    }
    let clickTarget: Element | null = target;
    if (primaryShadowSelector !== null) {
      const shadow = (
        target as HTMLElement & { shadowRoot?: ShadowRoot | null }
      ).shadowRoot;
      if (shadow === null || shadow === undefined) return { ok: false };
      clickTarget = shadow.querySelector(primaryShadowSelector);
    }
    if (
      !(clickTarget instanceof HTMLElement) ||
      typeof clickTarget.click !== "function"
    )
      return { ok: false };
    clickTarget.click();
    if (followup !== null && followupSelector !== null) {
      let appeared = findExactlyOne(followupSelector);
      if (appeared === null) {
        const timeout = Math.max(
          0,
          Math.min(
            rule.postClickTimeoutMs ?? plan.revalidation.max_wait_ms,
            plan.revalidation.max_wait_ms,
            5000,
          ),
        );
        appeared = await new Promise<Element | null>((resolve) => {
          let observer: MutationObserver | null = null;
          const timer = setTimeout(() => {
            observer?.disconnect();
            resolve(findExactlyOne(followupSelector!));
          }, timeout);
          observer = new MutationObserver(() => {
            const candidate = findExactlyOne(followupSelector!);
            if (candidate !== null) {
              clearTimeout(timer);
              observer?.disconnect();
              resolve(candidate);
            }
          });
          observer.observe(document.documentElement, {
            childList: true,
            subtree: true,
            attributes: true,
          });
        });
      }
      if (
        appeared === null ||
        (followup.fingerprint !== null &&
          fingerprint(appeared) !== followup.fingerprint)
      )
        return { ok: false };
    }
    return { ok: true };
  }
  if (rule.method === "api") {
    const api = graph.api;
    if (
      api === null ||
      typeof api.endpoint !== "string" ||
      api.endpoint !== plan.url ||
      typeof api.result_field !== "string" ||
      api.result_field === ""
    )
      return { ok: false };
    try {
      const endpoint = new URL(api.endpoint);
      const route = graph.route;
      if (
        endpoint.protocol !== "https:" ||
        route === null ||
        route.origin !== endpoint.origin ||
        route.pathname !== endpoint.pathname
      )
        return { ok: false };
      const response = await fetch(endpoint.href, { credentials: "include" });
      if (!response.ok) return { ok: false };
      const data: unknown = await response.json();
      if (data === null || typeof data !== "object" || Array.isArray(data))
        return { ok: false };
      const raw = (data as Record<string, unknown>)[api.result_field];
      if (typeof raw !== "string") return { ok: false };
      const resolved = new URL(raw, location.href);
      if (
        resolved.protocol !== "https:" ||
        resolved.origin !== api.result_origin
      )
        return { ok: false };
      return { ok: true, url: resolved.href };
    } catch {
      return { ok: false };
    }
  }
  if (rule.method === "url") {
    const downloadURL = orNull(plan.url);
    if (downloadURL === null)
      return no("the plan derived no download url");
    if (plan.required_consequence !== "download")
      return no(
        `the plan's required consequence is ${String(plan.required_consequence)}, not download`,
      );
    return { ok: true, url: downloadURL };
  }
  const raw =
    target.getAttribute(rule.method === "meta" ? "content" : "href") ?? "";
  if (orNull(plan.url) === null || plan.required_consequence !== "download")
    return { ok: false };
  try {
    const resolved = new URL(raw.trim(), location.href);
    if (resolved.protocol !== "https:" || resolved.href !== plan.url)
      return { ok: false };
    const page = new URL(location.href);
    const allowed =
      resolved.origin === page.origin ||
      (Array.isArray(rule.allowedDestinations) &&
        rule.allowedDestinations.some(
          (destination) =>
            destination.origin === resolved.origin &&
            typeof destination.pathPrefix === "string" &&
            destination.pathPrefix.length > 0 &&
            resolved.pathname.startsWith(destination.pathPrefix),
        ));
    if (!allowed) return { ok: false };
  } catch {
    return { ok: false };
  }
  return { ok: true, url: plan.url };
}

/** Bare `scheme://host` for a scanned tab's URL, or null when the page is
 * not an ordinary secure page (ADR-0019 Decision 6: source.origin is bare
 * scheme+host only, and the daemon's page_bulk_submit_request rejects
 * anything but https). */
function bareHTTPSOrigin(rawURL: string | undefined): string | null {
  if (typeof rawURL !== "string" || rawURL.length === 0) return null;
  try {
    const parsed = new URL(rawURL);
    return parsed.protocol === "https:"
      ? `${parsed.protocol}//${parsed.host}`
      : null;
  } catch {
    return null;
  }
}

/** ADR-0019 operator UX requirement: the selection workspace header names
 * the source page and when it was scanned. Both are strictly local UI
 * decoration, never sent to the daemon — page-scan.ts's PageBulkSnapshot,
 * the shape shared with the detector and the daemon-facing status/submit
 * round trip, deliberately excludes them (Decision 6: source.origin is bare
 * scheme+host only, never a page title) — so they travel as a background-local
 * intersection instead of widening that shared shape. */
export type PageBulkSnapshotView = PageBulkSnapshot & {
  sourceTitle: string;
  scannedAt: string;
  pdfGrabAvailable?: boolean;
};

export class Bridge {
  private hydrated = false;
  private port: NativePort | null = null;
  /** Serialized page-acquire requests keyed by their originating msg_id. */
  private readonly pageAcquireWaiters = new Map<
    string,
    (ack: PageAcquireAckPayload) => void
  >();
  /** Signed provider URL -> job for the narrow interval between calling
   * chrome.downloads.download and receiving its ID. Memory-only: never stored
   * or framed. This lets onDeterminingFilename steer the exact adapter-started
   * download even when stale provider tabs make host correlation ambiguous. */
  private readonly pendingDownloadURLs = new Map<string, string>();
  /** Exact provider-direct requests awaiting the single effect permit. */
  private readonly pendingDirectGets = new Map<string, BrowserMessage>();
  /** Explicit sign-in intents retained while another effect owns the permit. */
  private readonly pendingSessionSignIns = new Map<
    string,
    string | undefined
  >();
  private readonly pendingPdfGrabRequests = new Map<
    string,
    {
      tab_id: number;
      url?: string | undefined;
      title?: string | undefined;
      workspace_tab_id?: number | undefined;
      scan_id?: string | undefined;
    }
  >();
  private readonly pendingMaterializationEffects = new Set<string>();
  private readonly pendingAuthReloads = new Map<
    string,
    { jobID: string; tabID: number }
  >();
  private readonly pendingFreshHandoffs = new Map<
    string,
    { job: ActiveJob; trigger: "automatic" | "explicit" }
  >();
  /** Firefox < 140 requires an explicit durable choice before any
   * page_capture frame may leave the extension. Chrome and newer Firefox
   * remain always-on. */
  private captureTransmissionAllowed = true;
  private captureConsentRequired = false;
  private captureTransmissionPolicyReady: Promise<void> = Promise.resolve();
  private captureConsentNoteLogged = false;
  private readonly pendingGrabDownloadURLs = new Map<
    string,
    { grabID: string; tabID: number; steeringPath: string }
  >();
  private readonly pdfGrabCorrelations = new Map<string, PdfGrabCorrelation>();
  private seq = 0;
  private store: StoreShape = emptyStore();
  private ready: Promise<void> = Promise.resolve();
  /** Serializes full-snapshot persistence. Concurrent Chrome events apply their
   * state transforms synchronously in event order, but chrome.storage gives no
   * write-ordering guarantee, so saves are chained: each runs after the prior
   * settles and persists the latest snapshot, so a stale write never wins. */
  private saveChain: Promise<void> = Promise.resolve();
  private listenersBound = false;
  private keepaliveAlarmInFlight = false;
  private keepaliveAlarmHandledAt = 0;
  private readonly downloads = new Map<string, DownloadTrack>();
  /** Page-derived generic evidence stays worker-local and is not durable. */
  private readonly genericEvidence = new Map<string, string[]>();
  private readonly grabDownloads = new Map<string, PdfGrabTrack>();
  /** Download ids for which a click-adapter vs armed-grab conflict was already
   * surfaced. Both listeners may observe the same item; notify once. */
  private readonly downloadGrabConflictNotified = new Set<number>();
  /** Browser-driven fixture capture shares the two-slot handoff governor. */
  private pageCaptureDriving = false;
  /** Serializes every managed-tab ledger load/mutate/save transaction. */
  private tabLedgerChain: Promise<void> = Promise.resolve();
  /** Lazily-loaded durable ledger of broker tabs papio created, migrated to
   * URL-free birth certificates on first touch (Slice 2b). */
  private tabLedgerCache: Record<string, SurfaceBirthRecord> | undefined;
  /** IDs most recently counted from the durable ledger. A worker restart
   * recovers this set during the first badge paint; it lets navigation/removal
   * repaint only when a surface that actually contributed a human sign-in
   * count stops being a wall, rather than turning every tab event into a
   * storage scan. */
  private readonly lastBadgedAuthWallTabs = new Set<number>();
  /** Bumped synchronously (before any async work) by the onActivated/
   * onUpdated listeners whenever Chrome reports a tab change — activation,
   * pin, or navigation alike. closeOwnedTab captures this per-tab counter
   * at entry and compares it again immediately before tabs.remove, with no
   * intervening await, so a touch that happens (and even reverts) during a
   * close attempt is never invisible to a single before/after tabs.get. */
  private readonly tabTouchEpoch = new Map<number, number>();
  /** papio-issued focus action tokens (surface-lifecycle-plan.md Slice 2,
   * "Causal operator cession"): one pending token per tab that papio itself
   * is about to activate. The matching onActivated event consumes the token
   * and is therefore NOT operator takeover; an activation with no token is.
   *
   * Worker memory is the correct tier for the same reason deliberateRemovals
   * is: the activation event for a focus this worker requests always arrives
   * in the same worker lifetime. A token that is somehow never consumed
   * decays into ambiguity, and ambiguity retains rather than cedes. */
  private readonly papioFocusTokens = new Map<number, number>();
  /** Last tab observed active per window. An activation makes the previous
   * tab in that window inactive, which is the event-driven moment a retained
   * surface becomes retirable. */
  private readonly lastActiveTabByWindow = new Map<number, number>();
  /** Pre-cutover ledger entries retained for one-time manual review because
   * their provenance could not be re-verified at migration (no jobID to
   * correlate against). Recomputed by the same migration pass every worker
   * start; surfaced through orphanTabStatus(). */
  private legacyLedgerReview: string[] = [];
  /** This worker lifetime's browser-session epoch (Slice 2b), resolved by
   * classifyRestart(). Undefined until bootstrapSurfaceLifecycle() runs. */
  private browserEpoch: string | undefined;
  private restartClass: "worker" | "update" | "browser" | undefined;
  /** Most recently observed daemon browser-holder-generation fence, tapped
   * from any response that carries one. Undefined until the daemon has told
   * this session a generation at least once — closeOwnedSurface refuses
   * locally rather than send a request with a made-up value. */
  private lastKnownBrowserHolderGeneration: number | undefined;
  /** Coalescing trigger state for scheduleCloseTombstoneReplay: a replay
   * already in flight absorbs a concurrent trigger as one more full pass
   * instead of racing it — same shape as outboxDrainRunning/
   * outboxDrainRerunRequested below. */
  private closeTombstoneReplayRunning = false;
  private closeTombstoneReplayRerunRequested = false;
  /** Per-job authentication-claim grant (Slice 3), worker-memory only: the
   * job whose sign-in surface is currently answering an `open_new`/
   * `navigate_existing`/`focus_owner` outcome. `nextOrdinal` is this job's
   * own monotonic `event_ordinal` counter for `gateOccurrenceID` — the
   * daemon rejects a non-increasing ordinal per §3, so this must never
   * reset except when the daemon hands back a new occurrence id. Cleared by
   * clearClaimGrant on job removal/close; lost on worker restart, same as
   * every other worker-memory correlation in this file. */
  private readonly claimGrants = new Map<
    string,
    { authenticationClaimID: string; gateOccurrenceID: string; nextOrdinal: number }
  >();
  /** `park` outcome's dependent_count (Decision 6), for the popup/inbox
   * count surface only — never persisted, never itself resumes anything. */
  private readonly claimDependentCounts = new Map<string, number>();
  /** Per-job in-flight mint latch (plan line 335-336): openFreshHandoff
   * reserves this before ever calling openManagedTab, so two racing drives
   * for the same job can never both mint. Counted toward HANDOFF_DRIVE_LIMIT
   * alongside handoffDrives, since the tab id a real slot needs doesn't
   * exist yet at reservation time. */
  private readonly mintingFreshHandoffs = new Set<string>();
  /** At-most-once latch for observation kinds the tab-update handler would
   * otherwise re-fire on every poll while a claim-owned tab sits still (wall
   * observed, the wall→post-wall transition standing in for login_started
   * per the design's fallback). Keyed by the SAME identity that builds
   * observation_id — `${authentication_claim_id}:${binding_id}:
   * ${gate_occurrence_id}:${event_kind}` (observationSuppressionKey) — never
   * by jobID/event_kind alone: a job-scoped key would keep suppressing a
   * fresh gate occurrence's first wall/login event with a stale prior
   * occurrence's latch (oracle finding 4). */
  private readonly claimObservationLatch = new Set<string>();
  /** Reverse index of claimObservationLatch: exactly which keys the CURRENT
   * grant latched for a job, so clearClaimGrant can retire only this job's
   * own entries — authentication_claim_id is shared across every dependent
   * parked on the same institutional gate, so a prefix scan on it alone
   * would also wipe a sibling job's still-active latch. */
  private readonly claimObservationLatchKeysByJob = new Map<
    string,
    Set<string>
  >();
  /** Durable claim_observation outbox (chrome.storage.session), keyed by
   * observation_id. Hydrated from deps.claimObservationOutbox at
   * bootstrapSurfaceLifecycle; every mutation re-persists it. */
  private readonly claimObservationOutboxEntries = new Map<
    string,
    ClaimObservationOutboxEntry
  >();
  /** Chrome storage writes have no ordering guarantee; serialize snapshots so
   * a late older write cannot resurrect a deleted observation. */
  private claimObservationOutboxSaveChain: Promise<void> = Promise.resolve();

  /** Resolves once managed-state load, ledger migration, group/window
   * adoption, and close-tombstone replay have all completed (Slice 2b's
   * `surfaceReady` barrier). Awaited by native job offers, runtime opens,
   * drive-queue drains, materialization retries, and close paths; never by
   * hello/poll/read paths. */
  private surfaceReady: Promise<void> = Promise.resolve();
  /** Resolves once the durable claim_observation outbox has finished its
   * post-barrier replay (§4.5). Split from surfaceReady on purpose: that
   * barrier's own callers (job offers, runtime opens, reads) must never
   * block on a correlated ack this replay awaits on the SAME serialized
   * inbound FIFO those callers can themselves be queued behind — only
   * lease-renewing observation emission (consultAuthenticationClaim,
   * enqueueClaimObservation) awaits this promise instead. */
  private outboxReplayed: Promise<void> = Promise.resolve();
  /** Serializes drainObservationOutbox: a drain already running absorbs a
   * concurrent trigger as one more full pass instead of racing it. */
  private outboxDrainRunning = false;
  private outboxDrainRerunRequested = false;
  private readonly pageCaptureLoadWaiters = new Map<
    number,
    (loaded: boolean) => void
  >();
  private readonly adoptedViewerTabs = new Map<string, number>();
  /** A finished download keeps its broker tab open until the daemon has
   * acknowledged the adoption attempt for that job. */
  private readonly completedDownloadTabs = new Map<string, number>();
  /** Jobs currently owned by the operator's direct PDF delivery. This
   * worker-local marker prevents ack cleanup from closing the user's tab. */
  private readonly deliveryJobs = new Set<string>();
  private lastDeliveryState:
    | { job_id: string; state: "adopted"; message: string; at: number }
    | undefined;
  // Volatile one-shot nonce for DOI-less picker. Never persisted. Keyed by
  // interaction nonce; value is frozen page identity + offered candidates.
  private readonly deliveryChoiceNonces = new Map<
    string,
    { pageIdentity: PageIdentity; candidates: string[]; mintedAt: number }
  >();
  // Per-tab same-document navigation sequence. Invalidated on tab close/replace.
  // Worker-local and therefore only ever corroborating evidence: the document
  // epoch that actually gates authority is read live from webNavigation.
  private readonly pageNavSeq = new Map<number, number>();
  private webNavigationBound = false;
  /** Tab id -> deps.now() timestamp of the last unconsumed top-frame
   * navigation error observed on a papio-managed tab. Consulted (and
   * consumed) by the generic auth-wall detector before it charges an
   * auth attempt: a dead end is not a human sign-in wall (surface-
   * lifecycle-plan.md invariant "every dead end has a daemon-side
   * disposition"). Worker-memory only, but no longer the ONLY record
   * (oracle finding 5): reconcileNavigationErrorMarkers restores it from
   * navigationErrorMarkerEntries at bootstrap, so a fresh worker that
   * inherits a durable marker starts with it already set instead of
   * starting empty. */
  private readonly navigationErrors = new Map<number, number>();
  /** Durable shadow of navigationErrors (chrome.storage.session), keyed by
   * tab id — oracle finding 5. Written synchronously at onErrorOccurred,
   * before any later classification runs, so a worker teardown before the
   * document settles still leaves durable evidence behind. Cleared exactly
   * where navigationErrors itself is: a settled successful landing, a
   * settled unsuccessful one (the real emission consumes it), or
   * reconciliation folding it into the outbox directly. */
  private readonly navigationErrorMarkerEntries = new Map<
    number,
    NavigationErrorMarkerEntry
  >();
  /** Resolver-provided offer URLs are cached here after storage hydration. */
  private readonly offerURLs = new Map<string, string>();
  /** Institution Shibboleth entityIDs from job offers (login_entity_id), used to
   * build an adapter's federated-login route on a `login` verdict. Worker-local;
   * re-offers repopulate it. */
  private readonly loginEntityIDs = new Map<string, string>();
  /** Provider account ids from job offers (proquest_account_id), appended to the
   * provider URL to unlock institutional access. Worker-local. */
  private readonly proquestAccountIDs = new Map<string, string>();
  /** Jobs whose provider URL was already account-id-appended this drive, so a
   * still-walled page doesn't loop. Cleared on job removal. */
  private readonly accountIdAppended = new Set<string>();
  /** Jobs whose handoff tab was already routed to federated login this drive, so
   * repeated `login` classifies do not re-navigate mid sign-in. Cleared on job
   * removal. */
  private readonly federatedLoginRouted = new Set<string>();
  /** The exact route navigation emitted by tabs.update is not authentication
   * evidence. Keep its first loading/complete lifecycle separate so only a
   * later operator navigation to the IdP can enter auth_pending. */
  private readonly federatedLoginRouteEvents = new Map<
    string,
    { url: string; loadingSeen: boolean }
  >();
  /** A later operator navigation to the IdP is the local evidence that the
   * routed page was actively used; merely completing our own route is not. */
  private readonly federatedLoginOperatorNavigated = new Set<string>();
  /** The papio-driven federated navigation has completed its first document;
   * redirect-chain loads before that boundary are not operator evidence. */
  private readonly federatedLoginRouteSettled = new Set<string>();
  /** Jobs whose openurl was re-driven once after federated login returned, so a
   * still-walled page doesn't loop. Cleared on job removal. */
  private readonly federatedReDriven = new Set<string>();
  /** Jobs that already reported a given terminal handoff or provider outcome,
   * so retries of one drive do not spam the daemon. Cleared for a fresh drive
   * and on job removal. */
  private readonly handoffOutcomeSent = new Set<string>();
  /** Generic epoch tuples already sent a terminal result this worker life.
   * The daemon also deduplicates durably, but this guard closes the concurrent
   * onChanged/startup-reconcile race before either request can be observed. */
  private readonly genericEpochResultsSent = new Set<string>();
  /** Challenge/dead-end browser.error reports are once per active drive. */
  private readonly challengeBlockedOutcomeSent = new Set<string>();
  /** Worker-local wakeups complement durable cooldown expiry timestamps. */
  private readonly challengeCooldownTimers = new Map<string, object>();
  /** job_id -> the tab whose challenge reading is awaiting confirmation. Worker
   * memory by design (see confirmThenBlockChallenge): losing it raises nothing,
   * which is the safe direction. */
  private readonly challengeConfirmations = new Map<string, number>();
  /** Jobs whose work window was already raised for a detected IdP failure this
   * worker lifetime, so a bounded re-drive loop cannot yank focus repeatedly.
   * Cleared on job removal. */
  private readonly authFailureSurfaced = new Set<string>();
  /** Chrome can dispatch a document's `complete` and title updates without
   * awaiting either callback; their shared epoch prevents one stale page from
   * consuming multiple recovery attempts. */
  private readonly staleRecoveryEpochs = new Map<string, number>();
  private readonly staleRecoveryAttemptedEpochs = new Map<string, number>();
  /** Stale-page surfacing is one action per tab document, even when Chrome
   * races title and completion callbacks for that same generation. */
  private readonly staleRecoverySurfacedEpochs = new Map<string, number>();
  private readonly staleRecoveryInFlightEpochs = new Map<string, number>();
  /** Bounded stale redrive retries are cancelled with their job. */
  private readonly staleRecoveryRetryTimers = new Map<string, object>();
  /** Document epoch already given its one late OpenAthens body probe. Retaining
   * the epoch after the timer fires prevents repeated title events from polling. */
  private readonly openAthensErrorRecheckEpochs = new Map<string, number>();
  /** Resolver pages that conclusively show zero electronic holdings are terminal
   * for this offer. Keep this worker-local debounce until the job is removed so
   * reloads and SPA completion events cannot report the same outcome repeatedly. */
  private readonly resolverNoEntitlementSent = new Set<string>();
  /** Route traversal evidence observed for each active handoff. */
  private readonly resolverRoutes = new Set<string>();
  /** Per-job auth evidence used for the next completed browser delivery. */
  private readonly deliverySessionEvidence = new Map<
    string,
    DeliverySessionEvidence
  >();
  /** A completed OA landing can release only OA concurrency queues; it is never
   * evidence that an institutional SSO session exists. */
  private openAccessLandingObserved = false;
  // Per-origin auth evidence lives in ONE place: store.authEvidenceByOrigin
  // (state.ts), timestamped and TTL'd. Three worker-local mirrors used to
  // shadow it — a release-grade Set, a landing Set, and a timestamp Map — and
  // every one of them was append-only. hasAuthEvidence() consulted the
  // release-grade Set first and returned true unconditionally, so signing out
  // revoked nothing: papio kept releasing that origin's queued handoffs for
  // the rest of the worker's life, past the TTL the persisted entry was
  // supposed to enforce. A single expiring source can be revoked with one
  // delete, and cannot disagree with itself.
  /** Current keepalive reauthentication pause, used by computeBadge. */
  private keepaliveReauthNeeded = false;
  /** Attached synchronously at worker startup, before bridge.start() binds
   * listeners, so a wake-triggered navigation can never observe it unset. */
  private keepaliveManager: KeepaliveManager | undefined;
  /** Human-auth stalls and their resolver offers remain worker-local so an
   * operator can explicitly reset and re-drive them without persistence. */
  private readonly stalledAuthHandoffs = new Map<string, StalledAuthHandoff>();
  private authUnblockedCount = 0;
  private authUnblockedAt: number | null = null;
  /** Atomically reserves the one visible handoff while tabs.create is in flight. */
  private handoffOpening = false;
  /** FIFO for accepted offers that are waiting only for a governor slot. */
  private readonly handoffDriveQueue: QueuedHandoffDrive[] = [];
  private readonly queuedDriveJobIDs = new Set<string>();
  private readonly handoffDrives = new Map<string, HandoffDrive>();
  private readonly handoffDriveTimeouts = new Map<string, object>();
  private handoffDriveDrainChain: Promise<void> = Promise.resolve();
  /** Single reducer state for the papio tab-group's human-attention surface. */
  private handoffGroupDesiredExpanded = false;
  private handoffGroupLastStateChangeAt: number | undefined;
  private handoffGroupUpdateToken: object | undefined;
  private drainingHandoffDriveQueue = false;
  private drainingQueuedHandoffs = false;
  /** Callers that arrive while the single queue drain is opening a tab wait for
   * that drain to settle before inspecting the job's resulting tab. */
  private readonly queuedHandoffDrainWaiters = new Set<() => void>();
  /** Pending fallback-release timers, keyed by queued job. Worker-local only. */
  private readonly queuedHandoffTimers = new Map<string, object>();
  /** Forced job IDs awaiting release; consumed by the single active drain so
   * overlapping fallback timers cannot drop each other's requests. */
  private readonly pendingForcedReleases = new Set<string>();
  /** Ownership tokens never leave this worker. Durable lease metadata omits
   * them, so a restarted worker waits only for the persisted expiry. */
  private readonly providerDrainLeaseOwners = new Map<string, string>();
  /** Browser-local governor for irreversible provider effects. It is separate
   * from handoffDrives: a tab may be classified before it starts its effect,
   * while tabless provider-direct work has no tab to register. */
  private effectGovernorOwner: { jobID: string; token: string } | undefined;
  /** Release wakeups defer while a provider tab effect still needs to publish
   * its managed-tab/drive consequence. */
  private effectGovernorWakePending = false;
  /** Local owner job for the provider lease token; durable lease state omits
   * this identity, so it is only used to reject same-worker sibling bypasses. */
  private readonly providerDrainLeaseJobs = new Map<string, string>();
  /** Lease-expiry wakeups are best-effort; startup and the keepalive alarm
   * re-derive expiry from session state after MV3 discards these timers. */
  private readonly providerDrainLeaseTimers = new Map<string, object>();
  /** Deferred resolver redrives retain only the in-memory job correlation and
   * re-read the one-use offer URL when the permit becomes available. */
  private readonly resolverRedriveRetryTimers = new Map<string, object>();
  private readonly resolverRouteRetryTimers = new Map<string, object>();
  /** A bounded retry budget tracks only ordinary provider render races. */
  private readonly classifyRetries = new Map<string, ClassifyRetry>();
  /** Effective provider access is stable between permission changes, so retries
   * and repeated tab updates do not repeatedly ask Chrome about the same host. */
  private readonly providerAccessByHost = new Map<string, boolean>();
  /** Broker-tab ids whose auth attempt is already counted, so the SSO redirect
   * dance within one drive increments the budget only once. Worker-local. */
  private readonly authCountedTabs = new Set<number>();
  /** Tabs this worker is removing as its OWN housekeeping — reconcile dedupe,
   * a superseded correlation, a non-chosen candidate. `onTabRemoved` consumes
   * the marker and treats the removal as deliberate: no `provider_outcome`,
   * no daemon-side cancellation, no `owner_closed`. Without it papio reads its
   * own tidy-up as the operator giving up and cancels the paper — measured
   * twice on a real library (see the open-defect table in
   * dev/active/surface-lifecycle-plan.md), which is why reviewers asked for
   * this marker rather than a rewrite of onTabRemoved.
   *
   * Worker-memory is the RIGHT tier here, unlike the durable claim identity
   * beside it: the `onRemoved` event for a removal we initiate always arrives
   * in the same worker lifetime, and a worker that dies first loses the event
   * entirely, so there is nothing for a persisted marker to answer. */
  private readonly deliberateRemovals = new Set<number>();
  private readonly pageBulkRecovery: PageBulkCohortRecovery;
  private managedTabChain: Promise<unknown> = Promise.resolve();
  /** Coalesce concurrent inbox clicks for one job so one request owns the
   * open/focus choreography and all callers receive its result. */
  private readonly openHandoffRequests = new Map<
    string,
    Promise<BrokerReply<{ opened: true }>>
  >();
  constructor(private readonly deps: BridgeDeps) {
    this.workerEpoch = deps.randomUUID().replace(/-/g, "");
    this.pageBulkRecovery =
      deps.pageBulkRecovery ?? new PageBulkCohortRecovery();
    // A Firefox-only runtime probe is asynchronous; fail closed until its
    // version and durable consent have been resolved. Chrome has no probe and
    // therefore retains the existing always-on default above.
    if (deps.browserInfo !== undefined) this.captureTransmissionAllowed = false;
  }
  /** A job refreshes the daemon's human action at most once per spin-up. */
  private readonly authStalledReported = new Set<string>();
  /** Serializes work-window creation so concurrent offers cannot race two
   * dedicated windows into existence. Worker-local only. */
  private workTabChain: Promise<unknown> = Promise.resolve();
  private handoffGroupChain: Promise<void> = Promise.resolve();
  private readonly handoffGroupIDsByWindow = new Map<number, number>();
  /** Serializes adoption scans, group folding, tombstone replay, and
   * terminal reconciliation (Slice 2b) — never the effect governor, which
   * only irreversible provider navigation, page mutation, and download
   * initiation acquire. */
  private lifecycleChain: Promise<void> = Promise.resolve();
  /** Native port messages may await storage, tabs, or downloads. Preserve
   * receipt order across those awaits so state transitions never interleave. */
  /** Best-effort display cache only, refreshed from daemon counts or snapshots. */
  private triagePendingCount: number | undefined;
  private triageRequiredTurnCount: number | undefined;
  private triageRequiredTurnsComplete = false;
  private triageCountsSchemaV3 = false;
  private triageWatchHits = 0;
  private triageRetractions = 0;
  private toolbarCountMode: ToolbarCountMode = "required";
  private lastBadgePaint: BadgeResult | undefined;
  private inboundChain: Promise<void> = Promise.resolve();
  /** The durable alarm carries this backoff across MV3 worker sleep. This
   * map only remembers the last rung while the worker remains alive; it never
   * decides whether a retry is allowed. */
  private readonly institutionalRetryAttempts = new Map<string, number>();
  /** One resolver per correlated native triage request. It is intentionally
   * worker-memory only; daemon state remains the authority after a restart. */
  private readonly pendingNativeRequests = new Map<
    string,
    PendingNativeRequest
  >();
  /** One detached response-loss retry timer per materialization job. */
  private readonly materializationRetryTimers = new Map<string, object>();
  /** One offline-revival timer per materialization job, deliberately SEPARATE
   * from materializationRetryTimers. That map doubles as the "a retry is
   * already pending, do not drive now" guard, so parking an offline recheck in
   * it makes a fresh online offer wait for the timer instead of driving — the
   * first attempt at this fix deadlocked its own test that way. This map is
   * consulted by nobody else, so it can keep a paper alive across an outage
   * without shadowing any drive. */
  private readonly materializationOfflineTimers = new Map<string, object>();
  /** Typed pulse cache is worker-local; receipt time is browser time and never
   * replaced with daemon generated_at. A fresh worker starts with no trusted
   * reading, so callers render Unknown until the next validated response. */
  private pulseCache:
    | {
        pulse: WorkPulseResponsePayload;
        receivedAt: number;
        workerEpoch: string;
      }
    | undefined;
  private readonly workerEpoch: string;
  /** Materialization replies have no request_id by protocol design. Match
   * them only to the current opaque job/claim/binding correlation. */
  private readonly pendingMaterializationRequests: PendingMaterializationRequest[] =
    [];
  /** One detached workflow per job; duplicate offers are idempotent. */
  private readonly materializationRuns = new Map<string, Promise<void>>();
  /** A fresh same-candidate offer arriving during a run requests one replay
   * after that run releases the single-flight slot. */
  private readonly materializationReruns = new Set<string>();
  /** Jobs removed while a detached materialization run is between awaits. */
  private readonly cancelledMaterializationJobs = new Set<string>();
  private portGeneration = 0;
  private helloAckGeneration = -1;
  private helloSentGeneration = -1;
  /** Session role from THIS port's hello_ack. Deliberately worker-memory only
   * and cleared with the port: a role is a property of one live connection, so
   * a persisted copy would outlive the daemon that issued it and be readable as
   * "holder" during exactly the window after a worker restart when holder-only
   * work must not be attempted. `undefined` means no ack on this port yet. */
  private helloRole: BrowserSessionRole | undefined;
  /** Port generation whose hello the daemon answered with session_busy. That
   * hello never gets an ack, so without this every foreground request would
   * burn the full hello wait before failing — the popup's own status paint
   * arrived five seconds late in a browser that was merely not the holder. */
  private helloDeniedGeneration = -1;
  private helloRequestID: string | undefined;
  private readonly helloWaiters = new Set<(acknowledged: boolean) => void>();
  private requestIDSequence = 0;
  /** Best-effort display cache only, refreshed from daemon counts or snapshots. */
  /** Durable institutional demand from the most recent negotiated counts poll. */
  private triageActionsRequiresAuth: number | undefined;
  private triageActionsRequiresAuthAt: number | undefined;
  /** Per-origin session_evidence throttle. Keyed by origin (or "" when no
   * origin hint), so one institution's evidence can no longer suppress
   * another's for SESSION_EVIDENCE_THROTTLE_MS. */
  private readonly sessionEvidenceSentAt = new Map<string, number>();

  trackedJobCount(): number {
    return this.store.activeJobs.length;
  }

  warmDemand(): boolean {
    const count = this.triageActionsRequiresAuth;
    const receivedAt = this.triageActionsRequiresAuthAt;
    if (count === undefined || receivedAt === undefined || count <= 0)
      return false;
    const age = this.deps.now() - receivedAt;
    return age >= 0 && age <= TRIAGE_COUNTS_FRESH_MS;
  }

  private queuedAuthJobCount(): number {
    return this.store.activeJobs.filter(
      (job) => job.status === "queued" && job.requires_auth === true,
    ).length;
  }
  /** Only the daemon's exact delegated mode authorizes unattended browser
   * effects. Legacy/missing and assisted jobs remain parked until the operator
   * explicitly opens them. */
  private hasDelegatedAuthority(job: ActiveJob | undefined): boolean {
    return job?.access_mode === "delegated";
  }

  queuedAuthJobs(): number {
    return this.queuedAuthJobCount();
  }

  lastAuthReturnedAt(): number | undefined {
    return this.store.lastAuthReturnedAt;
  }

  stalledAuthJobIDs(): string[] {
    return [...this.authStalledReported];
  }

  challengeBlockedJobCount(): number {
    return this.store.activeJobs.filter((job) => job.challenge_blocked === true)
      .length;
  }

  attachKeepalive(manager: KeepaliveManager): void {
    this.keepaliveManager = manager;
    this.keepaliveReauthNeeded = manager.getSnapshot().pausedForReauth;
  }

  setKeepaliveReauthNeeded(paused: boolean): void {
    if (this.keepaliveReauthNeeded === paused) return;
    this.keepaliveReauthNeeded = paused;
    void this.syncConnectionBadge();
  }

  private resolverOriginHint(rawURL: string | undefined): string | undefined {
    if (rawURL === undefined || isAuthenticationURL(rawURL)) return undefined;
    try {
      const parsed = new URL(rawURL);
      const origin = `${parsed.protocol}//${parsed.host}`;
      return isBareHTTPSOrigin(origin) ? origin : undefined;
    } catch {
      return undefined;
    }
  }

  /** Derive one configured institution from durable offer metadata. The
   * daemon-supplied resolver set is authoritative; an offer/provider can
   * select from it but can never create a new institution. */
  private configuredInstitutionOrigin(
    offerURL: string | undefined,
    providerHosts: string[],
  ): string | undefined {
    const known = this.knownResolverOrigins();
    if (offerURL !== undefined) {
      try {
        const offered = new URL(offerURL);
        if (
          offered.protocol === "https:" &&
          known.includes(offered.origin)
        ) {
          return offered.origin;
        }
      } catch {
        // Provider-host fallback below remains fail-closed.
      }
    }
    const matches = new Set<string>();
    for (const origin of known) {
      try {
        if (hostMatches(new URL(origin).hostname, providerHosts)) {
          matches.add(origin);
        }
      } catch {
        // knownResolverOrigins already validates; refuse malformed residue.
      }
    }
    return matches.size === 1 ? matches.values().next().value : undefined;
  }

  /** The configured institution origin for one job. The persisted binding
   * survives worker sleep; old-daemon rows fall back to a current re-offer's
   * exact resolver origin. */
  private jobInstitutionOrigin(job: ActiveJob): string | undefined {
    const known = this.knownResolverOrigins();
    const stored = job.institution_origin;
    if (stored !== undefined && known.includes(stored)) return stored;
    const offered = this.offerURLs.get(job.job_id);
    // Older daemons advertise no configured-origin set. Preserve their prior
    // exact-offer fallback; a current non-empty set remains authoritative.
    if (known.length === 0) return this.resolverOriginHint(offered);
    return this.configuredInstitutionOrigin(
      offered,
      job.provider_hosts ?? [],
    );
  }

  knownResolverOrigins(): readonly string[] {
    const origins = new Set<string>();
    // Institutions are the daemon's CONFIG-derived resolver origins from the
    // hello ack — never offer traffic. OA/direct offers carry provider URLs,
    // and folding those in turned every provider that ever offered a job into
    // a phantom "institution" row in the popup session card.
    const candidates = [...(this.store.resolverOrigins ?? [])];

    for (const candidate of candidates) {
      // Configuration membership is authoritative and deliberately does not
      // use resolverOriginHint: an institution's configured bare origin may
      // contain auth-like labels (for example, sso.example.edu).
      if (isBareHTTPSOrigin(candidate)) origins.add(candidate);
    }
    return [...origins];
  }
  /** Browser-local authentication demand that is already parked at a visible
   * human sign-in step. A queued requires_auth offer is papio's future work,
   * not permission to inspect a library page now. */
  sessionAuthDemand(): SessionAuthDemand[] {
    return this.store.activeJobs
      .filter((job) => job.status === "auth_pending")
      .map((job) => {
        const origin = this.jobInstitutionOrigin(job);
        return origin === undefined
          ? undefined
          : { job_id: job.job_id, origin };
      })
      .filter((demand): demand is SessionAuthDemand => demand !== undefined);
  }

  authDemandOrigins(): string[] {
    return [
      ...new Set(this.sessionAuthDemand().map((demand) => demand.origin)),
    ];
  }

  /** Correlate a settled untracked publisher page with visible parked demand.
   * Demand is checked before URL parsing, so unrelated browsing is not read
   * when papio has no sign-in block. Multiple institutions fail closed. */
  private institutionalLandingOrigin(rawURL: string | undefined): string | undefined {
    const parked = this.store.activeJobs.filter(
      (job) => job.status === "auth_pending",
    );
    if (parked.length === 0 || rawURL === undefined) return undefined;
    let hostname: string;
    try {
      const parsed = new URL(rawURL);
      if (parsed.protocol !== "https:") return undefined;
      hostname = parsed.hostname.toLowerCase();
    } catch {
      return undefined;
    }
    const origins = new Set<string>();
    for (const job of parked) {
      if (!hostMatches(hostname, job.provider_hosts)) continue;
      const origin = this.jobInstitutionOrigin(job);
      if (origin === undefined) return undefined;
      origins.add(origin);
    }
    return origins.size === 1 ? origins.values().next().value : undefined;
  }

  sessionOriginStates(): KeepaliveOriginSnapshot[] {
    const manager = this.keepaliveManager;
    if (manager !== undefined) return manager.getOriginSnapshots();
    const state = this.sessionState();
    return this.knownResolverOrigins().map((origin) => {
      const isDefault = origin === state.resolverOrigin;
      return {
        origin,
        authenticated: isDefault && state.authenticated,
        verdict: isDefault ? (state.verdict ?? "unknown") : "unknown",
        probeSource: isDefault ? (state.probeSource ?? "none") : "none",
        ...(isDefault && state.lastProbeOutcome !== undefined
          ? { lastProbeOutcome: state.lastProbeOutcome }
          : {}),
        lastVerdictAt: isDefault ? (state.lastVerdictAt ?? null) : null,
        checking: isDefault && state.checking === true,
        likelyAuthenticated: isDefault && state.likelyAuthenticated === true,
        pausedForReauth: isDefault && state.pausedForReauth,
        lastProbeAt: isDefault ? state.lastProbeAt : null,
        dirtySince: null,
      };
    });
  }

  /** Any configured origin whose persisted evidence is still inside the TTL.
   * Only reached before the keepalive manager has a snapshot of its own. */
  private anyOriginAuthenticated(): boolean {
    return this.knownResolverOrigins().some((origin) =>
      this.hasAuthEvidence(origin),
    );
  }

  sessionState(): BridgeSessionState {
    const fallback: KeepaliveSnapshot = {
      enabled: true,
      intervalMinutes: 4,
      authenticated: this.anyOriginAuthenticated(),
      verdict: this.anyOriginAuthenticated() ? "in" : "unknown",
      probeSource: this.anyOriginAuthenticated() ? "keepalive_tab" : "none",
      lastVerdictAt: null,
      checking: false,
      likelyAuthenticated: false,
      pausedForReauth: this.keepaliveReauthNeeded,
      lastProbeAt: null,
      resolverOrigin: null,
      lastAuthReturnedAt: this.store.lastAuthReturnedAt ?? null,
      queuedAuthJobs: this.queuedAuthJobCount(),
      stalledAuthJobs: this.stalledAuthJobIDs(),
    };
    const snapshot = this.keepaliveManager?.getSnapshot() ?? fallback;
    return {
      ...snapshot,
      pausedForReauth: this.keepaliveReauthNeeded || snapshot.pausedForReauth,
      authenticated: snapshot.authenticated,
      lastAuthReturnedAt:
        this.store.lastAuthReturnedAt ?? snapshot.lastAuthReturnedAt,
      queuedAuthJobs: this.queuedAuthJobCount(),
      stalledAuthJobs: this.stalledAuthJobIDs(),
      releasedAuthJobs: this.authUnblockedCount,
      releasedAuthJobsAt: this.authUnblockedAt,
      authDemandComplete: true,
      authDemand: this.sessionAuthDemand(),
    };
  }

  /** Pure read for the popup's steady-state poll: never probes, never injects. */
  async sessionStateSnapshot(): Promise<BridgeSessionState> {
    await this.ready;
    return this.sessionState();
  }
  /** Pure read for the inbox's browser-local waiting overlay: return markers
   * whose display hints have not elapsed. The hint never demotes the park. */
  async waitingSessionJobsSnapshot(): Promise<
    { job_id: string; deadline?: number }[]
  > {
    await this.ready;
    const now = this.deps.now();
    return this.store.activeJobs
      .filter(
        (job) =>
          job.waiting_for_session === true &&
          job.waiting_deadline !== undefined &&
          job.waiting_deadline > now,
      )
      .map((job) => ({
        job_id: job.job_id,
        ...(job.waiting_deadline === undefined
          ? {}
          : { deadline: job.waiting_deadline }),
      }));
  }

  /** Refresh a stale/unknown keepalive verdict before replying to the popup.
   * The manager bounds the wait and leaves `checking` true when browser APIs
   * exceed the foreground budget. */
  async sessionStateWithProbe(): Promise<BridgeSessionState> {
    await this.ready;
    await this.keepaliveManager?.probeForeground();
    return this.sessionState();
  }

  async requestSessionSignIn(
    origin?: string,
  ): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    const effectJobID = `session-signin:${origin ?? "latest"}`;
    const effectToken = this.claimEffectGovernor(effectJobID);
    if (effectToken === undefined) {
      this.pendingSessionSignIns.set(effectJobID, origin);
      return failure(
        "effect_busy",
        "Sign-in will open when the current browser effect finishes",
      );
    }
    try {
      if (origin !== undefined) {
        if (!isBareHTTPSOrigin(origin)) {
          return failure(
            "resolver_unavailable",
            "No resolver configured yet — open a paper first",
          );
        }
        if (!this.hasCurrentHello()) {
          return failure(
            "resolver_unavailable",
            "The daemon has not confirmed configured institutions",
          );
        }
        const known = this.knownResolverOrigins();
        if (known.length > 0 && !known.includes(origin)) {
          return failure(
            "resolver_unavailable",
            "This institution is not currently configured",
          );
        }
        if (known.length === 0) {
          const manager = this.keepaliveManager;
          const snapshotOrigin = manager?.getSnapshot().resolverOrigin;
          const snapshotOrigins =
            manager !== undefined &&
            typeof manager.getOriginSnapshots === "function"
              ? manager.getOriginSnapshots().map((snapshot) => snapshot.origin)
              : [];
          const fallbackOrigins = new Set(
            [snapshotOrigin, ...snapshotOrigins].filter(
              (candidate): candidate is string => isBareHTTPSOrigin(candidate),
            ),
          );
          if (!fallbackOrigins.has(origin)) {
            return failure(
              "resolver_unavailable",
              "This institution is not currently configured",
            );
          }
        }
        // Hand the tab to the keepalive manager exactly as the no-origin branch
        // below does. It owns the tab for the duration of the sign-in: its
        // reload cycle pauses, so a scheduled reload cannot destroy an
        // in-flight SAML exchange, and the tab is never entered in the managed
        // tab ledger, so startup orphan reconciliation cannot close it while
        // the operator is still signing in.
        const originManager = this.keepaliveManager;
        if (originManager !== undefined) {
          try {
            if (await originManager.openReauth(origin))
              return { ok: true, opened: true };
          } catch {
            // Fall through to the unmanaged tab below.
          }
        }
        const tabID = await this.openManagedTab({
          url: origin,
          purpose: "session-signin",
        });
        return tabID === undefined
          ? failure(
              "session_open_failed",
              "Could not open the institution sign-in",
            )
          : { ok: true, opened: true };
      }
      const manager = this.keepaliveManager;
      let resolverOrigin =
        manager?.getSnapshot().resolverOrigin ?? this.latestResolverOrigin();
      if (manager !== undefined) {
        try {
          if (await manager.openReauth()) return { ok: true, opened: true };
        } catch {
          // Fall through to the explicit foreground-origin fallback below.
        }
        resolverOrigin = manager.getSnapshot().resolverOrigin ?? resolverOrigin;
      }
      if (resolverOrigin === undefined) {
        return failure(
          "resolver_unavailable",
          "No resolver configured yet — open a paper first",
        );
      }
      const tabID = await this.openManagedTab({
        url: resolverOrigin,
        purpose: "session-signin",
      });
      return tabID === undefined
        ? failure(
            "session_open_failed",
            "Could not open the institution sign-in",
          )
        : { ok: true, opened: true };
    } finally {
      this.releaseEffectGovernor(effectJobID, effectToken, false);
      this.wakeEffectGovernor();
    }
  }

  async retryAuthStalled(
    jobID: string,
  ): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    const current = findByJob(this.store, jobID);
    const saved =
      this.stalledAuthHandoffs.get(jobID) ??
      (current !== undefined && this.offerURLs.get(jobID) !== undefined
        ? {
            url: this.offerURLs.get(jobID)!,
            providerHosts: [...current.provider_hosts],
            ...(current.expected !== undefined
              ? { expected: current.expected }
              : {}),
            ...(current.requires_auth !== undefined
              ? { requiresAuth: current.requires_auth }
              : {}),
            ...(current.access_mode !== undefined
              ? { accessMode: current.access_mode }
              : {}),
          }
        : undefined);
    if (saved === undefined || !this.authStalledReported.has(jobID)) {
      return failure(
        "handoff_unavailable",
        "This authentication stall is no longer available",
      );
    }
    if (
      this.effectGovernorOwner !== undefined &&
      !this.handoffDrives.has(jobID) &&
      this.handoffDrives.size < HANDOFF_DRIVE_LIMIT
    ) {
      const now = this.deps.now();
      await this.upsertJobWithOffer(
        {
          job_id: jobID,
          tab_id: current?.tab_id ?? -1,
          offered_at: now,
          expires_at: now,
          status: "accepted",
          provider_hosts: [...saved.providerHosts],
          ...(saved.accessMode !== undefined
            ? { access_mode: saved.accessMode }
            : {}),
          ...(saved.requiresAuth !== undefined
            ? { requires_auth: saved.requiresAuth }
            : {}),
        },
        saved.url,
      );
      this.enqueueHandoffDrive({
        jobID,
        purpose: "redrive",
        focusExisting: false,
        operator: true,
      });
      await this.drainHandoffDriveQueue();
      return { ok: true, opened: true };
    }
    await this.update((s) => this.clearAuthAttempts(s, jobID));
    if (
      !this.handoffDrives.has(jobID) &&
      this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT
    ) {
      const now = this.deps.now();
      await this.upsertJobWithOffer(
        {
          job_id: jobID,
          tab_id: current?.tab_id ?? -1,
          offered_at: now,
          expires_at: now,
          status: "accepted",
          provider_hosts: [...saved.providerHosts],
          ...(saved.accessMode !== undefined
            ? { access_mode: saved.accessMode }
            : {}),
          ...(saved.requiresAuth !== undefined
            ? { requires_auth: saved.requiresAuth }
            : {}),
        },
        saved.url,
      );
      this.enqueueHandoffDrive({
        jobID,
        purpose: "redrive",
        focusExisting: false,
        operator: true,
      });
      this.authStalledReported.delete(jobID);
      this.stalledAuthHandoffs.delete(jobID);
      this.sendJobAccept(jobID);
      await this.drainHandoffDriveQueue();
      return { ok: true, opened: true };
    }
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      this.enqueueHandoffDrive({
        jobID,
        purpose: "redrive",
        focusExisting: false,
        operator: true,
      });
      await this.drainHandoffDriveQueue();
      return { ok: true, opened: true };
    }
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: saved.url,
        jobId: jobID,
        purpose: "redrive",
      });
    } catch {
      tabID = undefined;
    }
    if (tabID === undefined) {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
      return failure(
        "handoff_open_failed",
        "Could not reopen the institutional handoff",
      );
    }
    const openedAt = this.deps.now();
    try {
      await this.upsertJobWithOffer(
        {
          job_id: jobID,
          tab_id: tabID,
          offered_at: openedAt,
          expires_at: openedAt,
          status: "accepted",
          ...(saved.accessMode !== undefined
            ? { access_mode: saved.accessMode }
            : {}),
          provider_hosts: [...saved.providerHosts],
          ...(saved.requiresAuth !== undefined
            ? { requires_auth: saved.requiresAuth }
            : {}),
        },
        saved.url,
      );
      this.registerHandoffDrive(jobID, tabID);
      this.stalledAuthHandoffs.delete(jobID);
      this.sendJobAccept(jobID);
    } catch (error) {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
      throw error;
    }
    this.releaseEffectGovernor(jobID, effectToken, false);
    this.wakeEffectGovernor();
    return { ok: true, opened: true };
  }
  /** Papers a human can actually act on: `auth_pending` is set exactly when a
   * paper's own surface reaches a login page, so this is the set with a page
   * in front of the operator (including one parked after its drive budget,
   * which still needs the human to finish that sign-in).
   *
   * It deliberately EXCLUDES `queued && requires_auth`. Those papers have no
   * surface: an institution is served by one sign-in at a time, so they wait
   * for papio, not for the operator. Counting them reported the queue back as
   * a human ask - the badge read "13 papers waiting on your institution
   * sign-in" while exactly one could proceed, and a twenty-hour internal
   * stall looked like a polite wait on the operator (measured live
   * 2026-08-20). queuedAuthJobCount() reports them as papio's own work
   * instead. The old rationale here - that a cold preflight with no tab would
   * otherwise hide the sign-in signal - is served by `reauthNeeded`, which
   * states "your library needs a sign-in" without miscounting papers. */
  private signInBlockerCount(): number {
    return this.store.activeJobs.filter((job) => job.status === "auth_pending")
      .length;
  }

  /** Papers with a login page in front of the operator that `activeJobs`
   * cannot see. `chrome.storage.session` is wiped on every extension
   * reload/update, so a paper mid-sign-in loses its `auth_pending` status
   * while its tab stays live — and the daemon cannot re-offer it, because a
   * job owning a live claim is not a scheduler-eligible candidate. Measured
   * live 2026-08-20: right after a reload the badge read "connected" while a
   * real login page sat in papio's own group, so the one paper the operator
   * could actually finish was the one paper papio stopped mentioning.
   *
   * The durable birth ledger (`storage.local`) survives that wipe, so the ask
   * is recovered from it: same browser epoch, not ceded, tab still live, and
   * its CURRENT url is an authentication URL. This only ever REPORTS - it
   * never revives a job (ADR-0022 Decision 1: the extension is never a
   * durable queue), and it cannot invent an ask, because it requires a live
   * papio-owned tab actually sitting at a wall. Returns tab ids so the caller
   * can union them with jobs it already knows about rather than counting one
   * paper twice. */
  private async authWallSurfaceTabs(): Promise<Set<number>> {
    const walls = new Set<number>();
    const ledger = await this.snapshotTabLedger();
    for (const [key, record] of Object.entries(ledger)) {
      if (record.ceded === true) continue;
      // A prior-epoch record cannot prove this tab is still papio's (plan
      // line 151); those stay in the operator-review path.
      if (record.browser_epoch !== this.browserEpoch) continue;
      const tabID = Number(key);
      if (!Number.isInteger(tabID) || tabID < 0) continue;
      try {
        const tab = await this.deps.tabs.get(tabID);
        if (typeof tab.url === "string" && isAuthenticationURL(tab.url))
          walls.add(tabID);
      } catch {
        // Gone: nothing to report, and reconcile forgets the record.
      }
    }
    return walls;
  }

  private currentBlockedProviderHosts(): string[] {
    return [...new Set(this.store.blockedProviderHosts ?? [])];
  }

  /** A new broker tab starts a new page-observation epoch. Durable generic E1
   * state is keyed by the daemon-minted drive attempt and must survive
   * automatic redrives, tab replacement, and MV3 worker restart. */
  private beginProviderDrive(jobID: string): void {
    this.federatedLoginOperatorNavigated.delete(jobID);
    this.federatedLoginRouteSettled.delete(jobID);
    this.federatedLoginRouteEvents.delete(jobID);
    this.classifyRetries.delete(jobID);
    this.handoffOutcomeSent.delete(`${jobID}:ui_changed`);
    this.challengeBlockedOutcomeSent.delete(`${jobID}:challenge_blocked`);
  }

  /** Chrome answers this origin query from effective access: an all-sites grant
   * is sufficient to read a provider page even when no host-specific grant exists. */
  private async hasEffectiveProviderAccess(
    host: string,
  ): Promise<boolean | undefined> {
    const cached = this.providerAccessByHost.get(host);
    if (cached !== undefined) return cached;
    try {
      const allowed = await this.deps.permissions.contains({
        origins: [`https://${host}/*`],
      });
      this.providerAccessByHost.set(host, allowed);
      return allowed;
    } catch (error) {
      // A failed permission query is not proof of a missing grant, so keep the
      // handoff assisted instead of claiming a diagnosis we cannot establish.
      console.error(
        "papio: provider access check failed; staying assisted",
        error,
      );
      return undefined;
    }
  }

  /** Remember the standing host-level blocker and the exact governed job so
   * repeated pages do not duplicate attention and a later grant can resume. */
  private async reportBlockedProviderHost(
    jobID: string,
    host: string,
  ): Promise<void> {
    if (!this.currentBlockedProviderHosts().includes(host)) {
      await this.update((store) => ({
        ...store,
        blockedProviderHosts: [
          ...new Set([...(store.blockedProviderHosts ?? []), host]),
        ],
      }));
      await this.syncConnectionBadge();
    }

    const job = findByJob(this.store, jobID);
    if (job !== undefined && job.blocked_provider_host !== host) {
      // Keep the governed tab and job live. The popup's user-gesture-bound
      // permission grant can then resume this exact page instead of leaving a
      // terminal manual-download action behind.
      await this.update((store) =>
        patchJob(store, jobID, { blocked_provider_host: host }),
      );
    }
  }

  private async clearBlockedProviderHost(host: string): Promise<boolean> {
    const hasMarker = this.store.activeJobs.some(
      (job) => job.blocked_provider_host === host,
    );
    if (!hasMarker && !this.currentBlockedProviderHosts().includes(host))
      return false;
    await this.update((store) => ({
      ...store,
      activeJobs: store.activeJobs.map((job) => {
        if (job.blocked_provider_host !== host) return job;
        const { blocked_provider_host: _blockedProviderHost, ...unblocked } =
          job;
        return unblocked;
      }),
      blockedProviderHosts: (store.blockedProviderHosts ?? []).filter(
        (blockedHost) => blockedHost !== host,
      ),
    }));
    return true;
  }

  /** Permission changes invalidate the cache before repainting the durable
   * host-level signal, so an Options-page grant clears it without a page reload. */
  async onPermissionsChanged(): Promise<void> {
    this.providerAccessByHost.clear();
    const retryJobs = new Set<string>();
    for (const host of this.currentBlockedProviderHosts()) {
      if ((await this.hasEffectiveProviderAccess(host)) !== true) continue;
      for (const job of this.store.activeJobs) {
        if (job.blocked_provider_host === host) retryJobs.add(job.job_id);
      }
      await this.clearBlockedProviderHost(host);
    }
    for (const jobID of retryJobs) {
      try {
        await this.reclassifyCurrentProviderPage(jobID, true);
      } catch (error) {
        // A tab can disappear between the browser permission callback and the
        // retry. Normal tab-close recovery remains authoritative.
        console.error(
          "papio: provider access granted after its tab closed",
          error,
        );
      }
    }
    await this.syncConnectionBadge();
    // A RESOLVER grant is not provider work: the loop above only clears
    // provider-host markers, so before this call the grant was observed and
    // then never acted on for institutions — parked work kept waiting on a
    // library page papio had just been allowed to read. Placed after the badge
    // so an ungranted-resolver count repaints promptly instead of behind a
    // probe, and awaited so the ordering is deterministic for callers.
    await this.keepaliveManager?.onPermissionsChanged();
  }

  /** Resolve where handoffs open. `tab-group` degrades to `work-window` when
   * the platform lacks tab groups, and any window-backed mode degrades to
   * `in-window` without a windows API. */
  private async handoffSurface(): Promise<HandoffSurface> {
    let surface = await this.deps.settings.getHandoffSurface();
    if (surface === "tab-group" && this.deps.tabs.group === undefined)
      surface = "work-window";
    if (surface === "work-window" && this.deps.windows === undefined)
      surface = "in-window";
    return surface;
  }

  /** Open a broker tab. Work-window tabs stay unfocused and minimized unless a
   * directly matched adapter needs a visible window; tab-group tabs land in the
   * collapsed "papio" group; the fallback surface may take focus.
   *
   * Never throws, and — the invariant this enforces — never answers `undefined`
   * for a tab it already reported through `onTabMaterialized`. Each branch does
   * optional work after the tab exists (grouping, minimizing, remembering the
   * work window), and a refusal there used to unwind past the report: the
   * caller abandoned the drive while the tab stayed open, and the next drive
   * epoch opened another. Measured live 2026-08-23 on the tab-group branch —
   * Chrome refused 36 groupings in one worker session and the operator watched
   * three tabs pile up on one paper. Enforced once here rather than in each
   * branch, so a new surface cannot reintroduce it. */
  private async openBrokerTab(
    url: string,
    surfaceFallback: boolean,
    onTabMaterialized?: (tabID: number) => void,
  ): Promise<number | undefined> {
    let materialized: number | undefined;
    const report = (tabID: number): void => {
      materialized = tabID;
      onTabMaterialized?.(tabID);
    };
    const surface = await this.handoffSurface();
    if (surface === "work-window") {
      let targetAdapter: AdapterSpec | undefined;
      try {
        const host = new URL(url).hostname;
        targetAdapter = this.deps.adapterSpecs.find((candidate) =>
          hostMatches(host, candidate.hosts),
        );
      } catch {
        // The browser will reject malformed handoff URLs through the normal path.
      }
      const opened = this.workTabChain.then(() =>
        this.openWorkWindowTab(url, needsVisibleWindow(targetAdapter), report),
      );
      this.workTabChain = opened.catch(() => undefined);
      try {
        return await opened;
      } catch (e) {
        console.error("papio: work-window handoff incomplete", e);
        return materialized;
      }
    }
    if (surface === "tab-group") {
      const opened = this.workTabChain.then(() =>
        this.openTabGroupTab(url, report),
      );
      this.workTabChain = opened.catch(() => undefined);
      try {
        return await opened;
      } catch (e) {
        console.error("papio: tab-group handoff incomplete", e);
        return materialized;
      }
    }
    try {
      const tabID = (
        await this.deps.tabs.create({ url, active: surfaceFallback })
      ).id;
      if (tabID !== undefined) report(tabID);
      return tabID;
    } catch (e) {
      console.error("papio: tab creation failed", e);
      return materialized;
    }
  }

  /** Route every resolver/provider open through the selected handoff surface,
   * reusing a live tracked job tab; jobless resolver opens use URL equality
   * modulo fragment. Distinct jobs may legitimately share a resolver URL.
   * The whole lookup/create sequence is serialized because Chrome can deliver
   * two inbox clicks before the first create resolves. */
  private async openManagedTab(
    options: OpenManagedTabOptions,
  ): Promise<number | undefined> {
    const queued = this.managedTabChain.then(() =>
      this.openManagedTabUnlocked(options),
    );
    this.managedTabChain = queued.then(
      () => undefined,
      () => undefined,
    );
    return queued;
  }

  private async openManagedTabUnlocked(
    options: OpenManagedTabOptions,
  ): Promise<number | undefined> {
    const job =
      options.jobId === undefined
        ? undefined
        : findByJob(this.store, options.jobId);
    if (
      options.jobId !== undefined &&
      (options.purpose === "redrive" || options.purpose === "reoffer") &&
      !this.hasDelegatedAuthority(job)
    ) {
      return undefined;
    }
    const trackedTabID =
      job !== undefined && job.tab_id >= 0 ? job.tab_id : undefined;
    let ledgerOwnedTabID: number | undefined;
    const candidates: TabInfo[] = [];
    const seen = new Set<number>();
    const addCandidate = (candidate: TabInfo): void => {
      if (candidate.id === undefined || seen.has(candidate.id)) return;
      seen.add(candidate.id);
      candidates.push(candidate);
    };
    if (trackedTabID !== undefined) {
      try {
        addCandidate(await this.deps.tabs.get(trackedTabID));
      } catch {
        // A stale persisted id is not proof that a matching tab is absent.
      }
    }
    if (options.jobId === undefined && options.purpose === "session-signin") {
      const reusedTabID = await this.findLedgeredSignInTab(options.url);
      if (reusedTabID !== undefined) {
        try {
          await this.focusManagedTab(reusedTabID);
        } catch {
          // Focus denial leaves the live sign-in tab where it already is.
        }
        return reusedTabID;
      }
    }
    if (options.jobId !== undefined) {
      const ledger = await this.snapshotTabLedger();
      const ledgerIDs = new Set(
        Object.keys(ledger)
          .map((key) => Number(key))
          .filter((tabID) => Number.isInteger(tabID) && tabID >= 0),
      );
      const ledgerTabs =
        this.deps.tabs.query === undefined
          ? await Promise.all(
              [...ledgerIDs].map(async (tabID) => {
                try {
                  return await this.deps.tabs.get(tabID);
                } catch {
                  return undefined;
                }
              }),
            ).then((tabs) =>
              tabs.filter((tab): tab is TabInfo => tab !== undefined),
            )
          : await this.deps.tabs.query({}).catch(() => []);
      // Only a tab this exact job already owns is ever a reuse candidate:
      // with no persisted URL to match a DIFFERENT job's tab against,
      // cross-job "same resolver family" reuse (the legacy heuristic) no
      // longer has anything safe to compare, so it is retired rather than
      // reduced to a coarser, privacy-neutral origin guess.
      for (const candidate of ledgerTabs) {
        if (candidate.id === undefined) continue;
        const entry = ledger[String(candidate.id)];
        if (
          entry === undefined ||
          entry.purpose === PRIVATE_SURFACE_PURPOSE ||
          // Retained content keeps its paper identity so duplicates can be
          // counted, NOT so a drive can reuse it: a redrive navigates the tab
          // it reuses, which would read the acquired paper away and leave the
          // operator with no confirmation surface at all.
          entry.content === true ||
          entry.job_id !== options.jobId
        )
          continue;
        const trackedCandidate = findByTab(this.store, candidate.id);
        if (
          trackedCandidate !== undefined &&
          trackedCandidate.job_id !== options.jobId
        )
          continue;
        ledgerOwnedTabID = candidate.id;
        addCandidate(candidate);
      }
    }
    const reusable =
      options.jobId === undefined
        ? findManagedTab(candidates, options.url)
        : trackedTabID !== undefined || ledgerOwnedTabID !== undefined
          ? findManagedTab(
              candidates,
              options.url,
              trackedTabID ?? ledgerOwnedTabID,
            )
          : undefined;
    if (reusable?.id !== undefined) {
      const shouldNavigate =
        (options.purpose === "redrive" || options.purpose === "reoffer") &&
        (reusable.url === undefined ||
          normalizeManagedTabURL(reusable.url) !==
            normalizeManagedTabURL(options.url));
      try {
        if (shouldNavigate && this.deps.tabs.update !== undefined) {
          await this.deps.tabs.update(reusable.id, { url: options.url });
        }
        if (options.focusExisting !== false) {
          await this.focusManagedTab(reusable.id, {
            ...reusable,
            ...(shouldNavigate ? { url: options.url } : {}),
          });
        }
      } catch {
        // A tab can disappear between lookup and focus; callers still retain
        // the live id and the browser removal path will recover the job.
      }
      await this.recordManagedTab(options.jobId, reusable.id);
      return reusable.id;
    }
    const tabID = await this.openBrokerTab(
      options.url,
      options.surfaceFallback ?? true,
      options.onTabMaterialized,
    );
    if (tabID === undefined) return undefined;
    await this.recordManagedTab(options.jobId, tabID);
    await this.ledgerManagedTab(
      tabID,
      options.purpose,
      options.privateLedgerURL === true,
      options.jobId,
      options.bindingID,
    );
    if (options.purpose === "session-signin") {
      try {
        await this.focusManagedTab(tabID);
      } catch {
        // Sign-in remains available in the managed surface if focus is denied.
      }
    }
    return tabID;
  }

  /** Keep the durable active-job tab id aligned whenever a managed open finds
   * or creates a tab for an already-known job. Fresh offers upsert their full
   * job record immediately after this helper returns. */
  private async recordManagedTab(
    jobID: string | undefined,
    tabID: number,
  ): Promise<void> {
    if (jobID === undefined) return;
    const job = findByJob(this.store, jobID);
    if (job === undefined || job.tab_id === tabID) return;
    await this.update((s) => patchJob(s, jobID, { tab_id: tabID }));
  }
  /** The authoritative get is followed immediately by remove in this turn.
   * A failed fresh-link materialization is the one surface exception: the
   * private one-use tab never bound to a live job, so preserving it would let
   * a sibling open a duplicate institutional login. PDF content still stays,
   * with one narrow exception: `superseded-content`, a cold duplicate copy of
   * a paper a NEWER retained surface still shows. That exemption is minted
   * only by retireSupersededContent, behind a daemon authorization, so the
   * promise this guard exists for - never close the paper someone may be
   * reading - is kept by keeping the newest copy. */
  private async closeOwnedTab(
    tabID: number,
    reason: string,
  ): Promise<boolean> {
    const entry = this.tabLedgerCache?.[String(tabID)];
    const materializationCleanup = reason === "materialization-reconcile";
    const rollbackPrivate =
      reason === "fresh-materialization-rollback" &&
      entry?.purpose === PRIVATE_SURFACE_PURPOSE;
    if (
      !materializationCleanup &&
      (entry === undefined || findByTab(this.store, tabID) !== undefined)
    )
      return false;
    // Captured before the only await below: any onActivated/onUpdated
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
    if (
      reason !== "superseded-content" &&
      tab.url !== undefined &&
      isPDFPage(tab.url)
    )
      return false;
    if (materializationCleanup) {
      const base = this.deps.runtimeGetURL?.(MATERIALIZE_PAGE_PATH);
      if (
        base === undefined ||
        tab.active === true ||
        typeof tab.url !== "string"
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
      tab.windowId !== undefined && tab.windowId === this.store.workWindowID;
    const inPapioGroup =
      tab.groupId !== undefined && tab.groupId === this.store.handoffGroupID;
    if (
      !rollbackPrivate &&
      !materializationCleanup &&
      (tab.active === true ||
        tab.pinned === true ||
        (!inWorkWindow && !inPapioGroup))
    )
      return false;
    // Final recheck immediately before remove, no intervening await: a
    // touch epoch bumped since entry — even one the fresh get above cannot
    // see because it already reverted — means the operator touched this
    // tab sometime during this close attempt. Cede rather than risk it.
    if ((this.tabTouchEpoch.get(tabID) ?? 0) !== epochAtStart) {
      await this.cedeOwnedTab(tabID, entry?.binding_id, "touched_mid_close");
      return false;
    }
    await this.deps.tabs.remove(tabID).catch(() => undefined);
    return true;
  }
  /** Activate a tab papio owns, minting the focus token first so the
   * resulting onActivated event is recognizable as papio's own act rather
   * than the operator taking the surface over. Every papio-initiated
   * activation of an owned surface MUST go through here; a bare
   * tabs.update({active:true}) is indistinguishable from a click. */
  private async focusOwnedTab(tabID: number): Promise<void> {
    this.papioFocusTokens.set(tabID, (this.papioFocusTokens.get(tabID) ?? 0) + 1);
    try {
      await this.deps.tabs.update?.(tabID, { active: true });
    } catch (e) {
      const pending = (this.papioFocusTokens.get(tabID) ?? 0) - 1;
      if (pending > 0) this.papioFocusTokens.set(tabID, pending);
      else this.papioFocusTokens.delete(tabID);
      throw e;
    }
  }

  /** True when this activation was papio's own, consuming one token. */
  private consumePapioFocusToken(tabID: number): boolean {
    const pending = this.papioFocusTokens.get(tabID) ?? 0;
    if (pending <= 0) return false;
    if (pending === 1) this.papioFocusTokens.delete(tabID);
    else this.papioFocusTokens.set(tabID, pending - 1);
    return true;
  }

  /** Detach a surface from automation without removing it: ceded permanently
   * per the retained-forever contract, its pending tombstone and job binding
   * cleared so nothing (a replay, a later reconcile pass) acts on it again.
   * A no-op when the ledger no longer has a matching record for `tabID`, or
   * when `bindingID` is given and the current record binds to a different
   * one (the numeric id was reused under a stale record).
   *
   * `reason` is durable on purpose. A cede is terminal and it erases the job
   * binding, so a record that reached this call cannot afterwards say which
   * of the five call sites decided it - and attributing one live took two
   * full observation rounds on 2026-08-26 without an answer. The value is a
   * fixed call-site name, never page-derived text. */
  private async cedeOwnedTab(
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
      delete next.job_id;
      ledger[String(tabID)] = next;
      return { value: undefined, changed: true };
    });
  }
  /** Mark a surface as retained content: papio opened it, it now shows a PDF
   * inside papio's own container, and the acquired paper is on screen.
   *
   * This is the retention half of the same decision `cedeOwnedTab` makes for
   * an operator takeover, and it deliberately differs in one field: the job
   * binding STAYS. Ceding a content surface dropped it, which took the paper
   * identity with it - so `openManagedTab` could no longer recognise the
   * retained copy, every later drive minted another one, and each new copy
   * was retained in turn. Retention is meant to be one confirmation surface
   * per paper; without the identity it cannot count to one. The pending
   * tombstone is cleared for the same reason ceding clears it: a close the
   * operator's own content has overtaken must never be replayed.
   *
   * A no-op when the record is gone, already ceded, or binds elsewhere (a
   * recycled tab id under a stale record). */
  private async retainContentSurface(
    tabID: number,
    bindingID: string | undefined,
  ): Promise<void> {
    await this.runTabLedgerTransaction((ledger) => {
      const current = ledger[String(tabID)];
      if (
        current === undefined ||
        current.ceded === true ||
        (bindingID !== undefined && current.binding_id !== bindingID)
      )
        return { value: undefined, changed: false };
      if (current.content === true && current.pending_close === undefined)
        return { value: undefined, changed: false };
      const next: SurfaceBirthRecord = { ...current, content: true };
      delete next.pending_close;
      ledger[String(tabID)] = next;
      return { value: undefined, changed: true };
    });
  }
  private async saveTabLedger(
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<void> {
    const snapshot = { ...ledger };
    try {
      await this.deps.tabLedger?.save(snapshot);
    } catch {
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
  ): Promise<T> {
    const operation = this.tabLedgerChain.then(async () => {
      let cached = this.tabLedgerCache;
      if (cached === undefined) {
        let raw: unknown = {};
        try {
          raw = (await this.deps.tabLedger?.load()) ?? {};
        } catch {
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
        await this.saveTabLedger(this.tabLedgerCache);
      }

      const ledger = { ...cached };
      const result = await transaction(ledger);
      this.tabLedgerCache = { ...ledger };
      if (result.changed) await this.saveTabLedger(this.tabLedgerCache);
      return result.value;
    });
    this.tabLedgerChain = operation.then(
      () => undefined,
      () => undefined,
    );
    return operation;
  }
  private async snapshotTabLedger(): Promise<
    Record<string, SurfaceBirthRecord>
  > {
    if (this.deps.tabLedger === undefined) return {};
    return this.runTabLedgerTransaction((ledger) => ({
      value: { ...ledger },
      changed: false,
    }));
  }

  /** Record a broker tab papio CREATED as a URL-free birth certificate
   * (Slice 2b). Reused tabs are deliberately never ledgered: a URL-matched
   * reuse can be the user's own tab, and the ledger exists to authorize
   * closing — papio must never earn that authority over a tab it did not
   * open. `privateSurface` marks a one-use federated-login mint, isolated
   * from cross-job reuse the same way the legacy `privateLedgerURL` flag
   * isolated it. */
  private async ledgerManagedTab(
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

  /** The durable mirror of a live worker-memory claim grant. Undefined when
   * this job holds no grant, or when no holder generation has been observed
   * yet — an observation carrying a guessed generation is worse than one
   * that is never sent, because the daemon would apply it under the wrong
   * holder. */
  private durableClaimIdentity(
    jobID: string,
  ): SurfaceBirthRecord["claim"] | undefined {
    const grant = this.claimGrants.get(jobID);
    if (grant === undefined) return undefined;
    const generation = this.lastKnownBrowserHolderGeneration;
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
  private async persistClaimIdentity(
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

  private async forgetLedgeredTab(tabID: number): Promise<void> {
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
  private async findLedgeredSignInTab(url: string): Promise<number | undefined> {
    const requestedDigest = await originDigestOf(url);
    if (requestedDigest === undefined) return undefined;
    const ledger = await this.snapshotTabLedger();
    for (const key of Object.keys(ledger)) {
      const tabID = Number(key);
      if (!Number.isInteger(tabID) || tabID < 0) continue;
      const entry = ledger[key];
      if (entry === undefined || entry.origin_digest !== requestedDigest)
        continue;
      if (findByTab(this.store, tabID) !== undefined) continue;
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
    await this.ready;
    await this.surfaceReady;
    if (this.deps.tabLedger === undefined) return { auto: [], ask: [] };
    const tracked = new Set<number>();
    for (const job of this.store.activeJobs)
      if (job.tab_id >= 0) tracked.add(job.tab_id);
    for (const id of this.completedDownloadTabs.values()) tracked.add(id);
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
            this.enqueueRestartRecoveredObservation(
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
          tab.windowId === this.store.workWindowID;
        if (!ownedSurface && tab.groupId !== undefined && tab.groupId >= 0) {
          ownedSurface =
            (await this.knownHandoffGroup(tab.groupId, tab.windowId)) !==
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

  /** How often the keepalive wake may run the surface-repair pass. One pass is
   * a ledger walk plus a tabs.get per record, so it is rate-limited rather
   * than run on every one-minute wake; the interval only has to be short
   * relative to PARKED_SURFACE_COLD_MS and to the 3-minute drive timeout
   * whose refused closes it repairs. */
  private ownedTabReconcileDueAt = 0;

  /** Rate-limited reconcileOwnedTabs for the keepalive wake. The stamp is
   * advanced BEFORE the walk so two overlapping wakes cannot both run it; a
   * failed pass simply waits for the next interval, exactly as a failed
   * challenge recheck does. */
  private async reconcileOwnedTabsIfDue(): Promise<void> {
    const now = this.deps.now();
    if (now < this.ownedTabReconcileDueAt) return;
    this.ownedTabReconcileDueAt = now + OWNED_TAB_RECONCILE_INTERVAL_MS;
    await this.reconcileOwnedTabs();
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
   * tabs remain review-only. A surface the operator made active/pinned, opened
   * as a PDF, or moved out of papio's container is ceded, never closed. */
  async reconcileOwnedTabs(): Promise<{ closed: number }> {
    await this.classifyLedgeredTabs();
    const ledger = await this.snapshotTabLedger();
    let closed = 0;
    for (const [key, entry] of Object.entries(ledger)) {
      const tabID = Number(key);
      const owner =
        entry.job_id === undefined
          ? undefined
          : findByJob(this.store, entry.job_id);
      if (
        !Number.isInteger(tabID) ||
        tabID < 0 ||
        entry.ceded === true ||
        // Retained content is decided by the content pass below, and asserting
        // job_inactive for it would be a request to close the operator's
        // acquired paper. The PDF guards downstream refuse that anyway; not
        // asking spares a daemon round trip on every pass.
        entry.content === true ||
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
          !(owner.parked_with_tab === true && this.surfaceIsCold(entry)))
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
        tab.windowId !== undefined && tab.windowId === this.store.workWindowID;
      const inPapioGroup =
        tab.groupId !== undefined &&
        tab.groupId >= 0 &&
        (await this.knownHandoffGroup(tab.groupId, tab.windowId)) !== undefined;
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
      if (tab.url !== undefined && isPDFPage(tab.url)) {
        // Content papio must never auto-close, still inside papio's own
        // container: retained rather than ceded, so the paper identity
        // survives and the pass below can tell a second copy of THIS paper
        // from the one confirmation surface retention promises.
        await this.retainContentSurface(tabID, entry.binding_id);
        continue;
      }
      if (tab.active === true) {
        // Ambiguity retains, it does not cede (plan: "never for
        // engaged/active/PDF/adopted content. Unknown engagement => retain").
        // papio focuses its own surfaces - explicit Open, a work-window
        // raise - so treating active as takeover ceded papio's own tabs the
        // instant it opened them, permanently, and the record could never be
        // retired again. Operator activation is recorded where it happens,
        // in onTabActivated, against a papio-issued focus token.
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
        await this.update((s) => patchJob(s, owner.job_id, { tab_id: -1 }));
      }
      const result = await this.closeOwnedSurface(
        tabID,
        owner === undefined ? "job_inactive" : "handoff_parked",
      );
      if (result.closed) closed += 1;
    }
    closed += await this.retireSupersededContent();
    return { closed };
  }

  /** Retire cold, superseded copies of a paper papio already retains.
   *
   * Retention of content is deliberate and stays deliberate: one visible tab
   * showing an acquired paper is confirmation, not litter. The newest copy is
   * always the one kept, so the paper never leaves the operator's screen.
   * What this removes is the second, third and fourteenth copy of the SAME
   * paper, minted by successive drives of one job that each ended on the same
   * PDF - measured live 2026-08-26 at fourteen tabs for one paper.
   *
   * Every predicate is positive evidence, never age or title. papio created
   * the surface (birth record), for THIS paper (`job_id`), at the same origin
   * (`origin_digest`), in this browser session (`browser_epoch`), a newer copy
   * of the same pair exists, the surface is still content inside papio's own
   * container, the operator has not made it active or pinned it, and it has
   * outlived PARKED_SURFACE_COLD_MS. The daemon then decides independently:
   * `claim_abandoned` is eligible only when the binding's claim really is
   * abandoned, and a binding with no claim at all answers `unclaimed`, which
   * is browser-local by contract. A live or settled claim refuses, and the
   * copy is retained. */
  private async retireSupersededContent(): Promise<number> {
    const ledger = await this.snapshotTabLedger();
    const groups = new Map<
      string,
      { tabID: number; entry: SurfaceBirthRecord }[]
    >();
    for (const [key, entry] of Object.entries(ledger)) {
      const tabID = Number(key);
      if (
        !Number.isInteger(tabID) ||
        tabID < 0 ||
        entry.content !== true ||
        entry.ceded === true ||
        entry.job_id === undefined ||
        entry.browser_epoch !== this.browserEpoch
      )
        continue;
      // The paper is the whole grouping key. Only content records reach here,
      // and every content record is a PDF surface, so two records under one
      // job are two copies of one acquired paper - even when they were born at
      // different origins (a provider page and a CDN asset host digest
      // differently, and the birth digest is never re-dated). Adding the
      // digest to the key therefore only ever splits a real duplicate pair.
      const bucket = groups.get(entry.job_id);
      if (bucket === undefined) groups.set(entry.job_id, [{ tabID, entry }]);
      else bucket.push({ tabID, entry });
    }
    let closed = 0;
    for (const bucket of groups.values()) {
      if (bucket.length < 2) continue;
      bucket.sort((a, b) => b.entry.created_at - a.entry.created_at);
      for (const { tabID, entry } of bucket.slice(1)) {
        if (!this.surfaceIsCold(entry)) continue;
        let tab: TabInfo;
        try {
          tab = await this.deps.tabs.get(tabID);
        } catch {
          continue;
        }
        const inWorkWindow =
          tab.windowId !== undefined && tab.windowId === this.store.workWindowID;
        const inPapioGroup =
          tab.groupId !== undefined &&
          tab.groupId >= 0 &&
          (await this.knownHandoffGroup(tab.groupId, tab.windowId)) !==
            undefined;
        if (
          tab.active === true ||
          tab.pinned === true ||
          tab.url === undefined ||
          !isPDFPage(tab.url) ||
          (!inWorkWindow && !inPapioGroup)
        )
          continue;
        // `surface_superseded` is the assertion that is actually true here,
        // and the one the daemon can verify: this paper is driven from another
        // surface now. `claim_abandoned` was tried first and is false in the
        // common case - a re-drive mints a new claim and leaves the previous
        // one in `navigated` until the next holder promotion sweeps it, so
        // every ask was refused and the duplicates stayed (measured live
        // 2026-08-26).
        const result = await this.closeOwnedSurface(
          tabID,
          "surface_superseded",
          undefined,
          true,
        );
        if (result.closed) closed += 1;
      }
    }
    return closed;
  }

  /** Operator-initiated review focuses one bounded orphan surface; the
   * operator closes the reviewed tab through browser UI. */
  async cleanupOrphanTabs(): Promise<{ closed: number; focused: number }> {
    const { tab_ids } = await this.orphanTabStatus();
    const tabID = tab_ids[0];
    if (tabID === undefined) return { closed: 0, focused: 0 };
    try {
      await this.focusManagedTab(tabID);
      return { closed: 0, focused: 1 };
    } catch {
      return { closed: 0, focused: 0 };
    }
  }

  private async inLifecycleChain<T>(work: () => Promise<T>): Promise<T> {
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
  private async classifyRestart(): Promise<"worker" | "update" | "browser"> {
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
  private async ownedMemberTab(
    tabID: number,
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<TabInfo | undefined> {
    const record = ledger[String(tabID)];
    if (record === undefined || record.browser_epoch !== this.browserEpoch)
      return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      return tab.id === tabID ? tab : undefined;
    } catch {
      return undefined;
    }
  }

  private async groupHasOwnedMember(
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
  private async adoptWorkWindowFromOwnedMembers(
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
    const current = this.store.workWindowID;
    if (current !== undefined && owned.some((tab) => tab.windowId === current))
      return;
    const adopted = owned.find((tab) => tab.windowId !== undefined)?.windowId;
    if (adopted !== current) {
      await this.update((s) => {
        const next = { ...s };
        if (adopted === undefined) delete next.workWindowID;
        else next.workWindowID = adopted;
        return next;
      });
    }
  }

  /** Reconciles the persisted tab ledger, papio-owned groups/windows, and
   * any close tombstone left by a prior worker lifetime, under the
   * lifecycle mutex — never the effect governor. A bounded-scan failure
   * fails closed to no-adoption/no-close rather than throwing into a
   * crashed barrier (surfaceReady itself catches this).
   *
   * The replayPendingCloseTombstones call below is best-effort local
   * hydration only: on a fresh worker lastKnownBrowserHolderGeneration is
   * unset until rehydrateBrowserHolderGenerationFromMaterializations (just
   * above it) finds a cached one or the daemon supplies one, and this whole
   * pass can simply lose the race against hello_ack. It never strands a
   * tombstone permanently: scheduleCloseTombstoneReplay (called from the
   * hello_ack handler, after reconnect, and after role promotion) re-runs
   * the same idempotent transaction once the daemon has actually told this
   * session a live generation. */
  private async bootstrapSurfaceLifecycle(): Promise<void> {
    await this.inLifecycleChain(async () => {
      this.restartClass = await this.classifyRestart();
      await this.hydrateClaimObservationOutbox();
      const ledger = await this.snapshotTabLedger();
      await this.reconcileHandoffGroups();
      await this.adoptWorkWindowFromOwnedMembers(ledger);
      this.rehydrateBrowserHolderGenerationFromMaterializations();
      await this.replayPendingCloseTombstones();
      await this.reconcileNavigationErrorMarkers();
    });
    // §4.5 corrective: a restarted worker must replay any outstanding
    // claim_observation before any lease-renewing action runs, but
    // draining it INSIDE the barrier above deadlocked the inbound FIFO —
    // every caller of consultAuthenticationClaim/emitClaimObservation
    // awaits surfaceReady, and so does onJobOffer for unrelated reasons;
    // a job_offer delivered after hello_ack but before the replay's own
    // correlated ack would block behind this barrier while that ack queues
    // behind the job_offer on the SAME serialized chain. Schedule the drain
    // after the barrier resolves instead — surfaceReady is free immediately,
    // and outboxReplayed (awaited only by lease-renewing observation emission,
    // never job offers or reads) tracks the replay separately.
    this.scheduleObservationOutboxDrain();
  }

  /** Load the durable claim_observation outbox (chrome.storage.session) into
   * worker memory. Defensive: a foreign write, a stale schema, or a
   * hand-edited value is dropped rather than trusted, matching the other
   * `deps.*Correlations`-style hydration paths in this file. */
  private async hydrateClaimObservationOutbox(): Promise<void> {
    const outbox = this.deps.claimObservationOutbox;
    if (outbox === undefined) return;
    let stored: unknown;
    try {
      stored = await outbox.get();
    } catch {
      return;
    }
    if (!isClaimObservationOutboxRecord(stored)) return;
    for (const [observationID, entry] of Object.entries(stored)) {
      if (!isClaimObservationOutboxEntry(observationID, entry)) continue;
      this.claimObservationOutboxEntries.set(observationID, entry);
    }
  }

  /** Restart-safety half of oracle finding 5: hydrate every durable
   * navigation-error marker onNavigationError persisted, then resolve each
   * one against the tab's CURRENT live state — no onUpdated event is
   * guaranteed to ever arrive for a tab whose document already finished
   * settling while this worker was dead, so waiting for one can strand the
   * marker forever. A marker whose tab is gone or still shows an
   * unsuccessful/auth-wall document is promoted straight into the durable
   * claim_observation outbox — bypassing worker-memory claimGrants, which
   * does NOT survive a restart — so the daemon-visible auth-attempt count
   * never depends on this worker's memory surviving. A marker whose tab
   * has since landed successfully on its own is simply discarded. Either
   * way navigationErrors itself is restored first, so the generic
   * auth-wall detector's exclusion still applies to any LATE onUpdated
   * event Chrome does still deliver for that tab. */
  private async reconcileNavigationErrorMarkers(): Promise<void> {
    if (this.deps.navigationErrorMarkers === undefined) return;
    let stored: Record<string, NavigationErrorMarkerEntry>;
    try {
      stored = await this.deps.navigationErrorMarkers.get();
    } catch {
      return;
    }
    const retained: [number, NavigationErrorMarkerEntry][] = [];
    for (const [key, entry] of Object.entries(stored)) {
      if (
        typeof entry !== "object" ||
        entry === null ||
        typeof entry.tab_id !== "number" ||
        typeof entry.binding_id !== "string" ||
        typeof entry.at !== "number" ||
        String(entry.tab_id) !== key
      ) {
        continue;
      }
      let stillUnsuccessful = true;
      try {
        const tab = await this.deps.tabs.get(entry.tab_id);
        stillUnsuccessful =
          typeof tab.url !== "string" || isAuthenticationURL(tab.url);
      } catch {
        // Tab gone while this worker was dead: no further event will ever
        // arrive for it. Keep the conservative default so a genuine dead
        // end still reaches the daemon instead of silently leaking.
      }
      if (!stillUnsuccessful) continue; // Recovered on its own; discard.
      this.navigationErrors.set(entry.tab_id, entry.at);
      if (
        typeof entry.job_id === "string" &&
        typeof entry.authentication_claim_id === "string" &&
        typeof entry.gate_occurrence_id === "string" &&
        typeof entry.browser_holder_generation === "number"
      ) {
        this.enqueueRestartRecoveredObservation(
          entry as Required<NavigationErrorMarkerEntry>,
          "navigation_error",
        );
      } else {
        // Occurrence unknown at error time: nothing here can synthesize
        // one. Keep it queued for the normal document-settle path once a
        // fresh consult (if any) re-establishes a grant for this job.
        retained.push([entry.tab_id, entry]);
      }
    }
    this.navigationErrorMarkerEntries.clear();
    for (const [tabID, entry] of retained) {
      this.navigationErrorMarkerEntries.set(tabID, entry);
    }
    this.persistNavigationErrorMarkers();
  }

  /** Fold a fully-identified durable observation directly into the
   * claim_observation outbox, mirroring enqueueClaimObservation but without
   * needing a live claimGrants entry (gone on restart). Ordinal is computed
   * the same way consultAuthenticationClaim seeds a fresh grant's own
   * counter: the lowest value strictly greater than every already-queued
   * ordinal for this occurrence. */
  private enqueueRestartRecoveredObservation(
    entry: {
      job_id: string;
      authentication_claim_id: string;
      binding_id: string;
      browser_holder_generation: number;
      gate_occurrence_id: string;
    },
    eventKind: Extract<
      ClaimObservationPayload["event_kind"],
      "navigation_error" | "owner_closed"
    >,
  ): void {
    let ordinal = 0;
    for (const existing of this.claimObservationOutboxEntries.values()) {
      if (
        existing.gate_occurrence_id === entry.gate_occurrence_id &&
        existing.event_ordinal >= ordinal
      ) {
        ordinal = existing.event_ordinal + 1;
      }
    }
    const observationEntry: ClaimObservationOutboxEntry = {
      observation_id: this.deps.randomUUID(),
      job_id: entry.job_id,
      authentication_claim_id: entry.authentication_claim_id,
      binding_id: entry.binding_id,
      browser_holder_generation: entry.browser_holder_generation,
      gate_occurrence_id: entry.gate_occurrence_id,
      event_ordinal: ordinal,
      event_kind: eventKind,
    };
    this.claimObservationOutboxEntries.set(
      observationEntry.observation_id,
      observationEntry,
    );
    this.persistClaimObservationOutbox();
    // Same tail as enqueueClaimObservation: persisting alone leaves the
    // observation sitting until some unrelated event happens to drain it,
    // and owner_closed is the frame that frees the institution's login slot.
    this.scheduleObservationOutboxDrain();
  }

  /** The generic Slice 2b close transaction: request a one-use daemon
   * authorization for the tab's binding, persist its tombstone before
   * touching the tab, then re-verify liveness before running the removal
   * through the existing closeOwnedTab primitive. Positive evidence closes;
   * every other outcome retains the surface. */
  private async closeOwnedSurface(
    tabID: number,
    disposition: SurfaceCloseDisposition,
    gateOccurrenceID?: string,
    // Retained content is never closable by default: the two downstream PDF
    // guards refuse it, which is the standing promise never to close a paper
    // someone may be reading. retireSupersededContent is the one caller that
    // may set this, and only for a copy a NEWER retained copy of the same
    // paper supersedes, so the paper itself stays on screen either way.
    supersededContent = false,
  ): Promise<{ closed: boolean }> {
    await this.surfaceReady;
    const ledger = await this.snapshotTabLedger();
    const record = ledger[String(tabID)];
    if (record === undefined || record.ceded === true)
      return { closed: false };
    if (supersededContent && record.content !== true) return { closed: false };
    return this.inLifecycleChain(() =>
      this.closeAuthorizedRecord(
        tabID,
        record,
        disposition,
        gateOccurrenceID,
        supersededContent,
      ),
    );
  }

  /** §2.3: request a one-use close authorization for `bindingID` under
   * `disposition`. Shared by closeAuthorizedRecord's tab-tombstone dance
   * and the owner_closed reducer counterpart (a tab already gone has
   * nothing to tombstone, only the daemon to inform). */
  private async requestCloseAuthorization(
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
    const generation = this.lastKnownBrowserHolderGeneration;
    if (generation === undefined) return { authorized: false };
    if (disposition === "surface_superseded" && surfaceTabID === undefined)
      return { authorized: false };
    const result = await this.requestNative(
      "surface_close_request",
      {
        binding_id: bindingID,
        browser_holder_generation: generation,
        disposition,
        ...(disposition === "claim_abandoned" && gateOccurrenceID !== undefined
          ? { gate_occurrence_id: gateOccurrenceID }
          : {}),
        ...(disposition === "surface_superseded" && surfaceTabID !== undefined
          ? { surface_tab_id: surfaceTabID }
          : {}),
      },
      "surface_close_response",
      SURFACE_CLOSE_FEATURE,
      true,
    );
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
    this.lastKnownBrowserHolderGeneration = responseGeneration;
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
    supersededContent = false,
  ): Promise<{ closed: boolean }> {
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
        return this.retireOwnedSurface(
          tabID,
          record.binding_id,
          "unclaimed",
          supersededContent,
        );
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
    return this.retireOwnedSurface(
      tabID,
      bindingID,
      "authorized",
      supersededContent,
    );
  }

  /** The one fresh tabs.get the plan requires before the awaited removal,
   * shared by both close authorities: a daemon-authorized claim surface
   * consuming its tombstone, and an unclaimed ordinary handoff surface the
   * daemon has no stake in. The guards are identical because they are the
   * whole of the decision in the unclaimed case, so they must never diverge.
   *
   * A tab that became active, pinned, or navigated to content papio must
   * never auto-close (a PDF viewer — closeOwnedTab's own group/window gate
   * covers "not adopted-content") is ceded and detached instead of
   * retried: its tombstone is cleared so a later restart never re-requests
   * a closure the operator has already claimed by using the tab. This is
   * the FIRST of two independent freshness checks, not the only one —
   * closeOwnedTab below re-derives the same predicates off its own fresh
   * get, and additionally compares the touch epoch, so a touch landing
   * anywhere between this get and the eventual tabs.remove is still caught
   * even when it is invisible to this particular snapshot. */
  private async retireOwnedSurface(
    tabID: number,
    bindingID: string,
    authority: "authorized" | "unclaimed",
    // A superseded duplicate is content by construction, so the PDF predicate
    // here would cede every one of them. The exemption is narrow on purpose:
    // only a copy whose paper is retained on a NEWER surface reaches this, and
    // pinning still cedes, because pinning is an operator act on this tab.
    supersededContent = false,
  ): Promise<{ closed: boolean }> {
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
    if (
      !supersededContent &&
      tab.url !== undefined &&
      isPDFPage(tab.url)
    ) {
      // Content, which papio never auto-closes - but retaining it is not the
      // same act as ceding it, and conflating the two is what made retention
      // per-attempt. This is the close path a settled or abandoned
      // MATERIALIZATION surface takes, and it runs before any reconcile pass
      // sees the tab, so ceding here stripped the paper identity from every
      // scaffold that ended on a PDF - which is every copy measured live on
      // 2026-08-26, all of them `purpose: "materialization"`.
      await this.retainContentSurface(tabID, bindingID);
      return { closed: false };
    }
    if (tab.active === true) {
      // Retain, do not cede: papio's own focus is not takeover, and this
      // tombstone stays replayable so the surface retires once it is no
      // longer the foreground tab. Ceding here burned the one-use
      // authorization AND detached the binding, which is how an explicitly
      // opened sign-in tab became permanently unretirable (live 2026-08-20).
      return { closed: false };
    }
    const removed = await this.closeOwnedTab(
      tabID,
      supersededContent
        ? "superseded-content"
        : authority === "unclaimed"
          ? "unclaimed-close"
          : "authorized-close",
    );
    return { closed: removed };
  }

  /** Startup replay: a failed remove or a worker death between tombstone
   * persistence and removal leaves the tombstone in place. Re-requesting
   * the same binding's authorization is idempotent daemon-side (the same
   * live token comes back), so replay is just re-running the transaction
   * from its authorization step. */
  private async replayPendingCloseTombstones(): Promise<void> {
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

  /** Best-effort local guess for lastKnownBrowserHolderGeneration, scanned
   * from whatever browser_holder_generation values this worker's persisted
   * materializations still carry (the newest — highest — wins). Called from
   * bootstrapSurfaceLifecycle before its local tombstone-replay pass so that
   * pass has the best chance of an immediate authorization; a value found
   * here can still be stale (a holder transition since it was recorded) and
   * the daemon's own generation fence in surface_close_request rejects a
   * stale guess as harmlessly as an absent one. */
  private rehydrateBrowserHolderGenerationFromMaterializations(): void {
    let rehydrated: number | undefined;
    for (const entry of Object.values(this.store.materializations ?? {})) {
      if (typeof entry.browser_holder_generation !== "number") continue;
      if (rehydrated === undefined || entry.browser_holder_generation > rehydrated)
        rehydrated = entry.browser_holder_generation;
    }
    if (rehydrated !== undefined)
      this.lastKnownBrowserHolderGeneration = rehydrated;
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
  private scheduleCloseTombstoneReplay(): void {
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

  private handoffNeedsHumanNow(): boolean {
    return this.store.activeJobs.some(
      (job) => job.status === "auth_pending" || job.challenge_blocked === true,
    );
  }

  /** Reduce all human-attention signals to one papio-group state. Updates are
   * trailing-edge debounced so an auth redirect storm cannot thrash expand /
   * collapse; the first transition is immediate and later transitions are
   * limited to one browser update per five seconds. */
  private async reduceHandoffGroupState(tabID?: number): Promise<void> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return;
    const desiredExpanded = this.handoffNeedsHumanNow();
    this.handoffGroupDesiredExpanded = desiredExpanded;
    const groupID =
      tabID === undefined
        ? this.store.handoffGroupID
        : await this.handoffGroupIDForTab(tabID);
    if (groupID === undefined) return;
    let current: TabGroupInfo;
    try {
      current = await tabGroups.get(groupID);
    } catch {
      return;
    }
    const desiredCollapsed = !desiredExpanded;
    if (current.collapsed === desiredCollapsed) return;
    const elapsed =
      this.handoffGroupLastStateChangeAt === undefined
        ? HANDOFF_DRIVE_TIMEOUT_MS
        : this.deps.now() - this.handoffGroupLastStateChangeAt;
    if (elapsed < 5_000) {
      if (this.handoffGroupUpdateToken !== undefined) return;
      const token = {};
      this.handoffGroupUpdateToken = token;
      this.deps.setTimeout(
        async () => {
          if (this.handoffGroupUpdateToken !== token) return;
          this.handoffGroupUpdateToken = undefined;
          await this.reduceHandoffGroupState(tabID);
        },
        5_000 - Math.max(0, elapsed),
      );
      return;
    }
    try {
      await tabGroups.update(groupID, {
        title: HANDOFF_GROUP_TITLE,
        collapsed: desiredCollapsed,
      });
      this.handoffGroupLastStateChangeAt = this.deps.now();
    } catch {
      // A vanished group is recreated by the next managed handoff.
    }
  }

  /** Focus a managed tab and, when available, its papio group and containing
   * window. This is intentionally best-effort: focusing is operator UX, not a
   * prerequisite for the daemon handoff. */
  private async focusManagedTab(
    tabID: number,
    knownTab?: TabInfo,
  ): Promise<void> {
    const tab = knownTab ?? (await this.deps.tabs.get(tabID));
    await this.reduceHandoffGroupState(tabID);
    await this.focusOwnedTab(tabID);
    if (tab.windowId !== undefined && this.deps.windows !== undefined) {
      try {
        const win = await this.deps.windows.get(tab.windowId);
        await this.deps.windows.update(tab.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // A closed work window is handled by the normal tab-removal path.
      }
    }
  }
  /** The group is papio's tab-strip tidiness and its post-restart rediscovery
   * aid; it is NOT the drive, and the durable identity is the daemon's
   * SurfaceBirthRecord. A refusal here still rejects, deliberately: the caller
   * `openBrokerTab` owns the "never discard a reported tab" invariant, so the
   * disposition lives in one place rather than being re-decided per surface. */
  private async openTabGroupTab(
    url: string,
    onTabMaterialized?: (tabID: number) => void,
  ): Promise<number | undefined> {
    const tab = await this.deps.tabs.create({ url, active: false });
    if (tab.id === undefined) return undefined;
    onTabMaterialized?.(tab.id);
    await this.foldIntoHandoffGroup(tab.id, tab.windowId);
    return tab.id;
  }

  /** Queue every create-or-adopt decision because keepalive placement can race
   * broker-tab creation outside the work-tab chain. */
  private async inHandoffGroupChain<T>(work: () => Promise<T>): Promise<T> {
    const queued = this.handoffGroupChain.then(work);
    this.handoffGroupChain = queued.then(
      () => undefined,
      () => undefined,
    );
    return queued;
  }

  private async windowIDForTab(
    tabID: number,
    knownWindowID?: number,
  ): Promise<number | undefined> {
    if (knownWindowID !== undefined) return knownWindowID;
    try {
      return (await this.deps.tabs.get(tabID)).windowId;
    } catch {
      return undefined;
    }
  }

  private async knownHandoffGroup(
    groupID: number,
    windowID: number | undefined,
  ): Promise<TabGroupInfo | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      const found = await tabGroups.get(groupID);
      return isHandoffGroupTitle(found.title) &&
        (windowID === undefined || found.windowId === windowID)
        ? found
        : undefined;
    } catch {
      return undefined;
    }
  }

  private async findHandoffGroups(
    windowID?: number,
  ): Promise<TabGroupInfo[] | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      return (await tabGroups.query({})).filter(
        (candidate) =>
          isHandoffGroupTitle(candidate.title) &&
          (windowID === undefined || candidate.windowId === windowID),
      );
    } catch {
      return undefined;
    }
  }

  /** Never trusts a papio-titled group by title alone (surface-lifecycle
   * plan, Slice 2b): a candidate is preferred only once it is confirmed to
   * contain at least one positively owned member tab. A group with no
   * owned member is never adopted, merged, or closed. */
  private async preferredHandoffGroup(
    candidates: TabGroupInfo[],
    windowID: number | undefined,
    ledger: Record<string, SurfaceBirthRecord>,
  ): Promise<TabGroupInfo | undefined> {
    const remembered =
      windowID === undefined
        ? undefined
        : this.handoffGroupIDsByWindow.get(windowID);
    const ordered: TabGroupInfo[] = [];
    const seen = new Set<number>();
    const push = (candidate: TabGroupInfo | undefined): void => {
      if (candidate === undefined || seen.has(candidate.id)) return;
      seen.add(candidate.id);
      ordered.push(candidate);
    };
    push(candidates.find((candidate) => candidate.id === remembered));
    push(
      candidates.find(
        (candidate) => candidate.id === this.store.handoffGroupID,
      ),
    );
    push(candidates.find((candidate) => candidate.collapsed === false));
    for (const candidate of candidates) push(candidate);
    for (const candidate of ordered) {
      if (await this.groupHasOwnedMember(candidate, ledger)) return candidate;
    }
    return undefined;
  }

  /** Merge legacy duplicates before another tab is added, so adoption repairs
   * old reload races instead of merely avoiding the next one. */
  private async foldDuplicateHandoffGroups(
    primary: TabGroupInfo,
    candidates: TabGroupInfo[],
    windowID: number | undefined,
  ): Promise<void> {
    const tabs = this.deps.tabs;
    if (tabs.group === undefined || tabs.query === undefined) return;
    for (const duplicate of candidates) {
      if (duplicate.id === primary.id) continue;
      try {
        const tabIDs = (await tabs.query({ groupId: duplicate.id }))
          .filter(
            (tab) =>
              tab.id !== undefined &&
              (windowID === undefined || tab.windowId === windowID),
          )
          .map((tab) => tab.id!);
        if (tabIDs.length > 0)
          await tabs.group({ tabIds: tabIDs, groupId: primary.id });
      } catch {
        // A user can close a tab or group while startup is repairing it; the
        // remaining groups are still safe to reconcile.
      }
    }
  }

  private async rememberHandoffGroup(
    groupID: number,
    windowID: number | undefined,
  ): Promise<void> {
    if (windowID !== undefined)
      this.handoffGroupIDsByWindow.set(windowID, groupID);
    if (this.store.handoffGroupID === groupID) return;
    await this.update((s) => ({ ...s, handoffGroupID: groupID }));
  }

  /** Add a tab to the collapsed "papio" group, reusing the group across
   * handoffs (and the keepalive tab) or creating it collapsed on first use.
   * No-op when the platform lacks tab grouping. */
  private async foldIntoHandoffGroup(
    tabID: number,
    knownWindowID?: number,
  ): Promise<void> {
    await this.inHandoffGroupChain(() =>
      this.foldIntoHandoffGroupUnlocked(tabID, knownWindowID),
    );
  }

  private async foldIntoHandoffGroupUnlocked(
    tabID: number,
    knownWindowID?: number,
  ): Promise<void> {
    const tabs = this.deps.tabs;
    const tabGroups = this.deps.tabGroups;
    if (tabs.group === undefined) return;
    const windowID = await this.windowIDForTab(tabID, knownWindowID);
    const remembered =
      windowID === undefined
        ? undefined
        : this.handoffGroupIDsByWindow.get(windowID);
    let reuse =
      remembered === undefined
        ? undefined
        : await this.knownHandoffGroup(remembered, windowID);
    if (reuse === undefined && this.store.handoffGroupID !== undefined) {
      reuse = await this.knownHandoffGroup(this.store.handoffGroupID, windowID);
    }
    const found = await this.findHandoffGroups(windowID);
    if (found !== undefined) {
      const ledger = await this.snapshotTabLedger();
      reuse = (await this.preferredHandoffGroup(found, windowID, ledger)) ?? reuse;
    }
    if (reuse !== undefined) {
      if (found !== undefined)
        await this.foldDuplicateHandoffGroups(reuse, found, windowID);
      await tabs.group({ tabIds: [tabID], groupId: reuse.id });
      await this.rememberHandoffGroup(reuse.id, windowID);
      return;
    }
    const groupID = await tabs.group({
      tabIds: [tabID],
      ...(windowID === undefined ? {} : { createProperties: { windowId: windowID } }),
    });
    if (tabGroups !== undefined) {
      try {
        await tabGroups.update(groupID, {
          title: HANDOFF_GROUP_TITLE,
          collapsed: true,
          color: "orange",
        });
      } catch {
        // A grouped tab remains usable even if the browser declines its display update.
      }
    }
    await this.rememberHandoffGroup(groupID, windowID);
  }

  private async handoffGroupWindowID(
    group: TabGroupInfo,
  ): Promise<number | undefined> {
    if (group.windowId !== undefined) return group.windowId;
    const tabs = this.deps.tabs;
    if (tabs.query === undefined) return undefined;
    try {
      return (await tabs.query({ groupId: group.id })).find(
        (tab) => tab.windowId !== undefined,
      )?.windowId;
    } catch {
      return undefined;
    }
  }

  /** Recover all groups left by prior worker lifetimes before a new fold can
   * multiply them again. */
  private async reconcileHandoffGroups(): Promise<void> {
    await this.inHandoffGroupChain(() => this.reconcileHandoffGroupsUnlocked());
  }

  private async reconcileHandoffGroupsUnlocked(): Promise<void> {
    const candidates = await this.findHandoffGroups();
    if (candidates === undefined || candidates.length === 0) return;
    const ledger = await this.snapshotTabLedger();
    const byWindow = new Map<number, TabGroupInfo[]>();
    for (const candidate of candidates) {
      const windowID = await this.handoffGroupWindowID(candidate);
      if (windowID === undefined) continue;
      const groups = byWindow.get(windowID);
      if (groups === undefined) {
        byWindow.set(windowID, [candidate]);
      } else {
        groups.push(candidate);
      }
    }
    const selected: { group: TabGroupInfo; windowID: number }[] = [];
    for (const [windowID, groups] of byWindow) {
      const primary = await this.preferredHandoffGroup(groups, windowID, ledger);
      if (primary === undefined) continue;
      await this.foldDuplicateHandoffGroups(primary, groups, windowID);
      this.handoffGroupIDsByWindow.set(windowID, primary.id);
      selected.push({ group: primary, windowID });
    }
    const persisted =
      selected.find(
        (candidate) => candidate.group.id === this.store.handoffGroupID,
      ) ?? selected[0];
    if (persisted !== undefined) {
      await this.rememberHandoffGroup(persisted.group.id, persisted.windowID);
    }
  }

  /** Fold the keepalive resolver tab into the "papio" group when tab-group mode
   * is active, keeping papio's whole footprint in one collapsed group. In
   * work-window mode keepalive already places its tab in the work window. */
  async foldKeepaliveTab(tabID: number): Promise<void> {
    await this.ready;
    if ((await this.handoffSurface()) !== "tab-group") return;
    await this.foldIntoHandoffGroup(tabID);
  }

  /** Create the tab inside the dedicated work window, keeping a directly
   * matched visible-required adapter out of the minimized state. */
  private async openWorkWindowTab(
    url: string,
    visible: boolean,
    onTabMaterialized?: (tabID: number) => void,
  ): Promise<number | undefined> {
    const windows = this.deps.windows;
    if (windows === undefined) return undefined;
    const existing = this.store.workWindowID;
    if (existing !== undefined) {
      try {
        const win = await windows.get(existing);
        if (visible && win.state === "minimized") {
          await windows.update(existing, { focused: false, state: "normal" });
        }
        const tabID = (
          await this.deps.tabs.create({
            url,
            active: false,
            windowId: existing,
          })
        ).id;
        if (tabID !== undefined) onTabMaterialized?.(tabID);
        return tabID;
      } catch {
        // Window closed by the user (or the tab create raced its closing):
        // fall through and recreate.
      }
    }
    const created = await windows.create({
      url,
      focused: false,
      state: visible ? "normal" : "minimized",
    });
    const tabID = created.tabs?.find((tab) => tab.id !== undefined)?.id;
    if (tabID !== undefined) onTabMaterialized?.(tabID);
    // macOS Firefox often ignores `state`/`focused` at creation time
    // (bugzilla 1271047): the "minimized" work window arrives front and
    // center. Re-asserting the state after creation is the reliable form.
    if (!visible && created.id !== undefined && created.state !== "minimized") {
      try {
        await windows.update(created.id, {
          focused: false,
          state: "minimized",
        });
      } catch {
        // Cosmetic only: a visible work window still brokers correctly.
      }
    }
    if (created.id !== undefined) {
      const windowID = created.id;
      await this.update((s) => ({ ...s, workWindowID: windowID }));
    }
    return tabID;
  }

  /** Reveal the work window for an adapter whose SPA cannot paint while hidden,
   * then reload so the page loads in a visible context. Answers whether the
   * reload was issued; the reload's own completion re-enters classification, so
   * this deliberately does not wait for the load.
   *
   * Restoring the window is necessary but not sufficient once another paper
   * becomes its active tab. Measured 2026-08-26 after the first ScienceDirect
   * repair: a normal work window with this paper in a background tab still
   * yielded a 28 KB document whose View PDF anchor was disabled and had no
   * href. Activating that tab and reloading is the same positive visibility
   * boundary as restoring the minimized window.
   *
   * Measured 2026-08-24 on one entitled ScienceDirect article driven through
   * the institutional resolver: the hidden window yielded a 32 KB document
   * whose View PDF control carried no href and `aria-disabled="true"`, while
   * the same active article at the same 5000 ms settle yielded 262 KB with an
   * enabled `/pdfft` href. Changing visibility after the first load leaves the
   * unpainted document in place, so the page has to be fetched again.
   * `onPageCaptureRequest` already relies on this fact for fixture capture,
   * which is why diagnostic captures looked healthy while real drives parked.
   *
   * Focus is never taken: the window is restored with `focused: false` and the
   * tab is made active only inside it, so the operator keeps their foreground.
   * Once the window is normal AND the target tab is active, the reload's own
   * completion returns false here and terminates the cycle. */
  private async revealForHydration(
    spec: AdapterSpec,
    tabID: number,
  ): Promise<boolean> {
    if (!needsVisibleWindow(spec)) return false;
    const windowID = this.store.workWindowID;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return false;
    let changed = false;
    try {
      const win = await windows.get(windowID);
      if (win.state === "minimized") {
        await windows.update(windowID, { focused: false, state: "normal" });
        changed = true;
      }
    } catch {
      // The handoff continues assisted if the dedicated window disappeared.
      return false;
    }
    try {
      const tab = await this.deps.tabs.get(tabID);
      if (tab.active !== true) {
        if (this.deps.tabs.update === undefined) return false;
        // Through focusOwnedTab, never a raw tabs.update: an activation papio
        // does not claim is indistinguishable from an operator's click, so
        // onTabActivated reads it as takeover and cedes the record - which
        // erases the paper identity from the very surface this reveal exists
        // to make usable. Measured live 2026-08-26: every revealed
        // ScienceDirect surface came back `ceded_reason: operator_activated`.
        await this.focusOwnedTab(tabID);
        changed = true;
      }
    } catch {
      // An unreadable or unactivatable background tab cannot be made paint.
      return false;
    }
    if (!changed) return false;
    try {
      await this.deps.tabs.reload(tabID);
    } catch {
      // The tab is gone or refused the reload. Fall through to classify what is
      // there rather than dropping the verdict for this landing.
      return false;
    }
    return true;
  }

  /** The persisted singleton can name another window, so Chrome's membership
   * data is the authority when a handoff needs to be surfaced or folded away. */
  private async handoffGroupIDForTab(
    tabID: number,
  ): Promise<number | undefined> {
    const tabGroups = this.deps.tabGroups;
    if (tabGroups === undefined) return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      const windowID = tab.windowId;
      if (tab.groupId !== undefined && tab.groupId >= 0) {
        const group = await this.knownHandoffGroup(tab.groupId, windowID);
        if (group !== undefined) {
          if (windowID !== undefined)
            this.handoffGroupIDsByWindow.set(windowID, group.id);
          return group.id;
        }
      }
      const remembered =
        windowID === undefined
          ? undefined
          : this.handoffGroupIDsByWindow.get(windowID);
      if (remembered !== undefined) {
        const group = await this.knownHandoffGroup(remembered, windowID);
        if (group !== undefined) return group.id;
      }
      if (this.store.handoffGroupID !== undefined) {
        const group = await this.knownHandoffGroup(
          this.store.handoffGroupID,
          windowID,
        );
        if (group !== undefined) return group.id;
      }
      const found = await this.findHandoffGroups(windowID);
      if (found === undefined) return undefined;
      const ledger = await this.snapshotTabLedger();
      return (await this.preferredHandoffGroup(found, windowID, ledger))?.id;
    } catch {
      // A disappearing tab must not prevent the native handoff from progressing.
      return undefined;
    }
  }

  /** Bring the handoff tab to the human for authentication. In work-window mode
   * this activates the tab and restores/focuses the window; in tab-group mode it
   * expands the collapsed "papio" group and activates the tab. No-op for legacy
   * in-window tabs (already visible). Best-effort — auth proceeds regardless. */
  private async surfaceWorkTab(tabID: number): Promise<void> {
    const groupID = await this.handoffGroupIDForTab(tabID);
    if (groupID !== undefined && this.deps.tabGroups !== undefined) {
      await this.reduceHandoffGroupState(tabID);
      try {
        await this.focusOwnedTab(tabID);
      } catch {
        // The tab may already be gone; the badge/notification remain the signal.
      }
      return;
    }
    const windowID = this.store.workWindowID;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return;
    try {
      await this.focusOwnedTab(tabID);
    } catch {
      // The tab may already be gone; window focus below still helps.
    }
    try {
      const win = await windows.get(windowID);
      await windows.update(windowID, {
        focused: true,
        drawAttention: true,
        ...(win.state === "minimized" ? { state: "normal" as const } : {}),
      });
    } catch {
      // Window gone; the popup badge and notification remain the signal.
    }
  }

  /** A missing group id must not be reused after the physical group disappeared. */
  private async recollapseHandoffGroup(tabID?: number): Promise<boolean> {
    const groupID =
      tabID === undefined
        ? this.store.handoffGroupID
        : await this.handoffGroupIDForTab(tabID);
    if (groupID === undefined || this.deps.tabGroups === undefined)
      return false;
    try {
      await this.deps.tabGroups.get(groupID);
      await this.reduceHandoffGroupState(tabID);
      return true;
    } catch {
      // Group gone; its stored id must not be reused.
      return false;
    }
  }

  /** Clear parked_with_tab at the point INTENT to drive is expressed — either
   * caller below — not only on registerHandoffDrive's eventual success. Both
   * are reachable whenever the governor's two slots are already full, which
   * is the normal steady state once parkHandoffForManual drains its freed
   * slot straight into the next queued job: resumeHandoffAfterManual and the
   * offer handler's re-offer branches then defer through enqueueHandoffDrive
   * instead of registering directly. Clearing only on success left every
   * deferred path writing a live status back to storage with
   * parked_with_tab still true; a worker restart during that open-ended
   * queue wait (MV3 tears the worker down after ~30s idle) would see a live
   * status plus the stale marker and skip re-registering forever — stranded
   * outside governor supervision, with no timeout and no capacity
   * accounting. Both callers route through this one helper so a future
   * third caller cannot reopen the gap.
   * void, not awaited: `update` mutates the in-memory store and chains its
   * persistence synchronously before this call returns, so the clear is
   * queued for disk before either caller's own next await could yield to a
   * teardown. It cannot race parkHandoffForManual's own awaited set either:
   * JS has no true concurrency, and intent to drive a job again is only ever
   * expressed after that same job has already been parked. */
  private clearParkedMarker(jobID: string): void {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    if (job.parked_with_tab === true || job.waiting_for_session === true) {
      void this.update((s) =>
        patchJob(s, jobID, {
          parked_with_tab: false,
          waiting_for_session: false,
          waiting_for_session_key: undefined,
        }),
      );
    }
  }

  private enqueueHandoffDrive(request: QueuedHandoffDrive): void {
    if (
      this.handoffDrives.has(request.jobID) ||
      this.queuedDriveJobIDs.has(request.jobID)
    )
      return;
    if (findByJob(this.store, request.jobID) === undefined) return;
    this.queuedDriveJobIDs.add(request.jobID);
    this.handoffDriveQueue.push(request);
    this.clearParkedMarker(request.jobID);
  }

  private releaseHandoffDrive(jobID: string): void {
    this.handoffDrives.delete(jobID);
    this.handoffDriveTimeouts.delete(jobID);
    if (!this.queuedDriveJobIDs.delete(jobID)) return;
    const index = this.handoffDriveQueue.findIndex(
      (request) => request.jobID === jobID,
    );
    if (index >= 0) this.handoffDriveQueue.splice(index, 1);
  }

  private registerHandoffDrive(jobID: string, tabID: number): void {
    // Ownership of a tab is exactly when a claim's identity must become
    // durable, and this is the one place every path announces it. Placed
    // before the bookkeeping returns below: the surface is owned regardless of
    // whether a drive slot is free.
    void this.persistClaimIdentity(jobID, tabID);
    if (this.handoffDrives.has(jobID)) return;
    // A caller's own `handoffDrives.size >= HANDOFF_DRIVE_LIMIT` check and this
    // call are separated by awaits (openManagedTab, upsertJobWithOffer/patchJob),
    // so two entry points that are not both on the serialized inbound-frame
    // chain — e.g. a popup RPC racing a native re-offer frame — can each pass
    // their own check and both land here, exceeding the cap. Re-check it here
    // so every call site is safe by construction. The caller has always already
    // upserted/patched the job with this tabID before calling, so queuing (not
    // dropping) it lets the next drain reuse that same live tab once a slot frees.
    if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
      this.enqueueHandoffDrive({
        jobID,
        purpose: "handoff",
        focusExisting: false,
      });
      return;
    }
    // This job may have been parked by the timeout callback below with its
    // tab deliberately preserved (see parked_with_tab's own doc comment in
    // state.ts); a fresh registration here means it is being driven again —
    // by the operator finishing auth and a re-offer/redrive claiming this
    // same tab, or any other caller. clearParkedMarker above documents why
    // this must happen at intent, not just here at success.
    this.clearParkedMarker(jobID);
    const token = {};
    this.handoffDrives.set(jobID, { tabID, token });
    this.handoffDriveTimeouts.set(jobID, token);
    this.deps.setTimeout(async () => {
      if (this.handoffDriveTimeouts.get(jobID) !== token) return;
      this.handoffDriveTimeouts.delete(jobID);
      const current = findByJob(this.store, jobID);
      if (current !== undefined && current.tab_id === tabID) {
        // `auth_pending` asserts that this paper's surface reached a login page
        // - signInBlockerCount's contract, and what the daemon acts on: it
        // records auth-return profile evidence, opens a HumanGateLogin, and
        // RESERVES the institution's single sign-in slot. A drive that merely
        // ran out of time knows nothing about a login, so asserting one
        // fabricates all three.
        //
        // The offer already carries the answer and this path never read it:
        // `requires_auth` is the daemon's own statement that the route needs a
        // human sign-in. An open-access route sets it false, so the claim is
        // available exactly where it is true. A wall the paper reached anyway
        // still counts - a route can meet one unannounced - so the page is
        // consulted as well, and an unreadable tab simply adds nothing.
        //
        // Measured live 2026-08-23: an open-access ChemRxiv preprint, offered
        // with requires_auth false and serving a plain citation_pdf_url behind
        // no wall at all, reported auth_pending every three minutes for two
        // days. It held the library's sign-in slot while 22 papers queued
        // behind it, and the toolbar badge counted it as blocked on that
        // sign-in.
        let signInSurface = current.requires_auth === true;
        if (!signInSurface) {
          try {
            const tab = await this.deps.tabs.get(tabID);
            signInSurface =
              typeof tab.url === "string" && isAuthenticationURL(tab.url);
          } catch {
            signInSurface = false;
          }
        }
        if (signInSurface) {
          await this.update((s) =>
            patchJob(s, jobID, {
              status: "auth_pending",
              auth_started_ms: this.deps.now(),
            }),
          );
          this.send("auth_pending", {}, jobID);
        } else {
          // A spent drive still needs the operator, so silence is not the
          // alternative to the false claim - this is the same state the drive
          // governor already parks an undrivable paper in, and the popup lists
          // it with an Open button beside the sign-in asks. It says "take this
          // over" without asserting anything about a login, so no auth
          // evidence, no login gate, and no reservation of the one slot.
          await this.update((s) =>
            patchJob(s, jobID, {
              status: "queued",
              engagement_required: true,
            }),
          );
        }
        if (current.fresh_handoff === true) {
          // A fresh link is deliberately gone after materialization. Preserve
          // the live tab as a manual park; detaching it would leave the job
          // with neither a reusable URL nor a way back to the operator's page.
          await this.parkHandoffForManual(jobID);
          return;
        }
        // Legacy offers retain their URL and keep the established timeout
        // detach semantics.
        await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
      }
      // handoff_parked, never scaffold_idle. The next line parks this handoff,
      // so that is the fact papio is entitled to assert; scaffold_idle asserts
      // a claim phase of `claimed`/`bound`, which a drive that has already
      // opened and navigated a tab cannot be in. When a claim exists the
      // daemon therefore refused this close structurally - 14 live refusals
      // on 2026-08-21/22, every one "disposition does not match the binding's
      // current phase", at exact 3-minute drive-timeout intervals, stranding
      // one tab per drive. Claimless handoffs short-circuit to `unclaimed`
      // before the phase switch, so this is strictly wider: nothing that
      // closed before stops closing.
      await this.closeOwnedSurface(tabID, "handoff_parked");
      await this.parkHandoffForManual(jobID);
    }, HANDOFF_DRIVE_TIMEOUT_MS);
  }

  private async drainHandoffDriveQueueUnlocked(): Promise<void> {
    await this.surfaceReady;
    while (
      this.handoffDrives.size < HANDOFF_DRIVE_LIMIT &&
      this.handoffDriveQueue.length > 0
    ) {
      const request = this.handoffDriveQueue.shift();
      if (request === undefined) return;
      this.queuedDriveJobIDs.delete(request.jobID);
      const job = findByJob(this.store, request.jobID);
      if (job === undefined) continue;
      let tabID = job.tab_id >= 0 ? job.tab_id : undefined;
      if (tabID !== undefined) {
        try {
          const live = await this.deps.tabs.get(tabID);
          if (live.id !== tabID) tabID = undefined;
        } catch {
          tabID = undefined;
        }
      }
      if (
        tabID === undefined &&
        job.requires_auth === true &&
        request.operator !== true &&
        request.purpose !== "inbox-open"
      ) {
        if (!this.institutionalAuthGateOpen()) {
          // Slice 0 containment: a drive-queue entry with no live tab would
          // CREATE a sign-in surface. Autonomous callers (governor overflow,
          // startup requeue, daemon re-offers) all pass through here;
          // operator opens use openHandoff/retryAuthStalled directly. Park
          // for explicit engagement; a retained offer URL keeps the
          // operator's open usable.
          await this.update((s) =>
            patchJob(s, request.jobID, {
              status: "queued",
              engagement_required: true,
            }),
          );
          continue;
        }
        const candidateID = this.materializationCorrelation(
          request.jobID,
        )?.candidate_id;
        if (candidateID !== undefined) {
          // Slice 3: this drive answers a live institutional candidate —
          // consult the daemon's claim arbitration through openFreshHandoff
          // (the sole mint chokepoint) rather than this queue's own
          // cached-URL mint below, which a candidate-bearing job never
          // populates (candidate offers are URL-free by design). Detached:
          // this drain can itself run from inside the serialized inbound
          // frame chain (e.g. a native cancel or re-offer awaits
          // drainHandoffDriveQueue synchronously); consultAuthenticationClaim
          // awaits a correlated reply that can only arrive back through
          // that same chain, so awaiting it here would deadlock it exactly
          // as AGENTS.md describes. openFreshHandoff's own mint latch and
          // effect governor make it safe to let this run off-chain while
          // the drain moves on to its next queued entry.
          void this.openFreshHandoff(request.jobID, job, "automatic");
          continue;
        }
      }
      const url = this.offerURLs.get(request.jobID);
      const mustNavigate =
        url !== undefined &&
        (tabID === undefined || request.purpose === "redrive");
      const focusOnly =
        tabID !== undefined && request.focusExisting === true && !mustNavigate;
      // Opening a provider handoff or re-driving its existing tab is itself an
      // effect. Keep it under the same global permit as page mutations and
      // downloads; the handoff-drive lease remains separate and covers the
      // live tab after this bounded browser consequence.
      let effectToken: string | undefined;
      if (mustNavigate || focusOnly) {
        effectToken = this.claimEffectGovernor(request.jobID);
        if (effectToken === undefined) {
          // Preserve FIFO and retry when the current effect releases. Do not
          // reject an explicit offer merely because an unlike effect won the
          // slot first.
          this.handoffDriveQueue.unshift(request);
          this.queuedDriveJobIDs.add(request.jobID);
          return;
        }
      }
      try {
        if (focusOnly && tabID !== undefined) {
          try {
            await this.focusManagedTab(tabID);
          } catch {
            // The existing managed tab remains available in its papio surface.
          }
        }
        if (
          tabID !== undefined &&
          request.purpose === "redrive" &&
          url !== undefined &&
          this.deps.tabs.update !== undefined
        ) {
          let operatorActive = false;
          if (request.fenceOperatorActive === true) {
            // Renavigation fence: fresh read immediately before the act, no
            // unrelated awaits between. A claim-resume redrive never
            // renavigates an operator-active tab (mid-credential-entry is
            // exactly when this resume fires); the drive still registers so
            // the operator's own progress moves the job.
            try {
              operatorActive =
                (await this.deps.tabs.get(tabID)).active === true;
            } catch {
              operatorActive = false;
            }
          }
          if (!operatorActive) {
            try {
              await this.deps.tabs.update(tabID, { url });
            } catch {
              tabID = undefined;
            }
          }
        }
        if (tabID === undefined && url === undefined) {
          this.send("job_reject", {}, request.jobID);
          await this.removeJobWithOffer(request.jobID);
          continue;
        }
        if (tabID === undefined && url !== undefined) {
          try {
            tabID = await this.openManagedTab({
              url,
              jobId: request.jobID,
              purpose: request.purpose,
              ...(request.surfaceFallback !== undefined
                ? { surfaceFallback: request.surfaceFallback }
                : {}),
              ...(request.focusExisting !== undefined
                ? { focusExisting: request.focusExisting }
                : {}),
            });
          } catch (error) {
            console.error("papio: queued handoff tab creation failed", error);
          }
        }
      } finally {
        if (effectToken !== undefined)
          this.releaseEffectGovernor(request.jobID, effectToken, false);
      }
      if (focusOnly) {
        this.wakeEffectGovernor();
        continue;
      }
      if (tabID === undefined) {
        await this.parkUndrivableHandoff(request.jobID, "tab creation failed");
        continue;
      }
      this.beginProviderDrive(request.jobID);
      await this.update((s) =>
        patchJob(s, request.jobID, {
          tab_id: tabID,
          status: "accepted",
          download_initiated: false,
          unknown_count: 0,
        }),
      );
      // Announce the drive the moment it actually starts. The queued accept
      // above declared `queued`, which opens no epoch, so without this the
      // daemon would never charge a drive that began here — and under
      // HANDOFF_DRIVE_LIMIT=1 the queue is the NORMAL route to a drive, not an
      // edge. Trading over-charging for under-charging would leave a genuinely
      // runaway paper immortal instead of retiring a healthy one. A repeat
      // accept is safe by construction: the daemon folds an acknowledgement
      // into an already-open epoch within lease rather than opening a second.
      this.registerHandoffDrive(request.jobID, tabID);
      this.sendJobAccept(request.jobID);
      if (request.surfaceFallback === true) await this.surfaceWorkTab(tabID);
      this.wakeEffectGovernor();
    }
  }

  private async drainHandoffDriveQueue(): Promise<void> {
    const queued = this.handoffDriveDrainChain.then(async () => {
      this.drainingHandoffDriveQueue = true;
      try {
        await this.drainHandoffDriveQueueUnlocked();
      } finally {
        this.drainingHandoffDriveQueue = false;
        this.wakeEffectGovernor();
      }
    });
    this.handoffDriveDrainChain = queued.catch(() => undefined);
    await queued;
  }

  /** A challenge/auth stall leaves the exact page available to the operator,
   * but it is no longer an automated drive and therefore frees one governor
   * slot.
   *
   * That combination — a live tab the job still references, with no entry in
   * handoffDrives — is indistinguishable on its own from a job that IS mid
   * drive, because the slot lives only in worker memory while tab_id is
   * persisted. A service-worker restart (MV3 tears the worker down after
   * ~30s idle) would otherwise see the surviving tab and re-register a fresh
   * drive, silently re-consuming the slot this park just released and
   * re-arming its timeout. Across a slow institutional SSO that repeats on
   * every restart, halving effective governor capacity for everyone else.
   * Recording the park here — rather than at each of the three callers —
   * keeps the marker and the slot release inseparable. registerHandoffDrive
   * clears it whenever the job is genuinely driven again. */
  async parkHandoffForManual(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    this.releaseHandoffDrive(jobID);
    if (job !== undefined && job.tab_id >= 0) {
      await this.update((s) => patchJob(s, jobID, { parked_with_tab: true }));
      await this.reduceHandoffGroupState(job.tab_id);
    }
    await this.drainHandoffDriveQueue();
    await this.releaseQueuedHandoffs();
  }

  /** Reclaim a slot after the operator completes a challenge on the same tab.
   * No navigation occurs here; the next page update drives normal assessment.
   * Classification must wait when all governor slots remain occupied. */
  async resumeHandoffAfterManual(jobID: string): Promise<boolean> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    if (job === undefined || job.tab_id < 0) return false;
    if (!this.handoffDrives.has(jobID)) {
      if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
        this.enqueueHandoffDrive({
          jobID,
          purpose: "handoff",
          focusExisting: false,
        });
      } else {
        this.registerHandoffDrive(jobID, job.tab_id);
      }
    }
    await this.drainHandoffDriveQueue();
    return this.handoffDrives.has(jobID);
  }

  /** Keepalive must preserve an institutional session, not follow whichever
   * open-access offer happened to arrive last. */
  latestOpenURL(): string | undefined {
    for (let index = this.store.activeJobs.length - 1; index >= 0; index -= 1) {
      const job = this.store.activeJobs[index];
      if (job === undefined || job.requires_auth !== true) continue;
      const openurl = this.offerURLs.get(job.job_id);
      if (openurl !== undefined) return openurl;
    }
    return undefined;
  }

  /** Return only the configured resolver origin; signed offer paths and query
   * parameters never cross the popup reply or become persisted session state. */
  private latestResolverOrigin(): string | undefined {
    const openurl = this.latestOpenURL();
    if (openurl === undefined) return undefined;
    try {
      const url = new URL(openurl);
      return url.protocol === "https:" ? url.origin : undefined;
    } catch {
      return undefined;
    }
  }

  /** The keepalive manager pins its resolver tab inside the work window when
   * one exists, keeping papio's whole footprint out of the user's tab strip. */
  workWindowIDForKeepalive(): number | undefined {
    return this.store.workWindowID;
  }

  /** Keep the persistent daemon-health state visible without interrupting the
   * user. A badge failure is non-fatal: native bridging must keep recovering.
   * Precedence is disconnected, sign-in, a live provider-access block, resolver
   * setup, then triage: a blocked handoff outranks background work, but a dead
   * daemon or a sign-in the user can complete remains more immediate. */
  async syncConnectionBadge(
    status = this.store.connectionStatus,
  ): Promise<void> {
    try {
      const blockedProviderHosts = this.currentBlockedProviderHosts();
      // Union by tab so a paper this worker still knows about is not counted
      // twice, and add back the ones parked without a tab - a detached
      // auth_pending paper still needs the human to finish that sign-in.
      const wallTabs = await this.authWallSurfaceTabs();
      this.lastBadgedAuthWallTabs.clear();
      for (const tabID of wallTabs) this.lastBadgedAuthWallTabs.add(tabID);
      const pendingAuthJobs = this.store.activeJobs.filter(
        (job) => job.status === "auth_pending",
      );
      for (const job of pendingAuthJobs)
        if (job.tab_id >= 0) wallTabs.add(job.tab_id);
      const signInBlockers =
        wallTabs.size + pendingAuthJobs.filter((job) => job.tab_id < 0).length;
      let ungrantedResolverOrigins = 0;
      if (
        status === "connected" &&
        signInBlockers === 0 &&
        blockedProviderHosts.length === 0
      ) {
        for (const origin of this.store.resolverOrigins ?? []) {
          try {
            if (
              !(await this.deps.permissions.contains({
                origins: [`${origin}/*`],
              }))
            ) {
              ungrantedResolverOrigins += 1;
            }
          } catch {
            ungrantedResolverOrigins += 1;
          }
        }
      }
      // contains() is asynchronous; never paint a connected result after the
      // port has dropped while permission checks were in flight.
      if (status === "connected" && this.store.connectionStatus !== "connected")
        return;
      if (this.deps.toolbarCount !== undefined) {
        try {
          const mode = await this.deps.toolbarCount.get();
          if (mode === "required" || mode === "all" || mode === "off")
            this.toolbarCountMode = mode;
        } catch {
          // Default required mode remains safe when storage is unavailable.
        }
      }
      const badge = computeBadge({
        connectionStatus: status,
        reauthNeeded: this.keepaliveReauthNeeded,
        authBlockers: signInBlockers,
        queuedAuth: this.queuedAuthJobCount(),
        challengeBlocked: this.challengeBlockedJobCount(),
        blockedHosts: blockedProviderHosts,
        ungrantedResolvers: ungrantedResolverOrigins,
        triageCount: this.triagePendingCount,
        requiredTurnCount: this.triageRequiredTurnCount,
        requiredTurnsComplete: this.triageRequiredTurnsComplete,
        countsSchemaV3: this.triageCountsSchemaV3,
        watchHits: this.triageWatchHits,
        retractions: this.triageRetractions,
        toolbarCountMode: this.toolbarCountMode,
      });
      if (
        this.lastBadgePaint?.text === badge.text &&
        this.lastBadgePaint.color === badge.color &&
        this.lastBadgePaint.tooltip === badge.tooltip
      )
        return;
      this.lastBadgePaint = badge;
      await Promise.all([
        this.deps.action.setBadgeText({ text: badge.text }),
        this.deps.action.setBadgeBackgroundColor({ color: badge.color }),
        this.deps.action.setTitle?.({ title: badge.tooltip }),
      ]);
    } catch {
      // Browser action APIs are advisory; do not make a healthy bridge fail.
    }
  }
  private async resolveCaptureTransmissionPolicy(): Promise<void> {
    const browserInfo = this.deps.browserInfo;
    // Firefox-only API absence is the Chrome path; preserve today's
    // always-on capture behaviour there.
    if (browserInfo === undefined) {
      this.captureConsentRequired = false;
      this.captureTransmissionAllowed = true;
      return;
    }
    let info: { name?: string; version?: string };
    try {
      info = await browserInfo();
    } catch {
      // The Firefox-only API is present but unavailable; fail closed rather
      // than transmit before a pre-140 consent decision can be read.
      this.captureConsentRequired = true;
      this.captureTransmissionAllowed = false;
      return;
    }
    if (info.name?.toLowerCase() !== "firefox") {
      this.captureConsentRequired = false;
      this.captureTransmissionAllowed = true;
      return;
    }
    const major = Number.parseInt(info.version?.split(".")[0] ?? "", 10);
    if (!Number.isFinite(major)) {
      this.captureConsentRequired = true;
      this.captureTransmissionAllowed = false;
      return;
    }
    if (major >= 140) {
      this.captureConsentRequired = false;
      this.captureTransmissionAllowed = true;
      return;
    }
    this.captureConsentRequired = true;
    try {
      this.captureTransmissionAllowed =
        (await this.deps.captureConsent?.get()) === true;
    } catch {
      this.captureTransmissionAllowed = false;
    }
  }
  /** Re-read mutable consent at every capture boundary. Firefox options can
   * change while this worker remains alive, so startup policy is not enough. */
  private async refreshCaptureConsent(): Promise<void> {
    if (!this.captureConsentRequired) return;
    try {
      this.captureTransmissionAllowed =
        (await this.deps.captureConsent?.get()) === true;
    } catch {
      this.captureTransmissionAllowed = false;
    }
  }

  /** Rebuild direct-download tracking from the daemon tuple/envelope and
   * Chrome's durable download record after an MV3 worker restart. */
  private async reconcileDirectDownloads(): Promise<void> {
    for (const job of this.store.activeJobs) {
      const epoch = job.drive_epoch;
      const envelope = job.direct_envelope;
      if (
        epoch?.strategy !== "direct" ||
        envelope === undefined ||
        typeof envelope.allowed_origin !== "string" ||
        typeof envelope.path_family !== "string" ||
        typeof envelope.expected_identifier !== "string"
      )
        continue;
      if (epoch.in_flight_download_id === undefined) {
        // A crash before the exact browser download id was persisted leaves
        // completion unknown. Filename search is not correlation and must not
        // settle the daemon permit or authorize a replacement effect.
        continue;
      }
      const downloadID = epoch.in_flight_download_id;
      let found: DownloadItemLike[];
      try {
        found = await this.deps.downloads.search({ id: downloadID });
      } catch {
        continue;
      }
      const item = found[0];
      if (item === undefined) continue;
      this.downloads.set(job.job_id, {
        ids: new Set([downloadID]),
        ambiguous: false,
        directOffer: true,
        directEpoch: epoch,
        directAllowedOrigin: envelope.allowed_origin,
        directPathFamily: envelope.path_family,
        directExpectedIdentifier: envelope.expected_identifier,
      });
      const state = item?.state;
      if (state === "complete" || state === "interrupted") {
        await this.onDownloadChanged({
          id: downloadID,
          state: { current: state },
        });
      }
    }
  }

  /** Bind browser listeners (once), open the native connection, send hello, and
   * hydrate persisted job/tab correlation. Safe to call on every SW spin-up.
   * top-level-registration expectation. */
  async start(): Promise<void> {
    this.captureTransmissionPolicyReady =
      this.resolveCaptureTransmissionPolicy();
    this.bindListeners();
    this.ready = this.deps.backend.load().then(async (s) => {
      this.store = clearNegotiationState(s);
      const correlations =
        this.deps.pdfGrabCorrelations === undefined
          ? {}
          : await this.deps.pdfGrabCorrelations.get();
      for (const [grabID, correlation] of Object.entries(correlations)) {
        if (
          typeof correlation.scanID !== "string" ||
          !Number.isSafeInteger(correlation.tabID) ||
          correlation.tabID < 1 ||
          typeof correlation.steeringPath !== "string" ||
          correlation.steeringPath === "" ||
          isURLLike(correlation.steeringPath) ||
          !PDF_GRAB_CORRELATION_STATES.has(correlation.state) ||
          (correlation.abandonPending !== undefined &&
            typeof correlation.abandonPending !== "boolean") ||
          (correlation.downloadID !== undefined &&
            !isPositiveSafeInteger(correlation.downloadID)) ||
          (correlation.effectRequestID !== undefined &&
            (typeof correlation.effectRequestID !== "string" ||
              correlation.effectRequestID === "" ||
              isURLLike(correlation.effectRequestID)))
        ) {
          continue;
        }
        // Exactly one of the two shapes: a download papio started, or a grab
        // armed and waiting for the researcher's own viewer download, which
        // carries the route instead so it can re-register its steering.
        // Dropping the second shape here would silently discard the grab every
        // time MV3 recycled the worker — which it does within seconds of going
        // idle.
        const legacy = correlation as PdfGrabCorrelation & { url?: string };
        let route: string | undefined;
        if (typeof legacy.route === "string") {
          // `isDownloadRoute` is the whole test: a route equals its own
          // origin-and-path reduction, so a query, a fragment, credentials or a
          // non-web scheme all fail it. Do NOT also reject URL-shaped values —
          // a route IS an origin and a path, so that would drop every armed
          // capture at hydration, exactly when an awaiting-viewer grab depends
          // on surviving the worker death that precedes the researcher's click.
          if (!isDownloadRoute(legacy.route)) {
            continue;
          }
          route = legacy.route;
        } else if (typeof legacy.url === "string") {
          // A build before the route change persisted the full delivery URL,
          // signing token and all. Convert it rather than dropping the record:
          // the daemon-side capture is still armed and still holds its permit, so
          // discarding the correlation would strand it exactly as an orphan. The
          // route keeps the steering working and the token is left behind.
          route = downloadRoute(legacy.url);
        }
        const started = correlation.downloadID !== undefined;
        const armed =
          correlation.downloadID === undefined &&
          route !== undefined &&
          isDownloadRoute(route);
        if (!started && !armed) continue;
        const { url: _token, ...rest } = legacy;
        this.pdfGrabCorrelations.set(grabID, {
          ...rest,
          ...(route !== undefined ? { route } : {}),
        });
        // Re-persist unconditionally: a legacy record's token must be evicted
        // from storage now, not whenever the next mutation happens to run.
        this.persistPdfGrabCorrelations();
      }
      this.hydrated = true;
      await this.update((current) => current);
    });
    this.surfaceReady = this.ready
      .then(() => this.bootstrapSurfaceLifecycle())
      .catch((e) => {
        console.error(
          "papio: surface-lifecycle startup failed; adoption/close skipped",
          e,
        );
      });
    this.connect();
    // Wake this worker even when idle so queued daemon offers reach it (the
    // native connection originates here, so the daemon cannot wake a dormant
    // worker itself). Register only when absent: re-creating an existing alarm
    // on every MV3 spin-up resets its schedule and can deliver the wake twice.
    await this.ensureKeepaliveAlarm();
    await this.ready;
    await this.captureTransmissionPolicyReady;
    await this.reconcilePdfGrabCorrelations();
    await this.restoreProviderDrainLeaseTimers();
    await this.reconcileDirectDownloads();
    await this.reconcileGenericDownloads();
    // Reconcile persisted papio groups, ledger identity, and any close
    // tombstone before any new fold can race the startup repair and
    // multiply groups in the same browser window (surfaceReady barrier).
    await this.surfaceReady;
    await this.syncConnectionBadge();
    await this.reconcileTabs();
    const activeMaterializationJobs = new Set(
      this.store.activeJobs.map((job) => job.job_id),
    );
    for (const [jobID, entry] of Object.entries(
      this.store.materializations ?? {},
    )) {
      if (activeMaterializationJobs.has(jobID)) continue;
      if (entry.tab_id >= 0) await this.removeMaterializationTab(entry.tab_id);
      await this.applyMaterialization(jobID, { type: "clear" });
    }
    const governorQueuedAtRestart: string[] = [];
    for (const job of this.store.activeJobs) {
      if (!this.hasDelegatedAuthority(job)) {
        // Legacy and assisted jobs are durable records only. A worker restart
        // must not turn an old offer into a new autonomous tab/download.
        continue;
      }
      if (
        job.status !== "accepted" &&
        job.status !== "auth_pending" &&
        job.status !== "awaiting_download"
      ) {
        continue;
      }
      if (
        job.tab_id < 0 &&
        job.status === "auth_pending" &&
        job.parked_with_tab !== true
      ) {
        // A timeout-detached/auth-pending job has deliberately left the
        // governor. Only a fresh operator drive may reclaim its slot; restart
        // must not turn the durable auth state into an autonomous re-open.
        continue;
      }
      if (job.tab_id < 0) {
        // Governor-queued before this worker was suspended: the daemon
        // accepted it, but the FIFO holding it until a slot freed lives only
        // in memory. Nothing else recovers these — this scan used to skip
        // them, the queued-release pass below only handles status "queued",
        // and a daemon re-offer on the same URL merely re-acks. Left alone
        // they never open, never complete and never time out, which is worst
        // exactly under the flood the governor exists for.
        //
        // A tabless accepted handoff is governor work. Direct routes never
        // arrive as job offers; they use provider_direct_get_request.
        if (this.offerURLs.get(job.job_id) === undefined) continue;
        governorQueuedAtRestart.push(job.job_id);
        continue;
      }
      if (job.parked_with_tab === true) {
        // Deliberately parked by the handoff-drive timeout with its tab
        // preserved for the operator (see parked_with_tab's doc comment in
        // state.ts): the governor slot was already released at park time,
        // not merely dropped by this restart. Re-registering here would
        // silently re-consume a slot and re-arm a fresh 3-minute timeout for
        // a job nobody asked to resume driving, halving effective governor
        // capacity for every other queued job across a slow institutional
        // SSO. The tab-update listener still recovers it the moment the
        // operator finishes authenticating in that same tab.
        continue;
      }
      if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
        governorQueuedAtRestart.push(job.job_id);
        continue;
      }
      this.registerHandoffDrive(job.job_id, job.tab_id);
    }
    for (const jobID of governorQueuedAtRestart) {
      this.enqueueHandoffDrive({
        jobID,
        purpose: "handoff",
        focusExisting: false,
      });
    }
    if (governorQueuedAtRestart.length > 0) await this.drainHandoffDriveQueue();
    await this.reconcilePendingDeliveryOnStartup();
    await this.redrivePendingTermsGates();
    for (const job of this.store.activeJobs) {
      if (job.status === "queued")
        this.scheduleQueuedHandoffRelease(job.job_id);
    }
    await this.releaseQueuedHandoffs();
    await this.releaseQueuedHandoffsForLiveLanding();
    // papio owns its surfaces: after the daemon has had a moment to reclaim
    // live work through fresh offers, silently close ledgered leftovers still
    // sitting in the papio group or work window. Two passes: an early one for
    // the common case and a late one for offers that reclaim tabs slowly.
    for (const delay of [12_000, 90_000]) {
      this.deps.setTimeout(() => {
        void this.reconcileOwnedTabs();
      }, delay);
    }
  }

  /**
   * On spin-up the tracked tab_id can be stale: a tab closed while the MV3
   * worker slept (its onTabRemoved never fired), or session-restore reopened
   * provider tabs with fresh ids. Verify each tracked tab still exists and
   * recover the ones that don't, so a job never strands invisibly on a dead
   * tab (the "jobs stuck at auth_returned" failure).
   */
  private async reconcileTabs(): Promise<void> {
    for (const job of [...this.store.activeJobs]) {
      if (!this.hasDelegatedAuthority(job)) continue;
      if (job.tab_id < 0) continue; // already queued / awaiting an open
      let alive = false;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        alive = tab?.id === job.tab_id;
      } catch {
        alive = false;
      }
      if (alive) continue;
      const offerURL = this.offerURLs.get(job.job_id);
      const hasQueuedGovernorWork = this.store.activeJobs.some(
        (candidate) =>
          candidate.status === "accepted" &&
          candidate.tab_id < 0 &&
          this.store.pendingDelivery?.job_id !== candidate.job_id &&
          this.offerURLs.get(candidate.job_id) !== undefined,
      );
      if (
        !hasQueuedGovernorWork &&
        job.status !== "awaiting_download" &&
        this.store.pendingDelivery?.job_id !== job.job_id &&
        offerURL !== undefined &&
        (job.requires_auth !== true || this.institutionalAuthGateOpen())
      ) {
        const effectToken = this.claimEffectGovernor(job.job_id);
        if (effectToken === undefined) {
          this.enqueueHandoffDrive({
            jobID: job.job_id,
            purpose: "reoffer",
            focusExisting: false,
          });
          continue;
        }
        let recoveredTabID: number | undefined;
        try {
          recoveredTabID = await this.openManagedTab({
            url: offerURL,
            jobId: job.job_id,
            purpose: "reoffer",
            focusExisting: false,
          });
          if (recoveredTabID !== undefined) {
            const recoveredID = recoveredTabID;
            this.beginProviderDrive(job.job_id);
            await this.update((s) =>
              patchJob(s, job.job_id, { tab_id: recoveredID }),
            );
            this.registerHandoffDrive(job.job_id, recoveredID);
          }
        } finally {
          this.releaseEffectGovernor(job.job_id, effectToken, false);
          this.wakeEffectGovernor();
        }
        if (recoveredTabID !== undefined) {
          continue;
        }
      }
      if (
        this.store.pendingDelivery?.job_id === job.job_id &&
        this.store.pendingDelivery.status !== "failed"
      ) {
        await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
        continue;
      }
      if (job.status === "awaiting_download") {
        // Past auth: a download may have completed or be in flight into the
        // job's adoption dir, which the daemon's poll-scan adopts. Park it, as
        // onTabRemoved would have.
        this.completedDownloadTabs.delete(job.job_id);
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      // Pre-download tab vanished: re-queue so the handoff choreography reopens
      // it (one visible at a time, forced release within the fallback window)
      // instead of leaving the job pointed at a dead tab. Without a retained
      // offer URL there is nothing to reopen, so drop it.
      if (this.offerURLs.get(job.job_id) === undefined) {
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      if (this.authAttemptsFor(job.job_id) >= MAX_AUTH_ATTEMPTS) {
        // Already failed to authenticate this job MAX_AUTH_ATTEMPTS times this
        // session: surface the human step and leave it parked instead of
        // re-queueing it into another doomed drive.
        const offerURL = this.offerURLs.get(job.job_id);
        if (offerURL !== undefined) {
          this.rememberStalledAuthHandoff(job.job_id, {
            url: offerURL,
            providerHosts: job.provider_hosts,
            ...(job.expected !== undefined ? { expected: job.expected } : {}),
            ...(job.requires_auth !== undefined
              ? { requiresAuth: job.requires_auth }
              : {}),
            ...(job.access_mode !== undefined
              ? { accessMode: job.access_mode }
              : {}),
          });
        }
        await this.reportAuthStalled(job.job_id);
        await this.removeJobWithOffer(job.job_id);
        continue;
      }
      this.beginProviderDrive(job.job_id);
      await this.update((s) =>
        patchJob(s, job.job_id, {
          tab_id: -1,
          status: "queued",
          download_initiated: false,
          unknown_count: 0,
          // A gate-closed institutional job must not be re-opened
          // autonomously; require fresh operator engagement instead.
          ...(job.requires_auth === true && !this.institutionalAuthGateOpen()
            ? { engagement_required: true }
            : {}),
          parked_with_tab: false,
        }),
      );
      this.scheduleQueuedHandoffRelease(job.job_id);
    }
  }

  /** Cancel an active job on user request (popup cancel button). */
  async requestCancel(jobID: string): Promise<void> {
    await this.ready;
    const job = findByJob(this.store, jobID);
    if (!job) return;
    this.send("provider_outcome", { outcome: "cancelled" }, jobID);
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    this.authStalledReported.delete(jobID);
    this.stalledAuthHandoffs.delete(jobID);
    await this.removeJobWithOffer(jobID);
  }

  /** True while this port's hello_ack still describes the daemon, whether or
   * not this browser holds the offer/handoff flow. Holdership gates offers and
   * handoffs in the daemon, never the capabilities it acknowledged: a browser
   * demoted by `papio browser use` keeps every feature it negotiated, and a
   * browser refused at hello has no features to satisfy the callers below. */
  private daemonNegotiated(): boolean {
    return (
      this.store.connectionStatus === "connected" ||
      this.store.connectionStatus === "session_elsewhere"
    );
  }

  /** True while this session may do work only the session HOLDER may do: accept
   * or reject an offer, drive a handoff, claim/bind/route a materialization,
   * start an effect. The daemon refuses every one of those from a non-holder
   * (bridge.go's non-holder `default:` arm), and its refusal is a session_busy
   * error that surfaces as a failure the researcher can do nothing about.
   *
   * An acknowledged `role: "pending"` is the ONLY thing that makes this false.
   * No ack yet, a daemon old enough never to send `role`, and a worker that
   * restarted with a stale persisted status all read as holder: silence has
   * never been ambiguous, and the daemon's own arbitration stays the backstop.
   *
   * Capabilities are NOT gated here. A pending session negotiated the same
   * features and routes its own user-initiated work; see `pdfGrabAvailable`. */
  private holderRole(): boolean {
    return !(this.hasCurrentHello() && this.helloRole === "pending");
  }

  /** True only after this port's hello_ack has advertised page acquisition. */
  pageAcquireAvailable(): boolean {
    return (
      this.daemonNegotiated() &&
      (this.store.daemonFeatures ?? []).includes("page_acquire")
    );
  }

  /** `page_capture` is not on the daemon's holder-independent list, so a
   * pending session cannot upload a fixture however well it negotiated. */
  pageCaptureAvailable(): boolean {
    return (
      this.daemonNegotiated() &&
      this.holderRole() &&
      (this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_FEATURE)
    );
  }

  /** Second, independent enforcement point for the `terms` capture scenario.
   * The popup withholds the option, but it decides that when the panel is
   * populated, and the daemon underneath can be swapped between then and the
   * click (the two-binary skew AGENTS.md documents). Emitting `terms` to a
   * daemon that cannot validate it does not merely fail that capture: the
   * decode error tears down the entire native-messaging session, so the
   * boundary that actually sends the frame refuses it too. */
  termsCaptureAvailable(): boolean {
    return (
      this.daemonNegotiated() &&
      (this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_TERMS_FEATURE)
    );
  }

  /** Forward an active-page acquisition request and await the daemon ack. */
  async requestPageAcquire(
    payload: PageAcquirePayload,
  ): Promise<PageAcquireAckPayload> {
    await this.ready;
    if (!this.pageAcquireAvailable()) {
      return { error: "Page acquisition is not available from this daemon" };
    }
    if (typeof payload.doi !== "string" || payload.doi.trim() === "") {
      return { error: "page has no DOI" };
    }
    // Reduce here rather than at each caller. The daemon never reads this field
    // (`pageAcquireRequest` builds its request from DOI and title only), while a
    // landing-page URL routinely carries bearer-grade values — a Springer
    // content-sharing `?sharing_token=`, an EZproxy `?ticket=`. Doing it at one
    // caller left the other sending them, and a guarantee that depends on every
    // caller remembering is not a guarantee.
    const origin = pageAcquireOrigin(payload.url);
    if (origin === undefined) {
      return { error: "papio could not read this page's address" };
    }
    return new Promise<PageAcquireAckPayload>((resolve) => {
      const msgID = this.deps.randomUUID().replace(/-/g, "");
      this.pageAcquireWaiters.set(msgID, resolve);
      const frame: Record<string, unknown> = {
        url: origin,
        ...(payload.doi !== undefined ? { doi: payload.doi } : {}),
        // A tab with no document title gets one derived from its URL, and the
        // daemon PERSISTS this field into the job row (unlike url, which it
        // discards). `state.ts` already refuses a URL-shaped title on disk; the
        // wire boundary must not be weaker than the disk boundary.
        ...(payload.title !== undefined && !isURLLike(payload.title) ? { title: payload.title } : {}),
        ...(payload.source !== undefined ? { source: payload.source } : {}),
      };
      if (!this.send("page_acquire", frame, undefined, msgID)) {
        this.pageAcquireWaiters.delete(msgID);
        resolve({ error: "Could not send page acquisition request" });
      }
    });
  }

  private deliveryJobForDOI(doi: string | undefined): ActiveJob | undefined {
    if (doi === undefined || doi.trim() === "") return undefined;
    const normalized = doi
      .trim()
      .toLowerCase()
      .replace(/^doi:\s*/, "");
    const matches = this.store.activeJobs.filter(
      (job) =>
        job.expected?.doi
          ?.trim()
          .toLowerCase()
          .replace(/^doi:\s*/, "") === normalized,
    );
    return matches.length === 1 ? matches[0] : undefined;
  }
  private deliveryJobForOpener(tab: TabInfo): ActiveJob | undefined {
    if (tab.openerTabId === undefined) return undefined;
    const opener = findByTab(this.store, tab.openerTabId);
    if (opener === undefined) return undefined;
    if (opener.status !== "accepted" && opener.status !== "awaiting_download")
      return undefined;
    return opener;
  }
  private comparableDeliverySourceURL(rawURL: string): string {
    const viewer = providerViewerPDFURL(rawURL, this.deps.adapterSpecs);
    if (viewer !== undefined) return viewer;
    return pdfSourceURL(rawURL);
  }
  private pageSourceToken(rawURL: string): string {
    const comparable = this.comparableDeliverySourceURL(rawURL);
    try {
      const u = new URL(comparable);
      return `${u.origin}${u.pathname}`;
    } catch {
      const q = comparable.indexOf("?");
      const h = comparable.indexOf("#");
      let end = comparable.length;
      if (q !== -1) end = Math.min(end, q);
      if (h !== -1) end = Math.min(end, h);
      return comparable.slice(0, end);
    }
  }
  /** The top frame's current document epoch, straight from the browser.
   *
   * Never from `pageNavSeq` or any worker-local cache: MV3 suspends the worker
   * whenever it likes, and after a restart every such map is empty. An empty
   * map that reads as "unchanged" is precisely how a picker minted against one
   * document gets spent on a different one at the same path. `undefined` here
   * means "papio cannot tell what this tab is showing" — every caller must
   * treat that as a refusal, never as permission. */
  private async liveDocumentEpoch(tabId: number): Promise<string | undefined> {
    const nav = this.deps.webNavigation;
    if (nav?.getFrame === undefined) return undefined;
    try {
      const frame = await nav.getFrame({ tabId, frameId: 0 });
      const epoch = frame?.documentId;
      return typeof epoch === "string" && epoch.length > 0 ? epoch : undefined;
    } catch {
      return undefined;
    }
  }

  /** Authority-bearing page identity, or `undefined` when the platform cannot
   * supply a document epoch — on such a platform the picker and the persistent
   * manual continuation are simply unavailable, because nothing else here can
   * tell two documents at one path apart. */
  private async currentPageIdentity(
    tabId: number,
    sourceURL: string,
  ): Promise<PageIdentity | undefined> {
    const epoch = await this.liveDocumentEpoch(tabId);
    if (epoch === undefined) return undefined;
    return {
      tab_id: tabId,
      nav_seq: this.pageNavSeq.get(tabId) ?? 0,
      source_url: this.pageSourceToken(sourceURL),
      document_id: epoch,
    };
  }

  private pageIdentityMatches(a: PageIdentity, b: PageIdentity): boolean {
    if (a.tab_id !== b.tab_id) return false;
    if (a.nav_seq !== b.nav_seq) return false;
    if (a.source_url !== b.source_url) return false;
    // The token is origin+pathname only: a signed provider query is a bearer
    // credential and providers re-issue it for the same document, so the query
    // cannot be compared. Two *different* documents served from one path are
    // therefore indistinguishable here and the epoch is the only separator.
    // A missing epoch on either side is not "unchanged", it is "unknown".
    if (a.document_id === undefined || b.document_id === undefined) return false;
    return a.document_id === b.document_id;
  }

  private isFirefox(): boolean {
    return this.deps.downloads.onDeterminingFilename === undefined;
  }

  private advisoryCandidates(): DeliveryCandidate[] {
    const candidates = this.store.activeJobs
      .filter((j) => j.status === "awaiting_download")
      .sort((a, b) => b.offered_at - a.offered_at)
      .slice(0, 12)
      .map((j) => ({ job_id: j.job_id, title: j.expected?.title?.trim() || j.job_id }));
    return candidates;
  }

  private mintDeliveryChoice(pageIdentity: PageIdentity, candidates: DeliveryCandidate[]): DeliveryChoiceOffer {
    const interaction = this.deps.randomUUID().replace(/-/g, "");
    // Evict oldest if over cap
    if (this.deliveryChoiceNonces.size >= 32) {
      let oldestKey: string | undefined;
      let oldestTime = Infinity;
      for (const [k, v] of this.deliveryChoiceNonces) {
        if (v.mintedAt < oldestTime) {
          oldestTime = v.mintedAt;
          oldestKey = k;
        }
      }
      if (oldestKey !== undefined) this.deliveryChoiceNonces.delete(oldestKey);
    }
    // Evict by age (10 min)
    const now = this.deps.now();
    for (const [k, v] of [...this.deliveryChoiceNonces]) {
      if (now - v.mintedAt > 10 * 60_000) this.deliveryChoiceNonces.delete(k);
    }
    this.deliveryChoiceNonces.set(interaction, {
      pageIdentity,
      candidates: candidates.map((c) => c.job_id),
      mintedAt: now,
    });
    return { interaction, candidates };
  }

  private consumeDeliveryChoice(choice: DeliveryChoice): { pageIdentity: PageIdentity; candidates: string[] } | undefined {
    const entry = this.deliveryChoiceNonces.get(choice.interaction);
    if (entry === undefined) return undefined;
    if (this.deps.now() - entry.mintedAt > 10 * 60_000) {
      this.deliveryChoiceNonces.delete(choice.interaction);
      return undefined;
    }
    this.deliveryChoiceNonces.delete(choice.interaction);
    if (!entry.candidates.includes(choice.job_id)) return undefined;
    return entry;
  }

  private destroyDeliveryChoiceState(): void {
    this.deliveryChoiceNonces.clear();
  }

  private destroyDeliveryChoiceForTab(tabId: number): void {
    for (const [k, v] of [...this.deliveryChoiceNonces]) {
      if (v.pageIdentity.tab_id === tabId) this.deliveryChoiceNonces.delete(k);
    }
  }
  private destroyDeliveryChoicesForJob(jobId: string): void {
    for (const [k, v] of [...this.deliveryChoiceNonces]) {
      if (v.candidates.includes(jobId)) this.deliveryChoiceNonces.delete(k);
    }
  }

  private bindWebNavigation(): void {
    if (this.webNavigationBound) return;
    this.webNavigationBound = true;
    try {
      type NavListener = {
        addListener: (
          cb: (d: { tabId: number; frameId: number; documentId?: string }) => void,
        ) => void;
      };
      type NavAPI = {
        onCommitted?: NavListener;
        onHistoryStateUpdated?: NavListener;
        onReferenceFragmentUpdated?: NavListener;
        onTabReplaced?: {
          addListener: (
            cb: (d: { tabId: number; replacedTabId: number }) => void,
          ) => void;
        };
        onErrorOccurred?: {
          addListener: (
            cb: (d: {
              tabId: number;
              frameId: number;
              error?: string;
            }) => void,
          ) => void;
        };
      };
      // The MV3 platform globals are not in this module's lib types; when
      // `chrome.webNavigation` exists at all, the browser guarantees its shape.
      const globalChrome = globalThis as unknown as {
        chrome?: { webNavigation?: NavAPI };
      };
      const wn = globalChrome.chrome?.webNavigation;
      const depsWN = this.deps.webNavigation;
      const nav = (depsWN as unknown) ?? wn;
      if (nav === undefined || nav === null) return;
      const n = nav as NavAPI;
      const observeNavigation = (d: { tabId: number; frameId: number }): void => {
        if (d.frameId !== 0) return;
        this.pageNavSeq.set(d.tabId, (this.pageNavSeq.get(d.tabId) ?? 0) + 1);
        this.destroyDeliveryChoiceForTab(d.tabId);
      };
      n.onCommitted?.addListener((d) => {
        if (d.frameId !== 0) return;
        observeNavigation(d);
        // A committed top-frame navigation replaces the document the manual
        // continuation was granted against, so the continuation dies with it.
        const pi = this.store.pendingDelivery?.page_identity;
        if (pi !== undefined && pi.tab_id === d.tabId) {
          void this.update((s) => clearPendingDelivery(s, s.pendingDelivery?.job_id));
        }
      });
      n.onHistoryStateUpdated?.addListener(observeNavigation);
      n.onReferenceFragmentUpdated?.addListener(observeNavigation);
      // Prerender/instant activation swaps a whole tab out. `tabId` is the tab
      // that took over; `replacedTabId` is the one that went away. Neither id
      // keeps any authority: the page the researcher was looking at is gone.
      n.onTabReplaced?.addListener((d) => {
        this.pageNavSeq.delete(d.replacedTabId);
        this.pageNavSeq.delete(d.tabId);
        this.destroyDeliveryChoiceForTab(d.replacedTabId);
        this.destroyDeliveryChoiceForTab(d.tabId);
        const pi = this.store.pendingDelivery?.page_identity;
        if (pi !== undefined && (pi.tab_id === d.replacedTabId || pi.tab_id === d.tabId)) {
          void this.update((s) => clearPendingDelivery(s, s.pendingDelivery?.job_id));
        }
      });
      // A failed top-frame navigation is a dead end, not a human sign-in
      // wall; the marker it leaves is read (and consumed) by the generic
      // auth-wall detector in onTabUpdated before that detector charges an
      // auth attempt (surface-lifecycle-plan.md Slice 1).
      n.onErrorOccurred?.addListener((d) => {
        void this.onNavigationError(d);
      });
    } catch {
    }
  }
  /** Record navigation-error evidence for a papio-managed tab. Ordering
   * only: no URL is read or persisted, and the job itself is left
   * untouched — the marker is consulted (and consumed) by the generic
   * auth-wall detector in onTabUpdated before that detector could
   * otherwise mistake this dead end for a human sign-in wall. Also
   * synchronously enqueues a durable copy (oracle finding 5) before any
   * later classification runs: the worker-local map above does not
   * survive an MV3 worker restart, so this durable marker is the only
   * evidence left if the worker dies before the document settles. */
  private async onNavigationError(d: {
    tabId: number;
    frameId: number;
  }): Promise<void> {
    if (d.frameId !== 0) return;
    await this.ready;
    const managed =
      findByTab(this.store, d.tabId) !== undefined ||
      this.tabLedgerCache?.[String(d.tabId)] !== undefined;
    if (!managed) return;
    this.navigationErrors.set(d.tabId, this.deps.now());
    const bindingID = this.tabLedgerCache?.[String(d.tabId)]?.binding_id;
    if (bindingID === undefined) return;
    const job = findByTab(this.store, d.tabId);
    const grant = job === undefined ? undefined : this.claimGrants.get(job.job_id);
    const entry: NavigationErrorMarkerEntry = {
      tab_id: d.tabId,
      binding_id: bindingID,
      at: this.deps.now(),
      ...(job === undefined ? {} : { job_id: job.job_id }),
      ...(grant === undefined
        ? {}
        : {
            authentication_claim_id: grant.authenticationClaimID,
            gate_occurrence_id: grant.gateOccurrenceID,
            ...(this.lastKnownBrowserHolderGeneration === undefined
              ? {}
              : {
                  browser_holder_generation:
                    this.lastKnownBrowserHolderGeneration,
                }),
          }),
    };
    this.navigationErrorMarkerEntries.set(d.tabId, entry);
    this.persistNavigationErrorMarkers();
  }
  private persistNavigationErrorMarkers(): void {
    if (this.deps.navigationErrorMarkers === undefined) return;
    const record: Record<string, NavigationErrorMarkerEntry> = {};
    for (const [tabID, entry] of this.navigationErrorMarkerEntries) {
      record[String(tabID)] = entry;
    }
    void this.deps.navigationErrorMarkers.set(record).catch(() => {});
  }
  /** Consume the durable marker exactly where the worker-local one is
   * consumed: a settled successful landing (discard) or a settled
   * unsuccessful one (the real emitClaimObservation call is about to
   * durably represent it in the claim_observation outbox instead). */
  private clearNavigationErrorMarker(tabID: number): void {
    if (this.navigationErrorMarkerEntries.delete(tabID)) {
      this.persistNavigationErrorMarkers();
    }
  }
  private async reconcilePendingDeliveryOnStartup(): Promise<void> {
    const pending = this.store.pendingDelivery;
    if (pending === undefined) return;
    if (pending.status === "sending" && !this.deliveryJobs.has(pending.job_id)) {
      let live = false;
      try {
        const found = await this.deps.downloads.search({ filename: jobDownloadFilename(pending.job_id) });
        live = found.length > 0;
      } catch {}
      if (!live) {
        await this.update((s) => clearPendingDelivery(s, pending.job_id));
        this.deliveryJobs.delete(pending.job_id);
      }
    }
  }

  private async startDeliveryDownload(
    jobID: string,
    url: string,
  ): Promise<boolean> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return false;
    const ownedJobID = jobID;
    const effectToken = this.claimEffectGovernor(ownedJobID);
    if (effectToken === undefined) return false;
    try {
      await this.update((s) =>
        patchJob(s, jobID, { download_initiated: true }),
      );
    } catch {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
      return false;
    }
    this.deliveryJobs.add(jobID);
    this.downloads.set(jobID, {
      ids: new Set<number>(),
      ambiguous: false,
      directOffer: false,
      delivery: true,
    });
    this.pendingDownloadURLs.set(url, jobID);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: jobDownloadFilename(jobID),
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(jobID);
      if (track !== undefined) {
        track.ids.add(id);
        if (track.ids.size > 1) track.ambiguous = true;
        this.downloads.set(jobID, track);
      }
      return true;
    } catch {
      this.downloads.delete(jobID);
      this.deliveryJobs.delete(jobID);
      await this.update((s) =>
        updatePendingDelivery(
          patchJob(s, jobID, { download_initiated: false }),
          jobID,
          { status: "failed", error: "Could not start the browser download" },
        ),
      );
      return false;
    } finally {
      this.pendingDownloadURLs.delete(url);
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
    }
  }

  async startPDFDelivery(
    payload: DeliveryStartPayload,
  ): Promise<DeliveryReply> {
    await this.ready;
    // Choice accept path — handle before normal resolution, but after tab lookup
    if (payload.choice !== undefined) {
      const entry = this.consumeDeliveryChoice(payload.choice);
      if (entry === undefined) {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      // Revalidate before any await: job still awaiting_download, page identity still matches
      const pickedJob = findByJob(this.store, payload.choice.job_id);
      if (pickedJob === undefined || pickedJob.status !== "awaiting_download") {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      let tabCheck: TabInfo | undefined;
      try {
        tabCheck = await this.deps.tabs.get(payload.tab_id);
      } catch {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      // `Tab.url` is optional by API contract. Its absence means papio cannot
      // see what this tab is showing — it is not a licence to fall back on the
      // popup's own claim about the page it thinks it was on. Refuse.
      if (typeof tabCheck.url !== "string" || tabCheck.url.length === 0) {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      const liveTabURL = tabCheck.url;
      const liveEpoch = await this.liveDocumentEpoch(payload.tab_id);
      if (liveEpoch === undefined) {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      const liveIdentity: PageIdentity = {
        tab_id: payload.tab_id,
        nav_seq: this.pageNavSeq.get(payload.tab_id) ?? 0,
        source_url: this.pageSourceToken(liveTabURL),
        document_id: liveEpoch,
      };
      if (!this.pageIdentityMatches(entry.pageIdentity, liveIdentity)) {
        return { ok: false, state: "failed", code: "choice_expired", message: "That choice expired — click Send this PDF again." } as unknown as DeliveryReply;
      }
      this.destroyDeliveryChoiceForTab(payload.tab_id);
      const urlForChoice = this.comparableDeliverySourceURL(liveTabURL);
      const pending = this.store.pendingDelivery;
      const isStuckSending = pending !== undefined && pending.status === "sending" && pending.job_id === pickedJob.job_id;
      if (isStuckSending) {
        let liveDownload = false;
        try {
          const found = await this.deps.downloads.search({ filename: jobDownloadFilename(pending.job_id) });
          liveDownload = found.length > 0;
        } catch {}
        if (!liveDownload && !this.deliveryJobs.has(pending.job_id)) {
          await this.update((s) => clearPendingDelivery(s, pending.job_id));
        } else {
          return { ok: true, state: pending.status ?? "sending", job_id: pickedJob.job_id } as DeliveryReply;
        }
      } else if (pending !== undefined && pending.status !== "failed" && pending.job_id !== pickedJob.job_id) {
        return failure("delivery_busy", "Another PDF is already being sent to papio") as unknown as DeliveryReply;
      } else if (pending?.job_id === pickedJob.job_id && pending.status !== "failed") {
        return { ok: true, state: pending.status ?? "sending", job_id: pickedJob.job_id } as DeliveryReply;
      }
      if (requiresNativeViewerDownload(urlForChoice)) {
        if (this.isFirefox())
          // Firefox has no onDeterminingFilename, so a download papio did not
          // start cannot be steered or adopted: `correlate()` refuses it before
          // any host or tab ownership is considered. Promising to file the
          // viewer's download here would leave the delivery waiting forever.
          return failure(
            "not_permitted",
            "This publisher's link can only be used once, and papio can't adopt a download on Firefox — open this PDF in Chrome to send it",
          ) as unknown as DeliveryReply;
        const message = "Use the PDF viewer Download button — papio will adopt that authorized file";
        const deliveryPageHostAtStart = sanitizePageHost(liveTabURL);
        const sessionEvidenceAtStart = this.currentSessionEvidence(pickedJob);
        const pageIdentityForContinuation: PageIdentity = entry.pageIdentity;
        await this.update((s) => {
          const activeJobs = s.activeJobs.map((candidate) => {
            if (candidate.job_id === pickedJob.job_id) {
              return { ...candidate, tab_id: payload.tab_id, status: "awaiting_download" as const, download_initiated: false };
            }
            return candidate;
          });
          return startPendingDelivery(
            { ...s, activeJobs },
            {
              job_id: pickedJob.job_id,
              url: urlForChoice,
              initiated_at: this.deps.now(),
              status: "waiting_manual",
              error: message,
              ...(deliveryPageHostAtStart !== undefined ? { page_host: deliveryPageHostAtStart } : {}),
              session_evidence: sessionEvidenceAtStart,
              page_identity: pageIdentityForContinuation,
            },
          );
        });
        this.deliveryJobs.add(pickedJob.job_id);
        this.lastDeliveryState = undefined;
        return { ok: true, state: "waiting_manual", job_id: pickedJob.job_id, message } as DeliveryReply;
      }
      const deliveryPageHostAtStart = sanitizePageHost(liveTabURL);
      const sessionEvidenceAtStart = this.currentSessionEvidence(pickedJob);
      await this.update((s) =>
        startPendingDelivery(s, {
          job_id: pickedJob.job_id,
          url: urlForChoice,
          initiated_at: this.deps.now(),
          status: "sending",
          ...(deliveryPageHostAtStart !== undefined ? { page_host: deliveryPageHostAtStart } : {}),
          session_evidence: sessionEvidenceAtStart,
        }),
      );
      this.lastDeliveryState = undefined;
      const started = await this.startDeliveryDownload(pickedJob.job_id, urlForChoice);
      if (!started) return failure("download_start", "Could not start the browser download") as unknown as DeliveryReply;
      return { ok: true, state: "sending", job_id: pickedJob.job_id } as DeliveryReply;
    }
    if (
      !Number.isSafeInteger(payload.tab_id) ||
      payload.tab_id < 0 ||
      payload.url.length === 0
    ) {
      return failure("invalid_request", "Invalid PDF delivery request");
    }
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(payload.tab_id);
    } catch {
      return failure(
        "tab_unavailable",
        "The current PDF tab is no longer available",
      );
    }
    // `Tab.url` is optional by API contract; absent means papio cannot read the
    // page. Substituting the popup's claim would let stale caller-supplied
    // input decide which bytes get filed under which paper.
    if (typeof tab.url !== "string" || tab.url.length === 0) {
      return failure(
        "tab_unavailable",
        "papio can't read this tab — reload the page, then click Send this PDF again",
      );
    }
    const tabURL = tab.url;
    const viewerPDFURL = providerViewerPDFURL(tabURL, this.deps.adapterSpecs);
    const url = viewerPDFURL ?? this.comparableDeliverySourceURL(tabURL);
    if (viewerPDFURL === undefined && !isPDFPage(tabURL) && !isPDFPage(url)) {
      return failure("not_pdf", "No PDF detected on this page");
    }
    const doi = payload.doi;
    let job: ActiveJob | undefined =
      findByTab(this.store, payload.tab_id) ??
      this.deliveryJobForOpener(tab) ??
      this.deliveryJobForDOI(doi);
    let duplicate = false;
    if (job === undefined) {
      if (doi === undefined || doi.trim() === "") {
        if (requiresNativeViewerDownload(url) && this.isFirefox()) {
          // The old copy here promised papio would file the viewer's download,
          // which on Firefox it cannot: there is no onDeterminingFilename, so
          // `correlate()` refuses a download papio did not start.
          return failure(
            "not_permitted",
            "This publisher's link can only be used once, and papio can't adopt a download on Firefox — open this PDF in Chrome to send it",
          );
        }
        const candidates = this.advisoryCandidates();
        if (candidates.length > 0) {
          const pageIdentity = await this.currentPageIdentity(payload.tab_id, url);
          if (pageIdentity === undefined) {
            return failure(
              "page_unverified",
              "papio can't confirm which document this tab is showing — reload the page, then click Send this PDF again",
            ) as unknown as DeliveryReply;
          }
          const offer = this.mintDeliveryChoice(pageIdentity, candidates);
          return { ok: true, state: "needs_choice", choice: offer } as DeliveryReply;
        }
        if (this.pdfGrabAvailable()) {
          const grab = await this.requestPdfGrab({
            tab_id: payload.tab_id,
            url,
            title: payload.title,
          });
          if (grab.ok && grab.awaiting_viewer === true)
            // papio started no download, so it must not claim to be sending
            // one. The grab is armed; the researcher's own Download click is
            // the step that completes it.
            return {
              ok: true,
              state: "waiting_manual",
              message:
                "Use the PDF viewer Download button — papio will adopt that authorized file",
            };
          if (grab.ok)
            return {
              ok: true,
              state: "sending",
              message: "papio will identify this PDF from the file",
            };
          return grab as unknown as DeliveryReply;
        } else {
          // Same three causes as requestPdfGrab's own guard, and deliberately
          // the same words: this PDF carries no page identifier, and the lane
          // that would identify it from the file is unavailable.
          return failure("no_doi", this.grabUnavailableText());
        }
      }
      if (job === undefined) {
        // `requestPageAcquire` reduces `url` to an origin at the wire boundary,
        // so the full PDF URL is passed here and never leaves the extension.
        const ack = await this.requestPageAcquire({
          url,
          ...(doi !== undefined && doi.trim() !== "" ? { doi } : {}),
          ...(payload.title ? { title: payload.title } : {}),
          source: "popup",
        });
        if (ack.error !== undefined) return failure("page_acquire", ack.error);
        if (ack.job_id === undefined)
          return failure("page_acquire", "The daemon did not return a job");
        duplicate = ack.duplicate === true;
        await this.inboundChain;
        job = findByJob(this.store, ack.job_id);
        // KNOWN REGRESSION, deliberately left as a clear refusal rather than a
        // silent data loss. Before `outcome` existed, a terminal ready/imported
        // job was invisible to this check, so Send PDF created a fresh job and
        // filed the file. Now it reports duplicate, and this refuses.
        //
        // Bypassing the refusal is WORSE: it synthesizes an ActiveJob for a
        // terminal job, and `parkForBrowserAdoption` rejects terminal states
        // (internal/app/browser_adopt.go), so `download_complete` returns
        // ErrAdoptNotAwaiting and the PDF is dropped with no message. A refusal
        // the researcher can read beats a file that vanishes.
        //
        // The real fix is a forced re-submission for this path, which needs a
        // daemon-side option this change set does not add.
        if (job === undefined && duplicate) {
          return failure(
            "duplicate_not_live",
            "That paper is already queued, but its job is not live in this browser",
          );
        }
        if (job === undefined) {
          const now = this.deps.now();
          const synthetic: ActiveJob = {
            job_id: ack.job_id,
            tab_id: payload.tab_id,
            offered_at: now,
            expires_at: now + 24 * 60 * 60_000,
            status: "accepted",
            provider_hosts: [],
            ...(payload.title || doi
              ? {
                  expected: {
                    ...(payload.title ? { title: payload.title } : {}),
                    ...(doi ? { doi } : {}),
                  },
                }
              : {}),
          };
          await this.update((s) => upsertJob(s, synthetic));
          job = synthetic;
        }
      }
    }
    const pending = this.store.pendingDelivery;
    if (
      pending !== undefined &&
      pending.status !== "failed" &&
      pending.job_id !== job.job_id
    ) {
      this.destroyDeliveryChoiceForTab(payload.tab_id);
      return failure(
        "delivery_busy",
        "Another PDF is already being sent to papio",
      );
    }
    if (pending?.job_id === job.job_id && pending.status !== "failed") {
      return {
        ok: true,
        state: pending.status ?? "sending",
        job_id: job.job_id,
        ...(duplicate ? { duplicate: true } : {}),
      };
    }
    if (requiresNativeViewerDownload(url)) {
      if (this.isFirefox())
        // Same reason as the choice path above: Firefox cannot steer or adopt a
        // download papio did not start, so this instruction would be a promise
        // the platform cannot keep.
        return failure(
          "not_permitted",
          "This publisher's link can only be used once, and papio can't adopt a download on Firefox — open this PDF in Chrome to send it",
        );
      const message =
        "Use the PDF viewer Download button — papio will adopt that authorized file";
      const deliveryPageHostAtStart = sanitizePageHost(tabURL);
      const sessionEvidenceAtStart = this.currentSessionEvidence(job);
      // The manual continuation outlives this call and survives worker
      // restarts, so it is authority-bearing: mint it only against a document
      // epoch the browser can hand back later for comparison.
      const pageIdentity = await this.currentPageIdentity(payload.tab_id, url);
      if (pageIdentity === undefined) {
        return failure(
          "page_unverified",
          "papio can't confirm which document this tab is showing — reload the page, then click Send this PDF again",
        );
      }
      await this.update((s) => {
        const activeJobs = s.activeJobs.map((candidate) => {
          if (candidate.job_id === job.job_id) {
            return {
              ...candidate,
              tab_id: payload.tab_id,
              status: "awaiting_download" as const,
              download_initiated: false,
            };
          }
          return candidate;
        });
        return startPendingDelivery(
          { ...s, activeJobs },
          {
            job_id: job.job_id,
            url,
            initiated_at: this.deps.now(),
            status: "waiting_manual",
            error: message,
            ...(deliveryPageHostAtStart !== undefined
              ? { page_host: deliveryPageHostAtStart }
              : {}),
            session_evidence: sessionEvidenceAtStart,
            page_identity: pageIdentity,
          },
        );
      });
      this.deliveryJobs.add(job.job_id);
      this.lastDeliveryState = undefined;
      return {
        ok: true,
        state: "waiting_manual",
        job_id: job.job_id,
        message,
        ...(duplicate ? { duplicate: true } : {}),
      };
    }
    // Freeze the requesting page's host alongside the URL. The tab stays
    // interactive for the whole download, so this is the only moment the
    // page that actually produced these bytes is known for certain.
    const deliveryPageHostAtStart = sanitizePageHost(tabURL);
    // state, not scoped to this tab or download, so an institutional probe
    // or sign-in landing anywhere in the browser during the multi-second
    // download must not retroactively credit this delivery.
    // deliveryEvidenceFor reads this frozen value back at completion instead
    // of re-reading live state.
    const sessionEvidenceAtStart = this.currentSessionEvidence(job);
    await this.update((s) =>
      startPendingDelivery(s, {
        job_id: job.job_id,
        url,
        initiated_at: this.deps.now(),
        status: "sending",
        ...(deliveryPageHostAtStart !== undefined
          ? { page_host: deliveryPageHostAtStart }
          : {}),
        session_evidence: sessionEvidenceAtStart,
      }),
    );
    this.lastDeliveryState = undefined;
    const started = await this.startDeliveryDownload(job.job_id, url);
    if (!started) {
      return failure("download_start", "Could not start the browser download");
    }
    return {
      ok: true,
      state: "sending",
      job_id: job.job_id,
      ...(duplicate ? { duplicate: true } : {}),
    };
  }

  deliveryState(): DeliveryReply {
    const pending = this.store.pendingDelivery;
    if (pending !== undefined) {
      return {
        ok: true,
        state: pending.status ?? "sending",
        job_id: pending.job_id,
        ...(pending.error ? { message: pending.error } : {}),
      };
    }
    if (
      this.lastDeliveryState !== undefined &&
      this.deps.now() - this.lastDeliveryState.at < 10 * 60_000
    ) {
      return {
        ok: true,
        state: this.lastDeliveryState.state,
        job_id: this.lastDeliveryState.job_id,
        message: this.lastDeliveryState.message,
      };
    }
    return { ok: true, state: "idle" };
  }

  private failPageAcquireWaiters(error: string): void {
    for (const resolve of this.pageAcquireWaiters.values()) resolve({ error });
    this.pageAcquireWaiters.clear();
  }

  /** Focus an existing inbox tab before creating one. This is browser-local UI
   * state only; no tab id is retained because a worker can disappear at will. */
  async openInbox(inboxURL: string): Promise<void> {
    const effectJobID = `inbox:${inboxURL}`;
    const effectToken = this.claimEffectGovernor(effectJobID);
    if (effectToken === undefined) return;

    try {
      const existing = (await this.deps.tabs.query?.({ url: inboxURL })) ?? [];
      const tab = existing.find((candidate) => candidate.id !== undefined);
      if (tab?.id !== undefined) {
        await this.focusOwnedTab(tab.id);
        if (tab.windowId !== undefined && this.deps.windows !== undefined) {
          await this.deps.windows.update(tab.windowId, { focused: true });
        }
        return;
      }
      await this.deps.tabs.create({ url: inboxURL, active: true });
    } finally {
      this.releaseEffectGovernor(effectJobID, effectToken, false);
      this.wakeEffectGovernor();
    }
  }

  /** Open a hand-fetched paper on the page it actually lives on. The daemon
   * mints the institution's route for this gesture — the same route a handoff
   * gets — because the item's canonical link paywalls almost every one of
   * these. No delivery authority is taken and the page is never driven: the
   * researcher asked to fetch this one themselves. */
  async openManualDownload(
    payload: ManualOpenPayload,
  ): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    const minted = await this.requestFreshHandoffLink(payload.job_id);
    if (minted.ok !== true) return minted;
    const effectToken = this.claimEffectGovernor(payload.job_id);
    if (effectToken === undefined) {
      return failure(
        "manual_open_busy",
        "Another browser action for this job is still finishing",
      );
    }
    try {
      await this.deps.tabs.create({ url: minted.url, active: true });
    } catch {
      return failure(
        "tab_unavailable",
        "The manual-download page could not be opened",
      );
    } finally {
      this.releaseEffectGovernor(payload.job_id, effectToken, false);
      this.wakeEffectGovernor();
    }
    return { ok: true, opened: true };
  }
  /** Surface the browser-owned handoff already offered for an inbox row. This
   * boundary accepts only a job id: provider/resolver URLs remain local to the
   * extension and are never returned to the caller. */
  async openHandoff(jobID: string): Promise<BrokerReply<{ opened: true }>> {
    const pending = this.openHandoffRequests.get(jobID);
    if (pending !== undefined) return pending;
    const request = this.openHandoffUnlocked(jobID);
    this.openHandoffRequests.set(jobID, request);
    try {
      return await request;
    } finally {
      if (this.openHandoffRequests.get(jobID) === request) {
        this.openHandoffRequests.delete(jobID);
      }
    }
  }
  /** Mints a fresh sign-in surface for `jobID`. Per plan lines 329-336: when
   * this job has a live institutional candidate, the daemon's claim
   * arbitration is consulted before any tab exists — `navigate_existing`/
   * `focus_owner` route to the claim's already-live owner tab through the
   * renavigation fence; `park`/`refuse` never mint. `open_new` grants this
   * job's candidate the lease: an EXPLICIT operator gesture (the
   * engagement the lease exists to gate) mints the fresh link directly
   * below, exactly as before Slice 3; an AUTOMATIC drive never self-mints
   * a materialization binding — the extension never runs its own
   * claim/bind/scaffold sequence. It stays tabless and holds the grant;
   * the daemon admits a fresh `institutional_candidate_offer` for this
   * now-admitted claim within its next poll (Slice 4), and the existing
   * `onInstitutionalCandidateOffer`/`runMaterialization` pipeline — the
   * ONLY automatic surface path — drives the real scaffold from there. A
   * job with no candidate (institutional_candidate_offer never ran for
   * it) never mints automatically either: `claim_identity_known` alone is
   * not a claim. An explicit operator engagement with no candidate mints
   * exactly as before Slice 3 — no daemon-side collision avoidance exists
   * for it, matching the retirement of the extension-local
   * federatedLoginOwners mechanism this function used to implement that
   * with. */
  private async openFreshHandoff(
    jobID: string,
    job: ActiveJob,
    trigger: "automatic" | "explicit",
  ): Promise<BrokerReply<{ opened: true }>> {
    if (this.mintingFreshHandoffs.has(jobID)) {
      return failure(
        "handoff_opening",
        "A sign-in surface for this job is already being created",
      );
    }
    // Reserve-before-consult: the latch must cover
    // consultAuthenticationClaim itself, not only the mint that can follow
    // it — two concurrent wakes racing the has() check above must still
    // produce exactly one authentication_claim_request, never two.
    this.mintingFreshHandoffs.add(jobID);
    try {
      const candidateID = this.materializationCorrelation(jobID)?.candidate_id;
      if (candidateID !== undefined) {
        const consult = await this.consultAuthenticationClaim(
          jobID,
          candidateID,
          trigger,
        );
        if (consult.kind === "navigate_existing") {
          const ownerTabHint = consult.ownerTabHint;
          const focused =
            ownerTabHint !== undefined &&
            (await this.focusClaimOwnerTab(ownerTabHint));
          if (!focused) {
            // Only a proven-absent owner tab authorizes retiring the claim.
            // An unproven failure still parks this job — it cannot proceed
            // without the surface — but it reports nothing, because a false
            // loss report frees the institution and invites the duplicate
            // sign-in tab this mechanism exists to prevent.
            if (await this.claimOwnerSurfaceGone(consult.ownerTabHint))
              this.reportDeadClaimSurface(jobID, consult.ownerBindingID);
            await this.parkForEngagement(jobID);
            return failure(
              "handoff_unavailable",
              "The claimed sign-in surface is no longer live",
            );
          }
          await this.update((s) =>
            patchJob(s, jobID, {
              tab_id: ownerTabHint,
              status: "auth_pending",
              engagement_required: false,
            }),
          );
          this.registerHandoffDrive(jobID, ownerTabHint);
          return { ok: true, opened: true };
        }
        if (consult.kind === "focus_owner") {
          // Same proof, other branch: the sibling this job was told to wait
          // behind has no surface, so waiting is waiting for nothing.
          if (!(await this.focusClaimOwnerTab(consult.ownerTabHint))) {
            // Same proof requirement, other branch.
            if (await this.claimOwnerSurfaceGone(consult.ownerTabHint))
              this.reportDeadClaimSurface(jobID, consult.ownerBindingID);
            await this.parkForEngagement(jobID);
            return failure(
              "handoff_unavailable",
              "The claimed sign-in surface is no longer live",
            );
          }
          await this.parkOnClaim(jobID);
          return { ok: true, opened: true };
        }
        if (consult.kind === "park") {
          await this.parkOnClaim(jobID, consult.dependentCount);
          return failure(
            "handoff_busy",
            "Another sign-in for this institution is already in progress",
          );
        }
        if (consult.kind === "refuse") {
          await this.parkForEngagement(jobID);
          return failure(
            "handoff_unavailable",
            "The daemon could not authorize a sign-in surface right now",
          );
        }
        // consult.kind === "open_new": the authentication claim is
        // granted for this job's candidate.
        //
        // Architecture ruling: the extension never self-mints a
        // materialization binding. That holds for an EXPLICIT operator open
        // too, and used not to. The daemon's explicit open queues a candidate
        // offer AND a handoff_focus (bridge.go's serviceMaterializationCandidate
        // beside its focus frame), and the focus arrives while the correlation
        // is still pre-bind - exactly when tab_id is legitimately -1 and the
        // consult therefore answers open_new (§2.1.1 case 3, no owner surface
        // to navigate yet). Minting here raced the very scaffold that offer was
        // building and left TWO papio tabs for one paper: verified live twice,
        // one stranded at materialize.html for the same binding the daemon had
        // already navigated. The trigger decides whether someone is waiting for
        // the surface, never who is allowed to create it.
        //
        // Two conditions, both necessary. The daemon must have negotiated
        // institutional_materialization_v1 - the same gate it uses itself
        // (institutionalMaterializationAvailable); without it no candidate offer
        // can arrive, no scaffold can be built, and an explicit engagement is the
        // only surface path there is. And the pipeline must actually be driving
        // THIS job right now: a run in flight, a rerun queued, or one waiting on
        // the effect governor or a retry timer. A correlation nothing is driving
        // (hydrated but unscheduled, failed, or its candidate expired) will not
        // produce a surface, so the mint below stays the fallback it was before
        // Slice 3. A finished run needs no branch here: it left a tab, and the
        // tab_id check above focuses it.
        const materializing = this.materializationCorrelation(jobID);
        const pipelineOwnsSurface =
          (this.store.daemonFeatures ?? []).includes(
            INSTITUTIONAL_MATERIALIZATION_FEATURE,
          ) &&
          materializing !== undefined &&
          materializing.phase !== "failed" &&
          Date.parse(materializing.candidate_expires_at) > this.deps.now() &&
          (this.materializationRuns.has(jobID) ||
            this.materializationReruns.has(jobID) ||
            this.pendingMaterializationEffects.has(jobID) ||
            this.materializationRetryTimers.has(jobID));
        if (trigger === "automatic" || pipelineOwnsSurface) {
          await this.parkOnClaim(jobID);
          return failure(
            "handoff_pending",
            "The sign-in surface will open once the daemon materializes it",
          );
        }
        return this.mintFreshHandoffDirect(jobID, job);
      }
      // No live institutional candidate ever ran for this job.
      if (trigger === "automatic") {
        // Slice 0 containment, closed even when claim_identity_known: an
        // automatic release only ever answers a live daemon-arbitrated
        // candidate (the branch above) — identity metadata alone never
        // opens a surface autonomously.
        await this.parkForEngagement(jobID);
        return failure(
          "handoff_unavailable",
          "The daemon could not authorize a sign-in surface right now",
        );
      }
      if (job.claim_identity_known !== true) {
        // Neither identity source (a job_offer's login_entity_id, an
        // institutional_candidate_offer's candidate id, or a durably-marked
        // prior sighting of either — job.claim_identity_known) ever ran for
        // this job. There is nothing here to identify an institution to
        // arbitrate or mint against; refuse locally before any wire call,
        // exactly like every other structured engagement failure.
        return failure(
          "missing_claim",
          "The handoff is missing institution identity metadata",
        );
      }
      return this.mintFreshHandoffDirect(jobID, job);
    } finally {
      this.mintingFreshHandoffs.delete(jobID);
    }
  }

  /** Legacy direct mint: request a one-use handoff link and open it in a
   * fresh tab immediately — no scaffold, no daemon materialization claim.
   * Reached only for an explicit operator engagement (the operator's own
   * gesture on either a granted claim or a no-candidate job); the caller
   * already holds the mintingFreshHandoffs latch for jobID. */
  private async mintFreshHandoffDirect(
    jobID: string,
    job: ActiveJob,
  ): Promise<BrokerReply<{ opened: true }>> {
    const minted = await this.requestFreshHandoffLink(jobID);
    if (!minted.ok) return minted;
    const current = findByJob(this.store, jobID);
    if (current === undefined) {
      return failure(
        "handoff_unavailable",
        "The handoff changed before it could be opened",
      );
    }
    this.offerURLs.set(jobID, minted.url);
    let materializedTabID: number | undefined;
    let recordSave: Promise<void> | undefined;
    const onTabMaterialized = (tabID: number): void => {
      materializedTabID = tabID;
      recordSave = this.update((s) =>
        patchJob(s, jobID, {
          tab_id: tabID,
          status: "accepted",
          engagement_required: false,
          fresh_handoff: true,
        }),
      );
    };
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      this.pendingFreshHandoffs.set(jobID, { job, trigger: "explicit" });
      return failure(
        "effect_busy",
        "Handoff will open when the current browser effect finishes",
      );
    }
    let returnedTabID: number | undefined;
    try {
      returnedTabID = await this.openManagedTab({
        url: minted.url,
        jobId: jobID,
        purpose: "session-signin",
        onTabMaterialized,
        privateLedgerURL: true,
      });
      if (recordSave !== undefined) await recordSave;
    } catch {
      returnedTabID = undefined;
    } finally {
      // A minted link is a one-call capability. It is retained only across
      // the synchronous tab-creation handoff, never beyond materialization.
      this.offerURLs.delete(jobID);
    }
    const tabID = returnedTabID ?? materializedTabID;
    if (tabID === undefined) {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
      return failure(
        "tab_creation_failed",
        "The handoff tab could not be created",
      );
    }
    if (findByJob(this.store, jobID) === undefined) {
      // The job was cancelled/removed while the tab was being created.
      // This private one-use tab never bound to a live job — preserving
      // it would let a sibling open a duplicate institutional login.
      await this.closeOwnedTab(tabID, "fresh-materialization-rollback");
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
      return failure(
        "tab_creation_failed",
        "The handoff tab could not be created",
      );
    }
    if (returnedTabID === undefined) {
      await this.recordManagedTab(jobID, tabID);
      await this.ledgerManagedTab(tabID, "session-signin", true, jobID);
    }
    this.beginProviderDrive(jobID);
    this.registerHandoffDrive(jobID, tabID);
    try {
      await this.focusManagedTab(tabID);
    } catch {
      // The managed tab remains available in its papio surface.
    }
    this.releaseEffectGovernor(jobID, effectToken, false);
    this.wakeEffectGovernor();
    return { ok: true, opened: true };
  }

  private async focusExistingHandoff(
    jobID: string,
    tabID: number,
  ): Promise<boolean> {
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      this.enqueueHandoffDrive({
        jobID,
        purpose: "inbox-open",
        focusExisting: true,
      });
      await this.drainHandoffDriveQueue();
      return false;
    }
    try {
      await this.focusManagedTab(tabID);
      return true;
    } finally {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
    }
  }

  private async openHandoffUnlocked(
    jobID: string,
  ): Promise<BrokerReply<{ opened: true }>> {
    await this.ready;
    await this.surfaceReady;
    let job = findByJob(this.store, jobID);
    if (job?.requires_auth === true && !this.hasCurrentHello()) {
      await this.ensureConnected();
      job = findByJob(this.store, jobID);
    }
    if (job !== undefined && job.tab_id >= 0) {
      return (await this.focusExistingHandoff(jobID, job.tab_id))
        ? { ok: true, opened: true }
        : failure(
            "handoff_queued",
            "The handoff is waiting for the active browser effect",
          );
    }
    const engagementRequired =
      job?.requires_auth === true && job.engagement_required === true;
    if (
      job === undefined ||
      (!engagementRequired && !this.offerURLs.has(jobID))
    ) {
      // A just-acquired inbox item can race the native job_offer. Counts is a
      // safe read that prompts the daemon to flush its already-queued frames;
      // perform it at most once, then wait for the inbound FIFO before retrying.
      if (
        this.hasCurrentHello() &&
        (this.store.daemonFeatures ?? []).includes(TRIAGE_SNAPSHOT_FEATURE)
      ) {
        try {
          await this.requestTriageCounts();
        } catch {
          // A refresh failure is indistinguishable from no offer at this local
          // boundary; the bounded retry below returns the actionable result.
        }
      }
      await this.inboundChain;
      job = findByJob(this.store, jobID);
    }
    if (job === undefined) {
      return failure(
        "handoff_unavailable",
        "The requested handoff is not available",
      );
    }
    if (job.tab_id >= 0) {
      return (await this.focusExistingHandoff(jobID, job.tab_id))
        ? { ok: true, opened: true }
        : failure(
            "handoff_queued",
            "The handoff is waiting for the active browser effect",
          );
    }
    if (job.requires_auth === true && job.engagement_required === true) {
      // A Slice 0 legacy park retains its offer URL (the offer predates
      // fresh links or carried no institution identity); the operator's
      // open drives that URL through the forced queued release below —
      // openFreshHandoff would dead-end on requiring a fresh link. Fresh
      // parks have no retained URL and mint a new route.
      if (!this.offerURLs.has(jobID)) {
        return this.openFreshHandoff(jobID, job, "explicit");
      }
    }
    if (!this.offerURLs.has(jobID)) {
      return failure(
        "handoff_unavailable",
        "The requested handoff is not available",
      );
    }

    if (job.status === "queued") {
      // releaseQueuedHandoffs owns the cross-event drain latch. Calling any
      // lower-level opener here would let concurrent inbox clicks create two
      // tabs for the same queued offer.
      await this.releaseQueuedHandoffs(jobID, true);
      job = findByJob(this.store, jobID);
      if (job === undefined || !this.offerURLs.has(jobID) || job.tab_id < 0) {
        return failure(
          "handoff_open_failed",
          "The offered handoff could not be opened",
        );
      }
      return (await this.focusExistingHandoff(jobID, job.tab_id))
        ? { ok: true, opened: true }
        : failure(
            "handoff_queued",
            "The handoff is waiting for the active browser effect",
          );
    }
    if (job.tab_id < 0) {
      this.enqueueHandoffDrive({ jobID, purpose: "inbox-open" });
      await this.drainHandoffDriveQueue();
      job = findByJob(this.store, jobID);
      if (job !== undefined && job.tab_id >= 0) {
        return (await this.focusExistingHandoff(jobID, job.tab_id))
          ? { ok: true, opened: true }
          : failure(
              "handoff_queued",
              "The handoff is waiting for the active browser effect",
            );
      }
      return failure(
        "handoff_queued",
        "The handoff is waiting for an available browser slot",
      );
    }
    const openurl = this.offerURLs.get(jobID);
    if (openurl === undefined) {
      return failure(
        "handoff_open_failed",
        "The offered handoff could not be opened",
      );
    }
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      this.enqueueHandoffDrive({ jobID, purpose: "inbox-open" });
      await this.drainHandoffDriveQueue();
      return failure(
        "handoff_queued",
        "The handoff is waiting for an available browser slot",
      );
    }
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: openurl,
        jobId: jobID,
        purpose: "inbox-open",
      });
    } catch {
      tabID = undefined;
    } finally {
      this.releaseEffectGovernor(jobID, effectToken, false);
      this.wakeEffectGovernor();
    }
    return tabID === undefined
      ? failure(
          "handoff_open_failed",
          "The offered handoff tab could not be focused",
        )
      : { ok: true, opened: true };
  }

  /** A daemon-directed retry may refresh an expired authentication exchange;
   * the inbox and popup retain focus-only behavior so they cannot disrupt a
   * provider page that is already downloading. */
  private async focusDaemonHandoff(jobID: string): Promise<void> {
    await this.ready;
    // handoff_focus is daemon-initiated handoff work, and the fresh-link fetch
    // it triggers (handoff_link_request) is holder-only.
    if (!this.holderRole()) return;
    const job = findByJob(this.store, jobID);
    const openurl = this.offerURLs.get(jobID);
    if (job !== undefined && job.tab_id >= 0 && openurl !== undefined) {
      let needsFreshResolver = false;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        needsFreshResolver =
          job.status === "auth_pending" ||
          (typeof tab.url === "string" && isAuthenticationURL(tab.url));
      } catch {
        // A missing tab is handled by the focus/open fallback below.
      }
      if (needsFreshResolver) {
        const effectToken = this.claimEffectGovernor(jobID);
        if (effectToken === undefined) {
          await this.openHandoff(jobID);
          return;
        }
        let reopened: number | undefined;
        try {
          reopened = await this.openManagedTab({
            url: openurl,
            jobId: jobID,
            purpose: "redrive",
          });
        } finally {
          this.releaseEffectGovernor(jobID, effectToken, false);
          this.wakeEffectGovernor();
        }
        if (reopened !== undefined) {
          if (
            !this.handoffDrives.has(jobID) &&
            this.handoffDrives.size < HANDOFF_DRIVE_LIMIT
          ) {
            this.registerHandoffDrive(jobID, reopened);
          }
          return;
        }
      }
    }
    await this.openHandoff(jobID);
  }

  private nativeFailure(result: NativeRequestResult): BrokerFailure {
    switch (result.kind) {
      case "timeout":
        return failure("timeout", "The daemon did not respond in time");
      case "transport":
        return failure(
          result.code ?? "connection_lost",
          result.message ?? "The daemon is unavailable",
        );
      default:
        return failure(
          result.code ?? "daemon_error",
          result.message ?? "The daemon rejected the request",
        );
    }
  }

  /** A hello acknowledgement belongs to exactly one native port. */
  hasCurrentHello(): boolean {
    return (
      this.port !== null && this.helloAckGeneration === this.portGeneration
    );
  }

  private settleHelloWaiters(acknowledged: boolean): void {
    for (const waiter of this.helloWaiters) waiter(acknowledged);
    this.helloWaiters.clear();
  }

  private waitForCurrentHello(): Promise<boolean> {
    if (this.hasCurrentHello()) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      const waiter = (acknowledged: boolean) => {
        if (!this.helloWaiters.delete(waiter)) return;
        resolve(acknowledged);
      };
      this.helloWaiters.add(waiter);
      this.deps.setTimeout(() => waiter(false), HELLO_WAIT_TIMEOUT_MS);
    });
  }

  /** Foreground requests must never rely on the next passive reconnect tick. */
  private async ensureConnected(): Promise<boolean> {
    await this.ready;
    if (this.hasCurrentHello()) return true;
    this.reconnectAttempts = 0;
    // A denied hello is a settled answer, not a slow one: waiting for an ack
    // the daemon will never send, and reconnecting to ask again, only churns
    // native-host sessions into the daemon's pending list.
    if (
      this.port !== null &&
      this.helloDeniedGeneration === this.portGeneration
    ) {
      return false;
    }
    // A freshly opened port already has a current hello in flight; coalesce
    // foreground callers on that acknowledgement rather than churning ports.
    if (
      this.port !== null &&
      this.helloSentGeneration === this.portGeneration
    ) {
      return this.waitForCurrentHello();
    }
    if (this.port === null) {
      this.closingDeliberately = false;
      this.connect();
    } else {
      this.reconnectForHello();
    }
    return this.waitForCurrentHello();
  }

  private nextRequestID(): string {
    // UUID text is already a valid msg-id once hyphens are removed. A local
    // sequence makes a deterministic test seam and a late echo unable to
    // collide with a later request in this worker lifetime.
    const random = this.deps.randomUUID().replace(/-/g, "");
    const suffix = `_${this.requestIDSequence++}`;
    return random.length + suffix.length <= 64 ? `${random}${suffix}` : random;
  }

  private failPendingNativeRequests(code: string, message: string): void {
    for (const pending of this.pendingNativeRequests.values()) {
      pending.resolve({ kind: "transport", code, message });
    }
    this.pendingNativeRequests.clear();
  }

  private sendCorrelated(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    expectedType: BrowserMessageType,
    jobID?: string,
    suppliedRequestID?: string,
  ): Promise<NativeRequestResult> {
    const requestID = suppliedRequestID ?? this.nextRequestID();
    if (
      typeof requestID !== "string" ||
      requestID.length === 0 ||
      requestID.length > 64 ||
      /[\u0000-\u001f\u007f]/u.test(requestID)
    ) {
      return Promise.resolve({
        kind: "transport",
        code: "invalid_request_id",
        message: "The supplied request id is invalid",
      });
    }
    if (
      typeof payload["request_id"] === "string" &&
      payload["request_id"] !== requestID
    ) {
      return Promise.resolve({
        kind: "transport",
        code: "request_id_mismatch",
        message: "The supplied request id does not match the payload",
      });
    }
    if (this.pendingNativeRequests.has(requestID)) {
      return Promise.resolve({
        kind: "transport",
        code: "duplicate_request_id",
        message: "A request with this id is already pending",
      });
    }
    return new Promise<NativeRequestResult>((resolve) => {
      const pending: PendingNativeRequest = { expectedType, resolve };
      this.pendingNativeRequests.set(requestID, pending);
      this.deps.setTimeout(() => {
        if (this.pendingNativeRequests.get(requestID) !== pending) return;
        this.pendingNativeRequests.delete(requestID);
        resolve({ kind: "timeout" });
      }, TRIAGE_REQUEST_TIMEOUT_MS);
      if (!this.send(type, { ...payload, request_id: requestID }, jobID)) {
        this.pendingNativeRequests.delete(requestID);
        resolve({
          kind: "transport",
          code: "connection_lost",
          message: "The daemon connection was lost before the request was sent",
        });
        this.reconnectForHello();
      }
    });
  }
  private async requestNative(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    expectedType: BrowserMessageType,
    feature: string,
    mutation: boolean,
    jobID?: string,
    suppliedRequestID?: string,
    retryTransport = !mutation,
  ): Promise<NativeRequestResult> {
    if (!CORRELATED_RESULT_TYPES.has(expectedType)) {
      throw new Error(
        `papio: correlated request expects unrouted reply type ${expectedType}`,
      );
    }
    const attempts = retryTransport ? (mutation ? 1 : 2) : 1;
    for (let attempt = 0; attempt < attempts; attempt += 1) {
      if (!(await this.ensureConnected())) {
        // Two different failures reached the researcher as one sentence about
        // an unavailable daemon. A refused session has a remedy the generic
        // copy hides, and the inbox prints this text verbatim.
        return this.helloDeniedGeneration === this.portGeneration
          ? {
              kind: "transport",
              code: "session_busy",
              message:
                "Another browser holds the papio session; run 'papio browser use --latest' to move it here",
            }
          : {
              kind: "transport",
              code: "connection_timeout",
              message: "Could not establish a current daemon session",
            };
      }
      if (!(this.store.daemonFeatures ?? []).includes(feature)) {
        return {
          kind: "response",
          code: "feature_unavailable",
          message: "This daemon does not support the requested inbox feature",
        };
      }
      const result = await this.sendCorrelated(
        type,
        payload,
        expectedType,
        jobID,
        suppliedRequestID,
      );
      if (
        result.kind !== "transport" ||
        !retryTransport ||
        attempt + 1 === attempts
      )
        return result;
      // Reads are safe to retry once after a confirmed transport failure;
      // mutations deliberately return their ambiguous status to the page.
    }
    return {
      kind: "transport",
      code: "connection_lost",
      message: "The daemon is unavailable",
    };
  }

  private supportsFreshHandoffLinks(): boolean {
    return (this.store.daemonFeatures ?? []).includes(HANDOFF_LINK_FEATURE);
  }

  /** Slice 0 containment gate (dev/active/surface-lifecycle-plan.md): an
   * autonomous `requires_auth` surface needs a live daemon session that is
   * the holder AND advertises the authentication-claim feature (ADR-0022
   * Phase 4) AND a live network. hasCurrentHello() ties the negotiated
   * features to the CURRENT port: a disconnected worker's stale feature
   * list must not authorize surface creation during the reconnect gap, and
   * a pending (non-holder) session must not act on another browser's queue.
   * While the gate is closed — every shipped daemon today — institutional
   * work parks tabless as engagement_required; the operator's explicit open
   * (inbox click, popup retry) is the only path to a sign-in surface. */
  private institutionalAuthGateOpen(): boolean {
    return (
      this.hasCurrentHello() &&
      this.holderRole() &&
      (this.store.daemonFeatures ?? []).includes(
        AUTHENTICATION_CLAIM_FEATURE,
      ) &&
      (this.deps.online?.() ?? true)
    );
  }

  /** Retire every Slice 3 worker-memory marker for a job: the claim grant,
   * its dependent-count display hint, the mint latch, and any observation
   * latches this job itself set. Mirrors the old clearFederatedLoginOwnerForJob's
   * role, called from the same job-removal/close paths. Uses the reverse
   * index rather than a key-prefix scan: authentication_claim_id (part of
   * the latch key) is shared by every dependent parked on the same
   * institutional gate, so a prefix scan on it would also wipe a sibling
   * job's still-active latch. */
  private clearClaimGrant(jobID: string): void {
    this.claimGrants.delete(jobID);
    this.claimDependentCounts.delete(jobID);
    this.mintingFreshHandoffs.delete(jobID);
    const keys = this.claimObservationLatchKeysByJob.get(jobID);
    if (keys !== undefined) {
      for (const key of keys) this.claimObservationLatch.delete(key);
      this.claimObservationLatchKeysByJob.delete(jobID);
    }
  }

  /** §2.1/§2.1.1 of dev/active/claim-observation-protocol.md: one daemon
   * transaction resolving whether/how this job's candidate may have a human
   * sign-in surface right now. Only ever called for a job with a live
   * institutional candidate (materializationCorrelation); a job without one
   * never reaches this — item 1's "if a job has none, keep it tabless
   * engagement_required exactly as today" is the caller's job, not this
   * method's. Seeds lastKnownBrowserHolderGeneration from any response that
   * carries one, win or lose. */
  private async consultAuthenticationClaim(
    jobID: string,
    candidateID: string,
    trigger: "automatic" | "explicit",
  ): Promise<ClaimConsultResult> {
    // §4.5: a lease-renewing negotiation must never race the startup
    // replay of unacked pre-restart observations for the SAME occurrence —
    // the daemon needs that backlog applied first to arbitrate correctly.
    // Never awaited by an inbound-frame handler (this method is only ever
    // reached off-chain, from openFreshHandoff's automatic void-call or an
    // explicit non-native message), so awaiting it here cannot deadlock
    // the FIFO it itself depends on.
    await this.outboxReplayed;
    const result = await this.requestNative(
      "authentication_claim_request",
      { candidate_id: candidateID, materialization_kind: "browser_tab", trigger },
      "authentication_claim_response",
      AUTHENTICATION_CLAIM_FEATURE,
      true,
      jobID,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return { kind: "refuse" };
    const p = result.payload as Partial<AuthenticationClaimResponsePayload>;
    if (typeof p.browser_holder_generation === "number")
      this.lastKnownBrowserHolderGeneration = p.browser_holder_generation;

    if (
      (p.outcome === "open_new" ||
        p.outcome === "navigate_existing" ||
        p.outcome === "focus_owner") &&
      typeof p.authentication_claim_id === "string" &&
      typeof p.gate_occurrence_id === "string"
    ) {
      const gateOccurrenceID = p.gate_occurrence_id;
      this.registerClaimGrant(jobID, p.authentication_claim_id, gateOccurrenceID);
      if (p.outcome === "open_new") return { kind: "open_new" };
      if (typeof p.owner_binding_id === "string") {
        const ownerBindingID = p.owner_binding_id;
        const hint =
          p.owner_tab_hint === undefined ? {} : { ownerTabHint: p.owner_tab_hint };
        return p.outcome === "navigate_existing"
          ? { kind: "navigate_existing" as const, ownerBindingID, ...hint }
          : { kind: "focus_owner" as const, ownerBindingID, ...hint };
      }
    }
    if (p.outcome === "park") {
      const dependentCount =
        typeof p.dependent_count === "number" ? p.dependent_count : 0;
      this.claimDependentCounts.set(jobID, dependentCount);
      return { kind: "park", dependentCount };
    }
    return { kind: "refuse" };
  }
  /** Record the claim identity governing this job's surface.
   *
   * §5: the ordinal is monotonic per gate_occurrence_id, never per grant — a
   * fresh grant on the SAME occurrence (a restart re-consulting before its own
   * prior observations drained) must never restart at 0 while those entries
   * still queue; the daemon's unique (occurrence, ordinal) index would reject
   * the collision as stale.
   */
  private registerClaimGrant(
    jobID: string,
    authenticationClaimID: string,
    gateOccurrenceID: string,
  ): void {
    let nextOrdinal = 0;
    for (const entry of this.claimObservationOutboxEntries.values()) {
      if (
        entry.gate_occurrence_id === gateOccurrenceID &&
        entry.event_ordinal >= nextOrdinal
      ) {
        nextOrdinal = entry.event_ordinal + 1;
      }
    }
    this.claimGrants.set(jobID, {
      authenticationClaimID,
      gateOccurrenceID,
      nextOrdinal,
    });
  }

  /** The daemon-orchestrated pipeline's only source of claim identity. Shares
   * `registerClaimGrant` with the consult deliberately: two copies of the
   * ordinal rule would drift, and a wrong ordinal is silently rejected as
   * stale by the daemon rather than failing loudly. */
  private registerClaimGrantFromBind(
    jobID: string,
    payload: Record<string, unknown>,
  ): void {
    const authenticationClaimID = payload["authentication_claim_id"];
    const gateOccurrenceID = payload["gate_occurrence_id"];
    if (
      typeof authenticationClaimID !== "string" ||
      typeof gateOccurrenceID !== "string"
    )
      return;
    this.registerClaimGrant(jobID, authenticationClaimID, gateOccurrenceID);
  }

  /** The daemon just directed this job at another surface for the same
   * institution and that surface does not exist. That is first-hand proof the
   * claim behind it is dead - and it needs no durable ledger record, which is
   * what makes it the recovery of last resort: a record lost to a pre-fix
   * prune, a cleared profile, or a browser restart leaves the claim otherwise
   * immortal (its institutional effect permit is settled, so claim expiry
   * deliberately never retires it) and its institution held forever. Report
   * the loss with the same vocabulary an observed close uses; the reducer
   * abandons the claim and frees the entry, so the operator's NEXT click gets
   * a real surface. Measured live 2026-08-21: one such claim refused 273
   * sibling binds. */
  private reportDeadClaimSurface(jobID: string, ownerBindingID: string): void {
    const grant = this.claimGrants.get(jobID);
    if (grant === undefined) return;
    this.enqueueRestartRecoveredObservation(
      {
        job_id: jobID,
        authentication_claim_id: grant.authenticationClaimID,
        binding_id: ownerBindingID,
        browser_holder_generation: this.lastKnownBrowserHolderGeneration ?? 0,
        gate_occurrence_id: grant.gateOccurrenceID,
      },
      "owner_closed",
    );
  }

  /** True only when a claim's owner tab is PROVEN not to exist.
   *
   * `focusClaimOwnerTab` answers "did I focus it", which is a different
   * question: it returns false for a missing `owner_tab_hint` (the field is
   * optional in the protocol), for a transient `tabs.get` rejection, and for a
   * focus that simply failed. Reporting `owner_closed` on that answer abandons
   * a live binding and frees its institution for a duplicate sign-in surface —
   * the exact duplication this whole mechanism exists to prevent, arriving
   * through the repair path. Absence, or a hint that resolves to some other
   * tab, is proof; everything else is unknown and reports nothing.
   */
  private async claimOwnerSurfaceGone(
    ownerTabHint: number | undefined,
  ): Promise<boolean> {
    if (ownerTabHint === undefined) return false;
    try {
      const tab = await this.deps.tabs.get(ownerTabHint);
      return tab.id !== ownerTabHint;
    } catch (e) {
      return isTabAbsenceRejection(e);
    }
  }

  /** navigate_existing/focus_owner both point at a claim's already-live
   * owner tab. Re-proves `ownerTabHint` live before touching it — the
   * shipped renavigation fence (plan lines 175-184) — a stale hint is never
   * trusted bare. */
  private async focusClaimOwnerTab(ownerTabHint: number | undefined): Promise<boolean> {
    if (ownerTabHint === undefined) return false;
    try {
      const tab = await this.deps.tabs.get(ownerTabHint);
      if (tab.id !== ownerTabHint) return false;
    } catch {
      return false;
    }
    await this.reduceHandoffGroupState(ownerTabHint);
    // Renavigation fence (plan lines 175-184): re-prove the hint
    // immediately before the activating update, no unrelated await
    // between — a stale hint (owner closed, id reused mid-await) is
    // never trusted bare.
    let live: TabInfo;
    try {
      live = await this.deps.tabs.get(ownerTabHint);
      if (live.id !== ownerTabHint) return false;
    } catch {
      return false;
    }
    try {
      await this.focusOwnedTab(ownerTabHint);
    } catch {
      return false;
    }
    if (live.windowId !== undefined && this.deps.windows !== undefined) {
      try {
        const win = await this.deps.windows.get(live.windowId);
        await this.deps.windows.update(live.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // A closed work window is handled by the normal tab-removal path.
      }
    }
    return true;
  }

  /** `park`: tabless, daemon-driven — never engagement_required (that would
   * misreport "needs an operator click" for a state the daemon itself will
   * resolve via a fresh offer once the owning claim clears). dependentCount
   * is a display hint only, never persisted (plan's storage-tier design). */
  private async parkOnClaim(jobID: string, dependentCount?: number): Promise<void> {
    if (dependentCount !== undefined)
      this.claimDependentCounts.set(jobID, dependentCount);
    await this.update((s) =>
      patchJob(s, jobID, { status: "queued", tab_id: -1 }),
    );
  }

  /** `feature_disabled`/`not_eligible`/`busy`/`error`, or a refused/timed-out
   * request: refuse, stay tabless — the standing Slice 0 containment
   * behavior for a closed or failed gate. */
  private async parkForEngagement(jobID: string): Promise<void> {
    await this.update((s) =>
      patchJob(s, jobID, {
        status: "queued",
        tab_id: -1,
        engagement_required: true,
      }),
    );
  }

  /** Persist the mutation-time snapshot after all earlier snapshots settle.
   * Callers that require durability may await the returned promise; the
   * best-effort mutation sites intentionally ignore a failed storage write. */
  private persistClaimObservationOutbox(): Promise<void> {
    const outbox = this.deps.claimObservationOutbox;
    if (outbox === undefined) return Promise.resolve();
    const snapshot = Object.fromEntries(this.claimObservationOutboxEntries);
    const save = this.claimObservationOutboxSaveChain.then(() =>
      outbox.set(snapshot),
    );
    this.claimObservationOutboxSaveChain = save.catch(() => {});
    return save.catch(() => {});
  }

  /** §2.2/§5: append one claim_observation to the durable outbox and
   * best-effort schedule a flush. Never itself awaited — the caller fires
   * this and moves on, satisfying AGENTS.md's "never await a correlated
   * request inside an inbound-frame handler" for every emission site
   * below, since none of them are on the critical path of the frame that
   * triggered them. */
  private enqueueClaimObservation(
    jobID: string,
    bindingID: string,
    eventKind: ClaimObservationPayload["event_kind"],
  ): void {
    const grant = this.claimGrants.get(jobID);
    const generation = this.lastKnownBrowserHolderGeneration;
    if (grant === undefined || generation === undefined) return;
    const ordinal = grant.nextOrdinal;
    grant.nextOrdinal += 1;
    const entry: ClaimObservationOutboxEntry = {
      observation_id: this.deps.randomUUID(),
      job_id: jobID,
      authentication_claim_id: grant.authenticationClaimID,
      binding_id: bindingID,
      browser_holder_generation: generation,
      gate_occurrence_id: grant.gateOccurrenceID,
      event_ordinal: ordinal,
      event_kind: eventKind,
    };
    this.claimObservationOutboxEntries.set(entry.observation_id, entry);
    this.persistClaimObservationOutbox();
    this.scheduleObservationOutboxDrain();
  }

  /** Derive the SAME identity `observation_id` is built from (minus
   * ordinal): `authentication_claim_id`, `binding_id`, `gate_occurrence_id`,
   * `event_kind`. Used to scope at-most-once suppression to a single gate
   * occurrence (oracle finding 4) — a job-only key would keep suppressing a
   * fresh occurrence's first wall/login event with a stale prior
   * occurrence's latch, since consultAuthenticationClaim overwrites
   * claimGrants[jobID] in place on every new grant. Returns undefined if
   * the job has no live grant to scope against. */
  private observationSuppressionKey(
    jobID: string,
    bindingID: string,
    eventKind: ClaimObservationPayload["event_kind"],
  ): string | undefined {
    const grant = this.claimGrants.get(jobID);
    if (grant === undefined) return undefined;
    return `${grant.authenticationClaimID}:${bindingID}:${grant.gateOccurrenceID}:${eventKind}`;
  }

  /** Whether `eventKind` was already latched for `jobID`'s CURRENT gate
   * occurrence. Shares observationSuppressionKey with emitClaimObservation
   * so the login_started gate below can never disagree with the latch it
   * is reading. */
  private hasLatchedObservation(
    jobID: string,
    tabID: number,
    eventKind: ClaimObservationPayload["event_kind"],
  ): boolean {
    const bindingID = this.tabLedgerCache?.[String(tabID)]?.binding_id;
    if (bindingID === undefined) return false;
    const key = this.observationSuppressionKey(jobID, bindingID, eventKind);
    return key !== undefined && this.claimObservationLatch.has(key);
  }

  /** Resolve a claim-owned tab's binding id from the birth-certificate
   * ledger and enqueue one observation for it. `latch`ed kinds (wall/login)
   * fire at most once per gate occurrence — the tab-update handler would
   * otherwise re-fire on every settle while the tab sits still, and a
   * fresh occurrence for the SAME job must still get its own first
   * emission (oracle finding 4). The latch check-and-set stays fully
   * synchronous (tabLedgerCache, not the async ledger snapshot below) so
   * two overlapping calls for the same identity can never both pass it. */
  private async emitClaimObservation(
    jobID: string,
    tabID: number,
    eventKind: ClaimObservationPayload["event_kind"],
    latch = false,
  ): Promise<void> {
    if (!this.claimGrants.has(jobID)) return;
    if (latch) {
      const bindingID = this.tabLedgerCache?.[String(tabID)]?.binding_id;
      if (bindingID === undefined) return;
      const key = this.observationSuppressionKey(jobID, bindingID, eventKind);
      if (key === undefined) return;
      if (this.claimObservationLatch.has(key)) return;
      this.claimObservationLatch.add(key);
      let keys = this.claimObservationLatchKeysByJob.get(jobID);
      if (keys === undefined) {
        keys = new Set<string>();
        this.claimObservationLatchKeysByJob.set(jobID, keys);
      }
      keys.add(key);
    }
    // §4.5: never emit a fresh lease-renewing observation ahead of any
    // unacked backlog from a prior worker lifetime — this call is never
    // awaited by an inbound-frame handler (every call site above is
    // `void`), so waiting here cannot deadlock the FIFO outboxReplayed
    // itself depends on.
    await this.outboxReplayed;
    const ledger = await this.snapshotTabLedger();
    const bindingID = ledger[String(tabID)]?.binding_id;
    if (bindingID === undefined) return;
    this.enqueueClaimObservation(jobID, bindingID, eventKind);
  }

  /** Coalescing trigger for drainObservationOutbox: a drain already
   * running absorbs a concurrent trigger as one more full pass instead of
   * two racing to send the same entries. Exposes its outcome through
   * outboxReplayed for the one other consumer allowed to await it
   * (lease-renewing observation emission). */
  private scheduleObservationOutboxDrain(): void {
    if (this.outboxDrainRunning) {
      this.outboxDrainRerunRequested = true;
      return;
    }
    this.outboxDrainRunning = true;
    this.outboxReplayed = this.drainObservationOutboxLoop()
      .catch((error) =>
        console.error("papio: claim observation outbox drain failed", error),
      )
      .finally(() => {
        this.outboxDrainRunning = false;
        if (this.outboxDrainRerunRequested) {
          this.outboxDrainRerunRequested = false;
          this.scheduleObservationOutboxDrain();
        }
      });
  }

  /** §5's per-poll batch cap (32), reused here as a per-batch cap; loops
   * batches until the outbox is empty or a full pass makes no progress
   * (offline, feature withdrawn, or every request timed out) — retried by
   * the next enqueue, hello_ack, or reconnect instead of busy-looping. */
  private async drainObservationOutboxLoop(): Promise<void> {
    for (;;) {
      const before = this.claimObservationOutboxEntries.size;
      if (before === 0) return;
      await this.drainObservationOutboxBatch();
      if (this.claimObservationOutboxEntries.size >= before) return;
    }
  }

  /** Applied/duplicate/stale terminate the entry locally (a daemon-side
   * rejection is still logged server-side, nothing more to do here except
   * the out-of-order guard below); only a transport failure or an `error`
   * ack leaves it queued for the next drain, retried under the SAME
   * observation_id so the daemon's idempotency table absorbs it. */
  private async drainObservationOutboxBatch(): Promise<void> {
    const entries = [...this.claimObservationOutboxEntries.values()]
      .sort((a, b) =>
        a.gate_occurrence_id === b.gate_occurrence_id
          ? a.event_ordinal - b.event_ordinal
          : a.gate_occurrence_id < b.gate_occurrence_id
            ? -1
            : 1,
      )
      .slice(0, 32);
    if (entries.length === 0) return;
    // Negotiate BEFORE reading a generation off any entry. `requestNative`
    // establishes the port and waits for `hello_ack` itself, so reading
    // `lastKnownBrowserHolderGeneration` above that call captured whatever a
    // fresh worker happened to have — a rehydrated stale value, or nothing at
    // all, falling back to the entry's own historical generation. The daemon
    // then answered `stale` and the branch below discarded the entry as
    // terminal. `bootstrapSurfaceLifecycle` schedules this drain before the
    // first ack lands, so that race WAS the startup path, which is why
    // `claim_observation_journal` stayed empty through every restart this was
    // built to survive. Stamping at drain instead of enqueue was necessary and
    // not sufficient: drain has to happen after the handshake it stamps from.
    // A failure here leaves the whole backlog queued for the next drain.
    if (!(await this.ensureConnected())) return;
    for (const entry of entries) {
      const { job_id: jobID, ...payload } = entry;
      // The generation on the wire is the SENDER's, not the subject's. The
      // daemon's fence is `FrameGeneration != Generation -> stale`, where
      // Generation is its own current holder epoch, so an entry stamped at
      // enqueue time is rejected the moment a reconnect intervenes - and a
      // `stale` ack is terminal below, so the whole replayed backlog was
      // silently discarded by exactly the reconnect §4.5 exists to survive.
      // A restart-recovered owner_closed could never apply at all: its
      // subject generation is historical by construction. The observation's
      // subject stays exactly identified by binding_id + occurrence +
      // ordinal, which this does not touch; carrying the current generation
      // forward into the lease is what the reducer already documents wanting.
      // An older daemon's hello_ack carries no generation, so a fresh worker
      // can reach here never having learned one. Send the entry's own rather
      // than stalling the queue: that is exactly today's behaviour, and the
      // daemon fences a stale value as harmlessly as it always has.
      const generation =
        this.lastKnownBrowserHolderGeneration ?? payload.browser_holder_generation;
      const result = await this.requestNative(
        "claim_observation",
        { ...payload, browser_holder_generation: generation },
        "claim_observation_ack",
        AUTHENTICATION_CLAIM_FEATURE,
        true,
        jobID,
      );
      if (result.kind !== "response" || result.payload === undefined) continue;
      const ack = result.payload as Partial<ClaimObservationAckPayload>;
      if (typeof ack.browser_holder_generation === "number")
        this.lastKnownBrowserHolderGeneration = ack.browser_holder_generation;
      if (typeof ack.gate_occurrence_id === "string") {
        const occurrence = ack.gate_occurrence_id;
        for (const grant of this.claimGrants.values()) {
          if (grant.authenticationClaimID === entry.authentication_claim_id)
            grant.gateOccurrenceID = occurrence;
        }
      }
      if (ack.outcome === "error") continue;
      if (ack.outcome === "rejected") {
        // A rejection can be stale relative to a lower-ordinal sibling for
        // the SAME occurrence that has not sent yet (still queued from a
        // prior partial drain, or later in this very batch after a
        // transport failure) — dropping it now would silently lose that
        // sibling's ordering. Keep it queued only while such a sibling is
        // still pending; otherwise the rejection is final.
        const hasEarlierSibling = [
          ...this.claimObservationOutboxEntries.values(),
        ].some(
          (other) =>
            other.observation_id !== entry.observation_id &&
            other.gate_occurrence_id === entry.gate_occurrence_id &&
            other.event_ordinal < entry.event_ordinal,
        );
        if (hasEarlierSibling) continue;
      }
      if (ack.outcome === "applied" && entry.event_kind === "navigation_error") {
        // §2.3: the daemon has now durably recorded this route as exhausted
        // (the observation itself, not the eventual park response, is the
        // trigger — the park may never come if the job is abandoned first).
        // The scaffold this navigation error happened on is otherwise a
        // permanent dead end: nothing else ever retires it. Detach the job
        // from its tab first (closeOwnedTab's own safety guard — mirrors
        // the HANDOFF_DRIVE_TIMEOUT_MS legacy-park precedent above —
        // refuses to remove a tab a job still tracks) so the close
        // transaction can actually run the removal, not just tombstone it.
        const job = findByJob(this.store, jobID);
        if (job !== undefined && job.tab_id >= 0) {
          const tabID = job.tab_id;
          void (async () => {
            const current = findByJob(this.store, jobID);
            if (current === undefined || current.tab_id !== tabID) return;
            await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
            await this.closeOwnedSurface(
              tabID,
              "claim_abandoned",
              ack.gate_occurrence_id,
            );
          })();
        }
      }
      this.claimObservationOutboxEntries.delete(entry.observation_id);
      this.persistClaimObservationOutbox();
    }
  }

  private async applyMaterialization(
    jobID: string,
    event: MaterializationEvent,
  ): Promise<void> {
    await this.update((store) => reduceMaterialization(store, jobID, event));
  }
  /** Cancel only browser-local work for a materialization job. The daemon's
   * durable claim remains authoritative; resolving pending requests lets any
   * detached run observe the supersession/removal instead of mutating a new
   * correlation. */
  private cancelMaterializationWorkflow(jobID: string): void {
    this.cancelledMaterializationJobs.add(jobID);
    this.releaseHandoffDrive(jobID);
    this.materializationReruns.delete(jobID);
    this.pendingMaterializationEffects.delete(jobID);
    this.materializationRetryTimers.delete(jobID);
    this.materializationOfflineTimers.delete(jobID);
    this.materializationRuns.delete(jobID);
    const pending = this.pendingMaterializationRequests.filter(
      (request) => request.jobID === jobID,
    );
    for (const request of pending) {
      const index = this.pendingMaterializationRequests.indexOf(request);
      if (index >= 0) this.pendingMaterializationRequests.splice(index, 1);
      request.resolve(undefined);
    }
  }

  private materializationCurrent(jobID: string, candidateID: string): boolean {
    return (
      !this.cancelledMaterializationJobs.has(jobID) &&
      this.materializationCorrelation(jobID)?.candidate_id === candidateID
    );
  }
  private async clearMaterializationWorkflow(jobID: string): Promise<void> {
    this.cancelMaterializationWorkflow(jobID);
    await this.applyMaterialization(jobID, { type: "clear" });
  }

  private materializationCorrelation(
    jobID: string,
  ): MaterializationCorrelation | undefined {
    return this.store.materializations?.[jobID];
  }

  private materializationURL(bindingID: string): string | undefined {
    const base = this.deps.runtimeGetURL?.(MATERIALIZE_PAGE_PATH);
    if (base === undefined) return undefined;
    return `${base}#${bindingID}`;
  }

  private async scanMaterializationTabs(): Promise<{
    byBinding: Map<string, TabInfo[]>;
    reliable: boolean;
  }> {
    const byBinding = new Map<string, TabInfo[]>();
    const base = this.deps.runtimeGetURL?.(MATERIALIZE_PAGE_PATH);
    if (base === undefined || this.deps.tabs.query === undefined)
      return { byBinding, reliable: false };
    let tabs: TabInfo[];
    try {
      tabs = await this.deps.tabs.query({ url: `${base}*` });
    } catch {
      return { byBinding, reliable: false };
    }
    let baseURL: URL;
    try {
      baseURL = new URL(base);
    } catch {
      return { byBinding, reliable: false };
    }
    for (const tab of tabs) {
      if (tab.id === undefined || typeof tab.url !== "string") continue;
      try {
        const parsed = new URL(tab.url);
        const fragment = parsed.hash.startsWith("#")
          ? parsed.hash.slice(1)
          : "";
        if (
          parsed.origin !== baseURL.origin ||
          parsed.pathname !== baseURL.pathname ||
          parsed.search !== "" ||
          !MATERIALIZATION_ID_PATTERN.test(fragment)
        )
          continue;
        const list = byBinding.get(fragment) ?? [];
        list.push(tab);
        byBinding.set(fragment, list);
      } catch {
        return { byBinding, reliable: false };
      }
    }
    return { byBinding, reliable: true };
  }

  /** Every internal removal of a materialization surface routes through here,
   * so this is the one place that has to declare the removal papio's own. */
  private async removeMaterializationTab(tabID: number): Promise<void> {
    this.deliberateRemovals.add(tabID);
    let removed = false;
    try {
      removed = await this.closeOwnedTab(tabID, "materialization-reconcile");
    } finally {
      // closeOwnedTab refuses a tab the operator has claimed (active, pinned,
      // navigated away, foreign window). The tab survives, so the marker must
      // not: a genuine operator close later is a real cancellation and has to
      // be reported as one.
      if (!removed) this.deliberateRemovals.delete(tabID);
    }
  }

  private materializationResponseMatches(
    pending: PendingMaterializationRequest,
    msg: BrowserMessage,
  ): boolean {
    if (pending.responseType !== String(msg.type)) return false;
    if (pending.jobID !== undefined && msg.job_id !== pending.jobID)
      return false;
    const payload = msg.payload;
    if (payload["request_id"] !== pending.requestID) return false;
    if (
      pending.candidateID !== undefined &&
      payload["candidate_id"] !== undefined &&
      payload["candidate_id"] !== pending.candidateID
    )
      return false;
    if (
      pending.claimID !== undefined &&
      payload["claim_id"] !== undefined &&
      payload["claim_id"] !== pending.claimID
    )
      return false;
    if (
      pending.bindingID !== undefined &&
      payload["binding_id"] !== undefined &&
      payload["binding_id"] !== pending.bindingID
    )
      return false;
    return true;
  }

  private resolveMaterializationResponse(msg: BrowserMessage): void {
    for (
      let index = 0;
      index < this.pendingMaterializationRequests.length;
      index += 1
    ) {
      const pending = this.pendingMaterializationRequests[index];
      if (
        pending === undefined ||
        !this.materializationResponseMatches(pending, msg)
      )
        continue;
      this.pendingMaterializationRequests.splice(index, 1);
      pending.resolve(msg);
      return;
    }
  }

  private failPendingMaterializationRequests(): void {
    const pending = this.pendingMaterializationRequests.splice(0);
    for (const request of pending) request.resolve(undefined);
  }

  private requestMaterializationResponse(
    requestType: string,
    responseType: string,
    payload: Record<string, unknown>,
    jobID: string | undefined,
    correlation: Pick<
      PendingMaterializationRequest,
      "candidateID" | "claimID" | "bindingID"
    >,
    suppliedRequestID?: string,
  ): Promise<BrowserMessage | undefined> {
    const { promise, resolve } = Promise.withResolvers<
      BrowserMessage | undefined
    >();
    const requestID =
      suppliedRequestID ?? this.deps.randomUUID().replace(/-/g, "");
    const pending: PendingMaterializationRequest = {
      responseType,
      requestID,
      ...(jobID !== undefined ? { jobID } : {}),
      ...correlation,
      resolve,
    };
    this.pendingMaterializationRequests.push(pending);
    if (
      !this.send(
        requestType as BrowserMessageType,
        { ...payload, request_id: requestID },
        jobID,
      )
    ) {
      const index = this.pendingMaterializationRequests.indexOf(pending);
      if (index >= 0) this.pendingMaterializationRequests.splice(index, 1);
      resolve(undefined);
      return promise;
    }
    this.deps.setTimeout(() => {
      const index = this.pendingMaterializationRequests.indexOf(pending);
      if (index < 0) return;
      this.pendingMaterializationRequests.splice(index, 1);
      resolve(undefined);
    }, TRIAGE_REQUEST_TIMEOUT_MS);
    return promise;
  }

  private async materializationRPC(
    requestType: string,
    responseType: string,
    payload: Record<string, unknown>,
    jobID: string | undefined,
    correlation: Pick<
      PendingMaterializationRequest,
      "candidateID" | "claimID" | "bindingID"
    >,
    suppliedRequestID?: string,
  ): Promise<BrowserMessage | undefined> {
    if (
      !(this.store.daemonFeatures ?? []).includes(
        INSTITUTIONAL_MATERIALIZATION_FEATURE,
      )
    )
      return undefined;
    // Claim, bind, route and reconcile are holder-only in the daemon. The one
    // exception is the historical navigation result, which the daemon accepts
    // from any recognized session because it is cleanup authority for a permit
    // this browser already earned — mirrored here so the two agree.
    if (
      requestType !== "institutional_navigated_request" &&
      !this.holderRole()
    )
      return undefined;
    if (!(await this.ensureConnected())) return undefined;
    return this.requestMaterializationResponse(
      requestType,
      responseType,
      payload,
      jobID,
      correlation,
      suppliedRequestID,
    );
  }

  private async materializeScaffold(
    jobID: string,
    correlation: MaterializationCorrelation,
  ): Promise<{ tabID?: number; reliable: boolean }> {
    if (this.cancelledMaterializationJobs.has(jobID))
      return { reliable: false };
    const bindingID = correlation.binding_id;
    if (bindingID === undefined) return { reliable: true };
    const scan = await this.scanMaterializationTabs();
    if (this.cancelledMaterializationJobs.has(jobID))
      return { reliable: false };
    if (!scan.reliable) return { reliable: false };
    const candidates = scan.byBinding.get(bindingID) ?? [];
    // review round finding 1: an operator-active/pinned candidate is ceded
    // permanently, never flipped back to inactive and reclaimed — Chrome's
    // active flag is the one signal papio must never overrule. Adoption is
    // restricted to a candidate that positively re-proves itself as papio's
    // own — ledgered under this exact binding and the current browser
    // epoch, never ceded — so a merely-URL-matching tab (a restored or
    // user-duplicated scaffold papio never actually ledgered) is never
    // mistaken for ownership authority either.
    const ledger = await this.snapshotTabLedger();
    const positivelyOwned = (tab: TabInfo): boolean => {
      if (tab.id === undefined) return false;
      const record = ledger[String(tab.id)];
      return (
        record !== undefined &&
        record.ceded !== true &&
        record.binding_id === bindingID &&
        record.browser_epoch === this.browserEpoch
      );
    };
    const eligible = (tab: TabInfo): boolean =>
      tab.active !== true && tab.pinned !== true && positivelyOwned(tab);
    let chosen: TabInfo | undefined;
    if (correlation.tab_id >= 0) {
      const match = candidates.find((tab) => tab.id === correlation.tab_id);
      if (match !== undefined && eligible(match)) chosen = match;
    }
    chosen ??= candidates.find(eligible);
    // Every other candidate is a duplicate papio must never silently
    // adopt, deactivate, or delete out from under the operator or before
    // proving ownership: active/pinned is ceded permanently (a no-op when
    // unledgered — nothing to mark); a positively owned idle duplicate
    // retires through the same authorized-close transaction every other
    // owned surface uses, fired without blocking this reconciliation on
    // an unrelated tab's daemon round trip; anything else — unledgered,
    // or ledgered under a stale epoch that cannot re-prove itself — is
    // left open and untouched, never directly removed.
    for (const candidate of candidates) {
      if (candidate.id === undefined || candidate.id === chosen?.id)
        continue;
      if (candidate.active === true || candidate.pinned === true) {
        await this.cedeOwnedTab(
          candidate.id,
          ledger[String(candidate.id)]?.binding_id,
          "duplicate_operator_owned",
        );
        continue;
      }
      if (positivelyOwned(candidate)) {
        // Every tab in this loop shares ONE binding with `chosen`, and papio
        // drives `chosen`. So the true fact is that this surface is superseded,
        // not that the scaffold is idle: scaffold_idle asserts a claim phase of
        // claimed/bound, which any navigated claim structurally fails, and the
        // daemon refused it - leaving the operator looking at three tabs on one
        // paper (measured live 2026-08-22, the defect that opened this work).
        void this.closeOwnedSurface(candidate.id, "surface_superseded");
      }
    }
    if (chosen?.id !== undefined) {
      if (correlation.tab_id !== chosen.id) {
        await this.applyMaterialization(jobID, {
          type: "scaffolded",
          tab_id: chosen.id,
        });
      }
      // The close transaction (materialization_settled, §2.3) authorizes by
      // binding_id off this same birth-record ledger every other owned
      // surface uses — without an entry here a settled scaffold could never
      // be retired. Idempotent: a no-op once already ledgered under this
      // browser epoch, and a no-op entirely when no tabLedger backend is
      // configured (ledgerManagedTab's own guard).
      await this.ledgerManagedTab(chosen.id, "materialization", false, jobID, bindingID);
      return { tabID: chosen.id, reliable: true };
    }
    const scaffoldURL = this.materializationURL(bindingID);
    if (scaffoldURL === undefined) return { reliable: false };
    // Placed through the same broker-tab machinery every other papio tab
    // uses (work-window/tab-group/Firefox-fallback, always inactive) so it
    // is a member of the work window or papio tab group: closeOwnedTab's
    // ownership gate (inWorkWindow || inPapioGroup) would otherwise refuse
    // to ever remove it, even after a fully authorized close.
    const createdID = await this.openBrokerTab(scaffoldURL, false);
    if (createdID === undefined) return { reliable: false };
    await this.applyMaterialization(jobID, {
      type: "scaffolded",
      tab_id: createdID,
    });
    await this.ledgerManagedTab(createdID, "materialization", false, jobID, bindingID);
    return { tabID: createdID, reliable: true };
  }

  private materializationRetryExpiry(
    correlation: MaterializationCorrelation,
    phase: "claim" | "bind",
  ): number | undefined {
    const raw =
      phase === "claim"
        ? correlation.candidate_expires_at
        : correlation.lease_until;
    if (typeof raw !== "string" || !MATERIALIZATION_RFC3339_PATTERN.test(raw))
      return undefined;
    const expiry = Date.parse(raw);
    return Number.isFinite(expiry) && expiry > this.deps.now()
      ? expiry
      : undefined;
  }

  private async retryMaterializationAfterResponseLoss(
    jobID: string,
    phase: "claim" | "bind" | "route" | "navigated",
  ): Promise<void> {
    const correlation = this.materializationCorrelation(jobID);
    if (correlation === undefined) return;
    const expectedPhase: MaterializationPhase =
      phase === "claim"
        ? "claiming"
        : phase === "bind"
          ? correlation.phase
          : phase === "route"
            ? "bound"
            : "navigating";
    if (
      (phase === "bind" &&
        correlation.phase !== "claimed" &&
        correlation.phase !== "bound") ||
      correlation.phase !== expectedPhase ||
      this.materializationRetryExpiry(
        correlation,
        phase === "claim" ? "claim" : "bind",
      ) === undefined
    )
      return;
    const attempt = (correlation.retry_attempts ?? 0) + 1;
    const exhausted = attempt > MATERIALIZATION_MAX_RESPONSE_LOSS_RETRIES;
    const retryAfter =
      this.deps.now() +
      (exhausted
        ? MATERIALIZATION_RETRY_COOLDOWN_MS
        : Math.min(
            MATERIALIZATION_RETRY_MAX_MS,
            MATERIALIZATION_RETRY_BASE_MS * 2 ** (attempt - 1),
          ));
    const persistedAttempt = exhausted ? 0 : attempt;
    const type =
      phase === "claim"
        ? "retry_claim"
        : phase === "bind"
          ? "retry_bind"
          : phase === "route"
            ? "retry_route_response"
            : "retry_navigated";
    await this.applyMaterialization(jobID, {
      type,
      attempt: persistedAttempt,
      retry_after: retryAfter,
    });
    this.scheduleMaterializationRetry(jobID);
  }

  private scheduleMaterializationRetry(jobID: string, immediate = false): void {
    if (this.materializationRetryTimers.has(jobID)) return;
    const correlation = this.materializationCorrelation(jobID);
    if (
      correlation === undefined ||
      correlation.phase === "navigated" ||
      ((correlation.retry_attempts ?? 0) >=
        MATERIALIZATION_MAX_RESPONSE_LOSS_RETRIES &&
        correlation.retry_after === undefined)
    )
      return;
    const due = immediate
      ? this.deps.now()
      : (correlation.retry_after ?? this.deps.now());
    const delay = Math.max(0, due - this.deps.now());
    if (delay > 0) {
      const marker = {};
      this.materializationRetryTimers.set(jobID, marker);
      this.deps.setTimeout(() => {
        if (this.materializationRetryTimers.get(jobID) !== marker) return;
        this.materializationRetryTimers.delete(jobID);
        this.scheduleMaterialization(jobID, true);
      }, delay);
      return;
    }
    this.scheduleMaterialization(jobID, true);
  }

  private async runMaterialization(jobID: string): Promise<void> {
    await this.surfaceReady;
    if (this.cancelledMaterializationJobs.has(jobID)) return;
    let correlation = this.materializationCorrelation(jobID);
    if (correlation === undefined || correlation.phase === "navigated") return;
    if (
      !MATERIALIZATION_RFC3339_PATTERN.test(correlation.candidate_expires_at) ||
      Date.parse(correlation.candidate_expires_at) <= this.deps.now()
    ) {
      await this.applyMaterialization(jobID, { type: "failed" });
      return;
    }
    if (correlation.binding_id === undefined) {
      const claimingCandidateID = correlation.candidate_id;
      if (
        correlation.phase !== "offered" &&
        correlation.phase !== "claiming" &&
        correlation.phase !== "failed"
      )
        return;
      if (correlation.phase !== "claiming") {
        await this.applyMaterialization(jobID, { type: "claiming" });
        if (!this.materializationCurrent(jobID, claimingCandidateID)) return;
      }
      correlation = this.materializationCorrelation(jobID);
      if (
        correlation === undefined ||
        correlation.candidate_id !== claimingCandidateID
      )
        return;
      const claimCandidateID = correlation.candidate_id;
      const response = await this.materializationRPC(
        "institutional_claim_request",
        "institutional_claim_response",
        { candidate_id: claimCandidateID, materialization_kind: "browser_tab" },
        jobID,
        { candidateID: claimCandidateID },
      );
      if (!this.materializationCurrent(jobID, claimCandidateID)) return;
      if (response === undefined) {
        await this.retryMaterializationAfterResponseLoss(jobID, "claim");
        return;
      }
      const payload = response.payload;
      const claimOutcome = payload?.["outcome"];
      if (claimOutcome !== "claimed") {
        if (
          claimOutcome === "stale" ||
          claimOutcome === "not_eligible" ||
          claimOutcome === "feature_disabled"
        ) {
          await this.clearMaterializationWorkflow(jobID);
        } else {
          // busy/error are transient daemon conditions. Reuse the bounded
          // response-loss backoff instead of entering a dead failed phase.
          await this.retryMaterializationAfterResponseLoss(jobID, "claim");
        }
        return;
      }
      const claimID = payload?.["claim_id"];
      const bindingID = payload?.["binding_id"];
      const holderGeneration = payload?.["browser_holder_generation"];
      const leaseUntil = payload?.["lease_until"];
      if (
        typeof claimID !== "string" ||
        !MATERIALIZATION_ID_PATTERN.test(claimID) ||
        typeof bindingID !== "string" ||
        !MATERIALIZATION_ID_PATTERN.test(bindingID) ||
        typeof holderGeneration !== "number" ||
        !Number.isSafeInteger(holderGeneration) ||
        holderGeneration < 1 ||
        typeof leaseUntil !== "string" ||
        !MATERIALIZATION_RFC3339_PATTERN.test(leaseUntil) ||
        !Number.isFinite(Date.parse(leaseUntil))
      ) {
        await this.retryMaterializationAfterResponseLoss(jobID, "claim");
        return;
      }
      this.lastKnownBrowserHolderGeneration = holderGeneration;
      await this.applyMaterialization(jobID, {
        type: "claimed",
        claim_id: claimID,
        binding_id: bindingID,
        browser_holder_generation: holderGeneration,
        lease_until: leaseUntil,
      });
    }
    correlation = this.materializationCorrelation(jobID);
    if (
      correlation?.binding_id === undefined ||
      correlation.claim_id === undefined
    )
      return;
    if (correlation.phase === "failed") {
      await this.applyMaterialization(
        jobID,
        correlation.tab_id >= 0
          ? { type: "retry_route", tab_id: correlation.tab_id }
          : { type: "retry_route" },
      );
      correlation = this.materializationCorrelation(jobID);
    }
    if (
      correlation === undefined ||
      correlation.binding_id === undefined ||
      correlation.claim_id === undefined
    )
      return;
    if (
      correlation.phase === "claimed" ||
      (correlation.phase === "bound" && correlation.tab_id < 0)
    ) {
      const scaffold = await this.materializeScaffold(jobID, correlation);
      if (!scaffold.reliable) {
        await this.retryMaterializationAfterResponseLoss(jobID, "bind");
        return;
      }
      const tabID = scaffold.tabID;
      if (tabID === undefined) {
        await this.retryMaterializationAfterResponseLoss(jobID, "bind");
        return;
      }
      correlation = this.materializationCorrelation(jobID);
      if (
        correlation === undefined ||
        correlation.tab_id !== tabID ||
        correlation.binding_id === undefined ||
        correlation.claim_id === undefined
      )
        return;
      const bindCandidateID = correlation.candidate_id;
      const bindClaimID = correlation.claim_id;
      const bindBindingID = correlation.binding_id;
      const bindResponse = await this.materializationRPC(
        "institutional_bind_request",
        "institutional_bind_response",
        { claim_id: bindClaimID, binding_id: bindBindingID, tab_id: tabID },
        jobID,
        { claimID: bindClaimID, bindingID: bindBindingID },
      );
      const currentAfterBind = this.materializationCorrelation(jobID);
      if (
        !this.materializationCurrent(jobID, bindCandidateID) ||
        currentAfterBind?.claim_id !== bindClaimID ||
        currentAfterBind?.binding_id !== bindBindingID
      )
        return;
      if (bindResponse === undefined) {
        await this.retryMaterializationAfterResponseLoss(jobID, "bind");
        return;
      }
      const bindOutcome = bindResponse.payload["outcome"];
      if (bindOutcome !== "bound") {
        if (
          bindOutcome === "stale" ||
          bindOutcome === "not_eligible" ||
          bindOutcome === "feature_disabled"
        ) {
          const institutionBusy =
            bindOutcome === "not_eligible" &&
            bindResponse.payload["detail"] ===
              "another sign-in for this institution is in progress";
          await this.removeMaterializationTab(tabID);
          if (institutionBusy) {
            // Retire the scaffold - holding a tab open while another paper
            // signs in is the whole point of this refusal - but KEEP the
            // correlation. The daemon leaves this candidate `claimed`, and its
            // scheduler only re-offers an `eligible` one, so no daemon poll
            // re-drives this paper. The durable alarm is the only wake-up, and
            // it resumes through scheduleMaterialization, which returns
            // silently when the correlation is gone - so clearing here made
            // the alarm decorative and deferred every retry to the claim's
            // ~30-minute reconciliation. The kept phase is `claimed`, which
            // materializeScaffold re-enters by scanning for the binding
            // rather than trusting the removed tab id.
            this.scheduleInstitutionalBindRetry(jobID);
          } else {
            await this.clearMaterializationWorkflow(jobID);
          }
        } else {
          await this.retryMaterializationAfterResponseLoss(jobID, "bind");
        }
        return;
      }
      // The bind is this pipeline's ONLY source of claim identity — it has no
      // consult — so register the grant here exactly as the consult does.
      // Without it `onTabRemoved` has nothing to report from and the surface's
      // loss is invisible: measured on the operator's own machine, zero
      // observations had ever been produced for a pipeline-created surface.
      this.registerClaimGrantFromBind(jobID, bindResponse.payload);
      await this.applyMaterialization(jobID, { type: "bound" });
      // Ownership is already recorded, so mirror the identity onto the
      // scaffold's durable record now rather than waiting for a drive.
      await this.persistClaimIdentity(jobID, tabID);
    }
    correlation = this.materializationCorrelation(jobID);
    if (
      correlation === undefined ||
      correlation.binding_id === undefined ||
      correlation.claim_id === undefined
    )
      return;
    if (correlation.phase === "route_issued") {
      await this.applyMaterialization(jobID, {
        type: "retry_route",
        tab_id: correlation.tab_id,
      });
      correlation = this.materializationCorrelation(jobID);
    }
    if (
      correlation === undefined ||
      correlation.binding_id === undefined ||
      correlation.claim_id === undefined
    )
      return;
    if (correlation.phase === "navigating") {
      if (
        correlation.tab_id < 0 ||
        correlation.route_issuance_ordinal === undefined
      )
        return;
      const navigationCandidateID = correlation.candidate_id;
      const navigationClaimID = correlation.claim_id;
      const navigationBindingID = correlation.binding_id;
      const navigationTabID = correlation.tab_id;
      let providerNavigation = false;
      try {
        const tab =
          this.deps.tabs.get === undefined
            ? undefined
            : await this.deps.tabs.get(navigationTabID);
        const tabURL = tab?.url;
        const scaffoldBase = this.deps.runtimeGetURL?.(MATERIALIZE_PAGE_PATH);
        providerNavigation =
          tab?.id === navigationTabID &&
          typeof tabURL === "string" &&
          /^https:\/\//u.test(tabURL) &&
          (scaffoldBase === undefined ||
            !tabURL.startsWith(`${scaffoldBase}#`));
      } catch {
        providerNavigation = false;
      }
      const legacyCleanup =
        (this.store.daemonFeatures ?? []).includes(
          INSTITUTIONAL_MATERIALIZATION_FEATURE,
        ) &&
        !(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE);
      if (
        correlation.effect_ordinal === undefined ||
        correlation.institutional_request_id === undefined
      ) {
        if (!legacyCleanup) return;
      }
      const navigationPayload: Record<string, unknown> = {
        claim_id: navigationClaimID,
        binding_id: navigationBindingID,
        route_issuance_ordinal: correlation.route_issuance_ordinal!,
        tab_id: navigationTabID,
      };
      if (!legacyCleanup) {
        navigationPayload.effect_ordinal = correlation.effect_ordinal!;
        navigationPayload.institutional_request_id =
          correlation.institutional_request_id!;
      }
      const recoveredNavigation = await this.materializationRPC(
        "institutional_navigated_request",
        "institutional_navigated_response",
        navigationPayload,
        jobID,
        { claimID: navigationClaimID, bindingID: navigationBindingID },
      );
      const currentAfterNavigation = this.materializationCorrelation(jobID);
      if (
        !this.materializationCurrent(jobID, navigationCandidateID) ||
        currentAfterNavigation?.claim_id !== navigationClaimID ||
        currentAfterNavigation?.binding_id !== navigationBindingID ||
        currentAfterNavigation?.tab_id !== navigationTabID
      )
        return;
      if (recoveredNavigation === undefined) {
        await this.retryMaterializationAfterResponseLoss(jobID, "navigated");
        return;
      }
      const outcome = recoveredNavigation.payload["outcome"];
      if (outcome === "acknowledged")
        await this.applyMaterialization(jobID, { type: "navigated" });
      else if (
        outcome === "stale" ||
        outcome === "not_eligible" ||
        outcome === "feature_disabled"
      ) {
        await this.removeMaterializationTab(correlation!.tab_id);
        await this.clearMaterializationWorkflow(jobID);
      } else {
        await this.retryMaterializationAfterResponseLoss(jobID, "navigated");
      }
      return;
    }
    const routeCandidateID = correlation.candidate_id;
    const routeClaimID = correlation.claim_id;
    const routeBindingID = correlation.binding_id;
    if (!(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE))
      return;
    if (
      correlation.institutional_request_id === undefined ||
      correlation.expected_effect_ordinal === undefined
    ) {
      await this.applyMaterialization(jobID, {
        type: "route_prepared",
        institutional_request_id: this.deps.randomUUID().replace(/-/g, ""),
        expected_effect_ordinal: correlation.effect_ordinal ?? 0,
      });
      correlation = this.materializationCorrelation(jobID);
    }
    const institutionalRequestID = correlation?.institutional_request_id;
    const expectedEffectOrdinal = correlation?.expected_effect_ordinal;
    if (
      institutionalRequestID === undefined ||
      expectedEffectOrdinal === undefined
    )
      return;
    const routeResponse = await this.materializationRPC(
      "institutional_route_request",
      "institutional_route_response",
      {
        claim_id: routeClaimID,
        binding_id: routeBindingID,
        expected_effect_ordinal: expectedEffectOrdinal,
        institutional_request_id: institutionalRequestID,
      },
      jobID,
      { claimID: routeClaimID, bindingID: routeBindingID },
      institutionalRequestID,
    );
    const currentAfterRoute = this.materializationCorrelation(jobID);
    if (
      !this.materializationCurrent(jobID, routeCandidateID) ||
      currentAfterRoute?.claim_id !== routeClaimID ||
      currentAfterRoute?.binding_id !== routeBindingID
    )
      return;
    if (routeResponse === undefined) {
      await this.retryMaterializationAfterResponseLoss(jobID, "route");
      return;
    }
    const routePayload = routeResponse.payload;
    const routeOrdinal = routePayload["route_issuance_ordinal"];
    const effectOrdinal = routePayload["effect_ordinal"];
    const responseInstitutionalRequestID =
      routePayload["institutional_request_id"];
    const freshURL = routePayload["url"];
    const replayingRoute = correlation!.route_replay_ordinal === routeOrdinal;
    if (
      routePayload["outcome"] !== "issued" ||
      typeof routeOrdinal !== "number" ||
      !Number.isSafeInteger(routeOrdinal) ||
      routeOrdinal < 1 ||
      typeof effectOrdinal !== "number" ||
      !Number.isSafeInteger(effectOrdinal) ||
      effectOrdinal < 1 ||
      responseInstitutionalRequestID !== institutionalRequestID ||
      (correlation!.route_issuance_ordinal !== undefined &&
        (routeOrdinal < correlation!.route_issuance_ordinal ||
          (!replayingRoute &&
            routeOrdinal <= correlation!.route_issuance_ordinal))) ||
      typeof freshURL !== "string" ||
      !/^https:\/\//u.test(freshURL)
    ) {
      const routeOutcome = routePayload["outcome"];
      if (
        routeOutcome === "stale" ||
        routeOutcome === "not_eligible" ||
        routeOutcome === "feature_disabled"
      ) {
        await this.removeMaterializationTab(correlation!.tab_id);
        await this.clearMaterializationWorkflow(jobID);
      } else {
        await this.retryMaterializationAfterResponseLoss(jobID, "route");
      }
      return;
    }
    await this.applyMaterialization(jobID, {
      type: "route_issued",
      route_issuance_ordinal: routeOrdinal,
      effect_ordinal: effectOrdinal,
      institutional_request_id: institutionalRequestID,
    });
    correlation = this.materializationCorrelation(jobID);
    if (
      correlation === undefined ||
      correlation.phase !== "route_issued" ||
      correlation.candidate_id !== routeCandidateID ||
      correlation.claim_id !== routeClaimID ||
      correlation.binding_id !== routeBindingID ||
      correlation.tab_id < 0
    )
      return;
    const navigationCandidateID = correlation.candidate_id;
    const navigationClaimID = correlation.claim_id;
    const navigationBindingID = correlation.binding_id;
    const navigationOrdinal = correlation.route_issuance_ordinal;
    if (navigationOrdinal === undefined) return;
    await this.applyMaterialization(jobID, { type: "navigating" });
    const currentTabID = correlation.tab_id;
    const beforeUpdate = this.materializationCorrelation(jobID);
    if (
      !this.materializationCurrent(jobID, navigationCandidateID) ||
      beforeUpdate?.claim_id !== navigationClaimID ||
      beforeUpdate?.binding_id !== navigationBindingID ||
      beforeUpdate?.route_issuance_ordinal !== navigationOrdinal ||
      beforeUpdate?.tab_id !== currentTabID
    )
      return;
    const activeDrive = findByJob(this.store, jobID);
    if (
      activeDrive?.tab_id === currentTabID &&
      activeDrive.generic_drive_epoch !== undefined
    ) {
      // Register before navigation: a fast provider page can complete while
      // the correlated navigated acknowledgement is still in flight.
      this.beginProviderDrive(jobID);
      this.registerHandoffDrive(jobID, currentTabID);
    }
    try {
      if (this.deps.tabs.update === undefined) {
        this.releaseHandoffDrive(jobID);
        await this.removeMaterializationTab(currentTabID);
        await this.applyMaterialization(jobID, { type: "scaffold_lost" });
        return;
      }
      await this.deps.tabs.update(currentTabID, { url: freshURL });
    } catch {
      const currentAfterFailure = this.materializationCorrelation(jobID);
      if (
        !this.materializationCurrent(jobID, navigationCandidateID) ||
        currentAfterFailure?.claim_id !== navigationClaimID ||
        currentAfterFailure?.binding_id !== navigationBindingID ||
        currentAfterFailure?.route_issuance_ordinal !== navigationOrdinal ||
        currentAfterFailure?.tab_id !== currentTabID
      )
        return;
      // The scaffold can disappear between route issuance and navigation.
      // Never replay against that tab: remove it, preserve the claim/binding
      // and ordinal while marking the scaffold absent, then the post-run wake
      // creates and binds a replacement before requesting another route.
      await this.removeMaterializationTab(currentTabID);
      this.releaseHandoffDrive(jobID);
      await this.applyMaterialization(jobID, { type: "scaffold_lost" });
      return;
    }
    correlation = this.materializationCorrelation(jobID);
    const legacyCleanup =
      (this.store.daemonFeatures ?? []).includes(
        INSTITUTIONAL_MATERIALIZATION_FEATURE,
      ) &&
      !(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE);
    if (
      correlation?.effect_ordinal === undefined ||
      correlation?.institutional_request_id === undefined
    ) {
      if (!legacyCleanup) return;
    }
    if (
      !this.materializationCurrent(jobID, navigationCandidateID) ||
      correlation?.phase !== "navigating" ||
      correlation.claim_id !== navigationClaimID ||
      correlation.binding_id !== navigationBindingID ||
      correlation.route_issuance_ordinal !== navigationOrdinal ||
      correlation.tab_id !== currentTabID
    )
      return;
    const navigationPayload: Record<string, unknown> = {
      claim_id: navigationClaimID,
      binding_id: navigationBindingID,
      route_issuance_ordinal: navigationOrdinal,
      tab_id: currentTabID,
    };
    if (!legacyCleanup) {
      navigationPayload.effect_ordinal = correlation.effect_ordinal!;
      navigationPayload.institutional_request_id =
        correlation.institutional_request_id!;
    }
    const navigatedResponse = await this.materializationRPC(
      "institutional_navigated_request",
      "institutional_navigated_response",
      navigationPayload,
      jobID,
      { claimID: navigationClaimID, bindingID: navigationBindingID },
    );
    const currentAfterNavigated = this.materializationCorrelation(jobID);
    if (
      !this.materializationCurrent(jobID, navigationCandidateID) ||
      currentAfterNavigated?.claim_id !== navigationClaimID ||
      currentAfterNavigated?.binding_id !== navigationBindingID ||
      currentAfterNavigated?.route_issuance_ordinal !== navigationOrdinal ||
      currentAfterNavigated?.tab_id !== currentTabID
    )
      return;
    if (navigatedResponse === undefined) {
      await this.retryMaterializationAfterResponseLoss(jobID, "navigated");
    } else {
      const navigatedOutcome = navigatedResponse.payload["outcome"];
      if (navigatedOutcome === "acknowledged") {
        await this.applyMaterialization(jobID, { type: "navigated" });
        // The drive lease was registered before navigation so fast onUpdated
        // callbacks cannot outrun generic classification authority.
      } else if (
        navigatedOutcome === "stale" ||
        navigatedOutcome === "not_eligible" ||
        navigatedOutcome === "feature_disabled"
      ) {
        await this.removeMaterializationTab(currentTabID);
        await this.clearMaterializationWorkflow(jobID);
      } else {
        await this.retryMaterializationAfterResponseLoss(jobID, "navigated");
      }
    }
  }

  private scheduleMaterialization(jobID: string, immediate = false): void {
    // Stop before the workflow, not only before its frames: materializing opens
    // a real browser tab to bind, and a pending session that cannot claim would
    // leave that tab orphaned with nothing to attach it to.
    if (!this.holderRole()) return;
    // Slice 0 containment, on the one pipeline the nine legacy paths' shared
    // gate never reached. institutionalAuthGateOpen()'s last clause is
    // `online`; this path checked only holdership, so a woken worker with no
    // network claimed, built a scaffold tab, and navigated it straight into a
    // DNS failure — "spawns new tabs before the internet is ready".
    //
    // Park a revival rather than returning bare. A reviewer falsified the
    // earlier claim that "the daemon re-offers every poll, so nothing is
    // dropped": the scheduler only re-offers candidates it still sees as
    // `eligible`, so a candidate already `claimed` or `bound` has NO
    // daemon-side re-drive, and both local triggers consume themselves before
    // reaching here — scheduleMaterializationRetry's timer callback and
    // runMaterialization's rerun in `finally`. Without this the paper stayed
    // claimed and tabless forever, even after the network came back.
    //
    // The revival lives in its own map on purpose: materializationRetryTimers
    // doubles as the "a retry is already pending, do not drive now" guard, so
    // parking this there made a fresh online offer wait for the timer instead
    // of driving, which deadlocked the first version of this fix.
    if (this.deps.online?.() === false) {
      if (!this.materializationOfflineTimers.has(jobID)) {
        const marker = {};
        this.materializationOfflineTimers.set(jobID, marker);
        this.deps.setTimeout(() => {
          if (this.materializationOfflineTimers.get(jobID) !== marker) return;
          this.materializationOfflineTimers.delete(jobID);
          this.scheduleMaterialization(jobID, true);
        }, MATERIALIZATION_RETRY_MAX_MS);
      }
      return;
    }
    this.materializationOfflineTimers.delete(jobID);
    if (this.materializationRuns.has(jobID)) {
      this.materializationReruns.add(jobID);
      return;
    }
    if (this.materializationRetryTimers.has(jobID)) return;
    const correlation = this.materializationCorrelation(jobID);
    if (
      correlation === undefined ||
      correlation.phase === "navigated" ||
      ((correlation.retry_attempts ?? 0) >=
        MATERIALIZATION_MAX_RESPONSE_LOSS_RETRIES &&
        correlation.retry_after === undefined)
    )
      return;
    const due = immediate
      ? this.deps.now()
      : (correlation.retry_after ?? this.deps.now());
    const delay = Math.max(0, due - this.deps.now());
    if (delay > 0) {
      const marker = {};
      this.materializationRetryTimers.set(jobID, marker);
      this.deps.setTimeout(() => {
        if (this.materializationRetryTimers.get(jobID) !== marker) return;
        this.materializationRetryTimers.delete(jobID);
        this.scheduleMaterialization(jobID, true);
      }, delay);
      return;
    }
    const effectJobID = `materialization:${jobID}`;
    const effectToken = this.claimEffectGovernor(effectJobID);
    if (effectToken === undefined) {
      this.pendingMaterializationEffects.add(jobID);
      return;
    }
    const run = this.runMaterialization(jobID)
      .catch((error) =>
        console.error("papio: institutional materialization failed", error),
      )
      .finally(() => {
        this.releaseEffectGovernor(effectJobID, effectToken, false);
        this.wakeEffectGovernor();
        if (this.materializationRuns.get(jobID) !== run) return;
        this.materializationRuns.delete(jobID);
        const rerun = this.materializationReruns.delete(jobID);
        if (rerun) {
          this.scheduleMaterialization(jobID, true);
          return;
        }
        const correlation = this.materializationCorrelation(jobID);
        if (correlation?.phase === "claimed" && correlation.tab_id < 0) {
          this.scheduleMaterialization(jobID, true);
        }
      });
    this.materializationRuns.set(jobID, run);
  }

  private async reconcileMaterializationTabs(): Promise<void> {
    if (
      !(this.store.daemonFeatures ?? []).includes(
        INSTITUTIONAL_MATERIALIZATION_FEATURE,
      )
    )
      return;
    const scan = await this.scanMaterializationTabs();
    if (!scan.reliable) return;
    const records = this.store.materializations ?? {};
    const byBinding = scan.byBinding;
    const bindings: {
      binding_id: string;
      tab_id: number;
      job_id: string;
      candidate_id: string;
    }[] = [];
    const retained = new Set<number>();
    const seenBindings = new Set<string>();
    const stillCurrent = (
      jobID: string,
      candidateID: string,
      bindingID: string,
    ): boolean => {
      const current = this.materializationCorrelation(jobID);
      return (
        current?.candidate_id === candidateID &&
        current.binding_id === bindingID
      );
    };
    for (const [jobID, correlation] of Object.entries(records)) {
      const bindingID = correlation.binding_id;
      if (bindingID === undefined) continue;
      const candidateID = correlation.candidate_id;
      if (seenBindings.has(bindingID)) {
        if (stillCurrent(jobID, candidateID, bindingID)) {
          await this.clearMaterializationWorkflow(jobID);
        }
        continue;
      }
      seenBindings.add(bindingID);
      const candidates = byBinding.get(bindingID) ?? [];
      // Once the route has been navigated, the surface is deliberately no
      // longer at the scaffold URL, so it can never appear among these
      // candidates - the operator's own login wall is that tab now. Anything
      // still sitting at this binding's scaffold page is therefore a leftover
      // papio minted and failed to retire, and choosing one would repoint both
      // the correlation and (below) the daemon at a dead placeholder while the
      // paper waits on a sign-in nobody is watching.
      if (correlation.phase === "navigating" || correlation.phase === "navigated") {
        for (const candidate of candidates) {
          if (candidate.id === undefined) continue;
          if (!stillCurrent(jobID, candidateID, bindingID)) break;
          await this.removeMaterializationTab(candidate.id);
        }
        if (correlation.tab_id >= 0) {
          retained.add(correlation.tab_id);
          bindings.push({
            binding_id: bindingID,
            tab_id: correlation.tab_id,
            job_id: jobID,
            candidate_id: candidateID,
          });
        }
        continue;
      }
      const chosen =
        candidates.find((tab) => tab.id === correlation.tab_id) ??
        candidates[0];
      if (chosen?.id === undefined) {
        if (correlation.tab_id >= 0) {
          bindings.push({
            binding_id: bindingID,
            tab_id: correlation.tab_id,
            job_id: jobID,
            candidate_id: candidateID,
          });
        }
        continue;
      }
      for (const candidate of candidates) {
        if (
          candidate.id !== undefined &&
          candidate.id !== chosen.id &&
          stillCurrent(jobID, candidateID, bindingID)
        ) {
          await this.removeMaterializationTab(candidate.id);
        }
      }
      retained.add(chosen.id);
      if (
        correlation.tab_id !== chosen.id &&
        stillCurrent(jobID, candidateID, bindingID)
      ) {
        await this.applyMaterialization(jobID, {
          type: "reconcile_tab",
          tab_id: chosen.id,
        });
      }
      bindings.push({
        binding_id: bindingID,
        tab_id: chosen.id,
        job_id: jobID,
        candidate_id: candidateID,
      });
    }
    for (const [bindingID, tabs] of byBinding.entries()) {
      const owner = Object.entries(records).find(
        ([, entry]) => entry.binding_id === bindingID,
      );
      for (const tab of tabs) {
        if (tab.id === undefined || retained.has(tab.id)) continue;
        if (
          owner !== undefined &&
          !stillCurrent(owner[0], owner[1].candidate_id, bindingID)
        )
          continue;
        await this.removeMaterializationTab(tab.id);
      }
    }
    for (let offset = 0; offset < bindings.length; offset += 32) {
      await this.reconcileMaterializationPage(
        bindings.slice(offset, offset + 32),
      );
    }
  }

  private async reconcileMaterializationPage(
    submittedBindings: {
      binding_id: string;
      tab_id: number;
      job_id: string;
      candidate_id: string;
    }[],
  ): Promise<void> {
    const response = await this.materializationRPC(
      "institutional_reconcile_request",
      "institutional_reconcile_response",
      {
        bindings: submittedBindings.map(({ binding_id, tab_id }) => ({
          binding_id,
          tab_id,
        })),
      },
      undefined,
      {},
    );
    const payload = response?.payload;
    if (payload === undefined || !Array.isArray(payload["claims"])) return;
    const liveBindings = new Set<string>();
    const currentFor = (snapshot: {
      job_id: string;
      candidate_id: string;
      binding_id: string;
    }) => {
      const entry = this.materializationCorrelation(snapshot.job_id);
      return entry?.candidate_id === snapshot.candidate_id &&
        entry.binding_id === snapshot.binding_id
        ? entry
        : undefined;
    };
    for (const rawClaim of payload["claims"]) {
      if (typeof rawClaim !== "object" || rawClaim === null) continue;
      const claim = rawClaim as Record<string, unknown>;
      const bindingID = claim["binding_id"];
      if (typeof bindingID !== "string") continue;
      liveBindings.add(bindingID);
      const snapshot = submittedBindings.find(
        (binding) => binding.binding_id === bindingID,
      );
      if (snapshot === undefined) continue;
      let entry = currentFor(snapshot);
      if (entry === undefined) continue;
      const tabID = claim["tab_id"];
      if (
        typeof tabID === "number" &&
        Number.isInteger(tabID) &&
        tabID >= 0 &&
        entry.tab_id !== tabID
      ) {
        if (currentFor(snapshot) === undefined) continue;
        await this.applyMaterialization(snapshot.job_id, {
          type: "reconcile_tab",
          tab_id: tabID,
        });
      }
      entry = currentFor(snapshot);
      if (entry === undefined) continue;
      if (claim["phase"] === "navigated" && entry.phase === "navigating") {
        if (currentFor(snapshot) === undefined) continue;
        await this.applyMaterialization(snapshot.job_id, { type: "navigated" });
      }
      entry = currentFor(snapshot);
      if (entry === undefined) continue;
      if (claim["phase"] === "settled" || claim["phase"] === "abandoned") {
        if (entry.tab_id >= 0) {
          if (currentFor(snapshot) === undefined) continue;
          await this.removeMaterializationTab(entry.tab_id);
        }
        if (currentFor(snapshot) === undefined) continue;
        await this.clearMaterializationWorkflow(snapshot.job_id);
      }
    }
    for (const snapshot of submittedBindings) {
      if (liveBindings.has(snapshot.binding_id)) continue;
      const entry = currentFor(snapshot);
      if (entry === undefined) continue;
      if (payload["outcome"] !== "stale" && payload["outcome"] !== "reconciled")
        continue;
      if (entry.tab_id >= 0) {
        if (currentFor(snapshot) === undefined) continue;
        await this.removeMaterializationTab(entry.tab_id);
      }
      if (currentFor(snapshot) === undefined) continue;
      await this.clearMaterializationWorkflow(snapshot.job_id);
    }
  }
  private async onInstitutionalCandidateOffer(
    msg: BrowserMessage,
  ): Promise<void> {
    const jobID = msg.job_id;
    const payload = msg.payload;
    const candidateID = payload["candidate_id"];
    const kind = payload["materialization_kind"];
    const expiresAt = payload["expires_at"];
    const hostsRaw = payload["provider_hosts"];
    if (
      jobID === undefined ||
      !(this.store.daemonFeatures ?? []).includes(
        INSTITUTIONAL_MATERIALIZATION_FEATURE,
      ) ||
      kind !== "browser_tab" ||
      typeof candidateID !== "string" ||
      !MATERIALIZATION_ID_PATTERN.test(candidateID) ||
      typeof expiresAt !== "string" ||
      !MATERIALIZATION_RFC3339_PATTERN.test(expiresAt) ||
      !Number.isFinite(Date.parse(expiresAt)) ||
      Date.parse(expiresAt) <= this.deps.now() ||
      !Array.isArray(hostsRaw) ||
      hostsRaw.length < 1 ||
      hostsRaw.some((host) => typeof host !== "string")
    )
      return;
    const providerHosts = hostsRaw.filter(
      (host): host is string => typeof host === "string",
    );
    const expected = parseExpected(payload["expected"]);
    const accessMode =
      payload["access_mode"] === "assisted" ||
      payload["access_mode"] === "delegated"
        ? payload["access_mode"]
        : undefined;
    const requiresAuth =
      typeof payload["requires_auth"] === "boolean"
        ? payload["requires_auth"]
        : undefined;
    const loginEntityID =
      typeof payload["login_entity_id"] === "string"
        ? payload["login_entity_id"]
        : undefined;
    const proquestAccountID =
      typeof payload["proquest_account_id"] === "string"
        ? payload["proquest_account_id"]
        : undefined;
    const driveAttemptID =
      typeof payload["drive_attempt_id"] === "string"
        ? payload["drive_attempt_id"]
        : undefined;
    const driveOrdinal =
      typeof payload["drive_ordinal"] === "number"
        ? payload["drive_ordinal"]
        : undefined;
    const driveStrategy =
      typeof payload["drive_strategy"] === "string"
        ? payload["drive_strategy"]
        : undefined;
    const driveRevision =
      typeof payload["drive_revision"] === "string"
        ? payload["drive_revision"]
        : undefined;
    const offeredEpoch: ProviderDriveEpoch | undefined =
      driveAttemptID !== undefined &&
      driveOrdinal !== undefined &&
      driveStrategy === "generic" &&
      driveRevision !== undefined
        ? {
            drive_attempt_id: driveAttemptID,
            ordinal: driveOrdinal,
            strategy: "generic",
            revision: driveRevision,
            attempt_count: 0,
          }
        : undefined;
    const existingJob = findByJob(this.store, jobID);
    const existing = this.materializationCorrelation(jobID);
    // Candidate offers are the daemon's existing wake signal after an
    // institution becomes free. While a durable retry alarm is pending, the
    // daemon's ordinary 2s poll is only a refresh; it must not rebuild a tab.
    const institutionalRetryPending =
      await this.institutionalRetryAlarmPending(jobID);
    const now = this.deps.now();
    const expiresMs = Date.parse(expiresAt);
    // A re-offer of the SAME candidate refreshes the daemon's lease; it does
    // not restart materialization once a binding exists. reduceMaterialization
    // (state.ts) applies exactly that rule to the correlation, and this record
    // has to agree with it: a daemon restart re-offers with a fresh expiry, and
    // resetting tab_id here would orphan a live surface - the operator's own
    // sign-in tab - from its job. That silently stops every observation keyed
    // on job.tab_id (challenge, mfa, auth_returned, entitled_landing) and lets
    // the next open mint a second surface beside the one already on the wall.
    const boundSurfaceTabID =
      existing !== undefined &&
      existing.candidate_id === candidateID &&
      existing.binding_id !== undefined &&
      existing.tab_id >= 0
        ? existing.tab_id
        : -1;
    const candidateJob: ActiveJob = {
      ...(existingJob ?? {
        job_id: jobID,
        tab_id: -1,
        offered_at: now,
        expires_at: expiresMs,
        status: accessMode === "assisted" ? "queued" : "accepted",
        provider_hosts: providerHosts,
      }),
      // A fresh candidate offer is URL-free and has no scaffold tab yet;
      // binding later changes only tab_id. This record is the classifier's
      // authority, so it keeps a bound surface across a same-candidate re-offer.
      tab_id: boundSurfaceTabID,
      offered_at: existingJob?.offered_at ?? now,
      expires_at: expiresMs,
      provider_hosts: providerHosts,
      ...(accessMode !== undefined ? { access_mode: accessMode } : {}),
      ...(expected !== undefined ? { expected } : {}),
      ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
      ...(offeredEpoch !== undefined
        ? { generic_drive_epoch: offeredEpoch }
        : {}),
    };
    if (expected === undefined) delete candidateJob.expected;
    if (requiresAuth === undefined) delete candidateJob.requires_auth;
    await this.upsertJobWithoutOffer(candidateJob);
    if (loginEntityID !== undefined && loginEntityID.length > 0)
      this.loginEntityIDs.set(jobID, loginEntityID);
    if (proquestAccountID !== undefined && proquestAccountID.length > 0)
      this.proquestAccountIDs.set(jobID, proquestAccountID);
    // `existing` is read above, before the job upsert, which never touches
    // materializations.
    const sameCandidateRefresh =
      existing?.candidate_id === candidateID &&
      Date.parse(expiresAt) > Date.parse(existing.candidate_expires_at);
    const offer = async () => {
      this.cancelledMaterializationJobs.delete(jobID);
      if (sameCandidateRefresh) this.materializationRetryTimers.delete(jobID);
      await this.applyMaterialization(jobID, {
        type: "offer",
        correlation: {
          job_id: jobID,
          candidate_id: candidateID,
          materialization_kind: "browser_tab",
          candidate_expires_at: expiresAt,
          phase: "offered",
          tab_id: -1,
        },
      });
      if (!institutionalRetryPending)
        this.scheduleMaterialization(jobID, sameCandidateRefresh);
    };
    if (existing !== undefined && existing.candidate_id !== candidateID) {
      // A daemon re-offer is authoritative. Stop all browser-local work from
      // the old candidate before replacing its correlation, and close only
      // the old papio-owned scaffold.
      this.cancelMaterializationWorkflow(jobID);
      const oldTabID = existing.tab_id;
      void (async () => {
        try {
          if (oldTabID >= 0) await this.removeMaterializationTab(oldTabID);
        } finally {
          await offer();
        }
      })();
      return;
    }
    if (sameCandidateRefresh) {
      void offer();
      return;
    }
    this.cancelledMaterializationJobs.delete(jobID);
    if (existing === undefined) {
      void offer();
      return;
    }
    this.scheduleMaterialization(jobID);
  }

  private async requestFreshHandoffLink(
    jobID: string,
  ): Promise<{ ok: true; url: string } | BrokerFailure> {
    const result = await this.requestNative(
      "handoff_link_request",
      { job_id: jobID },
      "handoff_link_result",
      HANDOFF_LINK_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    const outcome = result.payload["outcome"];
    if (outcome === "opened" && typeof result.payload["url"] === "string") {
      return { ok: true, url: result.payload["url"] };
    }
    const details: Record<string, { code: string; message: string }> = {
      job_gone: {
        code: "job_gone",
        message: "The handoff is no longer available",
      },
      not_open_action: {
        code: "not_open_action",
        message: "The handoff has no open action",
      },
      not_openurl: {
        code: "not_openurl",
        message: "The handoff has no resolver URL",
      },
      unavailable: {
        code: "unavailable",
        message: "The daemon could not mint a fresh handoff URL",
      },
    };
    const mapped = typeof outcome === "string" ? details[outcome] : undefined;
    return failure(
      mapped?.code ?? "daemon_error",
      mapped?.message ?? "The daemon rejected the handoff link",
    );
  }

  /** The one pending toast. Bound 1 of the plan lives here rather than in the
   * page: a second loss replaces the first, so the surface can never become a
   * stack of windows. Worker memory is the right home despite MV3 suspension —
   * a toast that did not survive the worker sleeping was an offer nobody was
   * looking at, and the durable record of the loss is the Activity row the
   * daemon already writes. */
  private pendingToast: ToastPayload | undefined;
  private toastWindowID: number | undefined;
  /** The in-page route's one-use authorization. Present only while an injected
   * toast is live, and cleared by the first action, dismissal, or replacement.
   *
   * It exists because the injected toast's sender is the researcher's own page,
   * so `sender.url` cannot authorize it the way it authorizes the toast page.
   * The token is what makes the reply papio's own: a page cannot read it (the
   * isolated world is not reachable from page script), and a stale injected
   * toast left on a background tab cannot act after a replacement. */
  private pendingPageToast:
    | { readonly token: string; readonly job_id: string; readonly tab_id: number }
    | undefined;

  /** Try the in-page route. Returns whether an injected toast is now live, so
   * the caller falls back to the window rather than assuming either way.
   *
   * Every gate here is a refusal to interrupt, and they are ordered cheapest
   * first. Two of them are the researcher's own choices and neither implies the
   * other: the preference says they want papio's interruption in their page,
   * and the all-sites grant says papio may reach that page at all. A provider
   * grant is deliberately NOT sufficient — it was given so papio could finish a
   * download on that host, not so papio could draw on it. */
  private async raiseInPageToast(payload: ToastPayload): Promise<boolean> {
    const tabs = this.deps.tabs;
    if (tabs.query === undefined) return false;
    if (!(await this.deps.settings.getInPageToast().catch(() => false)))
      return false;
    const granted = await this.deps.permissions
      .contains({ origins: [ALL_SITES_ORIGIN] })
      .catch(() => false);
    if (granted !== true) return false;
    // Called through its owner, never as an extracted reference: the Chrome
    // adapter is a bound arrow but a class-backed seam is not, and an unbound
    // call throws inside the seam and reads here as "no active tab".
    const [tab] = await tabs
      .query({ active: true, lastFocusedWindow: true })
      .catch(() => []);
    const tabID = tab?.id;
    if (tabID === undefined || typeof tab?.url !== "string") return false;
    // Never into a papio surface. A tab papio tracks or owns is the work
    // surface, not the researcher's reading, and the tab whose loss started
    // this may still be in the ledger cache.
    if (findByTab(this.store, tabID) !== undefined) return false;
    if (this.tabLedgerCache?.[String(tabID)] !== undefined) return false;
    // Only an ordinary HTTPS page. The grant covers exactly that scheme, and a
    // PDF viewer or privileged page refuses injection anyway — reaching it
    // through the catch below would work, but failing here keeps the window
    // fallback fast instead of paying a rejected round trip.
    let httpsPage = false;
    try {
      httpsPage = new URL(tab.url).protocol === "https:";
    } catch {
      return false;
    }
    if (!httpsPage || isPDFPage(tab.url)) return false;
    const copy = TOAST_COPY[payload.kind];
    const token = this.deps.randomUUID();
    try {
      const [injected] = await this.deps.scripting.executeScript({
        target: { tabId: tabID },
        func: renderPageToast,
        args: [
          {
            kind: payload.kind,
            job_id: payload.job_id,
            token,
            message: copy.message,
            action: copy.action,
            window_ms: TOAST_WINDOW_MS,
            action_message: TOAST_PAGE_ACTION_MESSAGE,
            dismiss_message: TOAST_PAGE_DISMISS_MESSAGE,
            mark: PAPIO_MARK,
            mark_viewbox: PAPIO_MARK_VIEWBOX,
            mark_size_px: PAPIO_MARK_SIZE_PX,
            max_width_px: TOAST_WINDOW_SIZE.width,
          } satisfies ToastInjection,
        ],
      });
      if (injected?.result !== true) return false;
    } catch {
      // Withdrawn grant, a page type that refuses scripting, or a tab that
      // navigated mid-call. The window route still covers this loss.
      return false;
    }
    this.pendingPageToast = { token, job_id: payload.job_id, tab_id: tabID };
    return true;
  }

  /** Raise the seventh surface for a loss papio observed itself. Returns
   * whether a surface was delivered, so the caller can fall back to silence
   * rather than assume. */
  private async raiseToast(payload: ToastPayload): Promise<boolean> {
    // A papio surface already in front reports the same event, and Decision 9's
    // presence hint is exactly the signal for that. Interrupting a researcher
    // who is looking at the popup would be a duplicate, not an aid.
    if (this.papioSurfaceLikelyFocused()) return false;
    this.pendingToast = payload;
    // Replace rather than stack: retire the previous surface on BOTH routes
    // before raising either, so the researcher is never asked about two losses
    // at once and a superseded injected toast can no longer act.
    //
    // An injected toast in a tab the researcher has since left is not removed
    // from that page here — papio would have to inject again to do it. It is
    // bounded instead: it disappears on its own within TOAST_WINDOW_MS, and
    // dropping the token below means the stale offer is refused if clicked.
    await this.closeToastWindow();
    this.pendingPageToast = undefined;
    if (await this.raiseInPageToast(payload)) return true;
    const windows = this.deps.windows;
    const url = this.deps.runtimeGetURL?.(TOAST_PAGE_PATH);
    if (windows === undefined || url === undefined) {
      this.pendingToast = undefined;
      return false;
    }
    try {
      const created = await windows.create({
        url,
        focused: false,
        type: "popup",
        width: TOAST_WINDOW_SIZE.width,
        height: TOAST_WINDOW_SIZE.height,
      });
      this.toastWindowID = created.id;
      // macOS Firefox ignores `focused` at creation (bugzilla 1271047), the
      // same defect the work window already documents. Unfixed here it is
      // worse than there: a minimized work window arriving front is a nuisance,
      // but a toast that steals focus is the opposite of a toast.
      if (created.id !== undefined && created.focused === true) {
        try {
          await windows.update(created.id, { focused: false });
        } catch {
          // Best effort. A toast that took focus is still a delivered toast.
        }
      }
      return true;
    } catch {
      this.pendingToast = undefined;
      this.toastWindowID = undefined;
      return false;
    }
  }

  /** Decide whether this tab close deserves the seventh surface, and raise it
   * if so. Separate from `raiseToast` so the decision (what was lost) and the
   * delivery (how it is shown) stay testable apart. */
  private async reportLostSurface(
    job: ActiveJob,
    institutionalClaimAbandoned: boolean,
  ): Promise<void> {
    const kind: ToastKind | undefined = toastKindForLoss({
      institutionalClaimAbandoned,
      deliveryInFlight:
        this.deliveryJobs.has(job.job_id) ||
        this.store.pendingDelivery?.job_id === job.job_id,
      awaitingDownload: job.status === "awaiting_download",
    });
    if (kind === undefined) return;
    await this.raiseToast({ kind, job_id: job.job_id });
  }

  /** Close the toast window papio opened, and ONLY that window.
   *
   * The ownership re-check is not defensive padding. A researcher who closes
   * the toast themselves reports nothing — the page's own handlers never run —
   * so this id outlives the window it named. A browser is free to reuse a
   * window id after that, and removing a recycled id would close one of the
   * researcher's own windows. So the id alone never authorizes a removal: the
   * window must still be serving the toast page. */
  private async closeToastWindow(): Promise<void> {
    const windowID = this.toastWindowID;
    this.toastWindowID = undefined;
    const windows = this.deps.windows;
    if (windowID === undefined || windows === undefined) return;
    const toastURL = this.deps.runtimeGetURL?.(TOAST_PAGE_PATH);
    if (toastURL === undefined) return;
    try {
      const existing = await windows.get(windowID);
      const tabs = existing.tabs ?? [];
      // Exactly one tab, and it must be the toast page. A window the
      // researcher has navigated or added a tab to is theirs, not papio's.
      if (tabs.length !== 1 || tabs[0]?.url !== toastURL) return;
      await windows.remove(windowID);
    } catch {
      // Already gone, or unreadable: either way papio does not remove it.
    }
  }

  /** The page asks for its payload on load. Answering consumes nothing: the
   * page may reload, and a reload that rendered an empty toast would look like
   * a papio failure. The payload is dropped when the page reports an outcome. */
  toastPending(): ToastPayload | undefined {
    return this.pendingToast;
  }

  /** The researcher took the offer. `route_lost` and
   * `institution_claim_lost` both resolve to the same daemon call — the fresh
   * route `papio actions open` mints — because the extension cannot mint one
   * itself: `WithOpenRouteJob` is the daemon's authorization boundary, and an
   * offer that opened a tab by itself is exactly what papio must never do for
   * a paper it asked a human to fetch. */
  async toastAction(jobID: string): Promise<boolean> {
    const payload = this.pendingToast;
    this.pendingToast = undefined;
    this.toastWindowID = undefined;
    // Ignore an id that is not the offer papio made. A stale window reloaded
    // after a replacement would otherwise reopen the wrong paper.
    if (payload === undefined || payload.job_id !== jobID) return false;
    const minted = await this.requestFreshHandoffLink(jobID);
    if (minted.ok !== true) return false;
    try {
      await this.deps.tabs.create({ url: minted.url, active: true });
      return true;
    } catch {
      return false;
    }
  }

  /** Dismissed or expired. Both drop the offer and neither performs the
   * action; the recovery stays in the inbox, which is what keeps the eight
   * seconds from being a deadline. */
  toastDismiss(jobID: string): void {
    if (this.pendingToast?.job_id === jobID) this.pendingToast = undefined;
    this.toastWindowID = undefined;
  }

  /** Consume the injected route's one-use authorization, or refuse.
   *
   * All three facts must agree: the token papio minted, the job it minted it
   * for, and the tab it injected into. The token alone would be enough against
   * a page (it cannot read the isolated world), but the tab check is what makes
   * a replaced offer inert — a superseded toast still sitting on another tab
   * carries a real token for a job that is no longer the pending one.
   *
   * Consuming here rather than in the caller means a refused report cannot
   * silently clear a live offer — pinned by the wrong-token test, which asserts
   * the real offer survives a bad one.
   *
   * The clear on success is defence at this layer only, and deliberately not
   * claimed as more: `toastAction` drops `pendingToast` itself, so a second act
   * is already refused one level down. Keeping it here means this
   * authorization's one-use property does not depend on reading that method.
   */
  private consumePageToast(jobID: string, token: string, tabID: number): boolean {
    const pending = this.pendingPageToast;
    if (
      pending === undefined ||
      pending.token !== token ||
      pending.job_id !== jobID ||
      pending.tab_id !== tabID
    ) {
      return false;
    }
    this.pendingPageToast = undefined;
    return true;
  }

  /** The researcher took the offer in their own page. Same daemon call as the
   * window route, and deliberately the same method afterwards: the recovery is
   * one behaviour with two delivery routes, not two behaviours. */
  async pageToastAction(
    jobID: string,
    token: string,
    tabID: number,
  ): Promise<boolean> {
    if (!this.consumePageToast(jobID, token, tabID)) return false;
    return this.toastAction(jobID);
  }

  /** Dismissed or expired in the page. The injected surface has already removed
   * itself, so this only drops the offer. */
  pageToastDismiss(jobID: string, token: string, tabID: number): void {
    if (!this.consumePageToast(jobID, token, tabID)) return;
    this.toastDismiss(jobID);
  }

  async requestTriageSnapshot(request: {
    schema_versions: [1] | [2] | [3] | [4] | [5] | [4, 3] | [5, 4];
    limit?: number;
    cursor?: string;
  }): Promise<BrokerReply<{ snapshot: Record<string, unknown> }>> {
    const features = this.store.daemonFeatures ?? [];
    const schemaVersions: [1] | [2] | [3] | [4] | [5] | [4, 3] | [5, 4] =
      features.includes(TRIAGE_SNAPSHOT_SCHEMA_5_FEATURE)
        ? [5, 4]
        : features.includes(TRIAGE_SNAPSHOT_SCHEMA_4_FEATURE)
          ? [4, 3]
          : features.includes(TRIAGE_SNAPSHOT_SCHEMA_3_FEATURE)
            ? [3]
            : features.includes(TRIAGE_SNAPSHOT_SCHEMA_2_FEATURE)
              ? [2]
              : request.schema_versions;
    const result = await this.requestNative(
      "triage_snapshot_request",
      { ...request, schema_versions: schemaVersions },
      "triage_snapshot_response",
      TRIAGE_SNAPSHOT_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    const { request_id: _requestID, ...snapshot } = result.payload;
    const counts = snapshot["counts"];
    if (
      typeof counts === "object" &&
      counts !== null &&
      typeof (counts as Record<string, unknown>)["pending_total"] === "number"
    ) {
      this.updateTriageCounts(
        counts as Record<string, unknown>,
        features.includes(TRIAGE_COUNTS_SCHEMA_3_FEATURE),
      );
      await this.syncConnectionBadge();
    }
    await this.reconcileManualDownloadWindows(
      snapshot,
      request.cursor !== undefined,
    );
    return { ok: true, snapshot };
  }

  private updateTriageCounts(
    record: Record<string, unknown>,
    schemaV3: boolean,
  ): void {
    this.triagePendingCount =
      typeof record["pending_total"] === "number"
        ? record["pending_total"]
        : undefined;
    this.triageCountsSchemaV3 = schemaV3;
    this.triageRequiredTurnCount =
      typeof record["turns_required"] === "number"
        ? record["turns_required"]
        : undefined;
    this.triageRequiredTurnsComplete =
      record["required_turns_complete"] === true;
    this.triageWatchHits =
      typeof record["watch_hits"] === "number" ? record["watch_hits"] : 0;
    this.triageRetractions =
      typeof record["retractions"] === "number" ? record["retractions"] : 0;
  }

  async requestTriageCounts(): Promise<
    BrokerReply<{ counts: Record<string, unknown>; generated_at: string }>
  > {
    const features = this.store.daemonFeatures ?? [];
    const payload = features.includes(TRIAGE_COUNTS_SCHEMA_3_FEATURE)
      ? { schema_versions: [3] }
      : features.includes(TRIAGE_COUNTS_SCHEMA_2_FEATURE)
        ? { schema_versions: [2] }
        : {};
    const result = await this.requestNative(
      "triage_counts_request",
      payload,
      "triage_counts_response",
      TRIAGE_SNAPSHOT_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    const counts = result.payload["counts"];
    if (typeof counts !== "object" || counts === null)
      return failure("invalid_response", "The daemon returned invalid counts");
    const record = counts as Record<string, unknown>;
    this.updateTriageCounts(
      record,
      features.includes(TRIAGE_COUNTS_SCHEMA_3_FEATURE),
    );
    await this.syncConnectionBadge();
    const actionsRequiresAuth = record["actions_requires_auth"];
    if (typeof actionsRequiresAuth === "number") {
      this.triageActionsRequiresAuth = actionsRequiresAuth;
      this.triageActionsRequiresAuthAt = this.deps.now();
    } else {
      this.triageActionsRequiresAuth = undefined;
      this.triageActionsRequiresAuthAt = undefined;
    }
    await this.keepaliveManager?.sync();
    return {
      ok: true,
      counts: record,
      generated_at: new Date(this.deps.now()).toISOString(),
    };
  }
  async requestStats(): Promise<
    BrokerReply<{ stats: Record<string, unknown> }>
  > {
    const result = await this.requestNative(
      "stats_request",
      {},
      "stats_response",
      STATS_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    const { request_id: _requestID, ...stats } = result.payload;
    return { ok: true, stats };
  }
  async requestWorkPulse(): Promise<
    BrokerReply<{
      available: boolean;
      pulse?: WorkPulseResponsePayload;
      received_at?: number;
      worker_epoch: string;
    }>
  > {
    if (!(this.store.daemonFeatures ?? []).includes(WORK_PULSE_FEATURE)) {
      return { ok: true, available: false, worker_epoch: this.workerEpoch };
    }
    const result = await this.requestNative(
      "work_pulse_request",
      { schema_versions: [1] },
      "work_pulse_response",
      WORK_PULSE_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code === "feature_unavailable")
      return { ok: true, available: false, worker_epoch: this.workerEpoch };
    if (result.code !== undefined)
      return failure(result.code, result.message ?? "The pulse is unavailable");
    const pulse = result.payload as unknown as WorkPulseResponsePayload;
    if (pulse.schema !== 1 || typeof pulse.generated_at !== "string") {
      return failure(
        "invalid_response",
        "The daemon returned an invalid work pulse",
      );
    }
    const receivedAt = this.deps.now();
    this.pulseCache = { pulse, receivedAt, workerEpoch: this.workerEpoch };
    return {
      ok: true,
      available: true,
      pulse,
      received_at: receivedAt,
      worker_epoch: this.workerEpoch,
    };
  }

  /** The last focus report a papio surface sent, and when. Retained locally
   * because `sendSurfacePresence` is otherwise a pass-through to the daemon and
   * the toast needs the same fact browser-side.
   *
   * Deliberately short-lived. This is the MV3 worker-memory trap the popup's
   * `claimGrants` already fell into: a popup that closed while the worker slept
   * never sends `focused: false`, so a long-lived record would read "focused"
   * for ever and silence this surface permanently. Staleness therefore resolves
   * to NOT focused — the benign direction, where a toast appears beside an open
   * popup, rather than the silent one. */
  private lastSurfaceFocus: { focused: boolean; at: number } | undefined;

  /** True only on a fresh, positive focus report. `TOAST_PRESENCE_TTL_MS`
   * bounds it: two poll intervals of the popup's own presence cadence, long
   * enough that an open popup keeps reporting and short enough that a dead one
   * stops counting. */
  private papioSurfaceLikelyFocused(): boolean {
    const last = this.lastSurfaceFocus;
    if (last === undefined || !last.focused) return false;
    return this.deps.now() - last.at < TOAST_PRESENCE_TTL_MS;
  }

  async sendSurfacePresence(
    payload: Omit<SurfacePresencePayload, "request_id">,
  ): Promise<BrokerReply<{ accepted: boolean }>> {
    // Recorded before the feature gate: the fact is browser-local and true
    // whether or not the daemon negotiated the presence hint.
    this.lastSurfaceFocus = {
      focused: payload.focused === true,
      at: this.deps.now(),
    };
    if (!(this.store.daemonFeatures ?? []).includes(SURFACE_PRESENCE_FEATURE)) {
      return { ok: true, accepted: false };
    }
    const result = await this.requestNative(
      "surface_presence",
      payload,
      "surface_presence_ack",
      SURFACE_PRESENCE_FEATURE,
      false,
      undefined,
      undefined,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "Presence was not accepted",
      );
    return { ok: true, accepted: result.payload["accepted"] === true };
  }
  async requestActivity(
    request: {
      limit?: number;
      before_seq?: string;
      seen_through_seq?: string;
    } = {},
  ): Promise<BrokerReply<ActivityPageBrokerPayload>> {
    const features = this.store.daemonFeatures ?? [];
    if (features.includes(ACTIVITY_PAGE_FEATURE)) {
      const result = await this.requestNative(
        "activity_page_request",
        request,
        "activity_page_response",
        ACTIVITY_PAGE_FEATURE,
        false,
      );
      if (result.kind !== "response") return this.nativeFailure(result);
      if (result.code === "feature_unavailable")
        return { ok: true, feature: false, entries: [] };
      if (result.code !== undefined || result.payload === undefined)
        return failure(
          result.code ?? "unavailable",
          result.message ?? "The request is unavailable",
        );
      const { request_id: _requestID, ...payload } = result.payload;
      const entries = payload["entries"];
      if (!Array.isArray(entries))
        return failure(
          "invalid_response",
          "The daemon returned invalid activity entries",
        );
      return { ok: true, feature: true, ...payload, entries };
    }
    if (!features.includes(ACTIVITY_FEED_FEATURE))
      return { ok: true, feature: false, entries: [] };
    const result = await this.requestNative(
      "activity_request",
      request.limit === undefined ? {} : { limit: request.limit },
      "activity_response",
      ACTIVITY_FEED_FEATURE,
      false,
    );
    if (result.kind !== "response") return this.nativeFailure(result);
    if (result.code === "feature_unavailable")
      return { ok: true, feature: false, entries: [] };
    if (result.code !== undefined || result.payload === undefined)
      return failure(
        result.code ?? "unavailable",
        result.message ?? "The request is unavailable",
      );
    const entries = result.payload["entries"];
    if (!Array.isArray(entries))
      return failure(
        "invalid_response",
        "The daemon returned invalid activity entries",
      );
    return {
      ok: true,
      feature: true,
      entries,
      has_more: false,
      latest_seq: entries.reduce((max, entry) => Math.max(max, entry.seq), 0),
    };
  }

  // -------------------------------------------------------------------------
  // ADR-0019: on-page bulk acquisition. Scanning and the local snapshot store
  // are pure browser-local state (Decision 4); only the status/submit round
  // trips below touch the daemon.
  // -------------------------------------------------------------------------

  /** Tab-derived facts the snapshot needs beyond origin. `title` is
   * browser-local UI decoration only (ADR-0019 operator UX requirement:
   * the workspace header names the source page) — it is carried on
   * PageBulkSnapshotView, never on the daemon-facing PageBulkSubmitSource
   * (Decision 6: origin only, never page title). */
  private async pageBulkTabMeta(
    tabID: number,
  ): Promise<{ origin: string; title: string } | null> {
    try {
      const tab = await this.deps.tabs.get(tabID);
      const origin = bareHTTPSOrigin(tab.url);
      if (origin === null) return null;
      const title = tab.title?.trim();
      return {
        origin,
        title: title !== undefined && title !== "" ? title : origin,
      };
    } catch {
      return null;
    }
  }

  /** Inject scanDocument into the tab's top frame (Decision 3: no iframes —
   * executeScript's default target is the top frame only) and validate the
   * shape of what comes back. `scanned` is cast to page-scan.ts's own
   * declared ScanResult, the same convention capturePage's caller uses for
   * its PageCapture result, then checked field-by-field before use. */
  private async executePageScan(tabID: number): Promise<
    | {
        ok: true;
        items: DetectedPaper[];
        truncated: boolean;
        renderedRecordCountHint: number | null;
      }
    | BrokerFailure
  > {
    let tabURL: string | undefined;
    try {
      tabURL = (await this.deps.tabs.get(tabID)).url;
    } catch {
      return failure("scan_failed", "Could not scan the page");
    }
    let injected: { result?: unknown } | undefined;
    try {
      [injected] = await this.deps.scripting.executeScript({
        target: { tabId: tabID },
        func: scanDocument,
        args: [tabURL ?? ""],
      });
    } catch {
      return failure("scan_failed", "Could not scan the page");
    }
    const scanned = injected?.result as ScanResult | undefined;
    if (
      scanned === undefined ||
      !Array.isArray(scanned.papers) ||
      typeof scanned.truncated !== "boolean" ||
      (scanned.renderedRecordCountHint !== null &&
        typeof scanned.renderedRecordCountHint !== "number")
    ) {
      return failure("scan_failed", "Could not scan the page");
    }
    return {
      ok: true,
      items: scanned.papers,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
  }

  private async loadPageBulkStore(): Promise<PageBulkScanStore> {
    if (this.deps.pageBulkScans === undefined) return emptyPageBulkScanStore();
    return this.deps.pageBulkScans.get();
  }

  private sanitizePageBulkSnapshot(
    snapshot: PageBulkSnapshot,
  ): PageBulkSnapshot {
    const items = snapshot.items.map((item) => {
      if (item.kind !== "pdf_grab") return item;
      const record = { ...(item as unknown as Record<string, unknown>) };
      delete record["url"];
      delete record["href"];
      delete record["opened_href"];
      delete record["finalUrl"];
      return record as unknown as typeof item;
    });
    return { ...snapshot, items };
  }

  private async savePageBulkSnapshot(
    snapshot: PageBulkSnapshot,
  ): Promise<void> {
    if (this.deps.pageBulkScans === undefined) return;
    const store = await this.deps.pageBulkScans.get();
    await this.deps.pageBulkScans.set(
      withPageBulkSnapshot(store, this.sanitizePageBulkSnapshot(snapshot)),
    );
  }

  private async savePdfGrabSnapshotState(
    scanID: string,
    grabID: string,
    state: string,
    detail?: string,
  ): Promise<void> {
    if (this.deps.pageBulkScans === undefined || scanID === "") return;
    const store = await this.deps.pageBulkScans.get();
    const snapshot = store.byId[scanID];
    if (snapshot === undefined) return;
    const items = snapshot.items.map((item) => {
      if (item.kind !== "pdf_grab") return item;
      const { url: _url, ...safeItem } = item;
      return {
        ...safeItem,
        grab_id: grabID,
        grab_state: state,
        ...(detail !== undefined ? { grab_detail: detail } : {}),
      } as typeof item;
    });
    await this.deps.pageBulkScans.set(
      withPageBulkSnapshot(
        store,
        this.sanitizePageBulkSnapshot({ ...snapshot, items }),
      ),
    );
  }

  /** Scan the tab's top frame and persist a fresh snapshot (generation 1).
   * The explicit click is the consent for this one-shot local scan. */
  async runPageBulkScan(
    tabID: number,
    expectedOrigin: string,
  ): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const meta = await this.pageBulkTabMeta(tabID);
    if (meta === null) {
      return failure(
        "invalid_page",
        "papio can only scan an ordinary secure (https) page",
      );
    }
    if (meta.origin !== expectedOrigin)
      return failure("page_changed", "The source page changed — try again");
    const scanned = await this.executePageScan(tabID);
    if (!scanned.ok) return scanned;
    const snapshot: PageBulkSnapshotView = {
      scanId: this.deps.randomUUID(),
      sourceTabId: tabID,
      sourceOrigin: meta.origin,
      sourceTitle: meta.title,
      pdfGrabAvailable: this.pdfGrabAvailable(),
      scannedAt: new Date().toISOString(),
      documentGeneration: 1,
      items: scanned.items,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
    await this.savePageBulkSnapshot(snapshot);
    return { ok: true, snapshot };
  }

  /** Scan and open one selection workspace per active scan (Decision 4: a
   * new tab per scan, never a singleton like the inbox). */
  async startPageBulkScan(
    tabID: number,
    expectedOrigin: string,
    pageBulkBaseURL: string,
  ): Promise<BrokerReply<{ scan_id: string }>> {
    const scanned = await this.runPageBulkScan(tabID, expectedOrigin);
    if (!scanned.ok) return scanned;
    try {
      await this.deps.tabs.create({
        url: `${pageBulkBaseURL}?scan=${encodeURIComponent(scanned.snapshot.scanId)}`,
        active: true,
      });
    } catch {
      return failure("open_failed", "Could not open the selection workspace");
    }
    return { ok: true, scan_id: scanned.snapshot.scanId };
  }

  /** Re-run the scan for an already-open workspace's scanId (the Rescan
   * button), bumping documentGeneration so a superseded reply can be
   * detected client-side. Reuses the scanId — never a new storage slot. */
  async requestPageBulkRescan(
    scanID: string,
  ): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const store = await this.loadPageBulkStore();
    const existing = store.byId[scanID];
    if (existing === undefined)
      return failure("scan_not_found", "This scan is no longer open");
    const meta = await this.pageBulkTabMeta(existing.sourceTabId);
    if (meta === null)
      return failure(
        "tab_unavailable",
        "The source tab is no longer available",
      );
    // Keep the snapshot bound to sourceOrigin. A source tab that has since
    // moved elsewhere must not be scanned under the old snapshot binding.
    if (meta.origin !== existing.sourceOrigin)
      return failure(
        "source_changed",
        "The source tab moved to another site — start a new scan",
      );
    const scanned = await this.executePageScan(existing.sourceTabId);
    if (!scanned.ok) return scanned;
    const priorGrab = new Map(
      existing.items
        .filter((item) => item.kind === "pdf_grab")
        .map((item) => {
          const record = item as unknown as Record<string, unknown>;
          return [item.title ?? item.label ?? "", record] as const;
        })
        .filter((entry) => typeof entry[1]["grab_id"] === "string"),
    );
    const items = scanned.items.map((item) => {
      if (item.kind !== "pdf_grab") return item;
      const prior = priorGrab.get(item.title ?? item.label ?? "");
      if (prior === undefined) return item;
      return {
        ...item,
        grab_id: prior["grab_id"],
        ...(typeof prior["grab_state"] === "string"
          ? { grab_state: prior["grab_state"] }
          : {}),
        ...(typeof prior["grab_detail"] === "string"
          ? { grab_detail: prior["grab_detail"] }
          : {}),
      };
    });
    const snapshot: PageBulkSnapshotView = {
      scanId: scanID,
      sourceTabId: existing.sourceTabId,
      sourceOrigin: meta.origin,
      sourceTitle: meta.title,
      pdfGrabAvailable: this.pdfGrabAvailable(),
      scannedAt: new Date().toISOString(),
      documentGeneration: existing.documentGeneration + 1,
      items,
      truncated: scanned.truncated,
      renderedRecordCountHint: scanned.renderedRecordCountHint,
    };
    await this.savePageBulkSnapshot(snapshot);
    return { ok: true, snapshot };
  }

  /** Load an already-open workspace's snapshot without rescanning — the
   * page-bulk.ts route's initial `?scan=<id>` read, and the missing half of
   * the scan/rescan pair the predecessor landed (a workspace tab reloading
   * or a fresh tab opened at ?scan=<id> had no way to fetch its snapshot
   * without this). Returns scan_not_found once the snapshot has aged out of
   * the bounded PAGE_BULK_SNAPSHOT_LIMIT store or the browser session ended
   * (Decision 4: chrome.storage.session only, never persisted past the
   * session) — the operator-visible "scan expired" state. */
  async getPageBulkSnapshot(
    scanID: string,
  ): Promise<BrokerReply<{ snapshot: PageBulkSnapshotView }>> {
    const store = await this.loadPageBulkStore();
    const existing = store.byId[scanID] as PageBulkSnapshotView | undefined;
    if (existing === undefined)
      return failure("scan_not_found", "This scan is no longer open");
    return { ok: true, snapshot: existing };
  }

  /** Deliberately holder-independent, and `holderRole()` must never be added.
   * A grab is user-initiated and self-routing: the requesting session receives
   * its own steering path and performs its own download into it, and adoption
   * is by directory, not by session. The concurrency fence is the single
   * effect-permit lane, which is unchanged. Requiring the session slot here
   * only stole the researcher's other browser for nothing. */
  pdfGrabAvailable(): boolean {
    return (
      this.daemonNegotiated() &&
      this.deps.downloads.onDeterminingFilename !== undefined &&
      (this.store.daemonFeatures ?? []).includes(PDF_GRAB_FEATURE) &&
      (this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE)
    );
  }

  /** Why this browser cannot grab, in the researcher's words. Two boundaries
   * refuse independently — the delivery entry point and the grab call itself,
   * because the daemon underneath can be swapped between them — and they must
   * never disagree about the remedy. */
  private grabUnavailableText(): string {
    return this.deps.downloads.onDeterminingFilename === undefined
      ? "This browser can't hand a saved PDF to papio — use the viewer Download button instead."
      : pdfGrabRefusalText(
          this.daemonNegotiated() ? "daemon_unsupported" : "no_session",
        );
  }

  private notifyPdfGrab(
    scanID: string,
    grabID: string,
    state: string,
    detail?: string,
  ): void {
    const displayState = durablePdfGrabState(state) ?? state;
    void this.savePdfGrabSnapshotState(
      scanID,
      grabID,
      displayState,
      detail,
    ).catch(() => {});
    const send = this.deps.runtimeSendMessage;
    if (send === undefined) return;
    void send({
      type: "papio.pageBulk.grabState",
      scan_id: scanID,
      grab_id: grabID,
      state: displayState,
      ...(detail !== undefined ? { detail } : {}),
    }).catch(() => {});
  }
  /** Drop armed-route steering for a grab that is over. The correlation record
   * may already be gone; the route and track url are what keep matching later
   * downloads after terminal settlement. */
  private evictPdfGrabRouteSteering(
    grabID: string,
    correlation?: PdfGrabCorrelation,
  ): void {
    const record = correlation ?? this.pdfGrabCorrelations.get(grabID);
    const route = record?.route;
    if (route !== undefined) {
      const pending = this.pendingGrabDownloadURLs.get(route);
      if (pending?.grabID === grabID) {
        this.pendingGrabDownloadURLs.delete(route);
      }
    }
    const track = this.grabDownloads.get(grabID);
    if (track !== undefined) {
      const trackRoute = isDownloadRoute(track.url)
        ? track.url
        : downloadRoute(track.url);
      if (trackRoute !== undefined) {
        const pending = this.pendingGrabDownloadURLs.get(trackRoute);
        if (pending?.grabID === grabID) {
          this.pendingGrabDownloadURLs.delete(trackRoute);
        }
      }
    }
  }

  private persistPdfGrabCorrelations(): void {
    if (this.deps.pdfGrabCorrelations === undefined) return;
    void this.deps.pdfGrabCorrelations
      .set(Object.fromEntries(this.pdfGrabCorrelations.entries()))
      .catch(() => {});
  }
  private async reconcilePdfGrabCorrelations(): Promise<void> {
    for (const [grabID, correlation] of this.pdfGrabCorrelations) {
      if (correlation.abandonPending === true) {
        void this.finishAbandon(grabID, correlation);
        continue;
      }
      // No download of papio's own: this grab is armed and waiting for the
      // researcher to press the viewer's Download button. Re-register the
      // steering this worker lost when it died, and leave the grab alone.
      if (correlation.downloadID === undefined) {
        const route = correlation.route;
        if (route === undefined || !isDownloadRoute(route)) continue;
        this.grabDownloads.set(grabID, {
          ids: new Set<number>(),
          tabID: correlation.tabID,
          scanID: correlation.scanID,
          url: route,
          steeringPath: correlation.steeringPath,
        });
        this.pendingGrabDownloadURLs.set(route, {
          grabID,
          tabID: correlation.tabID,
          steeringPath: correlation.steeringPath,
        });
        continue;
      }
      const downloadID = correlation.downloadID;
      let items: DownloadItemLike[];
      try {
        items = await this.deps.downloads.search({ id: downloadID });
      } catch {
        continue;
      }
      const item = items[0];
      if (item?.state === "interrupted") {
        correlation.abandonPending = true;
        this.persistPdfGrabCorrelations();
        void this.finishAbandon(grabID, correlation);
        continue;
      }
      if (item?.state === "complete") continue;
      this.grabDownloads.set(grabID, {
        ids: new Set([downloadID]),
        tabID: correlation.tabID,
        scanID: correlation.scanID,
        url: item?.url ?? item?.finalUrl ?? "",
        steeringPath: correlation.steeringPath,
      });
    }
  }

  private async finishAbandon(
    grabID: string,
    correlation: PdfGrabCorrelation,
  ): Promise<void> {
    const result = await this.abandonPdfGrab(
      grabID,
      correlation.effectRequestID,
    );
    if (!result.ok) return;
    if (result.outcome === "conflict") {
      // The daemon still owns a live, conflicting state; report exactly it.
      this.notifyPdfGrab(
        correlation.scanID,
        grabID,
        result.state,
        result.detail,
      );
    } else if (
      result.outcome === "unavailable" ||
      result.outcome === "not_found"
    ) {
      // Refusals, not cancellations. Defaulting these to "abandoned" told the
      // researcher their download had been cancelled when the daemon had
      // declined to cancel it and still owned whatever the grab became. A
      // refusal may also carry no state at all, and "failed" is the only
      // honest display state for a grab this browser can no longer speak for.
      this.notifyPdfGrab(
        correlation.scanID,
        grabID,
        durablePdfGrabState(result.state) ?? "failed",
        pdfGrabRefusalText(undefined, result.detail),
      );
    } else {
      // "abandoned", or a daemon that classified nothing and whose reported
      this.notifyPdfGrab(
        correlation.scanID,
        grabID,
        "abandoned",
        "The PDF grab download was interrupted",
      );
    }
    this.evictPdfGrabRouteSteering(grabID, correlation);
    this.grabDownloads.delete(grabID);
    this.pdfGrabCorrelations.delete(grabID);
    this.persistPdfGrabCorrelations();
  }

  /** A grab allocation, plus at most one retry after clearing a stale grab this
   * browser can no longer steer. The retry is here rather than inside
   * `attemptPdfGrab` because that method holds the effect governor for the
   * duration, and re-entering it would answer `effect_busy`. */
  async requestPdfGrab(request: {
    tab_id: number;
    url?: string;
    title?: string | undefined;
    workspace_tab_id?: number | undefined;
    scan_id?: string | undefined;
  }): Promise<BrokerReply<{ grab_id: string; awaiting_viewer?: boolean }>> {
    const first = await this.attemptPdfGrab(request, false);
    if (first.ok || first.error?.code !== "orphan_cleared") return first;
    return this.attemptPdfGrab(request, true);
  }

  private async attemptPdfGrab(
    request: {
      tab_id: number;
      url?: string;
      title?: string | undefined;
      workspace_tab_id?: number | undefined;
      scan_id?: string | undefined;
    },
    retried: boolean,
  ): Promise<BrokerReply<{ grab_id: string; awaiting_viewer?: boolean }>> {
    // The old single sentence named "Chrome download steering and a compatible
    // daemon" for three unrelated causes at once; each has its own remedy.
    if (!this.pdfGrabAvailable())
      return failure("feature_unavailable", this.grabUnavailableText());
    let requestURL = request.url;
    if (requestURL === undefined) {
      try {
        requestURL = (await this.deps.tabs.get(request.tab_id)).url;
      } catch {
        requestURL = undefined;
      }
    }
    if (typeof requestURL !== "string" || requestURL === "")
      return failure(
        "invalid_request",
        "Reopen or rescan the PDF tab to grab it",
      );
    let host: string;
    try {
      host = new URL(requestURL).hostname;
    } catch {
      return failure("invalid_request", "The PDF tab URL is invalid");
    }
    const effectJobID = `pdf-grab:${request.tab_id}:${request.scan_id ?? ""}`;
    const effectToken = this.claimEffectGovernor(effectJobID);
    if (effectToken === undefined) {
      this.pendingPdfGrabRequests.set(effectJobID, request);
      return failure(
        "effect_busy",
        "PDF grab will start when the current browser effect finishes",
      );
    }
    // The daemon stores this id as the grab's `effect_request_id` and fences
    // cancellation on it: a grab id alone must never release occupancy. So the
    // id has to be minted here and retained, or no abandon this extension sends
    // can ever match — every interruption report was answered `conflict` and
    // the grab stayed occupying.
    const effectRequestID = this.deps.randomUUID().replace(/-/g, "");
    try {
      const result = await this.requestNative(
        "pdf_grab_request",
        {
          host,
          // Same rule as page_acquire: a URL-derived tab title must not smuggle
          // the address past a frame that was reduced to host and title.
          ...(request.title !== undefined && !isURLLike(request.title) ? { title: request.title } : {}),
        },
        "pdf_grab_result",
        PDF_GRAB_FEATURE,
        true,
        undefined,
        effectRequestID,
      );
      if (result.kind !== "response" || result.payload === undefined)
        return this.nativeFailure(result);
      if (result.code !== undefined)
        return failure(
          result.code,
          result.message ?? "The PDF grab is unavailable",
        );
      const outcome = result.payload["outcome"];
      const grabID = result.payload["grab_id"];
      const steeringPath = result.payload["steering_path"];
      if (outcome === "existing" && typeof grabID === "string") {
        const status = await this.requestPdfGrabStatus(grabID);
        if (status.ok)
          this.notifyPdfGrab(
            request.scan_id ?? "",
            grabID,
            status.state,
            status.detail,
          );
        // The daemon is idempotent per tab and deliberately withholds steering
        // for a grab it already allocated, so "existing" alone says nothing
        // about whether bytes are on their way. Only this browser knows: it is
        // the one that armed the grab, and it persists that arming. Three
        // distinct states hide behind this one outcome, and answering all of
        // them with a bare `ok` reported success for a grab that was waiting
        // for a download nobody was performing.
        const known = this.pdfGrabCorrelations.get(grabID);
        if (known === undefined) {
          // Armed by a browser generation whose memory is gone: session-scoped
          // correlations do not survive an extension reload, and only the
          // browser that armed a grab can steer bytes into it. Reaching here at
          // all means this session is the holder — a non-holder grab request is
          // refused long before — so nothing live can be speaking for it. It
          // would otherwise occupy this tab for the daemon's six-hour stale
          // bound, which is the same permanently-unsendable paper as before,
          // just quieter. Retire it on the evidence and allocate a fresh one.
          if (retried)
            return failure(
              "grab_unresumable",
              "papio can't start a new download for this tab yet — reopen the PDF from the article page and try again",
            );
          // `ok` alone is not clearance: the daemon answers a refused
          // cancellation with `outcome: "conflict"` inside a successful reply,
          // and retrying on that just repeats the same refusal.
          const cleared = await this.abandonPdfGrab(grabID);
          if (!cleared.ok || cleared.outcome !== "abandoned")
            return failure(
              "grab_unresumable",
              "papio is still finishing an earlier attempt for this tab — reopen the PDF from the article page and try again",
            );
          this.evictPdfGrabRouteSteering(grabID);
          // Retry outside this effect lease: re-claiming the same governor key
          // from inside it would only answer `effect_busy`. `orphan_cleared` is
          // internal and never reaches a researcher.
          return failure("orphan_cleared", "the stale grab was cleared");
        }
        if (known.downloadID !== undefined && known.abandonPending !== true)
          // papio's own fetch is genuinely in flight; "sending" is true. A
          // correlation already being given up on is not in flight, so it falls
          // through rather than reporting a download that is over.
          return { ok: true, grab_id: grabID };
        // Armed and waiting for the researcher's own download. Re-register the
        // steering for the route in front of them now — the earlier arming may
        // have been for a page this tab has since navigated away from — and ask
        // for the click that can still complete it.
        const resumeRoute = downloadRoute(requestURL);
        if (resumeRoute === undefined)
          return failure(
            "grab_unresumable",
            "papio can't read this page's address — reload the PDF and try again",
          );
        const resumeTabID = request.workspace_tab_id ?? request.tab_id;
        // The previous route must go, or it keeps matching: `sameDownloadRoute`
        // ignores the query, so a later download on the route this tab used to
        // hold would still be steered into this grab.
        if (known.route !== undefined && known.route !== resumeRoute)
          this.pendingGrabDownloadURLs.delete(known.route);
        this.grabDownloads.set(grabID, {
          ids: new Set<number>(),
          tabID: resumeTabID,
          scanID: known.scanID,
          url: resumeRoute,
          steeringPath: known.steeringPath,
        });
        this.pendingGrabDownloadURLs.set(resumeRoute, {
          grabID,
          tabID: resumeTabID,
          steeringPath: known.steeringPath,
        });
        this.pdfGrabCorrelations.set(grabID, {
          ...known,
          tabID: resumeTabID,
          state: "awaiting_viewer",
          route: resumeRoute,
        });
        this.persistPdfGrabCorrelations();
        return { ok: true, grab_id: grabID, awaiting_viewer: true };
      }
      if (
        outcome !== "steering" ||
        typeof grabID !== "string" ||
        typeof steeringPath !== "string"
      ) {
        // `reason` is the daemon's machine classification; `detail` is prose it
        // wrote for a log. One translation point decides which the researcher
        // sees, so no refusal reaches the popup naming holdership or permits.
        return failure(
          "grab_failed",
          pdfGrabRefusalText(
            result.payload["reason"],
            typeof result.payload["detail"] === "string"
              ? result.payload["detail"]
              : undefined,
          ),
        );
      }
      const workspaceTabID = request.workspace_tab_id ?? request.tab_id;
      const scanID = request.scan_id ?? "";
      const armedRoute = downloadRoute(requestURL) ?? requestURL;
      this.grabDownloads.set(grabID, {
        ids: new Set<number>(),
        tabID: workspaceTabID,
        scanID,
        url: armedRoute,
        steeringPath,
      });
      this.pendingGrabDownloadURLs.set(armedRoute, {
        grabID,
        tabID: workspaceTabID,
        steeringPath,
      });
      // A signed delivery URL cannot be fetched again, so do not try. The grab
      // and its steering are already armed above, and the pending entry is
      // deliberately left in place: the researcher's own viewer Download click
      // arrives later, matches this route in `pendingGrabFor`, and is steered
      // into the grab directory (a live grab outranks an inferred job there).
      // Returning `ok` without a download would claim work papio never did, so
      // the caller reports the one action that can still complete the grab.
      if (requiresNativeViewerDownload(requestURL)) {
        if (this.isFirefox())
          // Firefox has no onDeterminingFilename, so a download papio did not
          // start can never be steered into the grab or adopted from it. Asking
          // for the viewer button here would promise filing that cannot happen.
          return failure(
            "not_permitted",
            "This publisher's link can only be used once, and papio can't adopt a download on Firefox — open the PDF in Chrome to send it",
          );
        this.pdfGrabCorrelations.set(grabID, {
          scanID,
          tabID: workspaceTabID,
          state: "awaiting_viewer",
          route: armedRoute,
          steeringPath,
          effectRequestID,
        });
        this.persistPdfGrabCorrelations();
        return { ok: true, grab_id: grabID, awaiting_viewer: true };
      }
      try {
        const id = await this.deps.downloads.download({
          url: requestURL,
          conflictAction: "uniquify",
          saveAs: false,
        });
        const track = this.grabDownloads.get(grabID);
        if (track !== undefined) track.ids.add(id);
        this.pdfGrabCorrelations.set(grabID, {
          scanID,
          tabID: workspaceTabID,
          state: "grabbed",
          downloadID: id,
          steeringPath,
          effectRequestID,
        });
        this.persistPdfGrabCorrelations();
        // Chrome can interrupt a download before `download()` resolves — an
        // expired signing token does exactly that — and the `onChanged` delta
        // carrying that failure arrived while this id was still untracked, so
        // `trackedGrabFor` dropped it. The grab then sat `awaiting_file` with
        // its permit occupying the single effect lane until the next worker
        // start ran `reconcilePdfGrabCorrelations`. Re-read the item now that
        // it is tracked: the daemon settles this permit the moment it is given
        // definite evidence, and refuses to guess without it.
        let interrupted = false;
        try {
          const found = await this.deps.downloads.search({ id });
          interrupted = found[0]?.state === "interrupted";
        } catch {}
        if (interrupted) {
          const correlation = this.pdfGrabCorrelations.get(grabID);
          if (correlation !== undefined) {
            correlation.abandonPending = true;
            this.persistPdfGrabCorrelations();
            await this.finishAbandon(grabID, correlation);
          }
          return failure(
            "grab_failed",
            "papio couldn't download this PDF — the link didn't work. Use the PDF viewer Download button instead.",
          );
        }
        this.notifyPdfGrab(scanID, grabID, "grabbed");
        return { ok: true, grab_id: grabID };
      } catch {
        try {
          await this.abandonPdfGrab(grabID, effectRequestID);
        } catch {}
        const failedCorrelation = this.pdfGrabCorrelations.get(grabID);
        this.evictPdfGrabRouteSteering(grabID, failedCorrelation);
        this.grabDownloads.delete(grabID);
        this.pdfGrabCorrelations.delete(grabID);
        this.persistPdfGrabCorrelations();
        return failure("grab_failed", "Could not start the browser download");
      } finally {
        this.pendingGrabDownloadURLs.delete(requestURL);
      }
    } finally {
      this.releaseEffectGovernor(effectJobID, effectToken, false);
      this.wakeEffectGovernor();
    }
  }

  async requestPdfGrabStatus(grabID: string): Promise<
    BrokerReply<{
      grab_id: string;
      state: string;
      outcome?: string;
      detail?: string;
      job_id?: string;
    }>
  > {
    const result = await this.requestNative(
      "pdf_grab_status_request",
      { grab_id: grabID },
      "pdf_grab_status_result",
      PDF_GRAB_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The PDF grab status is unavailable",
      );
    const state = result.payload["state"];
    const returnedGrabID = result.payload["grab_id"];
    if (typeof state !== "string" || typeof returnedGrabID !== "string") {
      return failure(
        "grab_failed",
        "The daemon returned an invalid PDF grab status",
      );
    }
    return {
      ok: true,
      grab_id: returnedGrabID,
      state,
      ...(typeof result.payload["outcome"] === "string"
        ? { outcome: result.payload["outcome"] }
        : {}),
      ...(typeof result.payload["detail"] === "string"
        ? { detail: result.payload["detail"] }
        : {}),
      ...(typeof result.payload["job_id"] === "string"
        ? { job_id: result.payload["job_id"] }
        : {}),
    };
  }
  private async abandonPdfGrab(
    grabID: string,
    effectRequestID?: string,
  ): Promise<
    BrokerReply<{
      grab_id: string;
      state: string;
      outcome?: string;
      detail?: string;
    }>
  > {
    const request = this.requestNative(
      "pdf_grab_abandon_request",
      { grab_id: grabID },
      "pdf_grab_abandon_result",
      PDF_GRAB_FEATURE,
      true,
      undefined,
      // The daemon fences cancellation on the grab's originating request id, so
      // reusing it here is what makes this call effective rather than a
      // `conflict` the caller then has to interpret. Absent for a grab this
      // worker generation did not arm; the daemon answers on its own evidence.
      effectRequestID,
    );
    const result = await Promise.race([
      request,
      new Promise<NativeRequestResult>((resolve) =>
        setTimeout(() => resolve({ kind: "timeout" }), 2000),
      ),
    ]);
    if (result.kind !== "response" || result.payload === undefined) {
      return failure(
        result.kind === "timeout" ? "connection_timeout" : "transport_error",
        "The daemon did not acknowledge the PDF grab abandonment",
      );
    }
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The daemon could not abandon the PDF grab",
      );
    const state = result.payload["state"];
    const returnedGrabID = result.payload["grab_id"];
    if (typeof state !== "string" || typeof returnedGrabID !== "string") {
      return failure(
        "grab_failed",
        "The daemon returned an invalid PDF grab abandonment result",
      );
    }
    return {
      ok: true,
      grab_id: returnedGrabID,
      state,
      ...(typeof result.payload["outcome"] === "string"
        ? { outcome: result.payload["outcome"] }
        : {}),
      ...(typeof result.payload["detail"] === "string"
        ? { detail: result.payload["detail"] }
        : {}),
    };
  }

  async requestPageBulkStatus(request: {
    scan_id: string;
    identifiers: PageBulkIdentifier[];
    rendered_record_count_hint?: number;
  }): Promise<
    BrokerReply<{ items: PageBulkStatusItem[]; truncated: boolean }>
  > {
    const result = await this.requestNative(
      "page_bulk_status_request",
      request,
      "page_bulk_status_result",
      PAGE_BULK_ACQUIRE_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    const items = result.payload["items"];
    const truncated = result.payload["truncated"];
    if (!Array.isArray(items) || typeof truncated !== "boolean") {
      return failure(
        "invalid_response",
        "The daemon returned an invalid page-bulk status result",
      );
    }
    return { ok: true, items: items as PageBulkStatusItem[], truncated };
  }

  /** Submit a complete ordered manifest. Background owns v2 chunking and
   * recovery; old daemons receive only the first 50 keys through v1. */
  async requestPageBulkSubmit(request: {
    scan_id: string;
    canonical_keys: string[];
    source: PageBulkSubmitSource;
  }): Promise<
    BrokerReply<{
      mode: "v1" | "v2";
      processed_count: number;
      submitted: number;
      joined: number;
      already_owned: number;
      invalid: number;
      batch_id: string;
    }>
  > {
    const v2Available =
      this.daemonNegotiated() &&
      (this.store.daemonFeatures ?? []).includes(PAGE_BULK_COHORT_V2_FEATURE);
    if (!v2Available) {
      const keys = request.canonical_keys.slice(0, 50);
      const result = await this.requestNative(
        "page_bulk_submit_request",
        { ...request, canonical_keys: keys },
        "page_bulk_submit_result",
        PAGE_BULK_ACQUIRE_FEATURE,
        true,
      );
      if (result.kind !== "response" || result.payload === undefined)
        return this.nativeFailure(result);
      if (result.code !== undefined)
        return failure(
          result.code,
          result.message ?? "The request is unavailable",
        );
      const payload = result.payload;
      const submitted = payload["submitted"];
      const joined = payload["joined"];
      const alreadyOwned = payload["already_owned"];
      const invalid = payload["invalid"];
      const batchID = payload["batch_id"];
      if (
        typeof submitted !== "number" ||
        typeof joined !== "number" ||
        typeof alreadyOwned !== "number" ||
        typeof invalid !== "number" ||
        typeof batchID !== "string"
      )
        return failure(
          "invalid_response",
          "The daemon returned an invalid page-bulk submit result",
        );
      return {
        ok: true,
        mode: "v1",
        processed_count: keys.length,
        submitted,
        joined,
        already_owned: alreadyOwned,
        invalid,
        batch_id: batchID,
      };
    }

    const source: PageBulkRecoverySource = {
      kind: "browser_page",
      origin: request.source.origin,
      detector: request.source.detector,
    };
    const cohortID = this.nextRequestID();
    const total = request.canonical_keys.length;
    const totalChunks = Math.ceil(total / 50);
    const firstIndex = 0;
    const firstKeys = request.canonical_keys.slice(0, 50);
    const firstFinal = totalChunks === 1;
    const firstRequestID = this.nextRequestID();
    const firstDigest = await pageBulkPayloadDigest({
      scan_id: request.scan_id,
      cohort_id: cohortID,
      source,
      cohort_total: total,
      chunk_index: firstIndex,
      final_chunk: firstFinal,
      canonical_keys: firstKeys,
    });
    const first: PageBulkRecoveryCohort = {
      cohort_id: cohortID,
      scan_id: request.scan_id,
      source,
      cohort_total: total,
      canonical_keys: [...request.canonical_keys],
      next_chunk: 0,
      unresolved: {
        request_id: firstRequestID,
        chunk_index: firstIndex,
        payload_digest: firstDigest,
      },
      updated_at: new Date(this.deps.now()).toISOString(),
    };
    try {
      await this.pageBulkRecovery.put(first);
    } catch {
      return failure(
        "recovery_storage",
        "Could not persist page-bulk recovery state",
      );
    }

    let submitted = 0;
    let joined = 0;
    let alreadyOwned = 0;
    let invalid = 0;
    let batchID = "";
    for (let index = 0; index < totalChunks; index += 1) {
      const loaded = await this.pageBulkRecovery.load();
      const cohort = loaded.cohorts[cohortID];
      if (cohort === undefined) {
        // A completed final chunk removes the entry; there is no next send.
        break;
      }
      const keys = chunkKeysFor(cohort, index);
      const finalChunk = index === totalChunks - 1;
      let unresolved = cohort.unresolved;
      if (cohort.next_chunk !== index) {
        if (cohort.next_chunk > index) continue;
        return failure(
          "recovery_state",
          "Page-bulk recovery state is out of sequence",
        );
      }
      if (unresolved === undefined || unresolved.chunk_index !== index) {
        const requestID = this.nextRequestID();
        const digest = await pageBulkPayloadDigest({
          scan_id: cohort.scan_id,
          cohort_id: cohort.cohort_id,
          source: cohort.source,
          cohort_total: cohort.cohort_total,
          chunk_index: index,
          final_chunk: finalChunk,
          canonical_keys: keys,
        });
        unresolved = {
          request_id: requestID,
          chunk_index: index,
          payload_digest: digest,
        };
        try {
          await this.pageBulkRecovery.put({
            ...cohort,
            unresolved,
            updated_at: new Date(this.deps.now()).toISOString(),
          });
        } catch {
          return failure(
            "recovery_storage",
            "Could not persist page-bulk recovery state",
          );
        }
      }
      const result = await this.requestNative(
        "page_bulk_submit_v2_request",
        {
          scan_id: cohort.scan_id,
          cohort_id: cohort.cohort_id,
          source: cohort.source,
          cohort_total: cohort.cohort_total,
          chunk_index: index,
          final_chunk: finalChunk,
          canonical_keys: keys,
        },
        "page_bulk_submit_v2_result",
        PAGE_BULK_COHORT_V2_FEATURE,
        true,
        undefined,
        unresolved.request_id,
      );
      if (result.kind !== "response" || result.payload === undefined)
        return this.nativeFailure(result);
      if (result.code !== undefined)
        return failure(
          result.code,
          result.message ?? "The daemon rejected the page-bulk cohort",
        );
      const payload = result.payload;
      if (
        payload["request_id"] !== unresolved.request_id ||
        payload["scan_id"] !== cohort.scan_id ||
        payload["cohort_id"] !== cohort.cohort_id ||
        payload["chunk_index"] !== index ||
        payload["final_chunk"] !== finalChunk ||
        typeof payload["batch_id"] !== "string"
      )
        return failure(
          "invalid_response",
          "The daemon returned an invalid page-bulk cohort result",
        );
      const chunkSubmitted = payload["submitted"];
      const chunkJoined = payload["joined"];
      const chunkOwned = payload["already_owned"];
      const chunkInvalid = payload["invalid"];
      if (
        typeof chunkSubmitted !== "number" ||
        typeof chunkJoined !== "number" ||
        typeof chunkOwned !== "number" ||
        typeof chunkInvalid !== "number"
      ) {
        return failure(
          "invalid_response",
          "The daemon returned an invalid page-bulk cohort result",
        );
      }
      submitted += chunkSubmitted;
      joined += chunkJoined;
      alreadyOwned += chunkOwned;
      invalid += chunkInvalid;
      batchID = payload["batch_id"];
      let applied = false;
      try {
        await this.pageBulkRecovery.update(cohortID, (current) => {
          if (
            current.next_chunk !== index ||
            current.unresolved?.request_id !== unresolved?.request_id
          )
            return current;
          applied = true;
          if (finalChunk) return undefined;
          const { unresolved: _discard, ...withoutUnresolved } = current;
          return {
            ...withoutUnresolved,
            next_chunk: index + 1,
            updated_at: new Date(this.deps.now()).toISOString(),
          };
        });
      } catch {
        return failure(
          "recovery_storage",
          "Could not commit page-bulk recovery result",
        );
      }
      if (!applied) continue;
    }
    return {
      ok: true,
      mode: "v2",
      processed_count: total,
      submitted,
      joined,
      already_owned: alreadyOwned,
      invalid,
      batch_id: batchID,
    };
  }
  private async resumePageBulkCohort(cohortID: string): Promise<void> {
    while (true) {
      const loaded = await this.pageBulkRecovery.load();
      const cohort = loaded.cohorts[cohortID];
      if (cohort === undefined || cohort.unresolved === undefined) return;
      if (
        !(this.store.daemonFeatures ?? []).includes(PAGE_BULK_COHORT_V2_FEATURE)
      )
        return;
      const index = cohort.unresolved.chunk_index;
      const keys = chunkKeysFor(cohort, index);
      const finalChunk = index === Math.ceil(cohort.cohort_total / 50) - 1;
      const result = await this.requestNative(
        "page_bulk_submit_v2_request",
        {
          scan_id: cohort.scan_id,
          cohort_id: cohort.cohort_id,
          source: cohort.source,
          cohort_total: cohort.cohort_total,
          chunk_index: index,
          final_chunk: finalChunk,
          canonical_keys: keys,
        },
        "page_bulk_submit_v2_result",
        PAGE_BULK_COHORT_V2_FEATURE,
        true,
        undefined,
        cohort.unresolved.request_id,
      );
      if (
        result.kind !== "response" ||
        result.payload === undefined ||
        result.code !== undefined
      )
        return;
      const payload = result.payload;
      if (
        payload["request_id"] !== cohort.unresolved.request_id ||
        payload["scan_id"] !== cohort.scan_id ||
        payload["cohort_id"] !== cohort.cohort_id ||
        payload["chunk_index"] !== index ||
        payload["final_chunk"] !== finalChunk
      )
        return;
      let applied = false;
      try {
        await this.pageBulkRecovery.update(cohortID, (current) => {
          if (
            current.next_chunk !== index ||
            current.unresolved?.request_id !== cohort.unresolved?.request_id
          )
            return current;
          applied = true;
          if (finalChunk) return undefined;
          const { unresolved: _discard, ...withoutUnresolved } = current;
          return {
            ...withoutUnresolved,
            next_chunk: index + 1,
            updated_at: new Date(this.deps.now()).toISOString(),
          };
        });
      } catch {
        return;
      }
      if (!applied) return;
    }
  }

  private async resumePageBulkCohorts(): Promise<void> {
    if (
      !(this.store.daemonFeatures ?? []).includes(PAGE_BULK_COHORT_V2_FEATURE)
    )
      return;
    const loaded = await this.pageBulkRecovery.load();
    for (const cohortID of Object.keys(loaded.cohorts))
      void this.resumePageBulkCohort(cohortID);
  }


  async requestTriageDecision(request: {
    item_id: string;
    op: "acquire" | "dismiss";
    watch_scope?: "all" | number[];
  }): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "triage_decide",
      request,
      "triage_decide_result",
      TRIAGE_MUTATIONS_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string"
        ? { detail: result.payload["detail"] }
        : {}),
    };
  }

  async requestActionResolve(request: {
    action_id: number;
    verdict: "accept" | "reject" | "dismiss";
    expected_revision: number;
    expected_sha256?: string;
  }): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "human_action_resolve",
      request,
      "human_action_resolve_result",
      TRIAGE_MUTATIONS_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string"
        ? { detail: result.payload["detail"] }
        : {}),
    };
  }

  // Decision 4's confirm_request_exists/confirm_request_absent mutations
  // (triage-snapshot/3). Gated on the v3 snapshot feature rather than
  // TRIAGE_MUTATIONS_FEATURE: a daemon that never emits document_delivery
  // items has nothing for this RPC to act on, and open_request_history is
  // deliberately not here — it never mutates anything and is handled
  // locally by the inbox page.
  async requestDeliveryReconcile(request: {
    job_id: string;
    operation: "confirm_request_exists" | "confirm_request_absent";
    provider_reference?: string;
  }): Promise<BrokerReply<{ outcome: string; detail?: string }>> {
    const result = await this.requestNative(
      "delivery_reconcile_request",
      request,
      "delivery_reconcile_result",
      TRIAGE_SNAPSHOT_SCHEMA_3_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    return {
      ok: true,
      outcome: result.payload["outcome"] as string,
      ...(typeof result.payload["detail"] === "string"
        ? { detail: result.payload["detail"] }
        : {}),
    };
  }

  async requestPreview(request: { action_id: number }): Promise<
    BrokerReply<{
      outcome: string;
      detail?: string;
      preview?: Record<string, unknown>;
    }>
  > {
    const result = await this.requestNative(
      "review_preview_request",
      request,
      "review_preview_result",
      REVIEW_PREVIEW_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The request is unavailable",
      );
    const outcome = result.payload["outcome"] as string;
    if (outcome === "error") {
      return {
        ok: true,
        outcome,
        ...(typeof result.payload["detail"] === "string"
          ? { detail: result.payload["detail"] }
          : {}),
      };
    }
    const {
      request_id: _requestID,
      outcome: _outcome,
      ...preview
    } = result.payload;
    return { ok: true, outcome, preview };
  }

  // requestGrabSuggestions/requestGrabConfirm are the picker's read then
  // write, mirroring Bridge.SuggestGrabCandidates/ConfirmGrabCandidate on
  // the daemon: the suggest RPC re-validates the parked bytes and ranks the
  // live candidate pool fresh on every call (never cached — a stored list
  // would name a job the pool has since filed or abandoned), and confirm
  // binds through the same fenced operator_confirm path autonomous binding
  // uses. Both are gated on PDF_GRAB_SUGGEST_FEATURE so an older daemon that
  // never advertised the picker is never sent either frame: requestNative's
  // own feature check answers `feature_unavailable` before anything is sent,
  // and the inbox's own daemonFeatures gate (read from the persisted store)
  // keeps the button from offering the picker at all in that case — this is
  // the second, server-side backstop.
  async requestGrabSuggestions(request: { grab_id: string; limit?: number }): Promise<
    BrokerReply<{
      grab_id: string;
      outcome: string;
      detail?: string;
      document_identifiers: Array<{ kind: string; value: string; source: string }>;
      suggestions: Array<{
        job_id: string;
        title?: string;
        year?: number;
        doi?: string;
        verdict: string;
        reason?: string;
        evidence: string[];
      }>;
      truncated: boolean;
    }>
  > {
    const result = await this.requestNative(
      "pdf_grab_suggest_request",
      request,
      "pdf_grab_suggest_response",
      PDF_GRAB_SUGGEST_FEATURE,
      false,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "PDF grab suggestions are unavailable",
      );
    const payload = result.payload;
    return {
      ok: true,
      grab_id: payload["grab_id"] as string,
      outcome: payload["outcome"] as string,
      ...(typeof payload["detail"] === "string" ? { detail: payload["detail"] } : {}),
      document_identifiers:
        (payload["document_identifiers"] as
          | Array<{ kind: string; value: string; source: string }>
          | undefined) ?? [],
      suggestions:
        (payload["suggestions"] as
          | Array<{
              job_id: string;
              title?: string;
              year?: number;
              doi?: string;
              verdict: string;
              reason?: string;
              evidence: string[];
            }>
          | undefined) ?? [],
      truncated: payload["truncated"] === true,
    };
  }

  async requestGrabConfirm(request: { grab_id: string; job_id: string }): Promise<
    BrokerReply<{ grab_id: string; job_id?: string; outcome: string; detail?: string }>
  > {
    const result = await this.requestNative(
      "pdf_grab_confirm_request",
      request,
      "pdf_grab_confirm_response",
      PDF_GRAB_SUGGEST_FEATURE,
      true,
    );
    if (result.kind !== "response" || result.payload === undefined)
      return this.nativeFailure(result);
    if (result.code !== undefined)
      return failure(
        result.code,
        result.message ?? "The pick could not be confirmed",
      );
    const payload = result.payload;
    return {
      ok: true,
      grab_id: payload["grab_id"] as string,
      outcome: payload["outcome"] as string,
      ...(typeof payload["job_id"] === "string" ? { job_id: payload["job_id"] } : {}),
      ...(typeof payload["detail"] === "string" ? { detail: payload["detail"] } : {}),
    };
  }

  /**
   * Record the user's informed terms-consent choice (popup first-use prompt),
   * clear the pending-prompt flags, and — when they consented — re-drive the
   * still-open terms gate on every flagged job so the current downloads
   * complete without a second visit. Idempotent and safe if jobs have moved on.
   */
  async requestTermsConsent(
    value: Exclude<TermsConsent, undefined>,
  ): Promise<void> {
    await this.ready;
    await this.deps.settings.setTermsConsent(value);
    if (value !== "accept") {
      // User declined auto-accept: clear the one-time prompt flag so the popup
      // stops asking; any open gate stays assisted.
      for (const jobID of this.store.activeJobs
        .filter((j) => j.needs_terms_consent === true)
        .map((j) => j.job_id)) {
        await this.update((s) =>
          patchJob(s, jobID, { needs_terms_consent: false }),
        );
      }
      return;
    }
    await this.redrivePendingTermsGates();
  }

  /** Re-drive every job still parked at a terms gate now that consent is
   * accepted: clear the one-time prompt flag and re-run classification on the
   * live provider tab so an open terms modal is accepted and the download
   * completes without a second visit. Runs when the user grants consent AND on
   * worker startup, so a grant that landed while the worker was asleep (missing
   * the one-shot re-drive) still completes on the next connect. Idempotent: a
   * job with no live tab or an already-closed modal is a no-op. */
  private async redrivePendingTermsGates(): Promise<void> {
    if ((await this.deps.settings.getTermsConsent()) !== "accept") return;
    const flagged = this.store.activeJobs
      .filter(
        (j) =>
          j.needs_terms_consent === true &&
          j.tab_id >= 0 &&
          this.hasDelegatedAuthority(j),
      )
      .map((j) => j.job_id);
    for (const jobID of flagged) {
      await this.update((s) =>
        patchJob(s, jobID, { needs_terms_consent: false }),
      );
      try {
        await this.reclassifyCurrentProviderPage(jobID);
      } catch (e) {
        console.error("papio: terms re-drive failed; staying assisted", e);
      }
    }
  }

  private async storeTermsEffect(
    correlation: TermsEffectCorrelation,
  ): Promise<void> {
    await this.update((store) => ({
      ...store,
      termsEffects: {
        ...(store.termsEffects ?? {}),
        [correlation.job_id]: correlation,
      },
    }));
  }

  private async reportTermsEffectResult(
    correlation: TermsEffectCorrelation,
  ): Promise<boolean> {
    if (correlation.result_outcome === undefined) return false;
    const result = await this.requestNative(
      "terms_effect_result_request",
      {
        permit_id: correlation.permit_id,
        terms_occurrence_id: correlation.terms_occurrence_id,
        outcome: correlation.result_outcome,
      },
      "terms_effect_result",
      EFFECT_PERMIT_FEATURE,
      true,
      correlation.job_id,
    );
    const outcome = result.payload?.["outcome"];
    if (
      result.kind !== "response" ||
      (outcome !== "applied" && outcome !== "duplicate")
    )
      return false;
    await this.update((store) => {
      const current = store.termsEffects?.[correlation.job_id];
      if (
        current === undefined ||
        current.permit_id !== correlation.permit_id ||
        current.terms_occurrence_id !== correlation.terms_occurrence_id
      )
        return store;
      return {
        ...store,
        termsEffects: {
          ...(store.termsEffects ?? {}),
          [correlation.job_id]: { ...current, acknowledged: true },
        },
      };
    });
    return true;
  }

  /** Re-send only a persisted exact result after worker restart. A correlation
   * with no result_outcome represents unknown completion and stays untouched. */
  private async retryTermsEffectResults(): Promise<void> {
    for (const correlation of Object.values(this.store.termsEffects ?? {})) {
      if (
        !correlation.acknowledged &&
        correlation.result_outcome !== undefined
      ) {
        await this.reportTermsEffectResult(correlation);
      }
    }
  }

  /** Acquire the daemon-durable permit before the configured terms click. */
  private async acceptTerms(
    jobID: string,
    spec: AdapterSpec,
  ): Promise<TermsAcceptResult> {
    const job = findByJob(this.store, jobID);
    // terms_effect_start_request is holder-only in the daemon; a pending
    // session must not click a terms gate it cannot hold a permit for.
    if (job === undefined || !this.hasDelegatedAuthority(job) || !this.holderRole())
      return "not_dispatched";
    let plan: Plan;
    try {
      const planned = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: planExecution,
        args: [
          null,
          spec,
          { ...(job.expected ?? {}) },
          { access_mode: job.access_mode },
        ],
      });
      const candidate = planned[0]?.result as PlanResult | undefined;
      if (
        candidate === undefined ||
        typeof candidate !== "object" ||
        candidate === null ||
        "assisted" in candidate ||
        candidate.verdict.kind !== "terms" ||
        candidate.target_ref !== null ||
        candidate.method !== null ||
        candidate.effect_graph === null ||
        typeof candidate.effect_graph !== "object" ||
        candidate.effect_graph.primary_target !== null ||
        candidate.effect_graph.followup_target !== null ||
        candidate.effect_graph.terms_target === null ||
        typeof candidate.effect_graph.terms_target !== "object" ||
        candidate.expected_work === null ||
        typeof candidate.expected_work !== "object" ||
        (typeof candidate.expected_work.requested_doi === "string" &&
          candidate.expected_work.requested_doi.trim() !== "" &&
          (candidate.expected_work.doi === null ||
            typeof candidate.expected_work.doi !== "object")) ||
        (typeof candidate.expected_work.requested_title === "string" &&
          candidate.expected_work.requested_title.trim() !== "" &&
          (candidate.expected_work.title === null ||
            typeof candidate.expected_work.title !== "object"))
      )
        return "not_dispatched";
      plan = candidate;
    } catch (e) {
      console.error("papio: terms accept planning failed; staying assisted", e);
      return "not_dispatched";
    }
    const latest = findByJob(this.store, jobID);
    if (!this.hasDelegatedAuthority(latest)) return "not_dispatched";
    const authorityDigest = await termsAuthorityDigest(spec);
    if (authorityDigest === undefined) return "not_dispatched";
    const start = await this.requestNative(
      "terms_effect_start_request",
      {
        adapter_id: spec.id,
        adapter_version: spec.version,
        authority_digest: authorityDigest,
      },
      "terms_effect_start_result",
      EFFECT_PERMIT_FEATURE,
      true,
      jobID,
    );
    if (start.kind !== "response")
      return start.code === "feature_unavailable"
        ? "not_dispatched"
        : "occupied";
    if (start.payload?.["outcome"] !== "started") return "occupied";
    const permitID = start.payload["permit_id"];
    const occurrenceID = start.payload["terms_occurrence_id"];
    if (typeof permitID !== "string" || typeof occurrenceID !== "string")
      return "occupied";
    let correlation: TermsEffectCorrelation = {
      job_id: jobID,
      permit_id: permitID,
      terms_occurrence_id: occurrenceID,
      authority_digest: authorityDigest,
      dispatched: false,
      acknowledged: false,
    };
    await this.storeTermsEffect(correlation);
    let accepted: boolean;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: executePlannedPageEffect,
        args: [
          plan,
          {
            method: "click",
            followupSelector: undefined,
            postClickTimeoutMs: 0,
            shadowSelector: undefined,
            metaName: undefined,
          },
        ],
      });
      accepted =
        (results[0]?.result as { ok?: boolean } | undefined)?.ok === true;
    } catch (e) {
      // The page may have mutated before the script reply. No result is
      // conclusive; leave the permit occupying for daemon reconciliation.
      console.error("papio: terms accept completion is unknown", e);
      return "occupied";
    }
    correlation = {
      ...correlation,
      dispatched: accepted,
      result_outcome: accepted ? "accepted" : "not_dispatched",
    };
    await this.storeTermsEffect(correlation);
    const acknowledged = await this.reportTermsEffectResult(correlation);
    if (!acknowledged && !accepted) return "occupied";
    return accepted ? "accepted" : "not_dispatched";
  }

  private pendingJobFor(item: DownloadItemLike): string | undefined {
    const observed = [item.url, item.finalUrl].filter(
      (v): v is string => typeof v === "string",
    );
    const jobs = new Set<string>();
    for (const [pendingURL, jobID] of this.pendingDownloadURLs) {
      if (
        observed.some(
          (url) => url === pendingURL || sameDownloadRoute(url, pendingURL),
        )
      ) {
        jobs.add(jobID);
      }
    }
    return jobs.size === 1 ? jobs.values().next().value : undefined;
  }

  /** downloads.download may resolve with the ID before Chrome asks extensions
   * to determine the filename. IDs are exact and contain no provider secret. */
  private trackedJobFor(downloadID: number): string | undefined {
    let matched: string | undefined;
    for (const [jobID, track] of this.downloads) {
      if (!track.ids.has(downloadID)) continue;
      if (matched !== undefined && matched !== jobID) return undefined;
      matched = jobID;
    }
    return matched;
  }
  private pendingGrabIDFor(item: DownloadItemLike): string | undefined {
    const observed = [item.url, item.finalUrl].filter(
      (value): value is string => typeof value === "string",
    );
    for (const [pendingURL, pending] of this.pendingGrabDownloadURLs) {
      if (
        observed.some(
          (url) => url === pendingURL || sameDownloadRoute(url, pendingURL),
        )
      ) {
        return pending.grabID;
      }
    }
    return undefined;
  }
  private pendingGrabFor(item: DownloadItemLike): PdfGrabTrack | undefined {
    const grabID = this.pendingGrabIDFor(item);
    if (grabID === undefined) return undefined;
    return this.grabDownloads.get(grabID);
  }
  private trackedGrabFor(downloadID: number): string | undefined {
    for (const [grabID, track] of this.grabDownloads) {
      if (track.ids.has(downloadID)) return grabID;
    }
    return undefined;
  }
  /** A delegated click-adapter job whose `download_initiated` latch is live on
   * the download's tab. Host-wide `correlate()` is broader and survives tab
   * reuse; ambiguity parks only when this tab-scoped job is genuinely live. */
  private liveDelegatedClickJobOnTab(
    item: DownloadItemLike,
  ): ActiveJob | undefined {
    // Firefox has no onDeterminingFilename and disables broad correlate(); only
    // exact downloads.download ids are owned there — this Chrome-only path
    // cannot steer or promise grab-vs-job resolution on Firefox.
    if (this.deps.downloads.onDeterminingFilename === undefined) return undefined;
    if (typeof item.tabId !== "number") return undefined;
    const job = findByTab(this.store, item.tabId);
    if (
      job === undefined ||
      !this.hasDelegatedAuthority(job) ||
      job.download_initiated !== true
    ) {
      return undefined;
    }
    return job;
  }
  /** Both a live same-tab delegated click job and an armed grab match this
   * download. Exact papio-initiated bindings are excluded — they are not
   * ambiguous. */
  private downloadGrabConflict(
    item: DownloadItemLike,
  ): { job: ActiveJob; grab: PdfGrabTrack; grabID: string } | undefined {
    if (this.trackedJobFor(item.id) ?? this.pendingJobFor(item)) return undefined;
    const job = this.liveDelegatedClickJobOnTab(item);
    if (job === undefined) return undefined;
    const grabID =
      this.trackedGrabFor(item.id) ?? this.pendingGrabIDFor(item);
    if (grabID === undefined) return undefined;
    const grab = this.grabDownloads.get(grabID);
    if (grab === undefined) return undefined;
    return { job, grab, grabID };
  }
  private surfaceDownloadGrabConflict(
    downloadID: number,
    conflict: { job: ActiveJob; grab: PdfGrabTrack; grabID: string },
  ): void {
    if (this.downloadGrabConflictNotified.has(downloadID)) return;
    this.downloadGrabConflictNotified.add(downloadID);
    this.notifyPdfGrab(
      conflict.grab.scanID,
      conflict.grabID,
      "failed",
      "This download matches both a handoff acquisition in progress on this tab and an armed Send PDF grab for the same file — papio claimed it for neither. Finish or cancel the handoff job, or cancel the grab.",
    );
  }
  /** Synchronous, no async work: called from inside the onUpdated/
   * onActivated listener callbacks themselves, before those callbacks'
   * own async handlers ever run. See tabTouchEpoch. */
  private touchTab(tabID: number): void {
    this.tabTouchEpoch.set(tabID, (this.tabTouchEpoch.get(tabID) ?? 0) + 1);
  }

  private downloadFilenameSuggestion(
    item: DownloadItemLike,
  ): { filename: string; conflictAction: "uniquify" } | undefined {
    // An EXACT job binding means this extension started this download for that
    // job, so nothing may hijack it. `correlate()` is different in kind: an
    // inference from the tab, DOI or host.
    const exactJobID =
      this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    const exactJob = exactJobID
      ? findByJob(this.store, exactJobID)
      : undefined;
    const grabID = this.trackedGrabFor(item.id);
    const grab =
      grabID === undefined
        ? this.pendingGrabFor(item)
        : this.grabDownloads.get(grabID);
    const base = (item.filename ?? "").split(/[\\/]/).pop() ?? "";
    const conflict = this.downloadGrabConflict(item);
    if (conflict !== undefined) {
      // Same-tab delegated click job and armed grab both match; neither may
      // steer. Firefox never reaches this listener.
      this.surfaceDownloadGrabConflict(item.id, conflict);
      return undefined;
    }
    // A grab outranks an inferred job. It is the researcher naming THIS
    // document, in this tab, just now; the inference is about what the tab
    // used to hold. Observed live: a handoff tab opened for one paper was
    // reused to read another, and the viewer's own Download button — the only
    // byte source that works once a signed provider URL has expired — was
    // steered into the first paper's job directory while a grab for the
    // second sat unfulfilled. The bytes then failed identity validation and
    // parked for review, and the grab never settled at all.
    if (grab !== undefined && base.length > 0 && exactJob === undefined) {
      return {
        filename: `${grab.steeringPath}${base}`,
        conflictAction: "uniquify",
      };
    }
    const job = exactJob ?? this.correlate(item);
    if (!job || base.length === 0) return undefined;
    return {
      filename: `papio/${job.job_id}/${base}`,
      conflictAction: "uniquify",
    };
  }

  private bindListeners(): void {
    if (this.listenersBound) return;
    this.listenersBound = true;
    this.deps.tabs.onUpdated.addListener((tabID, change, tab) => {
      this.touchTab(tabID);
      return this.onTabUpdated(tabID, change, tab);
    });
    this.deps.tabs.onRemoved.addListener((tabID) => {
      this.tabTouchEpoch.delete(tabID);
      this.keepaliveManager?.noteTabRemoved(tabID);
      return this.onTabRemoved(tabID);
    });
    this.deps.tabs.onActivated.addListener(({ tabId }) => {
      this.touchTab(tabId);
      return this.onTabActivated(tabId);
    });
    this.bindWebNavigation();
    this.deps.downloads.onCreated.addListener((item) => {
      return this.onDownloadCreated(item);
    });
    this.deps.downloads.onChanged.addListener((delta) => {
      return this.onDownloadChanged(delta);
    });
    this.deps.downloads.onDeterminingFilename?.addListener((item, suggest) => {
      const determine = (): void => {
        const suggestion = this.downloadFilenameSuggestion(item);
        if (suggestion !== undefined) suggest(suggestion);
      };
      if (this.hydrated) {
        determine();
        return;
      }
      // Chrome can wake an MV3 worker with this event. Listener registration
      // must stay synchronous, but the persisted manual-download window is not
      // available until backend hydration finishes. Returning true keeps the
      // browser's filename decision open for the asynchronous suggest call.
      const releaseWithoutChange = (): void =>
        (
          suggest as unknown as (
            suggestion?: {
              filename: string;
              conflictAction: "uniquify";
            },
          ) => void
        )();
      void this.ready
        .then(() => {
          const suggestion = this.downloadFilenameSuggestion(item);
          if (suggestion === undefined) releaseWithoutChange();
          else suggest(suggestion);
        })
        .catch((error) => {
          console.error(
            "papio: download filename correlation could not hydrate",
            error,
          );
          releaseWithoutChange();
        });
      return true;
    });
    this.deps.alarms.onAlarm.addListener((alarm) => {
      if (alarm.name === KEEPALIVE_ALARM)
        return this.onKeepaliveAlarm();
      if (alarm.name.startsWith(INSTITUTIONAL_RETRY_ALARM_PREFIX))
        return this.onInstitutionalRetryAlarm(alarm.name);
    });
  }
  private institutionalRetryAlarmName(jobID: string, attempt: number): string {
    return `${INSTITUTIONAL_RETRY_ALARM_PREFIX}${jobID}:${attempt}`;
  }

  private async institutionalRetryAlarmPending(jobID: string): Promise<boolean> {
    const remembered = this.institutionalRetryAttempts.get(jobID);
    if (remembered !== undefined) {
      const name = this.institutionalRetryAlarmName(jobID, remembered);
      if (this.deps.alarms.get === undefined) return true;
      if ((await this.deps.alarms.get(name)) !== undefined) return true;
    }
    if (this.deps.alarms.get === undefined) return false;
    for (let attempt = 1; attempt <= INSTITUTIONAL_RETRY_MAX_ATTEMPTS; attempt += 1) {
      if (
        (await this.deps.alarms.get(
          this.institutionalRetryAlarmName(jobID, attempt),
        )) !== undefined
      )
        return true;
    }
    return false;
  }

  private scheduleInstitutionalBindRetry(jobID: string): void {
    const previous = this.institutionalRetryAttempts.get(jobID) ?? 0;
    const attempt = Math.min(
      INSTITUTIONAL_RETRY_MAX_ATTEMPTS,
      previous + 1,
    );
    this.institutionalRetryAttempts.set(jobID, attempt);
    const delay = Math.min(
      INSTITUTIONAL_RETRY_MAX_MS,
      INSTITUTIONAL_RETRY_BASE_MS * 2 ** (attempt - 1),
    );
    const name = this.institutionalRetryAlarmName(jobID, attempt);
    const create = (): void => {
      this.deps.alarms.create(name, { when: this.deps.now() + delay });
    };
    if (this.deps.alarms.get === undefined) {
      create();
      return;
    }
    void this.deps.alarms.get(name).then((existing) => {
      if (existing === undefined) create();
    });
  }

  private async onInstitutionalRetryAlarm(name: string): Promise<void> {
    const suffix = name.slice(INSTITUTIONAL_RETRY_ALARM_PREFIX.length);
    const separator = suffix.lastIndexOf(":");
    if (separator <= 0) return;
    const jobID = suffix.slice(0, separator);
    const attempt = Number(suffix.slice(separator + 1));
    if (
      jobID === "" ||
      !Number.isSafeInteger(attempt) ||
      attempt < 1 ||
      attempt > INSTITUTIONAL_RETRY_MAX_ATTEMPTS
    )
      return;
    this.institutionalRetryAttempts.set(jobID, attempt);
    await this.ready;
    this.scheduleMaterialization(jobID, true);
  }


  /** Register the MV3 keepalive alarm once. Chrome persists alarms across
   * worker termination; resetting an existing alarm on every start() is what
   * produces duplicate same-second wake callbacks observed in daemon.log. */
  private async ensureKeepaliveAlarm(): Promise<void> {
    if (this.deps.alarms.get !== undefined) {
      const existing = await this.deps.alarms.get(KEEPALIVE_ALARM);
      if (existing !== undefined) return;
    }
    this.deps.alarms.create(KEEPALIVE_ALARM, {
      periodInMinutes: KEEPALIVE_ALARM_MINUTES,
    });
  }

  /** The keepalive alarm woke the worker. The top-level start() on this same
   * spin-up already reconnects; this is the safety net that re-establishes the
   * daemon connection if it is still down, so any queued offers arrive. */
  /** The keepalive alarm both reconnects the broker and refreshes the
   * non-authoritative pending count when the negotiated schema supports it. */
  private async onKeepaliveAlarm(): Promise<void> {
    const now = this.deps.now();
    if (this.keepaliveAlarmInFlight) return;
    if (
      this.keepaliveAlarmHandledAt > 0 &&
      now - this.keepaliveAlarmHandledAt < KEEPALIVE_ALARM_DEDUPE_MS
    ) {
      return;
    }
    this.keepaliveAlarmInFlight = true;
    try {
      await this.runKeepaliveAlarm();
      this.keepaliveAlarmHandledAt = this.deps.now();
    } finally {
      this.keepaliveAlarmInFlight = false;
    }
  }

  /** Keep a challenge ask alive only while papio can CONFIRM a challenge is
   * still on that tab, and retire it otherwise.
   *
   * The ask exists to send the operator to a page with a check on it. Every
   * other outcome — the tab is gone, its URL is unreadable, the probe cannot
   * run, the page assesses normally — means papio cannot say that is still
   * true, and an ask papio cannot justify must not survive on the strength of
   * having once been justified. Measured live 2026-08-21: both of papio's tabs
   * were sitting on the article, no challenge anywhere, and the card had been
   * asking for over an hour.
   *
   * Clearing wrongly is cheap and self-correcting: the drive resumes, and if
   * the wall is really still there the next classification re-detects it and
   * blocks again — which is demonstrably live, since that is how each of these
   * blocks was created. Failing closed is what produced a permanent false ask,
   * and it is the same trap `waitForBotChallenge`'s own probe-failure path
   * already refuses ("a failed probe must retain the existing stale-adapter
   * path rather than silently make an unreadable provider page immortal").
   *
   * Navigates nothing and probes at most CHALLENGE_RECHECK_LIMIT tabs per wake.
   * Runs on the one-minute keepalive alarm, the wake that survives a worker
   * death, so the ask retires within a minute rather than waiting for a tab
   * event that may never arrive. */
  private async recheckChallengeBlocks(): Promise<void> {
    const blocked = this.store.activeJobs
      .filter((job) => job.challenge_blocked === true)
      .slice(0, CHALLENGE_RECHECK_LIMIT);
    for (const job of blocked) {
      if (!(await this.challengeStillPresent(job))) {
        await this.clearChallengeBlock(job);
      }
    }
  }

  /** True only on a positive, current reading of a live challenge on the job's
   * own tab. Every failure to read is a false: see recheckChallengeBlocks. */
  private async challengeStillPresent(job: ActiveJob): Promise<boolean> {
    if (job.tab_id < 0) return false;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(job.tab_id);
    } catch {
      return false;
    }
    if (tab.url === undefined) return false;
    let host: string;
    try {
      host = new URL(tab.url).hostname.toLowerCase();
    } catch {
      return false;
    }
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: assessDrivenPage,
        args: [null, host === OPENATHENS_LOGIN_HOST],
      });
      const assessment = results[0]?.result as DrivenPageAssessment | undefined;
      return assessment?.kind === "challenge" || assessment?.kind === "redirect_loop";
    } catch (e) {
      console.error("papio: challenge recheck could not read the page", e);
      return false;
    }
  }

  private async runKeepaliveAlarm(): Promise<void> {
    await this.ready;
    // Recovery runs FIRST and unconditionally on this wake, independent of
    // the native port: a service worker that died mid-probe still needs its
    // dirty/paused origins re-probed even while the daemon connection is
    // also down. It is deliberately NOT awaited — reconnecting the daemon
    // and refreshing the triage count are latency-sensitive on this one
    // wake, and a session probe can take seconds of browser API work with
    // nothing here depending on its result.
    void this.keepaliveManager?.onWake();
    if (this.port === null && !this.closingDeliberately) {
      // A disconnected wake reconnects and stops: releasing queued handoffs
      // here used to navigate work into a dead network after machine sleep
      // (surface-lifecycle plan, Slice 0). The next connected wake releases.
      this.reconnectAttempts = 0;
      this.connect();
      return;
    }
    // Queued releases run only against a live network, and only after the
    // probe above has been kicked off — release-then-probe was the wake
    // ordering that drove tabs into dead networks.
    if (this.deps.online?.() ?? true)
      await this.releaseExpiredQueuedHandoffs();
    // A solved challenge must retire its own ask. Clearing it depends on
    // assessing the tab again, and until now the ONLY trigger was a single
    // tabs.onUpdated event for that exact tab - so if the worker was asleep
    // when the operator finished (MV3 sleeps it after ~30s idle, and solving a
    // CAPTCHA takes longer than that), or the event carried no title change on
    // a provider page, nothing ever re-checked and the card asked forever for
    // work already done. Reported live 2026-08-21: "I already clicked Open tab
    // and solved the capture, but it still nags", with the job's last recorded
    // event being the block itself. `challenge_blocked` is persisted state, so
    // this wake can find it after any restart - the class of bug the
    // claim-grant work already paid for once.
    //
    // NOT awaited, for the reason stated at the top of this method: the triage
    // and pulse reads below are the latency-sensitive part of this wake, and a
    // scripting probe into a live tab is seconds of browser API work that
    // nothing here depends on.
    void this.recheckChallengeBlocks().catch((e: unknown) => {
      console.error("papio: challenge recheck failed", e);
    });
    // The surface-repair pass on the wake that survives a worker death, for
    // exactly the reason stated for the challenge recheck above. Until now
    // reconcileOwnedTabs ran ONLY from start(), at 12s and 90s — both of
    // which elapse BEFORE the 3-minute drive timeout whose refused close is
    // the failure this pass exists to repair. So the repair systematically
    // ran before the damage, the worker then slept, and a stranded surface
    // survived until the next extension restart, which restarts the same
    // race. Measured live 2026-08-22: three tabs for one paper, each from a
    // drive-timeout close refused minutes after the last reconcile pass.
    //
    // Rate-limited rather than run every wake: one pass is a ledger walk plus
    // a tabs.get per record, and nothing here depends on its result.
    void this.reconcileOwnedTabsIfDue().catch((e: unknown) => {
      console.error("papio: owned-tab reconcile failed", e);
    });
    if (
      this.hasCurrentHello() &&
      (this.store.daemonFeatures ?? []).includes(TRIAGE_SNAPSHOT_FEATURE)
    ) {
      await this.requestTriageCounts();
    }
    if (
      this.hasCurrentHello() &&
      (this.store.daemonFeatures ?? []).includes(WORK_PULSE_FEATURE)
    ) {
      await this.requestWorkPulse();
    }
  }

  /** Consecutive unplanned disconnects; resets on a healthy inbound frame. */
  private reconnectAttempts = 0;
  /** Set while disconnect() runs so the onDisconnect listener knows the
   * teardown was deliberate (protocol error / shutdown): deliberate
   * disconnects must NOT auto-reconnect — fail closed stays failed. */
  private closingDeliberately = false;

  private connect(): void {
    // handshake. Clear it before hello so no request can use stale features.
    this.store = clearNegotiationState(this.store);
    if (this.hydrated) void this.update((current) => current);
    const port = this.deps.connectNative(NATIVE_HOST);
    this.port = port;
    this.portGeneration += 1;
    this.helloAckGeneration = -1;
    this.helloRole = undefined;
    this.helloDeniedGeneration = -1;
    this.helloSentGeneration = -1;
    this.helloRequestID = undefined;
    this.triagePendingCount = undefined;
    this.triageActionsRequiresAuth = undefined;
    this.triageActionsRequiresAuthAt = undefined;
    this.sessionEvidenceSentAt.clear();
    port.onMessage.addListener((msg) => {
      if (this.port !== port) return;
      this.reconnectAttempts = 0;
      return this.enqueueInbound(msg, port);
    });
    port.onDisconnect.addListener(() => this.onPortDisconnect(port));
    // hello is the mandatory first frame after connect (seq 0).
    const adapterVersions: Record<string, string> = {};
    for (const spec of this.deps.adapterSpecs)
      adapterVersions[spec.id] = spec.version;
    this.helloSentGeneration = this.portGeneration;
    this.helloRequestID = this.deps.randomUUID().replace(/-/g, "");
    if (
      !this.send(
        "hello",
        {
          extension_version: this.deps.manifestVersion,
          adapter_versions: adapterVersions,
          // Every entry here is a capability the DAEMON gates a request on, so
          // an omission is a silently dead feature rather than a parse error:
          // `pdfGrabRefusalReason` (internal/browser/bridge.go) refuses a grab
          // with `extension_outdated` unless the SESSION advertised both
          // `pdf_grab_v1` and `effect_permit_v1`. `pdf_grab_v1` was missing
          // here while the daemon required it, so "Send PDF" was refused in
          // every browser, always — and no test saw it, because the Go tests
          // build their own hello (`grabCapableHello`) that does include it.
          // Adding a value is wire-safe both ways: both parsers validate this
          // array by shape only ([a-z0-9_]{1,64}, max 32, no duplicates), so
          // an older daemon accepts a name it does not know.
          features: [
            EFFECT_PERMIT_FEATURE,
            PDF_GRAB_FEATURE,
            INSTITUTIONAL_MATERIALIZATION_FEATURE,
            SURFACE_PRESENCE_FEATURE,
            WORK_PULSE_FEATURE,
          ],
        },
        undefined,
        this.helloRequestID,
      )
    ) {
      this.helloSentGeneration = -1;
      this.helloRequestID = undefined;
    }
  }

  private async onPortDisconnect(port: NativePort): Promise<void> {
    // A stale port may report its close after recovery opened a replacement.
    if (this.port !== port) return;
    this.port = null;
    this.failPendingMaterializationRequests();
    this.failPageAcquireWaiters(
      "The daemon disconnected before acknowledging this page",
    );
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon disconnected before acknowledging the request",
    );
    await this.update((s) => ({ ...s, connectionStatus: "disconnected" }));
    await this.syncConnectionBadge("disconnected");
    if (this.closingDeliberately) return;
    // Unplanned port death (daemon restart, host exit, Chrome nap): the daemon
    // owns all durable state, so reconnect + re-hello is always safe. Bounded
    // exponential backoff, capped at 60s, gives up after 8 attempts until the
    // next user-visible event restarts the cycle.
    if (this.reconnectAttempts >= 8) return;
    const delay = Math.min(60_000, 1_000 * 2 ** this.reconnectAttempts);
    this.reconnectAttempts += 1;
    this.deps.setTimeout(() => {
      if (this.port === null && !this.closingDeliberately) this.connect();
    }, delay);
  }

  private disconnect(): void {
    this.closingDeliberately = true;
    const port = this.port;
    this.port = null;
    this.failPendingMaterializationRequests();
    this.failPageAcquireWaiters(
      "The daemon disconnected before acknowledging this page",
    );
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon disconnected before acknowledging the request",
    );
    if (!port) return;
    try {
      port.disconnect();
    } catch {
      // Already torn down.
    }
  }

  private reconnectForHello(): void {
    const port = this.port;
    if (!port) return;
    // Clear ownership before closing: onDisconnect for this stale port must not
    // schedule a second recovery after connect() has installed its replacement.
    this.closingDeliberately = true;
    this.port = null;
    this.failPendingMaterializationRequests();
    this.failPageAcquireWaiters(
      "The daemon restarted before acknowledging this page",
    );
    this.settleHelloWaiters(false);
    this.failPendingNativeRequests(
      "connection_lost",
      "The daemon restarted before acknowledging the request",
    );
    try {
      port.disconnect();
    } catch {
      // Chrome can report an already-closed native port.
    } finally {
      this.closingDeliberately = false;
    }
    this.reconnectAttempts = 0;
    this.connect();
  }

  private async update(fn: (store: StoreShape) => StoreShape): Promise<void> {
    const signInBlockersBefore = this.signInBlockerCount();
    // Apply the transform synchronously so in-memory state stays in event order.
    this.store = fn(this.store);
    const signInBlockersChanged =
      signInBlockersBefore !== this.signInBlockerCount();
    // Persist after any in-flight save settles, writing the latest snapshot so
    // reordered chrome.storage writes cannot resurrect an older one.
    const save = this.saveChain.then(() => this.deps.backend.save(this.store));
    // Keep the chain alive across a failed save without unhandled rejections;
    // this caller still observes the real error below.
    this.saveChain = save.catch(() => {});
    await save;
    if (signInBlockersChanged) await this.syncConnectionBadge();
  }
  /** Reserve a job's download initiation at the state reducer boundary.
   * Classification may have crossed several awaits before reaching this
   * point; the synchronous transform is the per-job CAS that makes the
   * already-authorized decision single-use without serializing other jobs. */
  private async claimDownloadInitiated(jobID: string): Promise<boolean> {
    let claimed = false;
    await this.update((store) => {
      const current = findByJob(store, jobID);
      if (!this.hasDelegatedAuthority(current) || this.downloads.has(jobID))
        return store;
      const result = claimJobDownloadInitiated(store, jobID);
      claimed = result.claimed;
      return result.store;
    });
    return claimed;
  }
  /** Check the durable and worker-local authority needed by one generic
   * candidate. When allowClaimed is true, this is the immediate pre-download
   * re-read for the claim made by this invocation. */
  private genericCandidateAuthorized(
    store: StoreShape,
    jobID: string,
    epoch: ProviderDriveEpoch,
    strategyID: string,
    allowClaimed = false,
  ): boolean {
    const current = findByJob(store, jobID);
    if (
      current === undefined ||
      !this.handoffDrives.has(jobID) ||
      current.tab_id < 0 ||
      current.status !== "accepted" ||
      !this.hasDelegatedAuthority(current) ||
      current.generic_terminal === true ||
      this.downloads.has(jobID)
    ) {
      return false;
    }
    const currentEpoch = current.generic_drive_epoch;
    if (
      currentEpoch?.strategy !== "generic" ||
      currentEpoch.drive_attempt_id !== epoch.drive_attempt_id ||
      currentEpoch.ordinal !== epoch.ordinal ||
      currentEpoch.revision !== epoch.revision
    ) {
      return false;
    }
    if (allowClaimed) {
      return (
        current.download_initiated === true &&
        current.adapter_id === strategyID &&
        currentEpoch.strategy_id === strategyID
      );
    }
    return (
      current.download_initiated !== true &&
      (current.generic_positive_attempts ?? 0) < 2 &&
      !(current.generic_attempted_strategies ?? []).includes(strategyID)
    );
  }

  /** Atomically claim a generic candidate after all async work. */
  private async claimGenericCandidate(
    jobID: string,
    epoch: ProviderDriveEpoch,
    candidate: GenericCandidate,
  ): Promise<ProviderDriveEpoch | undefined> {
    let claimedEpoch: ProviderDriveEpoch | undefined;
    await this.update((store) => {
      if (
        !this.genericCandidateAuthorized(
          store,
          jobID,
          epoch,
          candidate.strategy_id,
        )
      )
        return store;
      const result = claimJobDownloadInitiated(store, jobID);
      if (!result.claimed) return store;
      const next = result.store.activeJobs.map((entry) => {
        if (entry.job_id !== jobID) return entry;
        const current = entry as ActiveJob & GenericJobState;
        const currentEpoch = current.generic_drive_epoch ?? epoch;
        claimedEpoch = { ...currentEpoch, strategy_id: candidate.strategy_id };
        return {
          ...entry,
          adapter_id: candidate.strategy_id,
          generic_drive_epoch: claimedEpoch,
          generic_positive_attempts:
            (current.generic_positive_attempts ?? 0) + 1,
          generic_attempted_strategies: [
            ...(current.generic_attempted_strategies ?? []),
            candidate.strategy_id,
          ],
        } as ActiveJob;
      });
      return { ...result.store, activeJobs: next };
    });
    return claimedEpoch;
  }

  private async upsertJobWithOffer(
    job: ActiveJob,
    offerURL: string,
  ): Promise<void> {
    const derived = this.configuredInstitutionOrigin(
      offerURL,
      job.provider_hosts,
    );
    const retained =
      job.institution_origin !== undefined &&
      this.knownResolverOrigins().includes(job.institution_origin)
        ? job.institution_origin
        : undefined;
    const institutionOrigin = derived ?? retained;
    const persisted = { ...job };
    if (institutionOrigin === undefined) delete persisted.institution_origin;
    else persisted.institution_origin = institutionOrigin;
    await this.update((s) => upsertJob(s, persisted));
    this.offerURLs.set(job.job_id, offerURL);
    if (job.requires_auth === true)
      this.keepaliveManager?.learnResolver(offerURL);
  }
  private async upsertJobWithoutOffer(job: ActiveJob): Promise<void> {
    this.offerURLs.delete(job.job_id);
    await this.update((s) => upsertJob(s, job));
  }

  /** A browser that will not give papio a surface has said nothing about the
   * paper. `job_reject` is terminal daemon-side (`browser_rejected` ->
   * `unavailable`), so answering a local surface failure with it retires work a
   * later epoch would drive: measured live, 19 papers between 2026-08-19 and
   * 08-23 ended exactly that way, each with an empty `browser.job_reject {}` as
   * its whole diagnosis, after two offers it had answered `queued`. Drop local
   * tracking, free the drive slot, and let the daemon re-offer - the same
   * disposition the MAX_AUTH_ATTEMPTS path already documents ("No job_reject -
   * that is terminal; the job stays parked and is re-offered"). */
  private async parkUndrivableHandoff(
    jobID: string,
    reason: string,
  ): Promise<void> {
    console.error(
      `papio: no handoff surface for ${jobID} (${reason}); left parked for a later offer`,
    );
    await this.removeJobWithOffer(jobID);
  }

  private async removeJobWithOffer(
    jobID: string,
    closeDisposition: SurfaceCloseDisposition = "job_inactive",
  ): Promise<void> {
    this.destroyDeliveryChoicesForJob(jobID);
    const job = findByJob(this.store, jobID);
    const materialization = this.materializationCorrelation(jobID);
    const tabID = job?.tab_id;
    const materializationTabID = materialization?.tab_id;
    const providerKey =
      job === undefined ? undefined : this.providerKeyForJob(job);
    this.pendingAuthReloads.delete(jobID);
    this.cancelMaterializationWorkflow(jobID);
    this.pendingFreshHandoffs.delete(jobID);
    this.releaseHandoffDrive(jobID);
    this.deliveryJobs.delete(jobID);
    this.resolverRoutes.delete(jobID);
    this.deliverySessionEvidence.delete(jobID);
    this.offerURLs.delete(jobID);
    this.classifyRetries.delete(jobID);
    this.loginEntityIDs.delete(jobID);
    this.federatedLoginRouted.delete(jobID);
    this.federatedLoginRouteEvents.delete(jobID);
    this.federatedLoginOperatorNavigated.delete(jobID);
    this.federatedLoginRouteSettled.delete(jobID);
    this.federatedReDriven.delete(jobID);
    this.handoffOutcomeSent.delete(`${jobID}:stale_sso`);
    this.handoffOutcomeSent.delete(`${jobID}:auth_error`);
    this.handoffOutcomeSent.delete(`${jobID}:ui_changed`);
    this.genericEvidence.delete(jobID);
    this.challengeBlockedOutcomeSent.delete(`${jobID}:challenge_blocked`);
    this.authFailureSurfaced.delete(jobID);
    this.staleRecoveryEpochs.delete(jobID);
    this.staleRecoveryAttemptedEpochs.delete(jobID);
    this.staleRecoverySurfacedEpochs.delete(jobID);
    this.staleRecoveryInFlightEpochs.delete(jobID);
    this.staleRecoveryRetryTimers.delete(jobID);
    for (const [key, request] of this.pendingDirectGets) {
      if (request.job_id === jobID || key.startsWith(`${jobID}:`))
        this.pendingDirectGets.delete(key);
    }
    this.openAthensErrorRecheckEpochs.delete(jobID);
    this.resolverNoEntitlementSent.delete(jobID);
    this.proquestAccountIDs.delete(jobID);
    this.accountIdAppended.delete(jobID);
    this.clearClaimGrant(jobID);
    if (materializationTabID !== undefined && materializationTabID >= 0) {
      await this.removeMaterializationTab(materializationTabID);
    }
    await this.update((s) => {
      const withoutJob = clearPendingDelivery(removeJob(s, jobID), jobID);
      return reduceMaterialization(withoutJob, jobID, { type: "clear" });
    });
    if (providerKey !== undefined)
      await this.releaseProviderDrainWhenUnused(providerKey);
    await this.dropStaleHandoffGroup();
    if (!this.drainingHandoffDriveQueue) {
      await this.drainHandoffDriveQueue();
      // Removing/cancelling a settled owner is itself a release boundary.
      // Re-run the evidence-scoped FIFO after its drive and claim are gone;
      // otherwise queued work waits for an unrelated timer despite capacity.
      if (!this.drainingQueuedHandoffs) await this.releaseQueuedHandoffs();
    }
    // `cancel` is itself an inbound native frame. Never await this correlated
    // request here: its surface_close_response can only arrive through the
    // same serialized inbound FIFO. Run the one-use authorization transaction
    // off-chain after the job is detached, so closeOwnedTab's tracked-job gate
    // no longer blocks it. The daemon decides whether the handoff is actually
    // inactive; every non-authorized outcome leaves the surface alone.
    const closeTabID =
      tabID !== undefined && tabID >= 0 ? tabID : materializationTabID;
    if (closeTabID !== undefined && closeTabID >= 0)
      void this.closeOwnedSurface(closeTabID, closeDisposition);
  }

  /** A keepalive tab can outlive a cancellation, so clear its removed paper's
   * title before retaining the group for reuse. */
  private async dropStaleHandoffGroup(): Promise<void> {
    const groupID = this.store.handoffGroupID;
    if (groupID === undefined) return;
    if (this.store.activeJobs.some((job) => job.tab_id >= 0)) return;
    if (await this.recollapseHandoffGroup()) return;
    await this.update((s) => {
      const next = { ...s };
      delete next.handoffGroupID;
      return next;
    });
  }

  /** Count at most one authentication attempt per broker-tab drive. The SSO
   * redirect dance can toggle auth_pending several times within one tab, so the
   * budget debounces on tab id; each fresh drive (a new tab from a re-offer or a
   * reconcile re-queue) is a distinct attempt. Persisted so attempts accumulate
   * across service-worker restarts within a browser session. */
  private async noteAuthAttempt(jobID: string, tabID: number): Promise<void> {
    if (this.authCountedTabs.has(tabID)) return;
    await this.chargeAuthAttempt(jobID, tabID);
  }

  /** Spend one unit of a job's durable authentication budget and claim its tab.
   * Claiming matters for the stale-IdP re-drive, which charges explicitly
   * because it reuses the SAME tab (noteAuthAttempt's debounce would swallow
   * it): the claim stops the ordinary auth_pending path, which this tab update
   * falls through to next, from charging the same drive a second time. */
  private async chargeAuthAttempt(jobID: string, tabID: number): Promise<void> {
    this.authCountedTabs.add(tabID);
    await this.update((s) => {
      const authAttempts = { ...(s.authAttempts ?? {}) };
      authAttempts[jobID] = (authAttempts[jobID] ?? 0) + 1;
      return { ...s, authAttempts };
    });
  }

  private authAttemptsFor(jobID: string): number {
    return (this.store.authAttempts ?? {})[jobID] ?? 0;
  }
  private rememberStalledAuthHandoff(
    jobID: string,
    handoff: StalledAuthHandoff,
  ): void {
    this.stalledAuthHandoffs.set(jobID, {
      url: handoff.url,
      providerHosts: [...handoff.providerHosts],
      ...(handoff.expected !== undefined ? { expected: handoff.expected } : {}),
      ...(handoff.requiresAuth !== undefined
        ? { requiresAuth: handoff.requiresAuth }
        : {}),
      ...(handoff.accessMode !== undefined
        ? { accessMode: handoff.accessMode }
        : {}),
    });
  }

  /** Report the human authentication step for a capped job, at most once per
   * worker lifetime. human_auth_required is non-terminal daemon-side: the job
   * stays parked (awaiting_human) and is re-offered on a future warm launch. */
  private async reportAuthStalled(jobID: string): Promise<void> {
    if (!this.authStalledReported.has(jobID)) {
      this.authStalledReported.add(jobID);
      this.send("provider_outcome", { outcome: "human_auth_required" }, jobID);
    }
    await this.parkHandoffForManual(jobID);
  }

  /** Clear a job's auth-failure budget once a real download proves the session
   * works, so an earlier expired-session streak cannot cap a now-valid job. */
  private clearAuthAttempts(store: StoreShape, jobID: string): StoreShape {
    if (store.authAttempts?.[jobID] === undefined) return store;
    const authAttempts = { ...store.authAttempts };
    delete authAttempts[jobID];
    return { ...store, authAttempts };
  }

  /** Release-grade evidence for `origin`: a persisted authEvidenceByOrigin
   * entry within AUTH_EVIDENCE_TTL_MS, written only by
   * recordFreshSessionEvidence and by a warm institutional landing
   * (recordInstitutionalSession), and dropped by revokeAuthEvidence when a
   * probe commits that the origin is no longer authenticated.
   *
   * Persisted rather than worker-local because an MV3 worker restarts
   * constantly, and expiring rather than sticky because a session ends: the
   * worker-local Set that used to short-circuit this check never expired and
   * was never deleted, so an operator who signed out kept authorizing
   * releases until the worker happened to die.
   *
   * The global lastAuthReturnedAt display field is still excluded on its own —
   * ADR-0013: a timing frame is not identity evidence; only an origin-scoped
   * observation is. */
  private hasAuthEvidence(origin: string): boolean {
    const evidencedAt = this.store.authEvidenceByOrigin?.[origin];
    if (typeof evidencedAt !== "number") return false;
    const age = this.deps.now() - evidencedAt;
    return age >= 0 && age <= AUTH_EVIDENCE_TTL_MS;
  }

  /** A committed probe says this origin is no longer authenticated, so its
   * evidence must stop authorizing releases immediately rather than idling
   * out over the TTL. Wired to keepalive's onOriginAuthenticationChanged,
   * which fires only on a real committed change — never on a preserved
   * verdict, so a closed tab or an unreadable page cannot revoke. */
  async revokeAuthEvidence(origin: string): Promise<void> {
    await this.ready;
    if (this.store.authEvidenceByOrigin?.[origin] === undefined) return;
    await this.update((s) => {
      const authEvidenceByOrigin = { ...s.authEvidenceByOrigin };
      delete authEvidenceByOrigin[origin];
      return { ...s, authEvidenceByOrigin };
    });
  }

  private async resolverAccessGranted(origin: string): Promise<boolean> {
    const known = this.knownResolverOrigins();
    // An older daemon sends no configured-origin set. Preserve its prior
    // behavior; current daemons supply the closed set this feature requires.
    if (known.length === 0) return true;
    if (!known.includes(origin)) return false;
    try {
      return await this.deps.permissions.contains({
        origins: [`${origin}/*`],
      });
    } catch {
      return false;
    }
  }

  /** Keeps an OA landing from opening an institutional queue while preserving
   * the existing one-visible-tab flow for ordinary offers. `origin` may be
   * undefined for a job whose offer URL never resolved to a bare HTTPS
   * origin; such a job can still qualify through the OA branch. */
  private hasHandoffReleaseEvidence(
    origin: string | undefined,
    requiresAuth: boolean | undefined,
  ): boolean {
    const known = this.knownResolverOrigins();
    const originAllowed =
      origin !== undefined &&
      (known.length === 0 || known.includes(origin));
    return (
      (originAllowed && this.hasAuthEvidence(origin)) ||
      (requiresAuth !== true && this.openAccessLandingObserved)
    );
  }
  /** A provider's declared host set is the only local grouping information the
   * bridge has. Canonicalizing it makes every re-offer use the same lease
   * without retaining a resolver or provider URL. */
  private providerKeyForHosts(providerHosts: string[]): string {
    const hosts = [
      ...new Set(
        providerHosts.map((host) => host.trim().toLowerCase()).filter(Boolean),
      ),
    ].sort();
    return hosts.length === 0 ? "unknown-provider" : hosts.join(",");
  }

  private providerKeyForJob(job: ActiveJob): string {
    return this.providerKeyForHosts(job.provider_hosts);
  }
  private challengeHostsFor(providerHosts: readonly string[]): string[] {
    return [
      ...new Set(
        providerHosts
          .map((host) => registrableProviderHost(host))
          .filter((host): host is string => host !== undefined),
      ),
    ];
  }

  private challengeCooldownActiveForHosts(
    providerHosts: readonly string[],
  ): boolean {
    const now = this.deps.now();
    return this.challengeHostsFor(providerHosts).some((host) => {
      const expiresAt = this.store.challengeCooldowns?.[host];
      return (
        typeof expiresAt === "number" &&
        Number.isFinite(expiresAt) &&
        expiresAt > now
      );
    });
  }

  private async expireChallengeCooldowns(): Promise<string[]> {
    const now = this.deps.now();
    const expired = Object.entries(this.store.challengeCooldowns ?? {})
      .filter(
        ([host, expiresAt]) =>
          registrableProviderHost(host) !== host ||
          !Number.isFinite(expiresAt) ||
          expiresAt <= now,
      )
      .map(([host]) => host);
    if (expired.length === 0) return expired;
    await this.update((store) => {
      const challengeCooldowns = { ...(store.challengeCooldowns ?? {}) };
      for (const host of expired) delete challengeCooldowns[host];
      return { ...store, challengeCooldowns };
    });
    for (const host of expired) this.challengeCooldownTimers.delete(host);
    return expired;
  }

  private scheduleChallengeCooldownExpiry(
    host: string,
    expiresAt: number,
  ): void {
    const token = {};
    this.challengeCooldownTimers.set(host, token);
    this.deps.setTimeout(
      async () => {
        if (this.challengeCooldownTimers.get(host) !== token) return;
        await this.ready;
        const expired = await this.expireChallengeCooldowns();
        if (!expired.includes(host)) return;
        await this.releaseQueuedHandoffs();
        await this.syncConnectionBadge();
      },
      Math.max(0, expiresAt - this.deps.now()),
    );
  }

  private async restoreChallengeCooldownTimers(): Promise<void> {
    await this.expireChallengeCooldowns();
    for (const [host, expiresAt] of Object.entries(
      this.store.challengeCooldowns ?? {},
    )) {
      if (
        registrableProviderHost(host) === host &&
        expiresAt > this.deps.now()
      ) {
        this.scheduleChallengeCooldownExpiry(host, expiresAt);
      }
    }
  }
  private challengeHostFor(
    job: ActiveJob,
    currentHost: string,
    currentURL?: string,
  ): string | undefined {
    const declaredHosts = this.challengeHostsFor(job.provider_hosts);
    if (currentURL !== undefined && isAuthenticationURL(currentURL))
      return declaredHosts[0];
    const current = registrableProviderHost(currentHost);
    if (current === undefined) return declaredHosts[0];
    if (
      hostMatches(currentHost, job.provider_hosts) ||
      this.deps.adapterSpecs.some((spec) =>
        hostMatches(currentHost, spec.hosts),
      )
    ) {
      return current;
    }
    return declaredHosts[0];
  }

  /** Raise a challenge ask only for a check that PERSISTS. A Cloudflare
   * managed challenge clears itself in seconds and announces itself as a
   * title-only update mid-navigation, so the first positive reading is not yet
   * evidence of a wall - see CHALLENGE_CONFIRM_MS for the live measurement.
   *
   * Deferral state is worker memory on purpose. A worker death loses the
   * pending confirmation and therefore raises nothing, which is the same
   * fail-open stance recheckChallengeBlocks already takes: an unraised ask
   * costs one more classification pass, a false ask costs the operator a
   * paper. The next assessment re-detects a real wall - that is demonstrably
   * live, since it is how every one of these blocks was created.
   */
  private async confirmThenBlockChallenge(
    job: ActiveJob,
    currentHost: string,
    kind: ChallengeBlockKind,
    currentURL?: string,
  ): Promise<void> {
    // A dead end, not a stage: waiting only delays a refusal already correct.
    if (kind === "redirect_loop") {
      await this.blockChallenge(job, currentHost, kind, currentURL);
      return;
    }
    if (job.challenge_blocked === true) {
      // Already asked: re-stamp through the ordinary path, which is idempotent
      // and emits no second outcome.
      await this.blockChallenge(job, currentHost, kind, currentURL);
      return;
    }
    const tabID = job.tab_id;
    const pending = this.challengeConfirmations.get(job.job_id);
    if (pending !== undefined && pending === tabID) return;
    this.challengeConfirmations.set(job.job_id, tabID);
    this.deps.setTimeout(async () => {
      await this.ready;
      if (this.challengeConfirmations.get(job.job_id) !== tabID) return;
      this.challengeConfirmations.delete(job.job_id);
      const current = findByJob(this.store, job.job_id);
      // Gone, re-tabbed, or already asked: nothing left for this reading to say.
      if (current === undefined || current.tab_id !== tabID) return;
      if (current.challenge_blocked === true) return;
      if (!(await this.challengeStillPresent(current))) return;
      await this.blockChallenge(current, currentHost, kind, currentURL);
    }, CHALLENGE_CONFIRM_MS);
  }

  private async blockChallenge(
    job: ActiveJob,
    currentHost: string,
    kind: ChallengeBlockKind,
    currentURL?: string,
  ): Promise<void> {
    const providerHost = this.challengeHostFor(job, currentHost, currentURL);
    if (providerHost === undefined) return;
    const now = this.deps.now();
    const alreadyBlocked =
      job.challenge_blocked === true &&
      job.challenge_host === providerHost &&
      job.challenge_kind === kind;
    const expiresAt = now + CHALLENGE_COOLDOWN_MS;
    await this.update((store) => ({
      ...patchJob(store, job.job_id, {
        challenge_blocked: true,
        challenge_host: providerHost,
        challenge_kind: kind,
        challenge_blocked_at: now,
        unknown_count: 0,
      }),
      challengeCooldowns: {
        ...(store.challengeCooldowns ?? {}),
        [providerHost]: expiresAt,
      },
    }));
    this.scheduleChallengeCooldownExpiry(providerHost, expiresAt);
    const outcomeKey = `${job.job_id}:challenge_blocked`;
    if (!alreadyBlocked && !this.challengeBlockedOutcomeSent.has(outcomeKey)) {
      if (
        this.send(
          "error",
          {
            code: "challenge_blocked",
            message:
              "Provider security check or redirect loop needs human attention",
          },
          job.job_id,
        )
      ) {
        this.challengeBlockedOutcomeSent.add(outcomeKey);
      }
    }
    if (!alreadyBlocked) {
      await this.parkProviderDrain(job);
      // §2.2.1's mfa/challenge event kinds share this one browser-visible
      // signal: nothing here can distinguish an IdP MFA prompt from a
      // bot/CAPTCHA challenge by page content alone. A cloudflare-style
      // block is the generic security-challenge case; a redirect loop is
      // the closest available proxy for an MFA hand-off stuck bouncing
      // between IdP steps — the two locally distinguishable
      // ChallengeBlockKinds map onto the two otherwise-unreachable event
      // kinds this site is responsible for.
      void this.emitClaimObservation(
        job.job_id,
        job.tab_id,
        kind === "cloudflare" ? "challenge" : "mfa",
      );
    }
    await this.parkHandoffForManual(job.job_id);
    await this.syncConnectionBadge();
  }
  /** Retire a challenge ask and report whether this job now HOLDS A DRIVE
   * SLOT. The two callers use that as their gate, so the name undersells it:
   * a clean page whose drive is only queued must not go on to classify, or a
   * queued job would download out of turn (pinned by "challenge resume queues
   * without a governor slot before classifying"). Evidence and scheduling are
   * genuinely coupled here - the block is always cleared, the answer is about
   * the slot. */
  private async clearChallengeBlock(job: ActiveJob): Promise<boolean> {
    if (job.challenge_blocked !== true) return false;
    const providerHost = job.challenge_host;
    await this.update((store) => {
      const activeJobs = store.activeJobs.map((candidate) => {
        if (candidate.job_id !== job.job_id) return candidate;
        const next = { ...candidate };
        delete next.challenge_blocked;
        delete next.challenge_host;
        delete next.challenge_kind;
        delete next.challenge_blocked_at;
        return next;
      });
      const challengeCooldowns = { ...(store.challengeCooldowns ?? {}) };
      if (providerHost !== undefined) delete challengeCooldowns[providerHost];
      return { ...store, activeJobs, challengeCooldowns };
    });
    if (providerHost !== undefined)
      this.challengeCooldownTimers.delete(providerHost);
    this.challengeBlockedOutcomeSent.delete(`${job.job_id}:challenge_blocked`);
    // Tell the daemon, once per block. Its fruitless-drive accounting cannot
    // see a security check at all: it only ever received the `challenge_blocked`
    // error, which is neither terminal nor progress, so the epoch aged out and
    // was charged as silence — and three of those retire the paper. Measured
    // live 2026-08-22: a paper quiesced at three epochs, every one of them
    // interrupted by a check the operator had gone on to solve. Timing-only
    // payload: the provider host stays in the browser.
    const blockedAt = job.challenge_blocked_at;
    this.send(
      "challenge_cleared",
      blockedAt === undefined
        ? {}
        : { elapsed_ms: Math.max(0, this.deps.now() - blockedAt) },
      job.job_id,
    );
    await this.clearProviderDrainPark(this.providerKeyForJob(job));
    const resumed = await this.resumeHandoffAfterManual(job.job_id);
    await this.releaseQueuedHandoffs();
    await this.syncConnectionBadge();
    return resumed;
  }
  private async drainPendingPdfGrabRequests(): Promise<void> {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingPdfGrabRequests.entries().next();
    if (next.done) return;
    const [key, request] = next.value;
    this.pendingPdfGrabRequests.delete(key);
    await this.requestPdfGrab(
      request as Parameters<typeof this.requestPdfGrab>[0],
    );
  }
  /** Reserve the single browser-local effect slot. A caller must hold this
   * through the observable initiation consequence (not merely planning). */
  private claimEffectGovernor(jobID: string): string | undefined {
    const current = this.effectGovernorOwner;
    if (current !== undefined) return undefined;
    const token = this.deps.randomUUID();
    this.effectGovernorOwner = { jobID, token };
    return token;
  }

  private wakeEffectGovernor(): void {
    if (!this.effectGovernorWakePending) return;
    if (this.drainingHandoffDriveQueue || this.drainingQueuedHandoffs) return;
    this.effectGovernorWakePending = false;
    void this.drainPendingFreshHandoffs();
    void this.drainPendingAuthReloads();
    void this.drainPendingSessionSignIns();
    void this.drainHandoffDriveQueue();
    void this.releaseQueuedHandoffs();
    void this.drainPendingMaterializations();
    void this.drainPendingDirectGets();
    void this.drainPendingPdfGrabRequests();
  }
  private drainPendingMaterializations(): void {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingMaterializationEffects.values().next();
    if (next.done) return;
    this.pendingMaterializationEffects.delete(next.value);
    this.scheduleMaterialization(next.value, true);
  }
  private async drainPendingAuthReloads(): Promise<void> {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingAuthReloads.entries().next();
    if (next.done) return;
    const [jobID, pending] = next.value;
    this.pendingAuthReloads.delete(jobID);
    const current = findByJob(this.store, jobID);
    if (
      current?.tab_id !== pending.tabID ||
      !this.hasDelegatedAuthority(current)
    )
      return;
    const token = this.claimEffectGovernor(`auth-reload:${jobID}`);
    if (token === undefined) {
      this.pendingAuthReloads.set(jobID, pending);
      return;
    }
    try {
      const tab = await this.deps.tabs.get(pending.tabID);
      // Renavigation fence: a reload wipes anything the operator has typed
      // into a credential form. Skip the operator-active tab; the evidence
      // reload is an optimization, and the operator's own progress moves it.
      if (
        typeof tab.url === "string" &&
        isAuthenticationURL(tab.url) &&
        tab.active !== true
      )
        await this.deps.tabs.reload(pending.tabID);
    } finally {
      this.releaseEffectGovernor(`auth-reload:${jobID}`, token, false);
      this.wakeEffectGovernor();
    }
  }
  private async drainPendingFreshHandoffs(): Promise<void> {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingFreshHandoffs.entries().next();
    if (next.done) return;
    const [jobID, queued] = next.value;
    this.pendingFreshHandoffs.delete(jobID);
    const current = findByJob(this.store, jobID);
    if (current === undefined || current.engagement_required !== true) return;
    if (queued.trigger === "automatic" && !this.institutionalAuthGateOpen()) {
      // The gate can close between the original queueing (effect permit
      // busy) and this drain running (reconnect, hello downgrade, feature
      // loss) — never mint an automatic drive against a gate that has
      // since closed; leave it for the standing engagement_required park.
      return;
    }
    await this.openFreshHandoff(jobID, current, queued.trigger);
  }
  private async drainPendingSessionSignIns(): Promise<void> {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingSessionSignIns.entries().next();
    if (next.done) return;
    const [key, origin] = next.value;
    this.pendingSessionSignIns.delete(key);
    await this.requestSessionSignIn(origin);
  }
  private async drainPendingDirectGets(): Promise<void> {
    if (this.effectGovernorOwner !== undefined) return;
    const next = this.pendingDirectGets.entries().next();
    if (next.done) return;
    const [key, request] = next.value;
    this.pendingDirectGets.delete(key);
    const jobID = request.job_id;
    const payload = request.payload;
    const current =
      jobID === undefined ? undefined : findByJob(this.store, jobID);
    const epoch = current?.drive_epoch;
    if (
      current === undefined ||
      !this.hasDelegatedAuthority(current) ||
      epoch === undefined ||
      epoch.drive_attempt_id !== payload["drive_attempt_id"] ||
      epoch.ordinal !== payload["ordinal"] ||
      epoch.route_revision !== payload["route_revision"]
    ) {
      return;
    }
    await this.onProviderDirectGetRequest(request);
  }

  private releaseEffectGovernor(
    jobID: string,
    token: string,
    wake = true,
  ): void {
    const current = this.effectGovernorOwner;
    if (current?.jobID !== jobID || current.token !== token) return;
    this.effectGovernorOwner = undefined;
    this.effectGovernorWakePending = true;
    if (wake) this.wakeEffectGovernor();
  }

  private currentProviderDrainLease(
    providerKey: string,
  ): ProviderDrainLease | undefined {
    const lease = this.store.providerDrainLeases?.[providerKey];
    if (
      lease === undefined ||
      lease.providerKey !== providerKey ||
      !Number.isFinite(lease.expiresAt) ||
      lease.expiresAt <= this.deps.now()
    ) {
      return undefined;
    }
    return lease;
  }

  private hasActiveProviderDrainLease(job: ActiveJob): boolean {
    return (
      this.currentProviderDrainLease(this.providerKeyForJob(job)) !== undefined
    );
  }

  private isProviderDrainParked(job: ActiveJob): boolean {
    return (
      this.currentProviderDrainLease(this.providerKeyForJob(job))
        ?.parkedReason === "challenge"
    );
  }

  /** Discard stale or malformed persisted leases before a drain chooses work.
   * The owner is intentionally absent from session storage, so an unexpired
   * lease from a prior service worker is respected until this bounded expiry. */
  private async expireProviderDrainLeases(): Promise<string[]> {
    const now = this.deps.now();
    const expired = Object.entries(this.store.providerDrainLeases ?? {})
      .filter(
        ([providerKey, lease]) =>
          lease.providerKey !== providerKey ||
          !Number.isFinite(lease.expiresAt) ||
          lease.expiresAt <= now,
      )
      .map(([providerKey]) => providerKey);
    if (expired.length === 0) return expired;
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      for (const providerKey of expired)
        delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    for (const providerKey of expired) {
      this.providerDrainLeaseOwners.delete(providerKey);
      this.providerDrainLeaseJobs.delete(providerKey);
      this.providerDrainLeaseTimers.delete(providerKey);
    }
    return expired;
  }

  private scheduleProviderDrainLeaseExpiry(
    providerKey: string,
    expiresAt: number,
  ): void {
    const token = {};
    this.providerDrainLeaseTimers.set(providerKey, token);
    this.deps.setTimeout(
      async () => {
        if (this.providerDrainLeaseTimers.get(providerKey) !== token) return;
        this.providerDrainLeaseTimers.delete(providerKey);
        await this.ready;
        const expired = await this.expireProviderDrainLeases();
        if (!expired.includes(providerKey)) return;
        await this.acknowledgePendingProviderHandoffs(providerKey);
        await this.releaseQueuedHandoffs();
      },
      Math.max(0, expiresAt - this.deps.now()),
    );
  }

  private async restoreProviderDrainLeaseTimers(): Promise<void> {
    await this.expireProviderDrainLeases();
    for (const [providerKey, lease] of Object.entries(
      this.store.providerDrainLeases ?? {},
    )) {
      if (this.currentProviderDrainLease(providerKey) !== undefined) {
        this.scheduleProviderDrainLeaseExpiry(providerKey, lease.expiresAt);
      }
    }
  }

  /** Claim one provider while opening its next queued tab. A live claim from
   * this or a prior worker blocks the candidate; callers continue with another
   * provider rather than starting a second drain. */
  private async claimProviderDrainLease(
    job: ActiveJob,
  ): Promise<string | undefined> {
    await this.expireProviderDrainLeases();
    const providerKey = this.providerKeyForJob(job);
    if (this.currentProviderDrainLease(providerKey) !== undefined)
      return undefined;
    const owner = this.deps.randomUUID();
    const expiresAt = this.deps.now() + PROVIDER_DRAIN_LEASE_MS;
    let claimed = false;
    await this.update((store) => {
      const current = store.providerDrainLeases?.[providerKey];
      if (
        current !== undefined &&
        current.providerKey === providerKey &&
        Number.isFinite(current.expiresAt) &&
        current.expiresAt > this.deps.now()
      ) {
        return store;
      }
      claimed = true;
      return {
        ...store,
        providerDrainLeases: {
          ...(store.providerDrainLeases ?? {}),
          [providerKey]: { providerKey, expiresAt },
        },
      };
    });
    if (!claimed) return undefined;
    this.providerDrainLeaseOwners.set(providerKey, owner);
    this.providerDrainLeaseJobs.set(providerKey, job.job_id);
    this.scheduleProviderDrainLeaseExpiry(providerKey, expiresAt);
    return owner;
  }

  private async releaseProviderDrainLease(
    providerKey: string,
    owner: string,
  ): Promise<void> {
    if (this.providerDrainLeaseOwners.get(providerKey) !== owner) return;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseJobs.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const lease = store.providerDrainLeases?.[providerKey];
      if (lease === undefined || lease.parkedReason !== undefined) return store;
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
  }

  /** A challenge remains human-visible in its existing tab. Its siblings stay
   * queued and every new re-offer remains unaccepted until the lease clears. */
  private async parkProviderDrain(job: ActiveJob): Promise<void> {
    const providerKey = this.providerKeyForJob(job);
    const owner = this.deps.randomUUID();
    const expiresAt = this.deps.now() + PROVIDER_DRAIN_LEASE_MS;
    this.providerDrainLeaseOwners.set(providerKey, owner);
    this.providerDrainLeaseJobs.set(providerKey, job.job_id);
    await this.update((store) => ({
      ...store,
      providerDrainLeases: {
        ...(store.providerDrainLeases ?? {}),
        [providerKey]: { providerKey, expiresAt, parkedReason: "challenge" },
      },
    }));
    this.scheduleProviderDrainLeaseExpiry(providerKey, expiresAt);
  }
  /** A non-challenge provider document proves this drain can advance. The next
   * queued sibling may claim a fresh lease; a challenge instead replaces it
   * with the parked form above. */
  private async completeProviderDrainLease(
    providerKey: string,
  ): Promise<boolean> {
    if (this.currentProviderDrainLease(providerKey) === undefined) return false;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseJobs.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    return true;
  }

  /** The only explicit resume is an existing human handoff-open request; a
   * cleared challenge also calls this when its provider document returns. */
  private async clearProviderDrainPark(providerKey: string): Promise<boolean> {
    const lease = this.currentProviderDrainLease(providerKey);
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseJobs.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
    return true;
  }

  private async acknowledgePendingProviderHandoffs(
    providerKey: string,
  ): Promise<boolean> {
    const pending = this.store.activeJobs.filter(
      (job) =>
        this.providerKeyForJob(job) === providerKey &&
        job.handoffAckPending === true,
    );
    for (const job of pending) {
      if (!this.sendJobAccept(job.job_id)) return false;
    }
    if (pending.length === 0) return true;
    const acknowledged = new Set(pending.map((job) => job.job_id));
    await this.update((store) => ({
      ...store,
      activeJobs: store.activeJobs.map((job) => {
        if (!acknowledged.has(job.job_id)) return job;
        const { handoffAckPending: _handoffAckPending, ...resumed } = job;
        return resumed;
      }),
    }));
    return true;
  }

  private async releaseProviderDrainWhenUnused(
    providerKey: string,
  ): Promise<void> {
    if (
      this.store.activeJobs.some(
        (job) => this.providerKeyForJob(job) === providerKey,
      )
    )
      return;
    this.providerDrainLeaseOwners.delete(providerKey);
    this.providerDrainLeaseJobs.delete(providerKey);
    this.providerDrainLeaseTimers.delete(providerKey);
    await this.update((store) => {
      if (store.providerDrainLeases?.[providerKey] === undefined) return store;
      const providerDrainLeases = { ...(store.providerDrainLeases ?? {}) };
      delete providerDrainLeases[providerKey];
      return { ...store, providerDrainLeases };
    });
  }

  /** A warm provider or resolver landing proves this job's institution has a
   * session; an unrelated completed page must not unlock its queued peers. */
  private isInstitutionalSessionLanding(
    job: ActiveJob,
    rawURL: string,
  ): boolean {
    if (job.requires_auth !== true || isAuthenticationURL(rawURL)) return false;
    try {
      const landing = new URL(rawURL);
      const offered = this.offerURLs.get(job.job_id);
      let offeredOrigin: string | undefined;
      if (offered !== undefined) {
        try {
          offeredOrigin = new URL(offered).origin;
        } catch {
          offeredOrigin = undefined;
        }
      }
      return (
        landing.origin === offeredOrigin ||
        hostMatches(landing.hostname, job.provider_hosts) ||
        this.deps.adapterSpecs.some((adapter) =>
          hostMatches(landing.hostname, adapter.hosts),
        )
      );
    } catch {
      return false;
    }
  }

  /** Merge a fresh release-grade observation for `origin` into the persisted
   * evidence map, pruning any entry (including this same origin's prior
   * value) that has already aged past AUTH_EVIDENCE_TTL_MS. Keeps
   * store.authEvidenceByOrigin bounded across a long-lived profile instead
   * of accumulating one entry per resolver ever seen. */
  private withAuthEvidence(
    store: StoreShape,
    origin: string,
    now: number,
  ): Record<string, number> {
    const merged: Record<string, number> = {};
    for (const [existingOrigin, at] of Object.entries(
      store.authEvidenceByOrigin ?? {},
    )) {
      const age = now - at;
      if (age >= 0 && age <= AUTH_EVIDENCE_TTL_MS) merged[existingOrigin] = at;
    }
    merged[origin] = now;
    return merged;
  }

  /** A landing is a reason to look again — ADR-0013 says a timing frame is
   * not identity evidence, so this never asserts an in/out session verdict.
   * But when papio itself drove the tab past authentication onto a page
   * resolving to a configured origin, that IS first-hand evidence for THAT
   * origin: it persists per-origin release evidence (surviving the MV3
   * restarts that wipe everything worker-local), releases only that origin's own
   * queued handoffs, and reloads only that origin's own stalled auth tabs —
   * reloadAuthenticationHandoffs still skips any job already reported to the
   * operator as authStalledReported. It never touches another origin's tabs
   * or queue. An offer origin outside the daemon's current configured set
   * fails closed (do nothing): this remains a best-effort, narrowly-scoped
   * release, never a source of truth beyond its own origin.
   *
   * The resolver probe is requested on every accepted institutional landing.
   * Recent release evidence may still release work immediately, but it must
   * never suppress the fresh check that updates the popup's session verdict. */
  private async recordInstitutionalSession(
    job: ActiveJob,
    rawURL: string,
    now: number,
  ): Promise<boolean> {
    if (!this.isInstitutionalSessionLanding(job, rawURL)) return false;
    const origin = this.jobInstitutionOrigin(job);
    if (origin === undefined) return false;
    // §2.2.1 auth_returned (Slice 3): a claim-owned job's warm landing is
    // exactly the same first-hand evidence this function already exists to
    // record; latched so only the first landing per grant reports it.
    void this.emitClaimObservation(job.job_id, job.tab_id, "auth_returned", true);
    await this.keepaliveManager?.noteInstitutionalLanding(
      origin,
      "tracked_auth_return",
    );
    if (
      !this.holderRole() ||
      !(await this.resolverAccessGranted(origin))
    ) {
      return true;
    }
    await this.update((s) => ({
      ...s,
      lastAuthReturnedAt: now,
      authEvidenceByOrigin: this.withAuthEvidence(s, origin, now),
    }));
    await this.drainQueuedHandoffs(origin, undefined, false);
    await this.reloadAuthenticationHandoffs(origin);
    return true;
  }

  /** OA completions retain the ordinary queue flow without becoming evidence
   * that it is safe to reload or open an institutional sign-in. */
  private async recordOpenAccessLanding(job: ActiveJob): Promise<void> {
    if (job.requires_auth === true) return;
    const firstOpenAccessLanding = !this.openAccessLandingObserved;
    this.openAccessLandingObserved = true;
    await this.releaseQueuedHandoffs();
    if (firstOpenAccessLanding)
      await this.reloadAuthenticationHandoffs(undefined, false);
  }

  // A popup delivery's route is deliberately never classified "oa" here,
  // even when job.requires_auth === false would say the job is OA-routed.
  // Session evidence and OA classification are orthogonal: an operator can
  // send an OA-routed PDF while an unrelated institutional session is warm
  // elsewhere in the same browser, and frozen evidence (below) correctly
  // reports "warm" for that case. The daemon's wire validator rejects any
  // frame with route "oa" and session_evidence not "none" (BrowserAccessBasis),
  // and a rejected delivery_context is a fatal decode that tears down the
  // whole native-messaging session (AGENTS.md) — so claiming "oa" here would
  // turn a merely conservative access_basis into a session-ending crash the
  // moment evidence is honestly "warm". Staying on "direct" always decodes,
  // and "direct" + "none" already resolves to the conservative "manual"
  // basis (never "institutional", never an unverified "open_access") — the
  // same fallback BrowserAccessBasis documents for missing/incomplete
  // context.
  private deliveryRouteFor(
    job: ActiveJob,
    track: DownloadTrack,
  ): DeliveryRoute {
    if (track.delivery === true) return "direct";
    if (track.route !== undefined) return track.route;
    if (this.resolverRoutes.has(job.job_id)) return "resolver";
    return job.requires_auth === false ? "oa" : "direct";
  }

  /** Live session-evidence read, scoped to this job's own configured
   * resolver origin — never the global fallback (that was the leak: any
   * job's delivery could be credited by an unrelated origin's evidence). */
  private currentSessionEvidence(job: ActiveJob): DeliverySessionEvidence {
    const perJob = this.deliverySessionEvidence.get(job.job_id);
    if (perJob !== undefined) return perJob;
    const origin = this.resolverOriginHint(this.offerURLs.get(job.job_id));
    if (origin === undefined) return "none";
    // Tiered off the one persisted source: inside the TTL the session is
    // freshly evidenced, an expired-but-present entry is merely warm, and a
    // revoked or never-seen origin has nothing.
    const lastAuth = this.store.authEvidenceByOrigin?.[origin];
    if (typeof lastAuth !== "number") return "none";
    const age = this.deps.now() - lastAuth;
    return age >= 0 && age <= AUTH_EVIDENCE_TTL_MS ? "fresh_auth" : "warm";
  }

  private deliveryEvidenceFor(
    job: ActiveJob,
    track: DownloadTrack,
    route: DeliveryRoute,
  ): DeliverySessionEvidence {
    if (route === "oa") return "none";
    // Mirrors deliveryPageHost's frozen-host guard below: a popup delivery's
    // session evidence is captured once, in startPDFDelivery, at request
    // time. The download can take seconds with the tab still interactive, so
    // store.authEvidenceByOrigin is live per-origin state that can flip true
    // mid-download — reading it here would credit a public-page delivery with
    // an institutional probe or sign-in that happened to land elsewhere in
    // the browser while the bytes were still in flight.
    if (track.delivery === true) {
      const frozen = this.store.pendingDelivery;
      if (
        frozen?.job_id === job.job_id &&
        frozen.status !== "failed" &&
        frozen.session_evidence !== undefined
      ) {
        return frozen.session_evidence;
      }
    }
    if (track.sessionEvidence !== undefined) return track.sessionEvidence;
    return this.currentSessionEvidence(job);
  }

  private async deliveryPageHost(
    owner: ActiveJob,
    item: DownloadItemLike,
    track: DownloadTrack,
  ): Promise<string | undefined> {
    // Provenance must name the page these bytes came from. For a popup
    // delivery that host was frozen when the download was requested: the
    // download can take seconds, the tab remains interactive throughout, and
    // re-reading it here would label the candidate with whatever the operator
    // navigated to in the meantime.
    //
    // The frozen host is only valid for the delivery download it was captured
    // for. A failed delivery (status: "failed") leaves page_host intact in
    // pendingDelivery, and clearPendingDelivery only runs on job
    // completion/removal — so a stale frozen host can poison a later
    // non-delivery download for the same job (sequence A: failed delivery
    // followed by a resolver-routed download from a different host).
    // Non-delivery downloads (handoff, directOffer) must never inherit a
    // delivery's frozen host (sequence B).
    const frozen = this.store.pendingDelivery;
    if (
      track.delivery === true &&
      frozen?.job_id === owner.job_id &&
      frozen.status !== "failed" &&
      frozen.page_host !== undefined
    ) {
      return frozen.page_host;
    }
    const tabID =
      typeof item.tabId === "number" && item.tabId >= 0
        ? item.tabId
        : owner.tab_id;
    if (tabID < 0) return undefined;
    try {
      const tab = await this.deps.tabs.get(tabID);
      if (typeof tab.url !== "string") return undefined;
      return sanitizePageHost(tab.url);
    } catch {
      return undefined;
    }
  }

  /** Waiting briefly avoids an unattended SAML exchange; a bounded fallback
   * prevents that safety check from parking a cold handoff forever. */
  private scheduleQueuedHandoffRelease(jobID: string): void {
    const job = findByJob(this.store, jobID);
    if (
      job === undefined ||
      job.status !== "queued" ||
      job.engagement_required === true
    ) {
      this.queuedHandoffTimers.delete(jobID);
      this.pendingForcedReleases.delete(jobID);
      return;
    }
    if (this.queuedHandoffTimers.has(jobID)) return;
    const token = {};
    this.queuedHandoffTimers.set(jobID, token);
    const delay = Math.max(
      0,
      job.offered_at + QUEUED_HANDOFF_RELEASE_MS - this.deps.now(),
    );
    this.deps.setTimeout(async () => {
      if (this.queuedHandoffTimers.get(jobID) !== token) return;
      this.queuedHandoffTimers.delete(jobID);
      await this.ready;
      await this.releaseQueuedHandoffs(jobID);
    }, delay);
  }

  /** MV3 timers die with their worker. The periodic wake checks durable offer
   * times so a cold queue cannot restart its fallback window forever on sleep. */
  private async releaseExpiredQueuedHandoffs(): Promise<void> {
    const deadline = this.deps.now() - QUEUED_HANDOFF_RELEASE_MS;
    const due = this.store.activeJobs.filter(
      (job) =>
        job.status === "queued" &&
        job.engagement_required !== true &&
        job.offered_at <= deadline,
    );
    for (const job of due) {
      if (job.requires_auth === true && !this.institutionalAuthGateOpen()) {
        // Slice 0 containment: the 45s fallback may not bypass evidence for
        // institutional authentication work. Park it for explicit
        // engagement instead of forcing a sign-in surface; the inbox action
        // mints or reuses the route on click.
        await this.update((s) =>
          patchJob(s, job.job_id, { engagement_required: true }),
        );
        continue;
      }
      await this.releaseQueuedHandoffs(job.job_id);
    }
  }

  /** Startup has no worker-local timer state. A tracked tab already settled
   * away from an IdP is the same usable-session evidence as a warm landing. */
  private async releaseQueuedHandoffsForLiveLanding(): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (job.tab_id < 0 || job.status === "queued") continue;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        const institutionalSession =
          typeof tab.url === "string" &&
          (await this.recordInstitutionalSession(
            job,
            tab.url,
            this.deps.now(),
          ));
        if (institutionalSession) return;
        if (typeof tab.url === "string" && !isAuthenticationURL(tab.url)) {
          await this.recordOpenAccessLanding(job);
          return;
        }
      } catch {
        // A closed tab is handled by the normal tab-removal path.
      }
    }
  }

  /** origin_hint is authoritative on the daemon: an absent hint scopes the
   * release to the default profile (safe), but a hint that resolves to the
   * WRONG institution is indistinguishable from a correct one and releases
   * that institution's parked handoffs without its session being verified
   * (papio-7d7a0ae96ca5726e). So this only ever forwards the origin the
   * caller actually observed for THIS evidence — never a keepalive
   * snapshot's resolver (which itself degrades to an arbitrary granted
   * host) or the most recent offer's origin, which need not be the origin
   * that produced this evidence at all. */
  emitSessionEvidence(
    evidence: "warm_verified" | "auth_returned",
    originHint?: string,
  ): boolean {
    const now = this.deps.now();
    const throttleKey = originHint ?? "";
    const sentAt = this.sessionEvidenceSentAt.get(throttleKey);
    if (sentAt !== undefined) {
      const age = now - sentAt;
      if (age >= 0 && age < SESSION_EVIDENCE_THROTTLE_MS) return false;
    }
    // session_evidence releases parked handoffs, which only the holder owns;
    // the daemon accordingly refuses the frame from a pending session.
    if (
      !this.holderRole() ||
      !(this.store.daemonFeatures ?? []).includes(SESSION_EVIDENCE_FEATURE)
    )
      return false;
    const payload: Record<string, unknown> = {
      evidence,
      at: new Date(now).toISOString(),
    };
    if (isBareHTTPSOrigin(originHint)) payload.origin_hint = originHint;
    if (!this.send("session_evidence", payload)) return false;
    this.sessionEvidenceSentAt.set(throttleKey, now);
    return true;
  }

  /** Fires from keepalive's onFreshSessionEvidence — the ONLY callback that
   * may authorize work from a decisive probe verdict (see keepalive.ts's
   * KeepaliveOptions doc). Marks `origin` release-grade for this worker life
   * AND persists that evidence (withAuthEvidence) so it survives an MV3
   * restart, unblocks ONLY that origin's queue and stuck tabs, and forwards
   * the timing fact to the daemon. Never resets an authentication-attempt
   * budget, never reopens an auth-stalled human action, never touches
   * another origin's tabs — ADR-0009's autonomous-retry line, held by
   * drainQueuedHandoffs'/reloadAuthenticationHandoffs's own origin scoping
   * below. drainQueuedHandoffs is called directly with an exact origin
   * (never through releaseQueuedHandoffs, whose fallback-driven callers
   * below always pass no origin); recordInstitutionalSession's warm-landing
   * path does the same for its own, narrower kind of evidence.
   *
   * Sibling resume for institutional work now rides the daemon (Slice 3):
   * a parked dependent's browser_candidates row stays eligible and the
   * daemon's own scheduler re-offers it once the claim it was waiting on
   * resolves — this method has nothing left to nudge locally beyond the
   * origin-scoped queue/reload above. */
  async recordFreshSessionEvidence(
    evidence: FreshSessionEvidence,
  ): Promise<void> {
    const { origin } = evidence;
    await this.ready;
    if (
      !this.holderRole() ||
      !(await this.resolverAccessGranted(origin))
    ) {
      return;
    }
    const now = this.deps.now();
    await this.update((s) => ({
      ...s,
      lastAuthReturnedAt: now,
      authEvidenceByOrigin: this.withAuthEvidence(s, origin, now),
    }));
    this.emitSessionEvidence("warm_verified", origin);
    await this.drainQueuedHandoffs(origin, undefined, false);
    await this.reloadAuthenticationHandoffs(origin);
  }

  /** Bypasses evidence for exactly one forced job — the 45s
   * QUEUED_HANDOFF_RELEASE_MS fallback timer, releaseExpiredQueuedHandoffs,
   * operator actions, and provider-lease/challenge-cooldown expiry, all
   * already-ratified ADR-0009 autonomous-retry behaviour. With no
   * fallbackJobID this is a pure opportunistic re-drain: every queued job is
   * still admitted only through its OWN origin's release-grade evidence
   * (hasHandoffReleaseEvidence), so this can never launder one origin's
   * evidence into another's queue. */
  private async releaseQueuedHandoffs(
    fallbackJobID?: string,
    forceProvider = false,
  ): Promise<void> {
    await this.drainQueuedHandoffs(undefined, fallbackJobID, forceProvider);
  }

  private async drainQueuedHandoffs(
    originScope: string | undefined,
    fallbackJobID: string | undefined,
    forceProvider: boolean,
  ): Promise<void> {
    if (fallbackJobID !== undefined) {
      const fallback = findByJob(this.store, fallbackJobID);
      // The operator's explicit inbox-open action may release assisted or
      // otherwise engagement-gated work once; timers may not.
      if (
        fallback !== undefined &&
        (fallback.engagement_required !== true || forceProvider)
      ) {
        this.pendingForcedReleases.add(fallbackJobID);
      }
    }
    if (forceProvider && fallbackJobID !== undefined) {
      const forced = findByJob(this.store, fallbackJobID);
      if (forced !== undefined)
        await this.clearProviderDrainPark(this.providerKeyForJob(forced));
    }
    const jobOrigin = (job: ActiveJob): string | undefined =>
      this.jobInstitutionOrigin(job);
    const matchesOrigin =
      originScope === undefined
        ? (_job: ActiveJob) => true
        : (job: ActiveJob) => jobOrigin(job) === originScope;
    await this.drainHandoffDriveQueue();
    const anyQueuedEligible = this.store.activeJobs.some(
      (job) =>
        matchesOrigin(job) &&
        job.status === "queued" &&
        job.engagement_required !== true &&
        this.hasHandoffReleaseEvidence(jobOrigin(job), job.requires_auth),
    );
    if (!anyQueuedEligible && this.pendingForcedReleases.size === 0) {
      return;
    }
    if (this.drainingQueuedHandoffs) {
      await new Promise<void>((resolve) =>
        this.queuedHandoffDrainWaiters.add(resolve),
      );
      return;
    }
    this.drainingQueuedHandoffs = true;
    let effectBlocked = false;
    try {
      await this.expireProviderDrainLeases();
      await this.expireChallengeCooldowns();
      // One loop opens at most one unclassified handoff per provider. A lease
      // stays with that tab until it proves normal, becomes a challenge park,
      // or expires.
      while (this.handoffDrives.size < HANDOFF_DRIVE_LIMIT) {
        let selected: ActiveJob | undefined;
        let forcedJobID: string | undefined;
        let forcedTemporarilyBlocked = false;
        for (const jobID of this.pendingForcedReleases) {
          const forcedJob = findByJob(this.store, jobID);
          if (forcedJob === undefined) {
            // A removed job is terminal; retire its worker-local marker.
            this.pendingForcedReleases.delete(jobID);
            continue;
          }
          if (forcedJob.status !== "queued") {
            if (forcedJob.status === "offered") {
              // An offer can be transiently re-materialized during a daemon
              // re-offer. Keep the exact request until it is queued again, but
              // do not let an out-of-scope marker block this profile's work.
              if (matchesOrigin(forcedJob)) forcedTemporarilyBlocked = true;
              continue;
            }
            // accepted/auth_pending/awaiting_download means this exact job has
            // already opened and no longer needs a queued-release marker.
            this.pendingForcedReleases.delete(jobID);
            continue;
          }
          // A scoped evidence drain must not consume a forced request for a
          // different resolver profile; leave it for its own unscoped drain.
          if (!matchesOrigin(forcedJob)) continue;
          const candidate = forcedJob;
          if (candidate.engagement_required === true && !forceProvider) {
            // Timer-driven forced release never admits engagement-gated work.
            // The marker is permanently ineligible under this drain mode.
            this.pendingForcedReleases.delete(jobID);
            continue;
          }
          if (
            candidate.requires_auth === true &&
            !forceProvider &&
            !this.institutionalAuthGateOpen()
          ) {
            // Slice 0 containment: a timer/expiry-driven forced release may
            // not mint a sign-in surface. Park for explicit engagement; the
            // operator's own open passes forceProvider and stays admitted.
            this.pendingForcedReleases.delete(jobID);
            await this.update((s) =>
              patchJob(s, jobID, { engagement_required: true }),
            );
            continue;
          }
          if (this.challengeCooldownActiveForHosts(candidate.provider_hosts)) {
            forcedTemporarilyBlocked = true;
            continue;
          }
          if (this.hasActiveProviderDrainLease(candidate)) {
            forcedTemporarilyBlocked = true;
            continue;
          }
          selected = candidate;
          forcedJobID = jobID;
          break;
        }
        // An explicit request has priority over opportunistic work. If its
        // exact job is temporarily blocked, retain the marker and wait rather
        // than substituting another queued job.
        if (selected === undefined && !forcedTemporarilyBlocked) {
          selected = this.store.activeJobs.find(
            (job) =>
              matchesOrigin(job) &&
              job.status === "queued" &&
              job.engagement_required !== true &&
              (job.requires_auth !== true ||
                this.institutionalAuthGateOpen()) &&
              this.hasHandoffReleaseEvidence(
                jobOrigin(job),
                job.requires_auth,
              ) &&
              !this.challengeCooldownActiveForHosts(job.provider_hosts) &&
              !this.hasActiveProviderDrainLease(job),
          );
        }
        if (selected === undefined) return;

        const queued = selected;
        const providerKey = this.providerKeyForJob(queued);
        const owner = await this.claimProviderDrainLease(queued);
        if (owner === undefined) {
          // A lease race is a temporary block for an exact forced request;
          // retain it rather than falling through to opportunistic work.
          if (forcedJobID !== undefined) return;
          continue;
        }
        let opened = false;
        try {
          const forceSurface =
            forcedJobID === queued.job_id &&
            (forceProvider || queued.requires_auth === true);
          this.queuedHandoffTimers.delete(queued.job_id);
          const url = this.offerURLs.get(queued.job_id);
          if (url === undefined) {
            this.pendingForcedReleases.delete(queued.job_id);
            this.send("job_reject", {}, queued.job_id);
            await this.removeJobWithOffer(queued.job_id);
            continue;
          }
          if (!(await this.acknowledgePendingProviderHandoffs(providerKey)))
            return;
          const effectToken = this.claimEffectGovernor(queued.job_id);
          if (effectToken === undefined) {
            // Keep the queued offer and release only this provider's drain
            // lease. The global effect owner will wake the drain when its
            // bounded browser consequence settles.
            effectBlocked = true;
            await this.releaseProviderDrainLease(providerKey, owner);
            return;
          }
          let tabID: number | undefined;
          try {
            tabID = await this.openManagedTab({
              url,
              jobId: queued.job_id,
              purpose: "handoff",
              surfaceFallback: forceSurface,
            });
          } catch (e) {
            console.error("papio: queued handoff tab creation failed", e);
          } finally {
            this.releaseEffectGovernor(queued.job_id, effectToken, false);
          }
          if (tabID === undefined) {
            this.pendingForcedReleases.delete(queued.job_id);
            await this.parkUndrivableHandoff(
              queued.job_id,
              "queued tab creation failed",
            );
            continue;
          }
          this.beginProviderDrive(queued.job_id);
          await this.update((s) =>
            patchJob(s, queued.job_id, {
              tab_id: tabID,
              status: "accepted",
              download_initiated: false,
              unknown_count: 0,
            }),
          );
          this.registerHandoffDrive(queued.job_id, tabID);
          if (queued.requires_auth === true) {
            this.authUnblockedCount += 1;
            this.authUnblockedAt = this.deps.now();
          }
          opened = true;
          this.pendingForcedReleases.delete(queued.job_id);
          if (forceSurface) await this.surfaceWorkTab(tabID);
          this.wakeEffectGovernor();
        } finally {
          if (!opened) await this.releaseProviderDrainLease(providerKey, owner);
        }
      }
    } finally {
      this.drainingQueuedHandoffs = false;
      for (const resolve of this.queuedHandoffDrainWaiters) resolve();
      this.queuedHandoffDrainWaiters.clear();
      this.wakeEffectGovernor();
      // The effect owner can settle during the awaits above. Re-run after the
      // drain latch clears so a queued offer cannot be stranded by that
      // narrow release race.
      if (effectBlocked && this.effectGovernorOwner === undefined)
        void this.releaseQueuedHandoffs();
    }
  }
  private async reloadAuthenticationHandoffs(
    origin: string | undefined,
    includeInstitutional = true,
  ): Promise<void> {
    for (const job of this.store.activeJobs) {
      if (
        !this.hasDelegatedAuthority(job) ||
        job.status === "queued" ||
        (!includeInstitutional && job.requires_auth === true) ||
        (origin !== undefined &&
          this.jobInstitutionOrigin(job) !== origin) ||
        this.authStalledReported.has(job.job_id)
      ) {
        continue;
      }
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        if (typeof tab.url !== "string" || !isAuthenticationURL(tab.url))
          continue;
        const effectToken = this.claimEffectGovernor(
          `auth-reload:${job.job_id}`,
        );
        if (effectToken === undefined) {
          this.pendingAuthReloads.set(job.job_id, {
            jobID: job.job_id,
            tabID: job.tab_id,
          });
          continue;
        }
        try {
          const current = findByJob(this.store, job.job_id);
          // Renavigation fence: fresh read under the held permit — a reload
          // wipes anything the operator has typed into a credential form.
          const fresh = await this.deps.tabs.get(job.tab_id);
          if (
            current?.tab_id === job.tab_id &&
            this.hasDelegatedAuthority(current) &&
            fresh.active !== true
          ) {
            await this.deps.tabs.reload(job.tab_id);
          }
        } finally {
          this.releaseEffectGovernor(
            `auth-reload:${job.job_id}`,
            effectToken,
            false,
          );
          this.wakeEffectGovernor();
        }
      } catch {
        // A closed handoff is handled by the normal tab-removal path.
      }
    }
  }
  public async sendPageCapture(
    payload: PageCapturePayload,
    jobID?: string,
  ): Promise<boolean> {
    await this.captureTransmissionPolicyReady;
    if (!this.pageCaptureAvailable()) return false;
    await this.refreshCaptureConsent();
    if (!this.captureTransmissionAllowed) {
      if (!this.captureConsentNoteLogged) {
        this.captureConsentNoteLogged = true;
        console.debug(
          "papio: Firefox page-capture transmission is disabled until consent is enabled in settings",
        );
      }
      return false;
    }
    return this.send(MsgPageCapture, payload, jobID);
  }

  private waitForPageCaptureLoad(tabID: number): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      let finished = false;
      const finish = (loaded: boolean): void => {
        if (finished) return;
        finished = true;
        if (this.pageCaptureLoadWaiters.get(tabID) === finish) {
          this.pageCaptureLoadWaiters.delete(tabID);
        }
        resolve(loaded);
      };
      this.pageCaptureLoadWaiters.set(tabID, finish);
      this.deps.setTimeout(() => finish(false), PAGE_CAPTURE_NAV_TIMEOUT_MS);
    });
  }

  private async onPageCaptureRequest(msg: BrowserMessage): Promise<void> {
    await this.captureTransmissionPolicyReady;
    await this.refreshCaptureConsent();
    const request = msg.payload as unknown as PageCaptureRequestPayload;
    const reply = (
      outcome: PageCaptureRequestResultPayload["outcome"],
      detail?: string,
    ): void => {
      const payload: PageCaptureRequestResultPayload = {
        request_id: request.request_id,
        outcome,
        ...(detail === undefined ? {} : { detail }),
      };
      this.send(MsgPageCaptureRequestResult, payload);
    };
    if (
      !this.pageCaptureAvailable() ||
      !(this.store.daemonFeatures ?? []).includes(PAGE_CAPTURE_REQUEST_FEATURE)
    ) {
      reply("not_permitted", "page capture is not available");
      return;
    }
    if (!this.captureTransmissionAllowed) {
      if (!this.captureConsentNoteLogged) {
        this.captureConsentNoteLogged = true;
        console.debug(
          "papio: Firefox page-capture transmission is disabled until consent is enabled in settings",
        );
      }
      reply(
        "not_permitted",
        "page capture transmission requires consent in settings",
      );
      return;
    }
    let requested: URL;
    try {
      requested = new URL(request.url);
    } catch {
      reply("nav_failed", "the requested URL is invalid");
      return;
    }
    try {
      if (
        !(await this.deps.permissions.contains({
          origins: [`${requested.origin}/*`],
        }))
      ) {
        reply("not_permitted", "provider host permission is not granted");
        return;
      }
    } catch {
      reply("not_permitted", "provider host permission could not be checked");
      return;
    }
    if (
      this.pageCaptureDriving ||
      this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT
    ) {
      reply("busy", "browser handoff slots are occupied");
      return;
    }

    this.pageCaptureDriving = true;
    const managedKey = `capture_${request.request_id}`;
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: request.url,
        jobId: managedKey,
        purpose: "capture",
        surfaceFallback: true,
        focusExisting: false,
      });
      if (tabID === undefined) {
        reply("nav_failed", "could not open the provider page");
        return;
      }
      // An explicit fixture-capture command may need a visible tab: heavy SPAs
      // and consent managers routinely stop rendering in the minimized work
      // window. The command itself is the operator's request to surface it.
      await this.surfaceWorkTab(tabID);
      const load = this.waitForPageCaptureLoad(tabID);
      try {
        if ((await this.deps.tabs.get(tabID)).status === "complete") {
          this.pageCaptureLoadWaiters.get(tabID)?.(true);
        }
      } catch {
        this.pageCaptureLoadWaiters.get(tabID)?.(false);
      }
      if (!(await load)) {
        reply("timeout", "provider page did not finish loading");
        return;
      }
      const settleMS = request.settle_ms ?? PAGE_CAPTURE_DEFAULT_SETTLE_MS;
      if (settleMS > 0) {
        await new Promise<void>((resolve) =>
          this.deps.setTimeout(resolve, settleMS),
        );
      }
      let injected: { result?: unknown } | undefined;
      try {
        [injected] = await this.deps.scripting.executeScript({
          target: { tabId: tabID },
          func: capturePage,
        });
      } catch {
        reply("not_permitted", "could not read the provider page");
        return;
      }
      const page = injected?.result as PageCapture | undefined;
      if (
        page === undefined ||
        typeof page.html !== "string" ||
        typeof page.origin !== "string" ||
        typeof page.path !== "string"
      ) {
        reply("nav_failed", "provider page capture returned no document");
        return;
      }
      let finalOrigin: URL;
      try {
        finalOrigin = new URL(page.origin);
      } catch {
        reply("nav_failed", "provider page has an invalid origin");
        return;
      }
      if (finalOrigin.protocol !== "https:" || finalOrigin.hostname === "") {
        reply("nav_failed", "provider page did not finish on https");
        return;
      }
      try {
        if (
          !(await this.deps.permissions.contains({
            origins: [`${finalOrigin.origin}/*`],
          }))
        ) {
          reply(
            "not_permitted",
            "final provider host permission is not granted",
          );
          return;
        }
      } catch {
        reply(
          "not_permitted",
          "final provider host permission could not be checked",
        );
        return;
      }
      const sanitized = sanitizeFixture(page.html, {
        provider: request.provider as Provider,
        scenario: request.scenario as Scenario,
        originNoQuery: `${page.origin}${page.path}`,
        capturedISO: new Date(this.deps.now()).toISOString(),
      });
      const leak = residualLeak(sanitized);
      if (leak !== null) {
        reply("nav_failed", "sanitized page did not pass the privacy check");
        return;
      }
      const encoded = await encodePageCapture(sanitized, {
        host: finalOrigin.hostname,
        scenario: request.scenario,
        adapterID: request.provider,
        // Binds this content frame to the request that asked for it. The
        // daemon used to match provider+scenario alone, so an unsolicited
        // capture of the same pair could satisfy this pending request and
        // hand its caller the other capture's file path
        // (papio-85a7420f4cd2564f).
        requestID: request.request_id,
      });
      if (!encoded.ok) {
        reply("nav_failed", encoded.error);
        return;
      }
      if (!(await this.sendPageCapture(encoded.payload))) {
        reply("nav_failed", "could not send the sanitized page capture");
        return;
      }
      reply("captured");
    } finally {
      if (tabID !== undefined) void this.closeOwnedTab(tabID, "page-capture");
      this.pageCaptureDriving = false;
    }
  }

  /** Acknowledge one offer, telling the daemon whether this is a DRIVE or a
   * place in this worker's queue.
   *
   * The authority is `handoffDrives` — the registry of slots this worker
   * actually holds — and never the job's persisted status. `status` is a proxy
   * that reads backwards on the path that matters: the governor queue behind
   * `HANDOFF_DRIVE_LIMIT` persists "accepted" (it *will* be driven) while
   * `enqueueHandoffDrive` has only queued it, so a status-derived disposition
   * reported the drive-slot wait — the exact wait that burned 438 accepts
   * across 62 papers — as a drive. Every caller either registers a drive
   * immediately before acking or enqueues one, so the registry answers
   * correctly at every site by construction.
   */
  private sendJobAccept(jobID: string): boolean {
    const driving = this.handoffDrives.has(jobID);
    return this.send(
      "job_accept",
      driving ? {} : { disposition: "queued" },
      jobID,
    );
  }

  /** Build, self-validate, and post one outbound frame. Validation is a safety
   * net: a frame that would not survive the shared parser is dropped, never
   * emitted. */
  private send(
    type: BrowserMessageType,
    payload: object,
    jobID?: string,
    msgID?: string,
  ): boolean {
    const port = this.port;
    if (!port) return false;
    const env: Record<string, unknown> = {
      protocol: BROWSER_PROTOCOL_VERSION,
      type,
      msg_id: msgID ?? this.deps.randomUUID().replace(/-/g, ""),
      seq: this.seq++,
      payload,
    };
    if (jobID !== undefined) env.job_id = jobID;
    try {
      if (
        new TextEncoder().encode(JSON.stringify(env)).byteLength >
        MAX_BROWSER_MESSAGE_BYTES
      ) {
        console.error(
          "papio: refusing to send frame over native message cap",
          type,
        );
        return false;
      }
    } catch (e) {
      console.error("papio: refusing to encode outbound frame", type, e);
      return false;
    }
    try {
      parseBrowserMessageWithLegacyInstitutionalNavigation(
        env,
        type === "institutional_navigated_request" &&
          (this.store.daemonFeatures ?? []).includes(
            INSTITUTIONAL_MATERIALIZATION_FEATURE,
          ) &&
          !(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE),
      );
    } catch (e) {
      console.error("papio: refusing to send invalid frame", type, e);
      return false;
    }
    try {
      port.postMessage(env);
      return true;
    } catch (e) {
      console.error("papio: native postMessage failed", e);
      return false;
    }
  }

  private enqueueInbound(raw: unknown, sourcePort: NativePort): Promise<void> {
    const dispatched = this.inboundChain.then(() => {
      if (this.port !== sourcePort) return;
      return this.onInbound(raw);
    });
    // Keep later frames progressing even if a single handler fails unexpectedly;
    // the returned promise still exposes that failure to the event emitter.
    this.inboundChain = dispatched.catch((e) => {
      console.error("papio: inbound frame handler failed", e);
    });
    return dispatched;
  }

  private resolveNativeResponse(msg: BrowserMessage): void {
    const requestID = msg.payload["request_id"];
    if (typeof requestID !== "string") return;
    const pending = this.pendingNativeRequests.get(requestID);
    if (pending === undefined || pending.expectedType !== msg.type) {
      console.debug(
        "papio: dropping unknown or late correlated response",
        msg.type,
        requestID,
      );
      return;
    }
    this.pendingNativeRequests.delete(requestID);
    pending.resolve({ kind: "response", payload: msg.payload });
  }
  private resolveNativeError(msg: BrowserMessage): void {
    const requestID = msg.payload["request_id"];
    const code =
      typeof msg.payload["code"] === "string"
        ? msg.payload["code"]
        : "daemon_error";
    const message =
      typeof msg.payload["message"] === "string"
        ? msg.payload["message"]
        : "The daemon rejected the request";
    if (typeof requestID !== "string") {
      console.warn("papio: dropping uncorrelated daemon error", msg.payload);
      return;
    }
    const pending = this.pendingNativeRequests.get(requestID);
    if (pending !== undefined) {
      this.pendingNativeRequests.delete(requestID);
      pending.resolve({ kind: "transport", code, message });
      return;
    }
    const pageAcquire = this.pageAcquireWaiters.get(requestID);
    if (pageAcquire !== undefined) {
      this.pageAcquireWaiters.delete(requestID);
      pageAcquire({ error: message });
      return;
    }
    if (requestID === this.helloRequestID) {
      this.helloSentGeneration = -1;
      this.helloRequestID = undefined;
      this.settleHelloWaiters(false);
      return;
    }
    console.debug("papio: dropping unknown or late daemon error", requestID);
  }
  private onUnsolicitedPdfGrab(msg: BrowserMessage): void {
    const grabID = msg.payload["grab_id"];
    const outcome = msg.payload["outcome"];
    if (typeof grabID !== "string" || typeof outcome !== "string") return;
    const track = this.grabDownloads.get(grabID);
    const persisted = this.pdfGrabCorrelations.get(grabID);
    const correlation =
      track === undefined
        ? persisted
        : { scanID: track.scanID, tabID: track.tabID, state: "identifying" };
    if (correlation === undefined) return;
    const detail =
      typeof msg.payload["detail"] === "string"
        ? msg.payload["detail"]
        : undefined;
    const terminal =
      outcome === "job_created"
        ? "job_created"
        : outcome === "already_owned"
          ? "already_owned"
          : outcome === "needs_identifier"
            ? "needs_identifier"
            : outcome === "abandoned"
              ? "abandoned"
              : "failed";
    this.notifyPdfGrab(correlation.scanID, grabID, terminal, detail);
    this.evictPdfGrabRouteSteering(grabID, persisted);
    this.grabDownloads.delete(grabID);
    this.pdfGrabCorrelations.delete(grabID);
    this.persistPdfGrabCorrelations();
  }
  private async onProviderDirectGetRequest(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (
      jobID === undefined ||
      !(this.store.daemonFeatures ?? []).includes(
        PROVIDER_DIRECT_GET_FEATURE,
      ) ||
      !(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE)
    )
      return;
    const p = msg.payload;
    const attemptID = p["drive_attempt_id"];
    const ordinal = p["ordinal"];
    const revision = p["route_revision"];
    const rawURL = p["url"];
    const origin = p["allowed_origin"];
    const pathFamily = p["path_family"];
    const expectedIdentifier = p["expected_identifier"];
    const termsPolicy = p["terms_policy"];
    if (
      typeof attemptID !== "string" ||
      typeof ordinal !== "number" ||
      typeof revision !== "string" ||
      typeof rawURL !== "string" ||
      typeof origin !== "string" ||
      typeof pathFamily !== "string" ||
      typeof expectedIdentifier !== "string" ||
      (termsPolicy !== "none" && termsPolicy !== "durable_consent")
    )
      return;
    let target: URL;
    let allowed: URL;
    try {
      target = new URL(rawURL);
      allowed = new URL(origin);
    } catch {
      return;
    }
    const envelopeValid =
      target.protocol === "https:" &&
      allowed.protocol === "https:" &&
      allowed.pathname === "/" &&
      allowed.search === "" &&
      allowed.hash === "" &&
      target.hash === "" &&
      (target.search === "" || target.search === "?download=true") &&
      target.host === allowed.host &&
      directEnvelopePath(target.pathname, pathFamily, expectedIdentifier);
    if (!envelopeValid) {
      this.send(
        "provider_direct_get_result",
        {
          drive_attempt_id: attemptID,
          ordinal,
          route_revision: revision,
          outcome: "foreign",
          landing_class: "foreign",
          detail:
            "direct route envelope is malformed or outside the allowed path family",
        },
        jobID,
      );
      return;
    }
    if (
      termsPolicy === "durable_consent" &&
      (await this.deps.settings.getTermsConsent()) !== "accept"
    ) {
      this.send(
        "provider_direct_get_result",
        {
          drive_attempt_id: attemptID,
          ordinal,
          route_revision: revision,
          outcome: "terms",
          landing_class: "terms",
          detail: "durable terms consent is required for this direct route",
        },
        jobID,
      );
      return;
    }
    const prior = findByJob(this.store, jobID);
    if (prior !== undefined && !this.hasDelegatedAuthority(prior)) return;
    const priorEpoch = prior?.drive_epoch;
    const priorDirect = prior as
      (ActiveJob & { direct_terminal?: boolean }) | undefined;
    if (
      priorEpoch !== undefined &&
      priorEpoch.drive_attempt_id === attemptID &&
      priorEpoch.ordinal === ordinal &&
      priorEpoch.route_revision === revision &&
      (priorDirect?.direct_terminal === true ||
        prior?.download_initiated === true ||
        this.downloads.has(jobID))
    )
      return;
    if (
      priorEpoch?.drive_attempt_id === attemptID &&
      priorEpoch.in_flight_download_id !== undefined
    ) {
      const found = await this.deps.downloads.search({
        id: priorEpoch.in_flight_download_id,
      });
      if (found.length !== 0) {
        this.downloads.set(jobID, {
          ids: new Set([priorEpoch.in_flight_download_id]),
          ambiguous: false,
          directOffer: true,
          directEpoch: priorEpoch,
          directURL: rawURL,
          directAllowedOrigin: allowed.origin,
          directPathFamily: pathFamily,
          directExpectedIdentifier: expectedIdentifier,
        });
        return;
      }
    }
    const now = this.deps.now();
    const epoch: ProviderDriveEpoch =
      priorEpoch?.drive_attempt_id === attemptID
        ? priorEpoch
        : {
            drive_attempt_id: attemptID,
            ordinal,
            strategy: "direct",
            route_revision: revision,
            attempt_count: (priorEpoch?.attempt_count ?? 0) + 1,
          };
    const job: ActiveJob = {
      ...(prior ?? {
        job_id: jobID,
        tab_id: -1,
        offered_at: now,
        expires_at: now + 24 * 60 * 60_000,
        status: "accepted",
        provider_hosts: [allowed.hostname],
        access_mode: "delegated",
      }),
      drive_epoch: epoch,
    };
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      if (prior === undefined) await this.update((s) => upsertJob(s, job));
      const requestKey = `${jobID}:${attemptID}:${ordinal}:${revision}`;
      this.pendingDirectGets.set(requestKey, msg);
      return;
    }
    const providerKey = this.providerKeyForJob(job);
    const providerLeaseJob = this.providerDrainLeaseJobs.get(providerKey);
    if (providerLeaseJob !== undefined && providerLeaseJob !== jobID) {
      this.releaseEffectGovernor(jobID, effectToken);
      this.send(
        "provider_direct_get_result",
        {
          drive_attempt_id: attemptID,
          ordinal,
          route_revision: revision,
          outcome: "network",
          landing_class: "unknown",
        },
        jobID,
      );
      return;
    }
    let providerLeaseOwner = this.providerDrainLeaseOwners.get(providerKey);
    try {
      if (
        providerLeaseOwner === undefined &&
        this.currentProviderDrainLease(providerKey) === undefined
      ) {
        providerLeaseOwner = await this.claimProviderDrainLease(job);
      }
    } catch {
      this.releaseEffectGovernor(jobID, effectToken);
      return;
    }
    if (providerLeaseOwner === undefined) {
      this.releaseEffectGovernor(jobID, effectToken);
      this.send(
        "provider_direct_get_result",
        {
          drive_attempt_id: attemptID,
          ordinal,
          route_revision: revision,
          outcome: "network",
          landing_class: "unknown",
        },
        jobID,
      );
      return;
    }
    try {
      await this.update((s) =>
        upsertJob(s, {
          ...job,
          status: "accepted",
          tab_id: -1,
          download_initiated: true,
          drive_epoch: epoch,
          direct_envelope: {
            allowed_origin: allowed.origin,
            path_family: pathFamily,
            expected_identifier: expectedIdentifier,
          },
          direct_terminal: false,
        } as ActiveJob),
      );
      this.pendingDownloadURLs.set(rawURL, jobID);
      try {
        let downloadID: number;
        try {
          downloadID = await this.deps.downloads.download({
            url: rawURL,
            filename: jobDownloadFilename(jobID),
            conflictAction: "uniquify",
            saveAs: false,
          });
        } catch {
          // The browser rejected initiation before returning an effect id, so
          // this exact attempt is conclusively non-dispatched.
          try {
            await this.update((s) =>
              patchJob(s, jobID, { download_initiated: false }),
            );
          } catch {
            // The daemon result remains authoritative even if local cleanup
            // cannot be persisted.
          }
          this.send(
            "provider_direct_get_result",
            {
              drive_attempt_id: attemptID,
              ordinal,
              route_revision: revision,
              outcome: "network",
              landing_class: "unknown",
            },
            jobID,
          );
          return;
        }
        const current = findByJob(this.store, jobID)?.drive_epoch ?? epoch;
        const inFlight = { ...current, in_flight_download_id: downloadID };
        // Install the exact in-memory correlation before persistence. Once
        // downloads.download returns, a storage failure is unknown completion,
        // never a network result that releases the daemon permit.
        this.downloads.set(jobID, {
          ids: new Set([downloadID]),
          ambiguous: false,
          directOffer: true,
          directEpoch: inFlight,
          directURL: rawURL,
          directAllowedOrigin: allowed.origin,
          directPathFamily: pathFamily,
          directExpectedIdentifier: expectedIdentifier,
        });
        try {
          await this.update((s) =>
            patchJob(s, jobID, { drive_epoch: inFlight }),
          );
        } catch {
          console.error(
            "papio: could not persist direct download correlation; keeping permit unresolved",
          );
        }
      } finally {
        this.pendingDownloadURLs.delete(rawURL);
      }
    } finally {
      try {
        if (providerLeaseOwner !== undefined)
          await this.releaseProviderDrainLease(providerKey, providerLeaseOwner);
      } finally {
        this.releaseEffectGovernor(jobID, effectToken);
      }
    }
  }

  /** Look up one already-correlated browser download. Reconciliation is not
   * allowed to search by filename or URL: an absent/failed exact-ID query is
   * simply an unknown observation. */
  private async exactDownloadPresent(
    downloadID: number | undefined,
  ): Promise<boolean> {
    if (
      downloadID === undefined ||
      !Number.isSafeInteger(downloadID) ||
      downloadID < 0
    )
      return false;
    try {
      const found = await this.deps.downloads.search({ id: downloadID });
      return found.length > 0;
    } catch {
      return false;
    }
  }

  /** Answer an effect-permit reconcile request from browser-local, URL-free
   * state only. This is a direct notification: awaiting requestNative here
   * would deadlock the inbound FIFO delivering this request. */
  private async onEffectPermitReconcileRequest(
    msg: BrowserMessage,
  ): Promise<void> {
    const p = msg.payload;
    const requestID = p["request_id"];
    const permitID = p["permit_id"];
    const jobID = msg.job_id;
    const base = {
      request_id: typeof requestID === "string" ? requestID : "",
      permit_id: typeof permitID === "string" ? permitID : "",
      outcome: "stale" as const,
      dispatched: false,
      download_present: false,
      acknowledged: false,
      tab_present: false,
    };
    if (typeof requestID !== "string" || typeof permitID !== "string") {
      this.send("effect_permit_reconcile_response", base, jobID);
      return;
    }
    const kind = p["effect_kind"];
    if (kind !== "pdf_grab" && jobID === undefined) {
      this.send("effect_permit_reconcile_response", base, jobID);
      return;
    }
    let matched = false;
    let dispatched = false;
    let downloadID: number | undefined;
    let acknowledged = false;
    let tabID: number | undefined;
    let settled = false;
    if (kind === "generic_drive" || kind === "direct_get") {
      const job = findByJob(this.store, jobID as string);
      const epoch =
        kind === "generic_drive" ? job?.generic_drive_epoch : job?.drive_epoch;
      const attemptID = p["drive_attempt_id"];
      const ordinal = p["ordinal"];
      const strategy = p["strategy"];
      const revision = p["revision"];
      const exact =
        epoch !== undefined &&
        typeof attemptID === "string" &&
        epoch.drive_attempt_id === attemptID &&
        typeof ordinal === "number" &&
        epoch.ordinal === ordinal &&
        typeof strategy === "string" &&
        ((kind === "generic_drive" &&
          epoch.strategy === "generic" &&
          strategy === "generic") ||
          (kind === "direct_get" &&
            epoch.strategy === "direct" &&
            strategy === "direct_get")) &&
        typeof revision === "string" &&
        (kind === "generic_drive"
          ? epoch.revision === revision
          : epoch.route_revision === revision);
      if (exact && job !== undefined) {
        matched = true;
        downloadID = epoch.in_flight_download_id;
        dispatched = downloadID !== undefined;
      }
    } else if (kind === "pdf_grab") {
      const grabID = p["grab_id"];
      const correlation =
        typeof grabID === "string"
          ? this.pdfGrabCorrelations.get(grabID)
          : undefined;
      if (correlation !== undefined) {
        matched = true;
        dispatched = true;
        downloadID = correlation.downloadID;
        tabID = correlation.tabID;
      }
    } else if (kind === "terms") {
      const occurrenceID = p["terms_occurrence_id"];
      const correlation =
        jobID === undefined ? undefined : this.store.termsEffects?.[jobID];
      if (
        correlation !== undefined &&
        correlation.permit_id === permitID &&
        typeof occurrenceID === "string" &&
        correlation.terms_occurrence_id === occurrenceID
      ) {
        matched = true;
        dispatched = correlation.dispatched;
        acknowledged = correlation.acknowledged;
        settled = correlation.acknowledged;
      }
    } else if (kind === "institutional") {
      const claimID = p["claim_id"];
      const bindingID = p["binding_id"];
      const requestedTabID = p["tab_id"];
      const correlation = Object.values(this.store.materializations ?? {}).find(
        (entry) =>
          entry.job_id === jobID &&
          typeof claimID === "string" &&
          entry.claim_id === claimID &&
          typeof bindingID === "string" &&
          entry.binding_id === bindingID &&
          (requestedTabID === undefined || entry.tab_id === requestedTabID),
      );
      if (correlation !== undefined) {
        matched = true;
        acknowledged = correlation.phase === "navigated";
        tabID = correlation.tab_id;
      }
    }
    if (!matched) {
      // Missing browser-local correlation is an unknown observation, not proof
      // that the daemon's occupying permit is stale. The worker can die after
      // durable acquire but before persisting its local tuple; recording all
      // false moves held -> unknown_completion without authorizing a retry.
      this.send(
        "effect_permit_reconcile_response",
        { ...base, outcome: "recorded" },
        jobID,
      );
      return;
    }
    const downloadPresent = await this.exactDownloadPresent(downloadID);
    let tabPresent = false;
    if (tabID !== undefined && Number.isSafeInteger(tabID) && tabID >= 0) {
      try {
        await this.deps.tabs.get(tabID);
        tabPresent = true;
      } catch {
        tabPresent = false;
      }
    }
    this.send(
      "effect_permit_reconcile_response",
      {
        request_id: requestID,
        permit_id: permitID,
        outcome: settled ? "settled" : "recorded",
        dispatched,
        download_present: downloadPresent,
        acknowledged,
        tab_present: tabPresent,
      },
      jobID,
    );
  }

  /** Reload this extension from disk on the daemon's command, replacing the
   * manual chrome://extensions Reload click. Refused unless this is an
   * unpacked development load: chrome.runtime.reload() on a store-installed
   * extension re-reads nothing and only restarts the worker, so obeying it
   * there would be pure disruption. The reload tears down the native port,
   * so the daemon learns the outcome as a NEW session id rather than a reply.
   */
  private async onDevReload(msg: BrowserMessage): Promise<void> {
    const reloadID = typeof msg.payload["reload_id"] === "string" ? msg.payload["reload_id"] : "";
    const seam = this.deps.devReload;
    if (seam === undefined) {
      console.warn("papio: dev_reload has no seam in this build");
      return;
    }
    let installType: string;
    try {
      installType = await seam.installType();
    } catch (e) {
      console.warn("papio: dev_reload installType check failed", e);
      return;
    }
    if (installType !== "development") {
      console.warn(`papio: refusing dev_reload for installType "${installType}": a store-installed extension is never restarted by dev_reload`);
      return;
    }
    console.log(`papio: dev_reload ${reloadID}: reloading extension from disk`);
    seam.reload();
  }

  private async onInbound(raw: unknown): Promise<void> {
    let msg: BrowserMessage;
    try {
      msg = parseBrowserMessage(raw);
    } catch (e) {
      // Fail closed: a malformed frame means the peer is untrustworthy.
      console.error("papio: protocol error on inbound frame; disconnecting", e);
      this.disconnect();
      return;
    }
    await this.ready;
    if (msg.type === "effect_permit_reconcile_request") {
      await this.onEffectPermitReconcileRequest(msg);
      return;
    }
    // Every correlated daemon result is routed from ONE list. When the switch
    // below enumerated these case-by-case, review_preview_result was simply
    // absent: the daemon issued the preview capability, the frame fell through
    // to the ignore-echo default, and every "View PDF" click sat until its
    // request timed out reporting that the daemon had not responded. A reply
    // type can no longer be named as a requestNative expectation and go
    // unrouted here.
    const grabRequestID = msg.payload["request_id"];
    if (
      msg.type === "pdf_grab_result" &&
      (grabRequestID === undefined || grabRequestID === "")
    ) {
      this.onUnsolicitedPdfGrab(msg);
      return;
    }
    if (MATERIALIZATION_RESPONSE_TYPES[String(msg.type)] === true) {
      this.resolveMaterializationResponse(msg);
      return;
    }
    if (String(msg.type) === "institutional_candidate_offer") {
      // Candidate offers are notifications. Do not await a claim workflow
      // inside the inbound FIFO; a response must be free to arrive next.
      void this.onInstitutionalCandidateOffer(msg);
      return;
    }
    if (CORRELATED_RESULT_TYPES.has(msg.type)) {
      this.resolveNativeResponse(msg);
      return;
    }
    switch (msg.type) {
      case "page_capture_request":
        await this.onPageCaptureRequest(msg);
        return;
      case "job_offer":
        await this.onJobOffer(msg);
        return;
      case "provider_direct_get_request":
        await this.onProviderDirectGetRequest(msg);
        return;
      case "cancel":
        await this.onCancel(msg);
        return;
      case "handoff_focus":
        if (msg.job_id !== undefined) {
          // A missing handoff may refresh counts, whose reply is serialized on
          // this FIFO; detach it so the correlated reply can be received.
          void this.focusDaemonHandoff(msg.job_id);
        }
        return;
      case "dev_reload":
        await this.onDevReload(msg);
        return;
      case "hello_ack": {
        const version =
          typeof msg.payload.daemon_version === "string"
            ? msg.payload.daemon_version
            : null;
        const features = Array.isArray(msg.payload.features)
          ? msg.payload.features.filter(
              (feature): feature is string => typeof feature === "string",
            )
          : [];
        const resolverOrigins = Array.isArray(msg.payload.resolver_origins)
          ? msg.payload.resolver_origins.filter(
              (o): o is string => typeof o === "string",
            )
          : [];
        const connectionStatus =
          version !== null && isSemverLowerThan(version, MIN_DAEMON_VERSION)
            ? "daemon_outdated"
            : "connected";
        const stampedVersion =
          typeof __PAPIO_DAEMON_VERSION__ === "string"
            ? __PAPIO_DAEMON_VERSION__
            : "";
        const negotiated: Pick<
          StoreShape,
          | "connectionStatus"
          | "daemonVersion"
          | "daemonUpdateHint"
          | "daemonFeatures"
          | "resolverOrigins"
        > = {
          connectionStatus,
          daemonVersion: version,
          daemonUpdateHint: hasDaemonUpdateHint(version, stampedVersion),
          daemonFeatures: features,
          resolverOrigins,
        };
        const reapplyAfterHydration = !this.hydrated;
        if (this.hydrated) {
          await this.update((s) => ({ ...s, ...negotiated }));
        } else {
          // hello can arrive while backend.load is still in flight. Publish
          // negotiation in memory now so the inbound chain can continue, then
          // reapply it after hydration instead of deadlocking hello against
          // startup work that itself waits for negotiated features.
          this.store = { ...this.store, ...negotiated };
        }
        await this.syncConnectionBadge(connectionStatus);
        // Set before anything below can consult it. An absent `role` is a
        // daemon that predates session roles, and it only ever acknowledged
        // the session it had just slotted, so its silence means holder.
        this.helloRole = msg.payload.role === "pending" ? "pending" : "holder";
        this.helloAckGeneration = this.portGeneration;
        // A claim can seat this session after it was refused; the ack arrives
        // on the same port, so the refusal must stop shortcutting requests.
        this.helloDeniedGeneration = -1;
        // Only ever present on a holder ack (protocol.ts rejects it
        // alongside role "pending"); always the freshest truth available,
        // so it unconditionally overwrites any earlier local guess.
        const ackHolderGeneration = msg.payload.browser_holder_generation;
        if (typeof ackHolderGeneration === "number") {
          this.lastKnownBrowserHolderGeneration = ackHolderGeneration;
        }
        this.keepaliveManager?.notifyConfiguredOriginsChanged();
        if (reapplyAfterHydration) {
          const acknowledgedGeneration = this.portGeneration;
          void this.ready.then(async () => {
            if (
              this.helloAckGeneration !== acknowledgedGeneration ||
              this.portGeneration !== acknowledgedGeneration
            ) {
              return;
            }
            await this.update((s) => ({ ...s, ...negotiated }));
            this.keepaliveManager?.notifyConfiguredOriginsChanged();
            await this.syncConnectionBadge(connectionStatus);
          });
        }
        if (features.includes(AUTHENTICATION_CLAIM_FEATURE)) {
          // Same off-chain rule as reconciliation below: schedule only,
          // never await — scheduleObservationOutboxDrain is itself
          // non-blocking and safe to call directly from this handler.
          this.scheduleObservationOutboxDrain();
        }
        if (
          features.includes(SURFACE_CLOSE_FEATURE) &&
          typeof ackHolderGeneration === "number"
        ) {
          // P0 fix: bootstrapSurfaceLifecycle's local replay can lose the
          // race against this very ack (or find no cached generation at
          // all) and has no other scheduled retry. Gated on THIS ack
          // supplying a generation (not merely negotiating the feature) so
          // an ordinary ack that repeats nothing new stays a no-op — no
          // storage touch, no request. Schedule only, never await, same
          // rule as above.
          this.scheduleCloseTombstoneReplay();
        }
        if (features.includes(INSTITUTIONAL_MATERIALIZATION_FEATURE)) {
          // Inbound frames are serialized. Reconciliation sends correlated
          // requests whose replies must traverse this same queue, so it must
          // never be awaited from the hello_ack handler. Exactly one
          // reconcile pass per handshake: this used to run again,
          // unconditionally, right after the handoff_link_v1 block below,
          // doubling every institutional_reconcile_request the daemon saw.
          void (async () => {
            await this.reconcileMaterializationTabs();
            for (const [jobID, entry] of Object.entries(
              this.store.materializations ?? {},
            )) {
              if (entry.phase !== "navigated")
                this.scheduleMaterialization(jobID, true);
            }
          })();
        } else {
          for (const [jobID, entry] of Object.entries(
            this.store.materializations ?? {},
          )) {
            if (entry.tab_id >= 0)
              await this.removeMaterializationTab(entry.tab_id);
            await this.applyMaterialization(jobID, { type: "clear" });
          }
        }
        if (features.includes(HANDOFF_LINK_FEATURE)) {
          const authJobIDs = this.store.activeJobs
            .filter((job) => job.requires_auth === true)
            .map((job) => job.job_id);
          for (const jobID of authJobIDs) this.offerURLs.delete(jobID);
          await this.update((s) => ({
            ...s,
            activeJobs: s.activeJobs.map((job) =>
              job.requires_auth === true && job.tab_id < 0
                ? { ...job, engagement_required: true, fresh_handoff: true }
                : job,
            ),
          }));
        }
        this.settleHelloWaiters(true);
        // Resume only after this serialized handler yields; a correlated
        // result must be able to traverse the inbound FIFO.
        this.deps.setTimeout(() => {
          void this.resumePageBulkCohorts();
        }, 0);
        if (features.includes(EFFECT_PERMIT_FEATURE)) {
          this.deps.setTimeout(() => {
            void this.retryTermsEffectResults();
          }, 0);
        }
        return;
      }
      case "page_acquire_ack": {
        const first = this.pageAcquireWaiters.entries().next();
        if (!first.done) {
          const [requestID, waiter] = first.value;
          this.pageAcquireWaiters.delete(requestID);
          waiter({
            ...(typeof msg.payload.job_id === "string"
              ? { job_id: msg.payload.job_id }
              : {}),
            ...(typeof msg.payload.duplicate === "boolean"
              ? { duplicate: msg.payload.duplicate }
              : {}),
            // `outcome` MUST be forwarded, not just validated: the popup's
            // third copy ("papio already has a validated artifact") is its only
            // consumer, and dropping it here made that branch dead while every
            // unit test still passed, because each side was tested alone.
            // Narrowed rather than cast: the parser already refused any other
            // value, so an unexpected one means the two sides disagree, and the
            // field is better absent than wrong.
            ...(msg.payload.outcome === "submitted" ||
            msg.payload.outcome === "already_queued" ||
            msg.payload.outcome === "already_validated"
              ? { outcome: msg.payload.outcome }
              : {}),
            ...(typeof msg.payload.error === "string"
              ? { error: msg.payload.error }
              : {}),
          });
        }
        return;
      }
      case "ack":
        await this.closeAfterAdoption(msg.job_id);
        return;
      case "error":
        console.warn("papio: daemon reported error", msg.payload);
        if (msg.payload["request_id"] !== undefined) {
          this.resolveNativeError(msg);
          return;
        }
        if (msg.payload.code === "expected_hello") this.reconnectForHello();
        if (msg.payload.code === "extension_outdated") {
          await this.update((s) => ({
            ...s,
            connectionStatus: "extension_outdated",
          }));
          await this.syncConnectionBadge("extension_outdated");
        }
        // The daemon is reachable and healthy; another browser holds its
        // offer/handoff flow. Reporting that as "daemon isn't reachable" sent
        // the researcher to `papio daemon status`, which answers "ok".
        if (msg.payload.code === "session_busy") {
          if (
            this.helloSentGeneration === this.portGeneration &&
            this.helloAckGeneration !== this.portGeneration
          ) {
            // This refusal IS the answer to our hello. Release every waiter
            // now; the port stays open and keeps polling, so a later
            // `papio browser use` still arrives as an ack on this session.
            this.helloDeniedGeneration = this.portGeneration;
            this.settleHelloWaiters(false);
          }
          await this.update((s) => ({
            ...s,
            connectionStatus: "session_elsewhere",
          }));
          await this.syncConnectionBadge("session_elsewhere");
        }
        return;
      default:
        // Extension->daemon-only types are ignored if echoed back.
        return;
    }
  }

  private async onJobOffer(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    // Every reply an offer earns — job_accept, job_reject, provider_outcome —
    // is holder-only. A pending session that answered anyway would open tabs
    // and drive a provider for a slot it does not hold.
    if (!this.holderRole()) return;
    await this.surfaceReady;
    const p = msg.payload;
    const openurl = p["openurl"];
    const hostsRaw = p["provider_hosts"];
    const expiresAt = p["expires_at"];
    // Shape is already guaranteed by parseBrowserMessage; these narrow for TS.
    if (
      typeof openurl !== "string" ||
      !Array.isArray(hostsRaw) ||
      typeof expiresAt !== "string"
    )
      return;
    const priorOfferURL = this.offerURLs.get(jobID);
    const providerHosts = hostsRaw.filter(
      (h): h is string => typeof h === "string",
    );
    const institutionOrigin = this.configuredInstitutionOrigin(
      openurl,
      providerHosts,
    );
    const providerKey = this.providerKeyForHosts(providerHosts);
    const providerParked =
      this.currentProviderDrainLease(providerKey)?.parkedReason === "challenge";
    const challengeCooldown =
      this.challengeCooldownActiveForHosts(providerHosts);
    const expected = parseExpected(p["expected"]);
    const requiresAuth =
      typeof p["requires_auth"] === "boolean" ? p["requires_auth"] : undefined;
    const offeredAccessMode =
      p["access_mode"] === "assisted" || p["access_mode"] === "delegated"
        ? p["access_mode"]
        : undefined;
    const driveAttemptID =
      typeof p["drive_attempt_id"] === "string"
        ? p["drive_attempt_id"]
        : undefined;
    const driveOrdinal =
      typeof p["drive_ordinal"] === "number" ? p["drive_ordinal"] : undefined;
    const driveStrategy =
      typeof p["drive_strategy"] === "string" ? p["drive_strategy"] : undefined;
    const driveRevision =
      typeof p["drive_revision"] === "string" ? p["drive_revision"] : undefined;
    const offeredEpoch: ProviderDriveEpoch | undefined =
      driveAttemptID !== undefined &&
      driveOrdinal !== undefined &&
      driveStrategy === "generic" &&
      driveRevision !== undefined
        ? {
            drive_attempt_id: driveAttemptID,
            ordinal: driveOrdinal,
            strategy: "generic",
            revision: driveRevision,
            attempt_count: 0,
          }
        : undefined;
    const loginEntityID = p["login_entity_id"];
    const previousLoginEntityID = this.loginEntityIDs.get(jobID);
    const loginEntityRestored =
      typeof loginEntityID === "string" &&
      loginEntityID.length > 0 &&
      previousLoginEntityID === undefined;
    if (typeof loginEntityID === "string" && loginEntityID.length > 0) {
      if (
        previousLoginEntityID === undefined ||
        previousLoginEntityID === loginEntityID
      ) {
        this.loginEntityIDs.set(jobID, loginEntityID);
      } else {
        // Job identity is immutable. Do not let inconsistent re-offer metadata
        // split one live login across two institution claims.
        console.error(
          "papio: login entity changed for a live job; retaining the original",
        );
      }
    }
    const proquestAccountID = p["proquest_account_id"];
    const previousProquestAccountID = this.proquestAccountIDs.get(jobID);
    if (typeof proquestAccountID === "string" && proquestAccountID.length > 0) {
      if (
        previousProquestAccountID === undefined ||
        previousProquestAccountID === proquestAccountID
      ) {
        this.proquestAccountIDs.set(jobID, proquestAccountID);
      } else {
        // Resolver account identity is immutable for the same live job, just
        // like the institution entity ID above.
        console.error(
          "papio: ProQuest account id changed for a live job; retaining the original",
        );
      }
    }

    // Restart/re-offer dedup normally re-accepts a live tab. A tab-less job
    // without its durable offer URL cannot represent an in-flight download:
    // discard that stale record so this offer recreates the real browser work.
    let existing = findByJob(this.store, jobID);
    if (offeredEpoch !== undefined && existing !== undefined) {
      await this.update((s) => ({
        ...s,
        activeJobs: s.activeJobs.map((entry) => {
          if (entry.job_id !== jobID) return entry;
          const state = entry as ActiveJob & GenericJobState;
          const prior = state.generic_drive_epoch;
          const sameEpoch =
            prior?.strategy === "generic" &&
            prior.drive_attempt_id === offeredEpoch.drive_attempt_id &&
            prior.ordinal === offeredEpoch.ordinal &&
            prior.revision === offeredEpoch.revision;
          const next = {
            ...entry,
            generic_drive_epoch: sameEpoch
              ? {
                  ...offeredEpoch,
                  ...prior,
                  ordinal: offeredEpoch.ordinal,
                  revision: offeredEpoch.revision,
                }
              : offeredEpoch,
          } as ActiveJob & Record<string, unknown>;
          if (!sameEpoch) {
            delete next["generic_evaluated"];
            delete next["generic_positive_attempts"];
            delete next["generic_attempted_strategies"];
            delete next["generic_terminal"];
            delete next["generic_deferred"];
          }
          if (sameEpoch && state.generic_deferred === true) {
            delete next["generic_evaluated"];
            delete next["generic_deferred"];
            delete next["generic_terminal"];
          }
          return next as ActiveJob;
        }),
      }));
      existing = findByJob(this.store, jobID);
    }
    if (
      existing !== undefined &&
      offeredAccessMode !== undefined &&
      existing.access_mode !== offeredAccessMode
    ) {
      await this.update((s) =>
        patchJob(s, jobID, { access_mode: offeredAccessMode }),
      );
      existing = findByJob(this.store, jobID);
    }
    if (
      existing !== undefined &&
      institutionOrigin !== undefined &&
      existing.institution_origin !== institutionOrigin
    ) {
      await this.update((s) =>
        patchJob(s, jobID, { institution_origin: institutionOrigin }),
      );
      existing = findByJob(this.store, jobID);
    }
    const effectiveAccessMode = offeredAccessMode ?? existing?.access_mode;

    const pendingDelivery = this.store.pendingDelivery;
    if (
      pendingDelivery?.job_id === jobID &&
      pendingDelivery.status !== "failed"
    ) {
      const now = this.deps.now();
      const expiresMs = Date.parse(expiresAt);
      const deliveryJob: ActiveJob = existing ?? {
        job_id: jobID,
        tab_id: -1,
        offered_at: now,
        expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
        status: "accepted",
        provider_hosts: providerHosts,
      };
      await this.upsertJobWithOffer(
        {
          ...deliveryJob,
          provider_hosts: providerHosts,
          ...(offeredEpoch !== undefined
            ? { generic_drive_epoch: offeredEpoch }
            : {}),
          ...(expected !== undefined ? { expected } : {}),
          ...(requiresAuth !== undefined
            ? { requires_auth: requiresAuth }
            : {}),
          ...(effectiveAccessMode !== undefined
            ? { access_mode: effectiveAccessMode }
            : {}),
        },
        openurl,
      );
      this.sendJobAccept(jobID);
      return;
    }
    const authorityMode = effectiveAccessMode;
    if (authorityMode !== "delegated") {
      const now = this.deps.now();
      const expiresMs = Date.parse(expiresAt);
      const parked: ActiveJob = {
        ...(existing ?? {
          job_id: jobID,
          tab_id: -1,
          offered_at: now,
          expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
          status: "queued",
        }),
        provider_hosts: providerHosts,
        ...(offeredEpoch !== undefined
          ? { generic_drive_epoch: offeredEpoch }
          : {}),
        ...(expected !== undefined ? { expected } : {}),
        ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
        ...(effectiveAccessMode !== undefined
          ? { access_mode: effectiveAccessMode }
          : {}),
        engagement_required: true,
      };
      if (expected === undefined) delete parked.expected;
      if (requiresAuth === undefined) delete parked.requires_auth;
      await this.upsertJobWithOffer(parked, openurl);
      this.sendJobAccept(jobID);
      return;
    }
    const freshLinks = this.supportsFreshHandoffLinks();
    const offerOrigin = this.resolverOriginHint(openurl);
    const hasReleaseEvidence = this.hasHandoffReleaseEvidence(
      offerOrigin,
      requiresAuth,
    );

    if (
      freshLinks &&
      requiresAuth === true &&
      existing !== undefined &&
      existing.tab_id >= 0
    ) {
      try {
        await this.deps.tabs.get(existing.tab_id);
      } catch {
        const candidateID =
          this.materializationCorrelation(jobID)?.candidate_id;
        if (this.institutionalAuthGateOpen() && candidateID !== undefined) {
          // Fresh-link recovery reopen: a dead sign-in tab is about to be
          // recreated for a job with a live institutional candidate —
          // consult the daemon's claim arbitration first (openFreshHandoff,
          // the sole mint chokepoint). That consult awaits a correlated
          // authentication_claim_response, which can only arrive back
          // through this same serialized inbound chain
          // (enqueueInbound/onInbound). Awaiting it here would deadlock it
          // exactly as AGENTS.md describes. Demote to tabless (the cold
          // park below takes it) and detach openFreshHandoff to run
          // off-chain; its own effect governor and mint latch make it safe
          // to race against any other drive of this job.
          await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
          existing = findByJob(this.store, jobID);
          if (existing !== undefined) {
            void this.openFreshHandoff(jobID, existing, "automatic");
          }
        } else {
          // Slice 0 containment: a dead sign-in tab is never recovered
          // autonomously — a closed gate, or no live institutional
          // candidate at all (claim_identity_known alone is never a
          // claim), both stay tabless. Demote so the cold park below
          // takes it.
          await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
          existing = findByJob(this.store, jobID);
        }
      }
    }
    if (
      freshLinks &&
      requiresAuth === true &&
      (!hasReleaseEvidence || !this.institutionalAuthGateOpen()) &&
      (existing === undefined || existing.tab_id < 0)
    ) {
      const now = this.deps.now();
      const expiresMs = Date.parse(expiresAt);
      // Slice 3 missing_claim (openFreshHandoff): the ONLY durable signal
      // that this job's institution was ever identified — a worker-local
      // Map does not survive the restart the sibling test below exercises.
      const claimIdentityKnown =
        existing?.claim_identity_known === true ||
        (typeof loginEntityID === "string" && loginEntityID.length > 0) ||
        this.materializationCorrelation(jobID)?.candidate_id !== undefined;
      const coldJob: ActiveJob = {
        ...(existing ?? {
          job_id: jobID,
          tab_id: -1,
          offered_at: now,
          expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
          status: "queued",
          provider_hosts: providerHosts,
        }),
        tab_id: -1,
        status: "queued",
        provider_hosts: providerHosts,
        ...(institutionOrigin === undefined
          ? {}
          : { institution_origin: institutionOrigin }),
        offered_at: now,
        expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
        engagement_required: true,
        fresh_handoff: true,
        ...(offeredEpoch !== undefined
          ? { generic_drive_epoch: offeredEpoch }
          : {}),
        ...(expected !== undefined ? { expected } : {}),
        ...(claimIdentityKnown ? { claim_identity_known: true } : {}),
        requires_auth: true,
        access_mode: "delegated",
      };
      if (expected === undefined) delete coldJob.expected;
      this.queuedHandoffTimers.delete(jobID);
      this.pendingForcedReleases.delete(jobID);
      await this.upsertJobWithoutOffer(coldJob);
      this.sendJobAccept(jobID);
      return;
    }
    if (freshLinks && requiresAuth === true && existing !== undefined) {
      this.offerURLs.delete(jobID);
      this.keepaliveManager?.learnResolver(openurl);
    }
    if (existing !== undefined && existing.requires_auth !== requiresAuth) {
      // A restored job can predate this field; its first re-offer must learn the
      // requirement before a fallback can recreate an expired sign-in request.
      if (requiresAuth === undefined) {
        await this.update((s) => ({
          ...s,
          activeJobs: s.activeJobs.map((job) => {
            if (job.job_id !== jobID) return job;
            const next = { ...job };
            delete next.requires_auth;
            return next;
          }),
        }));
      } else {
        await this.update((s) =>
          patchJob(s, jobID, { requires_auth: requiresAuth }),
        );
      }
    }
    if (existing) {
      if (existing.tab_id < 0) {
        if (priorOfferURL === undefined) {
          this.downloads.delete(jobID);
          await this.removeJobWithOffer(jobID);
        } else if (priorOfferURL === openurl) {
          if (providerParked || challengeCooldown) {
            await this.update((s) =>
              patchJob(s, jobID, { handoffAckPending: true }),
            );
            this.scheduleQueuedHandoffRelease(jobID);
            return;
          }
          if (existing.handoffAckPending === true) {
            if (!(await this.acknowledgePendingProviderHandoffs(providerKey)))
              return;
          } else {
            this.sendJobAccept(jobID);
          }
          if (existing.status === "queued") {
            this.scheduleQueuedHandoffRelease(jobID);
            await this.releaseQueuedHandoffs();
          }
          return;
        } else {
          this.downloads.delete(jobID);
          await this.removeJobWithOffer(jobID);
        }
      } else {
        let live = false;
        let liveTab: TabInfo | undefined;
        try {
          const tab = await this.deps.tabs.get(existing.tab_id);
          live = tab.id === existing.tab_id;
          if (live) liveTab = tab;
        } catch {
          live = false;
        }
        if (!live) {
          if (requiresAuth === true && !this.institutionalAuthGateOpen()) {
            // Slice 0 containment: never autonomously recreate a sign-in
            // surface for a job whose tab is gone. Park for explicit
            // engagement; a legacy offer keeps its URL so the operator's
            // open can use it (the fresh-link variant of this state was
            // already demoted to the tabless cold park above).
            await this.upsertJobWithOffer(
              {
                ...existing,
                tab_id: -1,
                status: "queued",
                offered_at: this.deps.now(),
                provider_hosts: providerHosts,
                engagement_required: true,
                parked_with_tab: false,
              },
              openurl,
            );
            this.sendJobAccept(jobID);
            return;
          }
          const recoveredTabID = await this.openManagedTab({
            url: openurl,
            jobId: jobID,
            purpose: "reoffer",
            focusExisting: false,
          });
          if (recoveredTabID !== undefined) {
            live = true;
            liveTab = { id: recoveredTabID, url: openurl };
            existing = findByJob(this.store, jobID) ?? {
              ...existing,
              tab_id: recoveredTabID,
            };
          }
        }
        if (
          live &&
          ((freshLinks && requiresAuth === true) ||
            priorOfferURL === undefined ||
            priorOfferURL === openurl)
        ) {
          if (providerParked || challengeCooldown) {
            await this.update((s) =>
              patchJob(s, jobID, { handoffAckPending: true }),
            );
            return;
          }
          if (
            existing.parked_with_tab !== true &&
            !this.handoffDrives.has(jobID)
          ) {
            if (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT) {
              this.enqueueHandoffDrive({
                jobID,
                purpose: "reoffer",
                focusExisting: false,
              });
            } else {
              this.registerHandoffDrive(jobID, existing.tab_id);
            }
          }
          if (existing.handoffAckPending === true) {
            await this.acknowledgePendingProviderHandoffs(providerKey);
          } else {
            this.sendJobAccept(jobID);
          }
          if (
            freshLinks &&
            requiresAuth === true &&
            loginEntityRestored &&
            liveTab !== undefined
          ) {
            // A restarted worker may have missed this tab's completed landing
            // and lost the worker-local entity ID. The first re-offer restores
            // that metadata; assess the authoritative current Chrome snapshot
            // once instead of waiting for a navigation event that already ran.
            await this.onTabUpdated(
              existing.tab_id,
              { status: "complete" },
              liveTab,
            );
          }
          return;
        }
        if (live && !providerParked && !challengeCooldown) {
          if (this.authAttemptsFor(jobID) >= MAX_AUTH_ATTEMPTS) {
            this.rememberStalledAuthHandoff(jobID, {
              url: openurl,
              providerHosts,
              ...(expected !== undefined ? { expected } : {}),
              ...(requiresAuth !== undefined ? { requiresAuth } : {}),
              ...(effectiveAccessMode !== undefined
                ? { accessMode: effectiveAccessMode }
                : {}),
            });
            await this.reportAuthStalled(jobID);
            return;
          }
          if (
            this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT &&
            !this.handoffDrives.has(jobID)
          ) {
            await this.upsertJobWithOffer(
              {
                ...existing,
                offered_at: this.deps.now(),
                status: "accepted",
                provider_hosts: providerHosts,
                // ...existing may still carry a stale parked_with_tab: true left
                // over from a prior timeout park. enqueueHandoffDrive just below
                // also clears it, but only after this write already lands — so
                // writing the stale value here first would still let a worker
                // restart between these two calls see a live status plus the
                // marker and skip re-registering the job forever.
                parked_with_tab: false,
              },
              openurl,
            );
            this.enqueueHandoffDrive({
              jobID,
              purpose: "reoffer",
              focusExisting: false,
            });
            if (existing.handoffAckPending === true) {
              await this.acknowledgePendingProviderHandoffs(providerKey);
            } else {
              this.sendJobAccept(jobID);
            }
            return;
          }
          const effectToken = this.claimEffectGovernor(jobID);
          if (effectToken === undefined) {
            await this.upsertJobWithOffer(
              {
                ...existing,
                status: "accepted",
                offered_at: this.deps.now(),
                provider_hosts: providerHosts,
                parked_with_tab: false,
              },
              openurl,
            );
            this.enqueueHandoffDrive({
              jobID,
              purpose: "reoffer",
              focusExisting: false,
            });
            this.sendJobAccept(jobID);
            await this.drainHandoffDriveQueue();
            return;
          }
          let tabID: number | undefined;
          try {
            tabID = await this.openManagedTab({
              url: openurl,
              jobId: jobID,
              purpose: "reoffer",
            });
          } catch (error) {
            console.error("papio: re-offer tab creation failed", error);
          } finally {
            this.releaseEffectGovernor(jobID, effectToken, false);
          }
          if (tabID === undefined) {
            this.wakeEffectGovernor();
            await this.parkUndrivableHandoff(
              jobID,
              "re-offer tab creation failed",
            );
            return;
          }
          this.beginProviderDrive(jobID);
          const expiresMs = Date.parse(expiresAt);
          const refreshed: ActiveJob = {
            ...existing,
            tab_id: tabID,
            offered_at: this.deps.now(),
            expires_at: Number.isNaN(expiresMs) ? this.deps.now() : expiresMs,
            status: "accepted",
            provider_hosts: providerHosts,
            ...(expected !== undefined ? { expected } : {}),
            ...(requiresAuth !== undefined
              ? { requires_auth: requiresAuth }
              : {}),
          };
          if (expected === undefined) delete refreshed.expected;
          if (requiresAuth === undefined) delete refreshed.requires_auth;
          delete refreshed.challenge_blocked;
          delete refreshed.challenge_host;
          delete refreshed.challenge_kind;
          delete refreshed.challenge_blocked_at;
          if (existing.handoffAckPending !== true)
            delete refreshed.handoffAckPending;
          await this.upsertJobWithOffer(refreshed, openurl);
          this.registerHandoffDrive(jobID, tabID);
          if (existing.handoffAckPending === true) {
            await this.acknowledgePendingProviderHandoffs(providerKey);
          } else {
            this.sendJobAccept(jobID);
          }
          this.wakeEffectGovernor();
          return;
        }
        await this.removeJobWithOffer(jobID);
      }
    }

    if (this.authAttemptsFor(jobID) >= MAX_AUTH_ATTEMPTS) {
      // This browser session has driven the job through human authentication
      // MAX_AUTH_ATTEMPTS times without a download: the warm session cannot
      // complete it. Report the human step (once) and decline to open another
      // broker tab. No job_reject — that is terminal; the job stays parked and
      // is re-offered on a future launch with a fresh budget.
      this.rememberStalledAuthHandoff(jobID, {
        url: openurl,
        providerHosts,
        ...(effectiveAccessMode !== undefined
          ? { accessMode: effectiveAccessMode }
          : {}),
        ...(expected !== undefined ? { expected } : {}),
        ...(requiresAuth !== undefined ? { requiresAuth } : {}),
      });
      await this.reportAuthStalled(jobID);
      return;
    }

    const now = this.deps.now();
    const expiresMs = Date.parse(expiresAt);
    const makeJob = (
      tabID: number,
      status: ActiveJob["status"] = "accepted",
    ): ActiveJob => ({
      job_id: jobID,
      tab_id: tabID,
      offered_at: now,
      expires_at: Number.isNaN(expiresMs) ? now : expiresMs,
      status,
      provider_hosts: providerHosts,
      ...(offeredEpoch !== undefined
        ? { generic_drive_epoch: offeredEpoch }
        : {}),
      ...(expected !== undefined ? { expected } : {}),
      ...(requiresAuth !== undefined ? { requires_auth: requiresAuth } : {}),
      ...(effectiveAccessMode !== undefined
        ? { access_mode: effectiveAccessMode }
        : {}),
    });

    if (requiresAuth === true && !this.institutionalAuthGateOpen()) {
      // Slice 0 containment (dev/active/surface-lifecycle-plan.md): no
      // autonomous sign-in surface without the daemon-side authentication
      // claim feature and a live network. Only legacy (non-fresh-link)
      // offers reach this tail with requires_auth — the fresh-link variants
      // were parked tabless above — so the offer URL is retained for the
      // operator's explicit open. No release timer: the 45s fallback may
      // not bypass this gate.
      await this.upsertJobWithOffer(
        { ...makeJob(-1, "queued"), engagement_required: true },
        openurl,
      );
      this.sendJobAccept(jobID);
      return;
    }

    const governorQueued =
      !providerParked &&
      !challengeCooldown &&
      hasReleaseEvidence &&
      (this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT ||
        this.handoffDriveQueue.length > 0);
    const queueHandoff =
      providerParked ||
      challengeCooldown ||
      governorQueued ||
      (!hasReleaseEvidence &&
        (requiresAuth === true ||
          this.handoffOpening ||
          this.store.activeJobs.some(
            (job) => job.tab_id >= 0 && job.status !== "queued",
          )));
    if (queueHandoff) {
      const queued = makeJob(-1, governorQueued ? "accepted" : "queued");
      await this.upsertJobWithOffer(
        providerParked || challengeCooldown
          ? { ...queued, handoffAckPending: true }
          : queued,
        openurl,
      );
      if (governorQueued) {
        this.enqueueHandoffDrive({ jobID, purpose: "handoff" });
        this.sendJobAccept(jobID);
        await this.drainHandoffDriveQueue();
      } else {
        this.scheduleQueuedHandoffRelease(jobID);
        if (!providerParked && !challengeCooldown)
          this.sendJobAccept(jobID);
      }
      return;
    }

    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      // This explicit offer is eligible, but an unlike effect currently owns
      // the browser permit. Keep the URL only in the existing offer ledger and
      // let the normal handoff queue materialize it after release.
      const queued = makeJob(-1, "accepted");
      await this.upsertJobWithOffer(queued, openurl);
      this.enqueueHandoffDrive({ jobID, purpose: "handoff" });
      this.sendJobAccept(jobID);
      await this.drainHandoffDriveQueue();
      return;
    }
    this.handoffOpening = true;
    let tabID: number | undefined;
    try {
      tabID = await this.openManagedTab({
        url: openurl,
        jobId: jobID,
        purpose: "handoff",
      });
    } catch (e) {
      console.error("papio: tab creation failed; rejecting job", e);
    } finally {
      this.handoffOpening = false;
      this.releaseEffectGovernor(jobID, effectToken, false);
    }
    if (tabID === undefined) {
      this.wakeEffectGovernor();
      await this.parkUndrivableHandoff(jobID, "tab creation failed");
      return;
    }
    this.beginProviderDrive(jobID);
    await this.upsertJobWithOffer(makeJob(tabID), openurl);
    this.registerHandoffDrive(jobID, tabID);
    this.sendJobAccept(jobID);
    this.wakeEffectGovernor();
  }
  private async failDelivery(
    jobID: string,
    downloadID: number,
    reason: string,
  ): Promise<void> {
    await this.discardDownload(jobID, downloadID);
    this.deliveryJobs.delete(jobID);
    await this.update((s) =>
      updatePendingDelivery(
        patchJob(s, jobID, { download_initiated: false }),
        jobID,
        { status: "failed", error: reason },
      ),
    );
    this.send("error", { code: "download_not_pdf", message: reason }, jobID);
  }

  /** Erase a download we refuse to adopt: tracking, file, and history entry. */
  private async discardDownload(
    jobID: string,
    downloadID: number,
  ): Promise<void> {
    this.downloads.delete(jobID);
    try {
      await this.deps.downloads.removeFile(downloadID);
    } catch {
      // Interrupted downloads may not have produced a removable file.
    }
    try {
      await this.deps.downloads.erase({ id: downloadID });
    } catch {
      // Clearing history is best-effort; opening the human-visible fallback is not.
    }
  }
  private async clearDirectDownloadState(
    jobID: string,
    epoch: ProviderDriveEpoch,
  ): Promise<void> {
    const { in_flight_download_id: _inFlightDownloadID, ...withoutDownload } =
      epoch;
    await this.update((s) => ({
      ...s,
      activeJobs: s.activeJobs.map((entry) => {
        if (entry.job_id !== jobID) return entry;
        const { direct_envelope: _directEnvelope, ...withoutEnvelope } = entry;
        return {
          ...withoutEnvelope,
          download_initiated: false,
          drive_epoch: withoutDownload,
          direct_terminal: true,
        };
      }),
    }));
  }

  private async onCancel(msg: BrowserMessage): Promise<void> {
    const jobID = msg.job_id;
    if (jobID === undefined) return;
    this.downloads.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    const job = findByJob(this.store, jobID);
    if (!job) {
      // A daemon/worker restart can lose browser-local activeJobs before the
      // durable terminal-claim poll emits cancel. The URL-free birth ledger
      // still names every same-epoch surface by job and binding; that is
      // enough to ASK the daemon, never enough to close on its own. Launch
      // off-chain for the same inbound-FIFO reason as removeJobWithOffer.
      const ledger = await this.snapshotTabLedger();
      for (const [key, entry] of Object.entries(ledger)) {
        const tabID = Number(key);
        if (
          !Number.isInteger(tabID) ||
          tabID < 0 ||
          entry.job_id !== jobID ||
          entry.ceded === true ||
          entry.browser_epoch !== this.browserEpoch
        )
          continue;
        void this.closeOwnedSurface(tabID, "job_inactive");
      }
      return;
    }
    await this.removeJobWithOffer(jobID);
  }

  /** The daemon acknowledges download_complete only after it has attempted
   * adoption. Close the broker-owned viewer then, never on a raw tab event. */
  private async closeAfterAdoption(jobID: string | undefined): Promise<void> {
    if (jobID === undefined) return;
    const isDelivery =
      this.deliveryJobs.has(jobID) ||
      this.store.pendingDelivery?.job_id === jobID;
    if (isDelivery) {
      this.completedDownloadTabs.delete(jobID);
      this.deliveryJobs.delete(jobID);
      this.lastDeliveryState = {
        job_id: jobID,
        state: "adopted",
        message: "papio adopted v (validating)",
        at: this.deps.now(),
      };
      await this.update((s) => clearPendingDelivery(s, jobID));
      await this.removeJobWithOffer(jobID);
      return;
    }
    const materialization = this.materializationCorrelation(jobID);
    if (materialization?.phase === "navigated") {
      // A successfully-delivered institutional materialize.html scaffold has
      // no other retirement path — the close authorization transaction is
      // the only thing that ever tells the daemon (and then this browser)
      // to tear it down. Detach the job from its tab first (closeOwnedTab's
      // own safety guard, mirrored at the navigation_error/scaffold_idle
      // close sites above, refuses to remove a tab any job still tracks —
      // and reduceMaterialization's "scaffolded"/"reconcile_tab" tabSync
      // mirrors the scaffold's tab id onto job.tab_id) so the close
      // transaction can actually run the removal, not just tombstone it.
      const settledTabID = materialization.tab_id;
      const gateOccurrenceID = this.claimGrants.get(jobID)?.gateOccurrenceID;
      void (async () => {
        const current = findByJob(this.store, jobID);
        if (current !== undefined && current.tab_id === settledTabID) {
          await this.update((s) => patchJob(s, jobID, { tab_id: -1 }));
        }
        await this.closeOwnedSurface(
          settledTabID,
          "materialization_settled",
          gateOccurrenceID,
        );
      })();
    }
    const tabID =
      this.adoptedViewerTabs.get(jobID) ??
      this.completedDownloadTabs.get(jobID);
    if (tabID === undefined) return;
    this.adoptedViewerTabs.delete(jobID);
    this.completedDownloadTabs.delete(jobID);
    await this.removeJobWithOffer(jobID);
    // The viewer holding the just-adopted paper IS the confirmation surface,
    // so it is retained on purpose - and it is marked as retained content
    // here, at the one moment papio positively knows which paper it shows.
    // The previous `closeOwnedTab(tabID, "adopted-viewer")` was dead code:
    // the primitive refused that reason unconditionally, so it read as
    // cleanup while doing nothing, and the record kept no content marker for
    // a later duplicate to supersede. `job_id` is supplied because
    // removeJobWithOffer above has already dropped the live job, so a record
    // minted here would otherwise carry no paper identity at all.
    await this.ledgerManagedTab(tabID, "adopted-viewer", false, jobID);
    await this.retainContentSurface(tabID, undefined);
  }

  /** Run the bounded DOM probe for a tracked page and preserve the existing
   * challenge-blocked contract. The boolean argument is origin evidence for
   * OpenAthens-only body markers; page text never leaves the injected function. */
  private async assessTrackedDrivenPage(
    job: ActiveJob,
    host: string,
    url: string,
  ): Promise<boolean> {
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: assessDrivenPage,
        args: [null, host === OPENATHENS_LOGIN_HOST],
      });
      const assessment = results[0]?.result as DrivenPageAssessment | undefined;
      if (
        assessment?.kind === "challenge" ||
        assessment?.kind === "redirect_loop"
      ) {
        await this.confirmThenBlockChallenge(
          job,
          host,
          assessment.kind === "challenge" ? "cloudflare" : "redirect_loop",
          url,
        );
        // The handoff still stops on this pass: papio has read a check on the
        // page, and a self-resolving one resumes on the next tab event.
        return true;
      }
      if (assessment?.kind === "normal" && job.challenge_blocked === true) {
        return !(await this.clearChallengeBlock(job));
      }
    } catch (e) {
      console.error(
        "papio: driven-page assessment failed; continuing handoff",
        e,
      );
    }
    return false;
  }

  /** Chrome may publish the terminal OpenAthens title before React/body text.
   * Give each document epoch exactly one late probe; it never navigates or
   * retries the page and retains the terminal tab when the marker appears. */
  private scheduleOpenAthensErrorRecheck(job: ActiveJob, epoch: number): void {
    if (this.openAthensErrorRecheckEpochs.get(job.job_id) === epoch) return;
    this.openAthensErrorRecheckEpochs.set(job.job_id, epoch);
    this.deps.setTimeout(async () => {
      await this.ready;
      if (this.openAthensErrorRecheckEpochs.get(job.job_id) !== epoch) return;
      if ((this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== epoch) return;
      const current = findByJob(this.store, job.job_id);
      if (
        current === undefined ||
        current.tab_id !== job.tab_id ||
        current.challenge_blocked === true
      )
        return;
      let tab: TabInfo;
      try {
        tab = await this.deps.tabs.get(current.tab_id);
      } catch {
        return;
      }
      if (tab.url === undefined || tab.title !== OPENATHENS_ERROR_TITLE) return;
      let host: string;
      try {
        host = new URL(tab.url).hostname.toLowerCase();
      } catch {
        return;
      }
      if (host !== OPENATHENS_LOGIN_HOST) return;
      await this.assessTrackedDrivenPage(current, host, tab.url);
    }, OPENATHENS_ERROR_RECHECK_MS);
  }

  private async onTabUpdated(
    tabID: number,
    change: TabChangeInfo,
    tab: TabInfo,
  ): Promise<void> {
    const pageCaptureWaiter = this.pageCaptureLoadWaiters.get(tabID);
    if (pageCaptureWaiter !== undefined && change.status === "complete") {
      pageCaptureWaiter(true);
    }
    // A tracked handoff tab landing on the resolver is the same evidence as
    // an untracked one — the manager parses to an origin and discards the
    // raw URL itself, so this runs unconditionally before the tracked-job
    // gate below (and before an in-flight injection could commit a result
    // against a document epoch this navigation just invalidated). SPA/history
    // navigation on the tracked path below can land without a "complete"
    // status, which is why "loading" is included here too.
    if (
      change.url !== undefined ||
      change.status === "complete" ||
      change.status === "loading"
    ) {
      this.keepaliveManager?.noteResolverNavigation(
        tabID,
        change.url ?? tab.url,
      );
    }
    await this.ready;
    const job = findByTab(this.store, tabID);
    if (!job) {
      if (change.status === "complete") {
        const origin = this.institutionalLandingOrigin(change.url ?? tab.url);
        if (origin !== undefined) {
          await this.keepaliveManager?.noteInstitutionalLanding(origin);
        }
      }
      // A provider "download" that opens the PDF in a NEW viewer tab (e.g. JSTOR
      // navigates to /stable/pdf/<id>.pdf) is untracked here. Adopt it for the
      // tracked handoff tab that spawned it so the PDF still flows to the daemon.
      if (change.status === "complete")
        await this.maybeAdoptViewerTab(
          tabID,
          change.url ?? tab.url,
          tab.openerTabId,
        );
      // After a worker restart this tab has no job mirror, so only the durable
      // wall-count cache can tell us that this navigation may clear a badge
      // the operator is looking at.
      if (
        this.lastBadgedAuthWallTabs.has(tabID) &&
        (change.url !== undefined || change.status === "complete") &&
        !isAuthenticationURL(change.url ?? tab.url ?? "")
      ) {
        await this.syncConnectionBadge();
      }
      return;
    }
    const staleRecoveryNavigationInFlight =
      change.status === "loading" &&
      this.staleRecoveryInFlightEpochs.has(job.job_id);
    if (staleRecoveryNavigationInFlight) return;
    const url = change.url ?? tab.url;
    if (url === undefined) return;
    if (change.status === "loading") this.advanceStaleRecoveryEpoch(job.job_id);
    const staleRecoveryEpoch = this.staleRecoveryEpochs.get(job.job_id) ?? 0;
    let host: string;
    try {
      host = new URL(url).hostname;
    } catch {
      return;
    }
    // A title-only update counts: the the default institution Shibboleth stale page is classifiable
    // ONLY by its title ("… Login Service - Stale Request"); its URL is byte-for-byte
    // the URL of the working login form. Chrome can deliver that title after the
    // `complete` event, and detection used to run on `complete` alone — so the one
    // page papio most needs to recognize was the one it could silently miss.
    if (change.status === "complete" || change.title !== undefined) {
      const failure = detectAuthFailure(url, tab.title);
      if (failure !== undefined) {
        // Surface every recognized IdP failure. Only a terminal stale-request
        // signature is safe to navigate away from; password recovery and retry
        // forms must remain where the human can use them.
        const surfacedEpoch = this.staleRecoverySurfacedEpochs.get(job.job_id);
        if (surfacedEpoch !== staleRecoveryEpoch) {
          this.staleRecoverySurfacedEpochs.set(job.job_id, staleRecoveryEpoch);
          this.authFailureSurfaced.add(job.job_id);
          await this.surfaceWorkTab(job.tab_id);
        }
        // Mark only after a successful send: a dropped native port must not
        // permanently swallow the one report this job gets for this outcome.
        if (
          !this.handoffOutcomeSent.has(`${job.job_id}:${failure}`) &&
          this.send(
            "handoff_outcome",
            { outcome: failure, final_host: host },
            job.job_id,
          )
        ) {
          this.handoffOutcomeSent.add(`${job.job_id}:${failure}`);
        }
        if (
          failure === "stale_sso" &&
          /\bstale\s+request\b/i.test(tab.title ?? "") &&
          (await this.redriveStaleHandoff(job, staleRecoveryEpoch))
        ) {
          return;
        }
      }
    }
    if (
      host === OPENATHENS_LOGIN_HOST &&
      change.title === OPENATHENS_ERROR_TITLE
    ) {
      this.scheduleOpenAthensErrorRecheck(job, staleRecoveryEpoch);
    }
    const adapter = this.deps.adapterSpecs.find((candidate) =>
      hostMatches(host, candidate.hosts),
    );
    // The registry is source-controlled and may cover hosts omitted from the
    // capped offer list. Persist its identity before any permission-dependent
    // classification so later native download events can safely correlate it.
    if (adapter !== undefined && job.adapter_id !== adapter.id) {
      await this.update((s) =>
        patchJob(s, job.job_id, { adapter_id: adapter.id }),
      );
    }
    const successfulLanding =
      change.status === "complete" && !isAuthenticationURL(url);
    // Navigation-error precedence (surface-lifecycle-plan.md Slice 1): a
    // failed top-frame load lands here as an unsuccessful document, same as
    // a genuine auth wall. Consult the marker before challenge assessment or
    // auth-wall detection ever runs for it — a dead end must not charge an
    // auth attempt, enter challenge cooldown, or send auth_pending. The job
    // is left untouched; a daemon-side disposition is Slice 3's addition.
    const documentSettled =
      change.status === "complete" || change.title !== undefined;
    if (
      documentSettled &&
      !successfulLanding &&
      this.navigationErrors.delete(tabID)
    ) {
      // §2.2.1 navigation_error (Slice 3): a daemon-committed park with no
      // auth charge, no cooldown — this early return already leaves the
      // job untouched and charges nothing; only the observation is new.
      this.clearNavigationErrorMarker(tabID);
      void this.emitClaimObservation(job.job_id, tabID, "navigation_error");
      return;
    }
    if (successfulLanding) {
      this.navigationErrors.delete(tabID);
      this.clearNavigationErrorMarker(tabID);
      const institutionalSession = await this.recordInstitutionalSession(
        job,
        url,
        this.deps.now(),
      );
      if (!institutionalSession) await this.recordOpenAccessLanding(job);
    } else if (documentSettled && isAuthenticationURL(url)) {
      // §2.2.1 wall_observed (Slice 3): the tracked landing-on-the-wall
      // path. Latched so a settled tab sitting still never re-fires it.
      void this.emitClaimObservation(job.job_id, tabID, "wall_observed", true);
    } else if (
      change.status === "loading" &&
      isAuthenticationURL(url) &&
      this.hasLatchedObservation(job.job_id, tabID, "wall_observed")
    ) {
      // §2.2.1 login_started (Slice 3): no real form-submit signal exists
      // in the browser. A further navigation while still on the auth wall,
      // after the wall was already observed once, is the closest available
      // proxy for "the human began interacting" (the wall→post-wall URL
      // transition the design falls back to).
      void this.emitClaimObservation(job.job_id, tabID, "login_started", true);
    }
    if (
      change.status === "complete" &&
      (await this.maybeRouteResolver(job, url))
    )
      return;
    // The offer's provider_hosts list is capped by the protocol (20 entries);
    // the adapter registry is the authoritative host source for classification,
    // so a tracked handoff landing on any registered family is on-provider.
    const onProvider =
      hostMatches(host, job.provider_hosts) || adapter !== undefined;
    if (onProvider) {
      // A completed provider landing after papio's federated route is the
      // concrete sign-in evidence for this drive. Preserve it for the auth
      // return reducer.
      if (successfulLanding && this.federatedLoginRouted.has(job.job_id)) {
        this.federatedLoginOperatorNavigated.add(job.job_id);
      }
    }
    // Back on the provider means this tab left the IdP (successfully or not);
    // either way it can no longer be the live sibling any waiting job is
    // deferring to for this origin.
    const shouldAssessBeforeRouting =
      (change.status === "complete" || change.title !== undefined) &&
      (onProvider || isAuthenticationURL(url));
    const routeWasSettled = this.federatedLoginRouteSettled.has(job.job_id);
    const routedAuthNavigation =
      !onProvider &&
      this.consumeFederatedLoginRouteEvent(job.job_id, url, change);
    if (
      !onProvider &&
      isAuthenticationURL(url) &&
      change.status === "loading" &&
      !routedAuthNavigation &&
      (!this.federatedLoginRouted.has(job.job_id) || routeWasSettled)
    ) {
      this.federatedLoginOperatorNavigated.add(job.job_id);
    }
    if (
      shouldAssessBeforeRouting &&
      (change.title !== undefined || !onProvider) &&
      (await this.assessTrackedDrivenPage(job, host, url))
    ) {
      return;
    }
    if (!onProvider) {
      // The extension's own federated route is navigation mechanics, not proof
      // that the operator reached or completed sign-in.
      if (routedAuthNavigation) return;
      // Reuse the durable resolver-provided offer URL that produced that viewer.
      if (change.status === "complete" && host === CHROME_PDF_VIEWER_HOST) {
        const offeredURL = this.offerURLs.get(job.job_id);
        if (offeredURL !== undefined) {
          await this.maybeDownloadPDFViewer(job.job_id, offeredURL, true);
          return;
        }
      }
      // A direct PDF can legitimately land on a CDN outside the offer's
      // provider-host list. Its URL alone is sufficient to preserve the
      // browser download flow without treating that redirect as an IdP hop.
      if (change.status === "complete" && this.isPDFNavigationURL(url)) {
        await this.maybeDownloadPDFViewer(job.job_id, url);
        return;
      }
      // A stable non-authentication landing outside the capped offer list is
      // still the resolver's provider result. Give it the same bounded
      // no-adapter evidence window instead of leaving a permanent spinner.
      if (successfulLanding) {
        await this.maybeClassify(job.job_id, host);
        return;
      }
      if (job.status !== "auth_pending" && !successfulLanding) {
        // Leaving every provider host for an IdP starts human authentication.
        // A completed non-IdP page is instead a usable resolver landing.
        await this.update((s) =>
          patchJob(s, job.job_id, {
            status: "auth_pending",
            auth_started_ms: this.deps.now(),
          }),
        );
        this.send("auth_pending", {}, job.job_id);
        await this.noteAuthAttempt(job.job_id, tabID);
        await this.surfaceWorkTab(tabID);
      }
      return;
    }
    let currentFederatedClassification = false;
    if (
      this.federatedLoginRouted.has(job.job_id) &&
      job.status === "auth_pending" &&
      (successfulLanding || change.url !== undefined) &&
      !this.federatedLoginOperatorNavigated.has(job.job_id)
    ) {
      const verdict = await this.maybeClassify(
        job.job_id,
        host,
        "evidence_only",
      );
      if (verdict === undefined) {
        const latest = findByJob(this.store, job.job_id);
        if (
          latest?.status === "auth_pending" &&
          latest.challenge_blocked !== true
        ) {
          this.scheduleClassifyRetry(job.job_id, "federated_evidence");
        }
        return;
      }
      if (verdict.kind === "login") return;
      if (verdict.kind === "unknown") {
        this.scheduleClassifyRetry(job.job_id, "federated_evidence");
        return;
      }
      currentFederatedClassification = true;
    }
    // `job` is a snapshot taken many awaits ago, so "status is auth_pending"
    // can already be false by the time this runs — a sibling tab-update for
    // this same navigation (loading/title/complete all reach here) may have
    // finalized the return first. finalizeAuthReturn re-reads and DECLINES in
    // that case, and this branch used to `return` regardless: the completion
    // event that would have classified the article was swallowed by a stale
    // read of a job another handler had already advanced. Fall through
    // instead. Every false it returns is a state where classifying the page
    // this tab is standing on is the right next act — either another handler
    // owns the return (classify is idempotent and bails on
    // download_initiated), or a redrive was blocked and its retry is already
    // scheduled.
    if (
      job.status === "auth_pending" &&
      (await this.finalizeAuthReturn(
        job.job_id,
        tabID,
        url,
        host,
        currentFederatedClassification,
      ))
    ) {
      return;
    }

    // Once the provider page has finished loading on the tracked tab (past any
    // human auth), run the declarative adapter — permission-gated, tracked-tab
    // only. Re-reads fresh job state; a stale local `job` here is fine.
    if (change.status === "complete") {
      await this.maybeDownloadPDFViewer(job.job_id, url);
      await this.maybeClassify(job.job_id, host);
    }
  }
  private scheduleResolverRedriveRetry(jobID: string, tabID: number): void {
    if (this.resolverRedriveRetryTimers.has(jobID)) return;
    const marker = {};
    this.resolverRedriveRetryTimers.set(jobID, marker);
    this.deps.setTimeout(async () => {
      if (this.resolverRedriveRetryTimers.get(jobID) !== marker) return;
      this.resolverRedriveRetryTimers.delete(jobID);
      const job = findByJob(this.store, jobID as string);
      // A spent redrive must not fire a second one (`federatedReDriven` is the
      // "still-walled page doesn't loop" marker). Fall into the classify branch
      // below instead: by then the tab is standing on whatever the redrive
      // produced, and classifying it where it stands is the whole point of
      // this net.
      const openurl = this.federatedReDriven.has(jobID)
        ? undefined
        : this.offerURLs.get(jobID);
      if (job === undefined || job.status !== "awaiting_download") return;
      if (openurl === undefined) {
        try {
          const tab = await this.deps.tabs.get(tabID);
          const currentURL =
            tab.url === undefined ? undefined : new URL(tab.url);
          if (currentURL !== undefined)
            await this.maybeClassify(jobID, currentURL.hostname);
        } catch {
          // A vanished tab is handled by the normal removal path.
        }
        return;
      }
      const effectToken = this.claimEffectGovernor(jobID);
      if (effectToken === undefined) {
        this.scheduleResolverRedriveRetry(jobID, tabID);
        return;
      }
      try {
        const current = findByJob(this.store, jobID);
        if (current?.tab_id === tabID && this.deps.tabs.update !== undefined) {
          await this.deps.tabs.update(tabID, { url: openurl });
        } else {
          const opened = await this.openManagedTab({
            url: openurl,
            jobId: jobID,
            purpose: "redrive",
            focusExisting: false,
          });
          if (opened === undefined) {
            this.scheduleResolverRedriveRetry(jobID, tabID);
            return;
          }
        }
        this.federatedLoginOperatorNavigated.delete(jobID);
        this.federatedReDriven.add(jobID);
      } catch {
        this.scheduleResolverRedriveRetry(jobID, tabID);
      } finally {
        this.releaseEffectGovernor(jobID, effectToken);
      }
    }, CLASSIFY_RETRY_MS);
  }

  private async finalizeAuthReturn(
    jobID: string,
    tabID: number,
    url: string,
    host: string,
    currentFederatedClassification: boolean,
  ): Promise<boolean> {
    const job = findByJob(this.store, jobID);
    if (!job || job.status !== "auth_pending" || job.tab_id !== tabID)
      return false;
    this.classifyRetries.delete(jobID);
    const started = job.auth_started_ms ?? this.deps.now();
    const now = this.deps.now();
    const elapsed = Math.max(0, now - started);
    this.deliverySessionEvidence.set(jobID, "fresh_auth");
    await this.update((s) =>
      patchJob(s, jobID, {
        status: "awaiting_download",
        parked_with_tab: false,
      }),
    );
    this.send("auth_returned", { elapsed_ms: elapsed }, jobID);
    const authReturnedOriginHint = this.jobInstitutionOrigin(job);
    this.emitSessionEvidence("auth_returned", authReturnedOriginHint);
    await this.recollapseHandoffGroup(tabID);
    const institutionalSession = await this.recordInstitutionalSession(
      job,
      url,
      now,
    );
    if (!institutionalSession) await this.recordOpenAccessLanding(job);
    const completedFederatedSignIn =
      institutionalSession ||
      this.federatedLoginOperatorNavigated.has(jobID) ||
      currentFederatedClassification;
    const openurl = this.offerURLs.get(jobID);
    if (
      openurl !== undefined &&
      this.hasDelegatedAuthority(findByJob(this.store, jobID)) &&
      this.federatedLoginRouted.has(jobID) &&
      completedFederatedSignIn &&
      !this.federatedReDriven.has(jobID)
    ) {
      const currentJob = findByJob(this.store, jobID);
      const effectToken = this.claimEffectGovernor(jobID);
      if (effectToken === undefined) {
        // Session evidence is durable; retry this exact resolver redrive
        // against the still-held one-use offer URL, not generic UI classify.
        this.scheduleResolverRedriveRetry(jobID, tabID);
        return false;
      }
      try {
        if (
          openurl !== undefined &&
          currentJob?.tab_id === tabID &&
          this.deps.tabs.update !== undefined
        ) {
          // The successful sign-in is still driving this exact tab. Reuse it
          // for the resolver return instead of creating a second effectful tab.
          //
          // Only navigate if that would actually move the tab. chrome.tabs.update
          // with a url ALWAYS navigates, even to the identical one, so a resolver
          // URL that resolves back to the page this tab already shows costs a
          // discarded render for nothing — and on a Cloudflare-fronted provider
          // it re-arms the check against a fresh automated navigation. Measured
          // live 2026-08-22 on job_012f55be2bbfe0abd0ce456e36: route issued,
          // `auth_returned` 311 ms later, `challenge_blocked` immediately after,
          // on a page whose own assessment scored `normal` on two captures. The
          // human then had to solve a check that this navigation had summoned.
          const currentURL = (await this.deps.tabs
            .get(tabID)
            .catch(() => undefined))?.url;
          if (
            currentURL === undefined ||
            normalizeManagedTabURL(currentURL) !== normalizeManagedTabURL(openurl)
          ) {
            await this.deps.tabs.update(tabID, { url: openurl });
          }
          this.federatedLoginOperatorNavigated.delete(jobID);
          this.federatedReDriven.add(jobID);
          // A redrive is a BET that the navigation it starts will come back as
          // a tab update and classify there. The bet is lost silently whenever
          // it does not: a resolver URL that redirects straight back to the
          // page this tab already shows produces no state change for Chrome to
          // report, and this branch has already consumed the one landing event
          // that would otherwise have classified. Measured live 2026-08-22 on
          // job_012f55be2bbfe0abd0ce456e36: `auth_returned`, then three minutes
          // of silence on a fully-rendered open-access article with its PDF
          // link on screen, then the drive timeout — and across 48 recorded
          // institutional permits, never one download effect. So net it: the
          // marker above makes this retry classify the tab where it stands
          // rather than redrive again.
          this.scheduleResolverRedriveRetry(jobID, tabID);
          return true;
        }
        if (openurl !== undefined) {
          const opened = await this.openManagedTab({
            url: openurl,
            jobId: jobID,
            purpose: "redrive",
            focusExisting: false,
          });
          if (opened === undefined) {
            this.scheduleResolverRedriveRetry(jobID, tabID);
            return false;
          }
          this.federatedLoginOperatorNavigated.delete(jobID);
          this.federatedReDriven.add(jobID);
          this.scheduleResolverRedriveRetry(jobID, opened);
          return true;
        }
      } catch {
        this.scheduleResolverRedriveRetry(jobID, tabID);
        return false;
      } finally {
        this.releaseEffectGovernor(jobID, effectToken);
      }
      return false;
    }

    await this.maybeClassify(jobID, host);
    return true;
  }

  /** Chrome's loading signal is the boundary between stale documents: title and
   * complete notifications for that document can run concurrently. */

  private advanceStaleRecoveryEpoch(jobID: string): void {
    const epoch = (this.staleRecoveryEpochs.get(jobID) ?? 0) + 1;
    this.staleRecoveryEpochs.set(jobID, epoch);
    this.staleRecoveryAttemptedEpochs.delete(jobID);
    this.staleRecoverySurfacedEpochs.delete(jobID);
    this.staleRecoveryInFlightEpochs.delete(jobID);
    this.staleRecoveryRetryTimers.delete(jobID);
  }
  private scheduleStaleRecoveryRetry(
    job: ActiveJob,
    recoveryEpoch: number,
  ): void {
    if (this.staleRecoveryRetryTimers.has(job.job_id)) return;
    const marker = {};
    this.staleRecoveryRetryTimers.set(job.job_id, marker);
    this.deps.setTimeout(() => {
      if (this.staleRecoveryRetryTimers.get(job.job_id) !== marker) return;
      this.staleRecoveryRetryTimers.delete(job.job_id);
      const current = findByJob(this.store, job.job_id);
      if (
        current === undefined ||
        this.offerURLs.get(job.job_id) === undefined ||
        (this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== recoveryEpoch ||
        !this.hasDelegatedAuthority(current)
      ) {
        return;
      }
      this.staleRecoveryAttemptedEpochs.delete(job.job_id);
      void this.redriveStaleHandoff(current, recoveryEpoch);
    }, CLASSIFY_RETRY_MS);
  }

  /**
   * Re-drive a handoff tab stranded on a dead IdP page through its retained
   * resolver offer URL, so the resolver mints a fresh SAML exchange against the
   * now-warmer session. The daemon only records the failure; recovery lives here.
   *
   * Charged against the same durable per-job authentication budget as a
   * broker-tab drive. The worker-local report debounce cannot bound this: a
   * service-worker restart clears it while the dead tab, the parked job, and the
   * user's next sign-in attempt all survive, so the old "once per outcome" latch
   * degenerated into an unbounded resolver loop across restarts. Past the cap the
   * tab is deliberately LEFT on the failure page — the user needs to see it — and
   * the job is reported human_auth_required, which keeps it parked daemon-side.
   *
   * Returns true once this document is claimed, so another callback cannot
   * fall through and spend a second entry in the same recovery budget.
   */
  private async redriveStaleHandoff(
    job: ActiveJob,
    recoveryEpoch: number,
  ): Promise<boolean> {
    if (!this.hasDelegatedAuthority(job)) return false;
    if ((this.staleRecoveryEpochs.get(job.job_id) ?? 0) !== recoveryEpoch)
      return true;
    if (
      this.staleRecoveryAttemptedEpochs.get(job.job_id) === recoveryEpoch ||
      this.staleRecoveryInFlightEpochs.get(job.job_id) === recoveryEpoch
    ) {
      return true;
    }
    const openurl = this.offerURLs.get(job.job_id);
    if (openurl === undefined || job.tab_id < 0) return false;
    this.staleRecoveryAttemptedEpochs.set(job.job_id, recoveryEpoch);
    this.staleRecoveryInFlightEpochs.set(job.job_id, recoveryEpoch);
    try {
      if (!this.institutionalAuthGateOpen()) {
        let live = false;
        try {
          const tab = await this.deps.tabs.get(job.tab_id);
          live = tab.id === job.tab_id;
        } catch {
          live = false;
        }
        if (!live) {
          // Slice 0 containment: with the tracked tab gone this recovery
          // would CREATE a replacement sign-in surface (and charge an auth
          // attempt for it). Park for explicit engagement instead; the
          // retained offer URL keeps the operator's open usable.
          await this.update((s) =>
            patchJob(s, job.job_id, {
              tab_id: -1,
              status: "queued",
              engagement_required: true,
              parked_with_tab: false,
            }),
          );
          return false;
        }
      }
      if (this.authAttemptsFor(job.job_id) >= MAX_AUTH_ATTEMPTS) {
        this.rememberStalledAuthHandoff(job.job_id, {
          url: openurl,
          providerHosts: job.provider_hosts,
          ...(job.access_mode !== undefined
            ? { accessMode: job.access_mode }
            : {}),
          ...(job.expected !== undefined ? { expected: job.expected } : {}),
          ...(job.requires_auth !== undefined
            ? { requiresAuth: job.requires_auth }
            : {}),
        });
        await this.reportAuthStalled(job.job_id);
        return false;
      }
      await this.chargeAuthAttempt(job.job_id, job.tab_id);
      if (
        !this.handoffDrives.has(job.job_id) &&
        this.handoffDrives.size >= HANDOFF_DRIVE_LIMIT
      ) {
        this.enqueueHandoffDrive({
          jobID: job.job_id,
          purpose: "redrive",
          focusExisting: false,
        });
        await this.drainHandoffDriveQueue();
        return true;
      }
      const effectToken = this.claimEffectGovernor(job.job_id);
      if (effectToken === undefined) {
        // Keep this exact recovery epoch pending; queue insertion would be a
        // no-op while the same job already owns the managed drive.
        this.scheduleStaleRecoveryRetry(job, recoveryEpoch);
        return true;
      }
      let tabID: number | undefined;
      try {
        tabID = await this.openManagedTab({
          url: openurl,
          jobId: job.job_id,
          purpose: "redrive",
          focusExisting: false,
        });
      } finally {
        this.releaseEffectGovernor(job.job_id, effectToken, false);
      }
      if (tabID === undefined) {
        this.scheduleStaleRecoveryRetry(job, recoveryEpoch);
        this.wakeEffectGovernor();
        return true;
      }
      if (!this.handoffDrives.has(job.job_id)) {
        this.registerHandoffDrive(job.job_id, tabID);
      }
      this.wakeEffectGovernor();
      return true;
    } catch {
      // Preserve the same document's recovery intent across a transient tab
      // creation/update failure; the retry callback clears the attempted latch.
      this.scheduleStaleRecoveryRetry(job, recoveryEpoch);
      return true;
    } finally {
      if (this.staleRecoveryInFlightEpochs.get(job.job_id) === recoveryEpoch) {
        this.staleRecoveryInFlightEpochs.delete(job.job_id);
      }
    }
  }

  /** Provider PDF endpoints are not required to end in `.pdf`: MDPI serves
   * `/.../pdf`, and similar publisher routes use `/download` or
   * `/full-text`. Keep this bounded to explicit PDF-ish path segments so a
   * tracked handoff navigation can be adopted without treating arbitrary
   * provider pages as files. */
  private isPDFNavigationURL(url: string): boolean {
    try {
      const pathname = new URL(url).pathname.toLowerCase();
      return (
        pathname.endsWith(".pdf") ||
        /\/(?:pdf|download|full[-_]?text)(?:\/|$)/u.test(pathname)
      );
    } catch {
      return false;
    }
  }

  /** Download a tracked PDF-viewer navigation through Chrome's download API.
   * The persisted latch and in-memory correlation jointly ensure that a
   * content-disposition download or repeated completion event cannot start a
   * second download for the same job. Page classification stays exclusively in
   * the declarative adapter path; this method accepts only a recognized direct
   * PDF route, browser PDF viewer, or packaged provider viewer route. */
  private async maybeDownloadPDFViewer(
    jobID: string,
    url: string,
    knownPDFViewer = false,
  ): Promise<void> {
    let job = findByJob(this.store, jobID);
    if (!this.hasDelegatedAuthority(job)) return;
    if (!job) return;
    if (this.isFirefoxClickDownload(job)) return;
    if (job.download_initiated === true || this.downloads.has(jobID)) return;

    let downloadURL = url;
    let viewer = knownPDFViewer;
    if (!viewer) {
      const providerPDFURL = providerViewerPDFURL(url, this.deps.adapterSpecs);
      if (providerPDFURL !== undefined) {
        downloadURL = providerPDFURL;
        viewer = true;
      }
    }
    if (!viewer) viewer = isPDFPage(url) || this.isPDFNavigationURL(url);
    if (!viewer) return;
    // The status gate follows the viewer gate on purpose. A settled PDF on the
    // tracked tab is positive evidence that authentication is BEHIND this job,
    // so `auth_pending` must not veto it: papio's own rule is that a solved
    // wall retires its own ask rather than staying armed on memory of it.
    // Measured live 2026-08-26 on doi 10.1016/j.sbspro.2014.01.1251: the
    // institutional route latched `auth_pending` during the redirect chain,
    // then the tab settled on the real file
    // (pdf.sciencedirectassets.com/.../main.pdf) and adoption was discarded
    // here, so an open-access paper already on screen was never filed.
    if (
      job.status !== "accepted" &&
      job.status !== "awaiting_download" &&
      job.status !== "auth_pending"
    )
      return;

    // Re-read after the permission/probe awaits: a content-disposition
    // download may have been correlated while this probe was in flight.
    job = findByJob(this.store, jobID);
    if (!job || job.download_initiated === true || this.downloads.has(jobID))
      return;
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) {
      this.deps.setTimeout(
        () => void this.maybeDownloadPDFViewer(jobID, url, knownPDFViewer),
        CLASSIFY_RETRY_MS,
      );
      return;
    }
    try {
      await this.update((s) =>
        patchJob(s, jobID, { download_initiated: true }),
      );

      this.pendingDownloadURLs.set(downloadURL, jobID);
      const id = await this.deps.downloads.download({
        url: downloadURL,
        filename: jobDownloadFilename(jobID),
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(jobID) ?? {
        ids: new Set<number>(),
        ambiguous: false,
        directOffer: false,
      };
      track.ids.add(id);
      if (track.ids.size > 1) track.ambiguous = true;
      this.downloads.set(jobID, track);
    } catch (e) {
      console.error(
        "papio: PDF-viewer download initiation failed; staying assisted",
        e,
      );
    } finally {
      this.pendingDownloadURLs.delete(downloadURL);
      this.releaseEffectGovernor(jobID, effectToken);
    }
  }

  /**
   * Adopt a PDF that a provider opened in a NEW viewer tab (target=_blank
   * navigation to a `.pdf`), correlating it to the tracked handoff tab that
   * spawned it. The adapter's click set `download_initiated` but produced a
   * viewer, not a `chrome.downloads` item — so gate on "no download tracked
   * yet" (this.downloads) rather than the latch. Downloads the URL through the
   * browser cookie jar so the daemon's adoption/import path runs. The viewer
   * remains open for the operator.
   */
  private async maybeAdoptViewerTab(
    viewerTabId: number,
    url: string | undefined,
    openerTabId: number | undefined,
  ): Promise<void> {
    if (url === undefined) return;
    const isPDF = this.isPDFNavigationURL(url);
    let host: string;
    try {
      host = new URL(url).hostname;
    } catch {
      return;
    }
    if (!isPDF) return;

    // Prefer the opener correlation; a recovered ledger id is authoritative
    // when Chrome loses openerTabId during a cross-origin PDF navigation.
    const ledger = await this.snapshotTabLedger();
    const openerLedgerEntry =
      openerTabId === undefined ? undefined : ledger[String(openerTabId)];
    const candidates = this.store.activeJobs.filter((j) => {
      if (this.downloads.has(j.job_id)) return false;
      if (this.isFirefoxClickDownload(j)) return false;
      if (j.status !== "accepted" && j.status !== "awaiting_download")
        return false;
      const openerMatches =
        this.hasDelegatedAuthority(j) &&
        openerTabId !== undefined &&
        (j.tab_id === openerTabId || openerLedgerEntry?.job_id === j.job_id);
      const packagedCDNMatch =
        openerTabId === undefined &&
        this.hasRecordedProviderCDNRelationship(j, host);
      return openerMatches || packagedCDNMatch;
    });
    const job =
      candidates.length === 1
        ? candidates[0]
        : candidates.find((j) => j.tab_id === openerTabId);
    if (!job) return;
    // Viewer adoption starts a browser download, so it participates in the
    // same single effect slot as every other download initiation. If another
    // effect is in flight, retry classification rather than parking the slot.
    const effectToken = this.claimEffectGovernor(job.job_id);
    if (effectToken === undefined) {
      this.deps.setTimeout(
        () => void this.maybeAdoptViewerTab(viewerTabId, url, openerTabId),
        CLASSIFY_RETRY_MS,
      );
      return;
    }
    this.adoptedViewerTabs.set(job.job_id, viewerTabId);

    this.pendingDownloadURLs.set(url, job.job_id);
    try {
      const id = await this.deps.downloads.download({
        url,
        filename: `papio/${job.job_id}/paper.pdf`,
        conflictAction: "uniquify",
        saveAs: false,
      });
      const track = this.downloads.get(job.job_id) ?? {
        ids: new Set<number>(),
        ambiguous: false,
        directOffer: false,
      };
      track.ids.add(id);
      if (track.ids.size > 1) track.ambiguous = true;
      this.downloads.set(job.job_id, track);
      if (job.download_initiated !== true) {
        await this.update((s) =>
          patchJob(s, job.job_id, { download_initiated: true }),
        );
      }
    } catch (e) {
      console.error(
        "papio: viewer-tab PDF adoption failed; staying assisted",
        e,
      );
    } finally {
      this.pendingDownloadURLs.delete(url);
      this.releaseEffectGovernor(job.job_id, effectToken);
    }
  }

  /**
   * Route a resolver's first electronic service in the same tracked tab.
   * A retained legacy offer or the daemon-advertised resolver-origin set proves
   * the landing belongs to configured institutional access; the injected
   * function separately accepts only same-origin Alma service links. Missing
   * host permission or no electronic service stays assisted.
   */
  private scheduleResolverRouteRetry(jobID: string, currentURL: string): void {
    if (this.resolverRouteRetryTimers.has(jobID)) return;
    const marker = {};
    this.resolverRouteRetryTimers.set(jobID, marker);
    this.deps.setTimeout(async () => {
      if (this.resolverRouteRetryTimers.get(jobID) !== marker) return;
      this.resolverRouteRetryTimers.delete(jobID);
      const job = findByJob(this.store, jobID as string);
      if (
        job === undefined ||
        job.tab_id < 0 ||
        (job.status !== "accepted" && job.status !== "awaiting_download")
      )
        return;
      try {
        const tab = await this.deps.tabs.get(job.tab_id);
        if (tab.url !== currentURL) {
          if (typeof tab.url === "string") {
            const current = new URL(tab.url);
            await this.maybeClassify(jobID, current.hostname);
          }
          return;
        }
      } catch {
        return;
      }
      await this.maybeRouteResolver(job, currentURL);
    }, CLASSIFY_RETRY_MS);
  }

  private async maybeRouteResolver(
    job: ActiveJob,
    currentURL: string,
  ): Promise<boolean> {
    if (!this.hasDelegatedAuthority(job)) return false;
    const offered = this.offerURLs.get(job.job_id);
    let landingURL: URL;
    try {
      landingURL = new URL(currentURL);
    } catch {
      return false;
    }
    let offerIsResolver = false;
    if (offered !== undefined) {
      try {
        const offerURL = new URL(offered);
        offerIsResolver =
          offerURL.origin === landingURL.origin &&
          /(?:openurl|uresolver)/i.test(offerURL.pathname);
      } catch {
        // A malformed legacy offer supplies no authority; the configured
        // resolver-origin path below can still prove a fresh landing.
      }
    }
    const institution = this.jobInstitutionOrigin(job);
    const configuredOrigin = this.knownResolverOrigins().includes(
      landingURL.origin,
    );
    const landedOnInstitutionResolver =
      (landingURL.origin === institution || configuredOrigin) &&
      /(?:openurl|uresolver)/i.test(landingURL.pathname);
    if (!offerIsResolver && !landedOnInstitutionResolver) return false;

    let granted = false;
    try {
      granted = await this.deps.permissions.contains({
        origins: [`${landingURL.origin}/*`],
      });
    } catch {
      return false;
    }
    if (!granted) return false;

    const effectToken = this.claimEffectGovernor(job.job_id);
    if (effectToken === undefined) {
      this.scheduleResolverRouteRetry(job.job_id, currentURL);
      return true;
    }
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: routeResolverService,
        args: [null],
      });
      const result = results[0]?.result as ResolverRoute | undefined;
      if (result?.kind === "routed") {
        this.resolverRoutes.add(job.job_id);
        return true;
      }
      if (result?.kind === "no_entitlement") {
        if (!this.resolverNoEntitlementSent.has(job.job_id)) {
          // Deliberately omit adapter metadata and all page/URL data: the
          // resolver's exact zero-holdings marker is sufficient to terminate
          // this institutional attempt.
          if (
            this.send(
              "provider_outcome",
              { outcome: "no_entitlement" },
              job.job_id,
            )
          ) {
            this.resolverNoEntitlementSent.add(job.job_id);
            await this.settleHandoffAfterOutcome(job.job_id, "no_entitlement");
          }
        }
        return true;
      }
      // `no_service` is inconclusive: retain the existing assisted behavior.
    } catch (e) {
      console.error("papio: resolver route execution failed", e);
    } finally {
      this.releaseEffectGovernor(job.job_id, effectToken);
    }
    return false;
  }
  /** Firefox cannot steer a native click download into papio/<job>, so a click
   * adapter must remain human-assisted there. Direct API downloads carry their
   * own filename and are unaffected. */
  private isFirefoxClickDownload(job: ActiveJob): boolean {
    if (
      this.deps.downloads.onDeterminingFilename !== undefined ||
      job.adapter_id === undefined
    )
      return false;
    const spec = this.deps.adapterSpecs.find(
      (candidate) => candidate.id === job.adapter_id,
    );
    return spec?.download?.method === "click";
  }

  /** The only openerless viewer relationship shipped in the extension is the
   * ScienceDirect provider -> science-direct-assets CDN redirect. It is valid
   * only while this exact delegated job has an active drive marker. */
  private hasRecordedProviderCDNRelationship(
    job: ActiveJob,
    host: string,
  ): boolean {
    if (!this.hasDelegatedAuthority(job)) return false;
    if (!host.toLowerCase().endsWith(".sciencedirectassets.com")) return false;
    const providerKnown =
      job.provider_hosts.some((candidate) =>
        hostMatches(candidate, ["sciencedirect.com"]),
      ) || job.adapter_id === "sciencedirect";
    return providerKnown && this.handoffDrives.has(job.job_id);
  }

  /** A manual browser download may originate from an offer host or from a
   * source-controlled adapter host that was recorded on the tracked landing. */
  private matchesManualDownloadHost(job: ActiveJob, host: string): boolean {
    if (hostMatches(host, job.provider_hosts)) return true;
    if (job.adapter_id === undefined) return false;
    const spec = this.deps.adapterSpecs.find(
      (candidate) => candidate.id === job.adapter_id,
    );
    return spec !== undefined && hostMatches(host, spec.hosts);
  }

  /** Classify the tracked provider page with the single injected plan executor.
   * `planExecution` function, then act on the verdict/plan. A registered
   * provider is diagnosed before injection when the browser cannot effectively
   * read it; all-sites access is effective access. Adapter execution never
   * touches a tab we do not own for this job.
   */
  private async maybeClassify(
    jobID: string,
    host: string,
    disposition: ClassificationDisposition = "apply",
  ): Promise<PageVerdict | undefined> {
    const job = findByJob(this.store, jobID);
    if (!job) return undefined;
    const allowAuthPending = disposition === "evidence_only";
    if (
      job.status !== "accepted" &&
      job.status !== "awaiting_download" &&
      !(allowAuthPending && job.status === "auth_pending")
    ) {
      return undefined;
    }
    const spec = this.deps.adapterSpecs.find((candidate) =>
      hostMatches(host, candidate.hosts),
    );
    if (!spec) {
      // Direct-PDF delivery does not need a page adapter. Otherwise verify that
      // the extension can inspect this host, then give the page one bounded
      // render window before declaring a durable coverage gap. Auth returns
      // and provider SPAs can replace their document after the first complete
      // event.
      if (disposition === "evidence_only") return undefined;
      if (job.download_initiated === true || this.downloads.has(job.job_id))
        return;
      const access = await this.hasEffectiveProviderAccess(host);
      if (access !== true) {
        if (access === false) await this.reportBlockedProviderHost(jobID, host);
        return;
      }
      if (await this.clearBlockedProviderHost(host))
        await this.syncConnectionBadge();
      const now = this.deps.now();
      const firstUnknownAt = job.last_unknown_ms;
      if (firstUnknownAt === undefined || now - firstUnknownAt < 5000) {
        if (firstUnknownAt === undefined) {
          await this.update((store) =>
            patchJob(store, job.job_id, {
              unknown_count: 1,
              last_unknown_ms: now,
            }),
          );
        }
        this.scheduleClassifyRetry(job.job_id);
        return;
      }
      const currentJob = findByJob(this.store, job.job_id);
      if (currentJob === undefined) return;
      const captured = await this.recordUnknown(currentJob, host);
      if (await this.runGenericOnSettledUnknown(currentJob)) return;
      const outcomeKey = `${job.job_id}:ui_changed`;
      if (!this.handoffOutcomeSent.has(outcomeKey)) {
        this.handoffOutcomeSent.add(outcomeKey);
        const evidence = this.genericEvidence.get(job.job_id) ?? [];
        const detail =
          "No source-controlled adapter matched this provider page." +
          (captured
            ? " A sanitized diagnostic was saved locally for adapter development."
            : "") +
          (evidence.length === 0
            ? ""
            : ` Generic evidence: ${evidence.join(", ")}.`);
        // Confirm the tab is still on the page we classified before naming it:
        // this branch runs after several awaits, so the operator may have
        // navigated away, and a wrong host would aim adapter work at an
        // innocent provider.
        const reported = await this.reportableHost(job.tab_id, host);
        if (
          !this.send(
            "provider_outcome",
            {
              outcome: "ui_changed",
              detail,
              ...(reported === undefined ? {} : { host: reported }),
            },
            job.job_id,
          )
        ) {
          this.handoffOutcomeSent.delete(outcomeKey);
        } else {
          await this.settleHandoffAfterOutcome(job.job_id, "ui_changed");
        }
      }
      return;
    }
    if (
      disposition === "apply" &&
      this.hasDelegatedAuthority(job) &&
      job.tab_id >= 0 &&
      (await this.revealForHydration(spec, job.tab_id))
    ) {
      // The reload's own completion re-enters this method against the painted
      // document. Classifying the unpainted one here is what recorded a false
      // `ui_changed` drift on every hidden-window ScienceDirect landing.
      return undefined;
    }
    const access = await this.hasEffectiveProviderAccess(host);
    if (access !== true) {
      if (disposition === "apply" && access === false) {
        await this.reportBlockedProviderHost(jobID, host);
      }
      return undefined;
    }
    if (
      disposition === "apply" &&
      (await this.clearBlockedProviderHost(host))
    ) {
      await this.syncConnectionBadge();
    }
    const currentJob = findByJob(this.store, jobID);
    if (
      !currentJob ||
      (currentJob.status !== "accepted" &&
        currentJob.status !== "awaiting_download" &&
        !(allowAuthPending && currentJob.status === "auth_pending"))
    ) {
      return;
    }

    let assessmentKind: DrivenPageAssessmentKind | undefined;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: currentJob.tab_id },
        func: assessDrivenPage,
        args: [null],
      });
      const result = results[0]?.result as DrivenPageAssessment | undefined;
      if (
        result?.kind === "challenge" ||
        result?.kind === "redirect_loop" ||
        result?.kind === "normal"
      ) {
        assessmentKind = result.kind;
      }
    } catch (e) {
      console.error(
        "papio: driven-page assessment failed; classifying normally",
        e,
      );
    }
    if (assessmentKind === "challenge" || assessmentKind === "redirect_loop") {
      await this.confirmThenBlockChallenge(
        currentJob,
        host,
        assessmentKind === "challenge" ? "cloudflare" : "redirect_loop",
      );
      return undefined;
    }
    if (assessmentKind === "normal" && currentJob.challenge_blocked === true) {
      if (disposition === "evidence_only") return undefined;
      if (!(await this.clearChallengeBlock(currentJob))) return undefined;
    }

    let plan: Plan | undefined;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: currentJob.tab_id },
        func: planExecution,
        // planExecution(null, spec, expected, policy): doc arrives null and
        // falls back to the page's document; all other inputs are JSON.
        args: [
          null,
          spec,
          { ...(currentJob.expected ?? {}) },
          currentJob.access_mode === undefined
            ? {}
            : { access_mode: currentJob.access_mode },
        ],
      });
      const first = results[0]?.result as PlanResult | undefined;
      if (
        first !== undefined &&
        typeof first === "object" &&
        first !== null &&
        !("assisted" in first)
      ) {
        plan = first;
      }
    } catch (e) {
      console.error("papio: adapter planning failed; staying assisted", e);
      return;
    }
    if (plan === undefined) return undefined;
    const verdict = plan.verdict;
    if (disposition === "evidence_only") return verdict;
    if (verdict.kind === "unknown") {
      let fallbackKind: ChallengeBlockKind | undefined;
      try {
        const results = await this.deps.scripting.executeScript({
          target: { tabId: job.tab_id },
          func: isBotChallenge,
          args: [null],
        });
        if (results[0]?.result === true) {
          fallbackKind = "cloudflare";
        } else {
          const redirectResults = await this.deps.scripting.executeScript({
            target: { tabId: job.tab_id },
            func: isRedirectLoopPage,
            args: [null],
          });
          if (redirectResults[0]?.result === true)
            fallbackKind = "redirect_loop";
        }
      } catch (e) {
        // A failed probe must retain the existing stale-adapter path rather
        // than silently make an unreadable provider page immortal.
        console.error(
          "papio: challenge detection failed; classifying normally",
          e,
        );
      }
      if (fallbackKind !== undefined) {
        await this.waitForBotChallenge(currentJob, host, fallbackKind);
        return;
      }
      if (currentJob.challenge_blocked === true)
        await this.clearChallengeBlock(currentJob);
    }
    const providerKey = this.providerKeyForJob(currentJob);
    let providerLeaseOwner = this.providerDrainLeaseOwners.get(providerKey);
    const providerLeaseJob = this.providerDrainLeaseJobs.get(providerKey);
    if (
      providerLeaseJob !== undefined &&
      providerLeaseJob !== currentJob.job_id
    ) {
      this.scheduleClassifyRetry(jobID, "effect");
      return;
    }
    if (
      providerLeaseOwner === undefined &&
      this.currentProviderDrainLease(providerKey) === undefined
    ) {
      providerLeaseOwner = await this.claimProviderDrainLease(currentJob);
    }
    // A persisted lease without a local owner belongs to another live drive
    // (possibly from a prior worker); do not let this tab bypass it. Keep the
    // exact classification queued so releasing that drive can make progress
    // instead of silently dropping this provider effect.
    if (
      providerLeaseOwner === undefined &&
      this.currentProviderDrainLease(providerKey) !== undefined
    ) {
      this.scheduleClassifyRetry(jobID, "effect");
      return;
    }
    let releasedProviderLease = false;
    try {
      await this.applyVerdict(jobID, spec, plan, host);
    } finally {
      if (providerLeaseOwner !== undefined) {
        await this.releaseProviderDrainLease(providerKey, providerLeaseOwner);
        releasedProviderLease = true;
      }
    }
    if (releasedProviderLease) {
      if (await this.acknowledgePendingProviderHandoffs(providerKey)) {
        await this.releaseQueuedHandoffs();
      }
    }
    // A decisive verdict ends the render race; `unknown` may just be an
    // un-upgraded page, so retry on a bounded schedule. A latched download-click
    // that opens a declared terms gate must ALSO keep retrying: providers like
    // JSTOR upgrade the terms modal (mfe-*) AFTER the click, so a single
    // post-click classify can miss it. A retry can never start a second
    // download — every download-initiation path bails on download_initiated —
    // so it only serves to catch the terms modal and accept it.
    const after = findByJob(this.store, jobID);
    const awaitingTermsGate =
      spec.termsAccept !== undefined &&
      (after?.status === "accepted" || after?.status === "awaiting_download") &&
      after?.download_initiated === true &&
      !this.downloads.has(jobID);
    const genericInFlight =
      after?.download_initiated === true &&
      this.downloads.get(jobID)?.generic !== undefined;
    if (
      (!genericInFlight && verdict.kind === "unknown") ||
      (verdict.kind !== "terms" && awaitingTermsGate)
    ) {
      this.scheduleClassifyRetry(jobID);
    } else {
      this.classifyRetries.delete(jobID);
    }
  }

  /** A challenge is a provider-wide human step, not a page retry. Keep its
   * existing tab available and park only siblings with a bounded lease.
   *
   * The name is now accurate: the wait is the confirmation window, because this
   * path reaches an `unknown` verdict on a page that may still be mid-check.
   * It never waited before. */
  private async waitForBotChallenge(
    job: ActiveJob,
    host: string,
    kind: ChallengeBlockKind = "cloudflare",
  ): Promise<void> {
    this.classifyRetries.delete(job.job_id);
    await this.confirmThenBlockChallenge(job, host, kind);
  }

  private scheduleClassifyRetry(
    jobID: string,
    kind: ClassifyRetryKind = "unknown",
  ): void {
    const retry = this.classifyRetries.get(jobID);
    if (kind === "unknown" && retry?.kind === "federated_evidence") return;
    const attempts = retry?.kind === kind ? retry.attempts : 0;
    if (attempts >= MAX_CLASSIFY_RETRIES) {
      this.classifyRetries.delete(jobID);
      return;
    }
    const next: ClassifyRetry = { kind, attempts: attempts + 1 };
    this.classifyRetries.set(jobID, next);
    this.deps.setTimeout(
      () => this.retryClassify(jobID, next),
      CLASSIFY_RETRY_MS,
    );
  }

  /** Consume only the browser lifecycle generated by our own federated route.
   * A later loading event at the same IdP URL is the operator's navigation
   * (the credential submission/return path) and is therefore allowed to enter
   * auth_pending. */
  private consumeFederatedLoginRouteEvent(
    jobID: string,
    url: string,
    change: TabChangeInfo,
  ): boolean {
    const pending = this.federatedLoginRouteEvents.get(jobID);
    if (pending === undefined || pending.url !== url) return false;
    if (change.status === "loading") {
      if (!pending.loadingSeen) {
        pending.loadingSeen = true;
        return true;
      }
      this.federatedLoginRouteSettled.add(jobID);
      return true;
    }
    if (change.status === "complete" && pending.loadingSeen) {
      this.federatedLoginRouteEvents.delete(jobID);
      this.federatedLoginRouteSettled.add(jobID);
      return false;
    }
    return false;
  }

  /** Auto-select the institution on a provider login wall: navigate the handoff
   * tab to the adapter's federated-login entry with the offer's entityID, once
   * per drive. Institution selection is deterministic config, not a secret; the
   * human still enters credentials at the IdP. No-op without a configured route,
   * a known entityID, or a `tabs.update` seam, and never re-navigates mid
   * sign-in (latched, cleared on job removal).
   *
   * Cross-job "one login tab per institution" dedup used to live here
   * (federatedLoginOwners); Slice 3 retired it with no extension-local
   * replacement — a job without a live institutional candidate gets no
   * daemon-side collision avoidance either (background.ts's Change 4). A
   * job WITH a candidate never reaches this function to begin with: its
   * sign-in surface is minted by openFreshHandoff's claim-consult path,
   * which never uses the adapter's `federatedLogin` template. */
  private async maybeRouteFederatedLogin(
    jobID: string,
    job: ActiveJob,
    spec: AdapterSpec,
  ): Promise<void> {
    if (!this.hasDelegatedAuthority(job)) return;
    const template = spec.federatedLogin;
    const entityID = this.loginEntityIDs.get(jobID);
    if (template === undefined || entityID === undefined) return;
    if (this.federatedLoginRouted.has(jobID)) return;
    if (this.deps.tabs.update === undefined) return;
    const url = template.replace("{entityID}", encodeURIComponent(entityID));
    if (!url.startsWith("https://")) return;
    try {
      new URL(url);
    } catch {
      return;
    }
    this.federatedLoginRouted.add(jobID);
    this.federatedLoginOperatorNavigated.delete(jobID);
    this.federatedLoginRouteSettled.delete(jobID);
    this.federatedLoginRouteEvents.set(jobID, { url, loadingSeen: false });
    try {
      await this.deps.tabs.update(job.tab_id, { url });
    } catch (e) {
      // Let a later classify retry route again if this navigation failed.
      this.federatedLoginRouted.delete(jobID);
    }
  }

  /** Unlock a provider's openurl link-resolver by appending its institutional
   * account id (ProQuest: ?accountid=<id>) to the current tab URL — fully
   * autonomous, no sign-in. Returns true if it navigated. No-op without a
   * configured param/account id or a `tabs.update` seam, if the current URL
   * already carries the param, or if already appended this drive (latched). */
  private async maybeAppendAccountId(
    jobID: string,
    job: ActiveJob,
    spec: AdapterSpec,
  ): Promise<boolean> {
    if (!this.hasDelegatedAuthority(job)) return false;
    const param = spec.accountIdParam;
    const accountID = this.proquestAccountIDs.get(jobID);
    if (param === undefined || accountID === undefined) return false;
    if (this.accountIdAppended.has(jobID)) return false;
    if (this.deps.tabs.update === undefined) return false;
    let current: string;
    try {
      current = (await this.deps.tabs.get(job.tab_id)).url ?? "";
    } catch {
      return false;
    }
    if (!current.startsWith("https://")) return false;
    const url = new URL(current);
    if (url.searchParams.get(param) === accountID) return false;
    url.searchParams.set(param, accountID);
    this.accountIdAppended.add(jobID);
    try {
      await this.deps.tabs.update(job.tab_id, { url: url.toString() });
      return true;
    } catch (e) {
      this.accountIdAppended.delete(jobID);
      console.error("papio: account-id unlock failed", e);
      return false;
    }
  }

  private async retryClassify(
    jobID: string,
    expected?: ClassifyRetry,
  ): Promise<void> {
    await this.ready;
    if (expected !== undefined && this.classifyRetries.get(jobID) !== expected)
      return;
    if (expected?.kind === "federated_evidence") {
      await this.retryFederatedEvidence(jobID, expected);
      return;
    }
    const job = findByJob(this.store, jobID);
    // Stop once the job is gone or an actual download is tracked. The guard is
    // the tracked download, NOT download_initiated: a click that latched to open
    // a terms gate has download_initiated=true but no download yet, and the
    // retry must continue so a late-upgrading terms model is caught. No download
    // can fire twice — every initiation path still bails on download_initiated.
    if (!job || this.downloads.has(jobID)) {
      this.classifyRetries.delete(jobID);
      return;
    }
    if (job.status !== "accepted" && job.status !== "awaiting_download") {
      this.classifyRetries.delete(jobID);
      return;
    }
    try {
      const live = await this.deps.tabs.get(job.tab_id);
      if (live.id !== job.tab_id) throw new Error("tab identity changed");
    } catch {
      this.classifyRetries.delete(jobID);
      return;
    }
    await this.reclassifyCurrentProviderPage(jobID);
  }
  private async retryFederatedEvidence(
    jobID: string,
    expected: ClassifyRetry,
  ): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (
      !job ||
      job.status !== "auth_pending" ||
      !this.federatedLoginRouted.has(jobID) ||
      job.tab_id < 0
    ) {
      this.classifyRetries.delete(jobID);
      return;
    }
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(job.tab_id);
    } catch {
      this.classifyRetries.delete(jobID);
      return;
    }
    if (tab.id !== job.tab_id || tab.url === undefined) {
      this.classifyRetries.delete(jobID);
      return;
    }
    let host: string;
    try {
      host = new URL(tab.url).hostname;
    } catch {
      this.classifyRetries.delete(jobID);
      return;
    }
    const verdict = await this.maybeClassify(jobID, host, "evidence_only");
    if (verdict === undefined) {
      if (findByJob(this.store, jobID)?.challenge_blocked !== true) {
        this.scheduleClassifyRetry(jobID, "federated_evidence");
      }
      return;
    }
    if (verdict.kind === "login") {
      this.classifyRetries.delete(jobID);
      return;
    }
    if (verdict.kind === "unknown") {
      this.scheduleClassifyRetry(jobID, "federated_evidence");
      return;
    }
    this.classifyRetries.delete(jobID);
    await this.finalizeAuthReturn(jobID, tab.id, tab.url, host, true);
  }

  private async reclassifyCurrentProviderPage(
    jobID: string,
    allowUnregistered = false,
  ): Promise<void> {
    const job = findByJob(this.store, jobID);
    // A queued handoff (tab_id -1) has no page yet, and a closed one never
    // will: normal tab-removal recovery stays authoritative. Callers include a
    // bare classify-retry timer, so a throw here would escape unhandled.
    if (!job || job.tab_id < 0) return;
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(job.tab_id);
    } catch {
      return;
    }
    if (tab.url === undefined || isAuthenticationURL(tab.url)) return;
    let host: string;
    try {
      host = new URL(tab.url).hostname;
    } catch {
      return;
    }
    const onRegisteredProvider =
      hostMatches(host, job.provider_hosts) ||
      this.deps.adapterSpecs.some((candidate) =>
        hostMatches(host, candidate.hosts),
      );
    const continuingUnregisteredLanding =
      allowUnregistered || job.last_unknown_ms !== undefined;
    if (!onRegisteredProvider && !continuingUnregisteredLanding) return;
    await this.maybeClassify(jobID, host);
  }

  /** Record a development capture for an unknown page. The caller decides
   * whether to remain assisted or report a terminal coverage gap. */
  private async recordUnknown(
    job: ActiveJob,
    host: string,
    adapter?: AdapterSpec,
    deferTerminal = false,
  ): Promise<boolean> {
    let captured = false;
    const captureStorage = this.deps.captureStorage;
    if (captureStorage !== undefined && this.pageCaptureAvailable()) {
      captured = await observeUnknown(
        {
          scripting: this.deps.scripting as ObserveChromeApi["scripting"],
          storage: captureStorage,
          sendPageCapture: (payload, jobID) =>
            this.sendPageCapture(payload, jobID),
        },
        job,
        host,
        {
          verifiedHosts:
            adapter === undefined
              ? [...job.provider_hosts, host]
              : [...job.provider_hosts, ...adapter.hosts],
          ...(adapter === undefined
            ? {}
            : { adapterID: adapter.id, adapterVersion: adapter.version }),
        },
        () => new Date(this.deps.now()),
      );
    }
    if (adapter === undefined) return captured;
    const now = this.deps.now();
    const count = job.unknown_count ?? 0;
    const last = job.last_unknown_ms ?? 0;
    if (count >= 1 && now - last >= 5000 && !deferTerminal) {
      // Retries wait for one document to render; they are not independent
      // provider failures, so one broker drive gets one terminal observation.
      const outcomeKey = `${job.job_id}:ui_changed`;
      if (!this.handoffOutcomeSent.has(outcomeKey)) {
        this.handoffOutcomeSent.add(outcomeKey);
        const reportedHost = await this.reportableHost(job.tab_id, host);
        if (
          !this.send(
            "provider_outcome",
            {
              outcome: "ui_changed",
              adapter_id: adapter.id,
              adapter_version: adapter.version,
              ...(reportedHost === undefined ? {} : { host: reportedHost }),
            },
            job.job_id,
          )
        ) {
          this.handoffOutcomeSent.delete(outcomeKey);
        } else {
          await this.settleHandoffAfterOutcome(job.job_id, "ui_changed");
        }
      }
    } else if (count === 0) {
      await this.update((s) =>
        patchJob(s, job.job_id, { unknown_count: 1, last_unknown_ms: now }),
      );
    }

    return captured;
  }
  private genericEpochKey(jobID: string, epoch: ProviderDriveEpoch): string {
    return `${jobID}\u0000${epoch.drive_attempt_id}\u0000${epoch.ordinal}\u0000${epoch.strategy}\u0000${epoch.revision ?? ""}`;
  }

  /** Send one terminal observation for the exact daemon-minted tuple. */
  private async sendGenericEpochResult(
    jobID: string,
    epoch: ProviderDriveEpoch,
    outcome: string,
    detail: string,
  ): Promise<NativeRequestResult | undefined> {
    const key = this.genericEpochKey(jobID, epoch);
    if (this.genericEpochResultsSent.has(key)) return undefined;
    this.genericEpochResultsSent.add(key);
    const result = await this.requestNative(
      "provider_drive_epoch_result_request",
      {
        drive_attempt_id: epoch.drive_attempt_id,
        ordinal: epoch.ordinal,
        strategy: "generic",
        revision: epoch.revision ?? "",
        outcome,
        detail,
      },
      "provider_drive_epoch_result",
      PROVIDER_DRIVE_EPOCH_FEATURE,
      true,
      jobID,
    );
    if (result.kind !== "response") this.genericEpochResultsSent.delete(key);
    return result;
  }

  /** Rebuild only generic download correlation after an MV3 worker restart.
   * Candidate URLs and their ordering are deliberately not recoverable: the
   * daemon's tuple is the sole durable authority for a fresh epoch. */
  private async reconcileGenericDownloads(): Promise<void> {
    for (const job of this.store.activeJobs) {
      const epoch = job.generic_drive_epoch;
      if (epoch?.strategy !== "generic") continue;
      if (job.generic_terminal === true) continue;
      if (epoch.in_flight_download_id === undefined) {
        // No exact id survived the worker boundary. Keep the occupying permit
        // unresolved; a filename miss or match cannot prove this effect's
        // outcome.
        continue;
      }
      const downloadID = epoch.in_flight_download_id;
      let found: DownloadItemLike[];
      try {
        found = await this.deps.downloads.search({ id: downloadID });
      } catch {
        continue;
      }
      const item = found[0];
      if (item === undefined) continue;
      this.downloads.set(job.job_id, {
        ids: new Set([downloadID]),
        ambiguous: false,
        directOffer: false,
        generic: { candidates: [], index: 0, epoch },
      });
      const state = item?.state;
      if (state === "complete" || state === "interrupted") {
        await this.onDownloadChanged({
          id: downloadID,
          state: { current: state },
        });
      }
    }
  }

  private async runGenericOnSettledUnknown(job: ActiveJob): Promise<boolean> {
    if (
      this.handoffDrives.has(job.job_id) === false ||
      job.tab_id < 0 ||
      job.status !== "accepted" ||
      (job.access_mode !== "delegated" && job.access_mode !== "assisted")
    ) {
      return false;
    }
    const state = job as ActiveJob & GenericJobState;
    if (
      state.generic_evaluated === true ||
      (state.generic_positive_attempts ?? 0) >= 2
    )
      return false;
    // Reserve the E0 observation before injection. This is the durable
    // check-and-set that prevents a restarted worker from re-running it.
    await this.update((s) => ({
      ...s,
      activeJobs: s.activeJobs.map((candidate) =>
        candidate.job_id === job.job_id
          ? ({ ...candidate, generic_evaluated: true } as ActiveJob)
          : candidate,
      ),
    }));
    let planned: GenericPlan | undefined;
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: job.tab_id },
        func: planGeneric,
        args: [
          null,
          { ...(job.expected ?? {}) },
          { access_mode: job.access_mode },
        ],
      });
      const result = results[0]?.result as GenericPlan | undefined;
      if (
        result !== undefined &&
        typeof result === "object" &&
        result !== null &&
        Array.isArray(result.evidence) &&
        Array.isArray(result.candidates)
      ) {
        planned = result;
      }
    } catch (error) {
      console.error("papio: generic planning failed; staying assisted", error);
    }
    if (planned === undefined) return false;
    const evidence = planned.evidence
      .filter((item): item is string => typeof item === "string")
      .slice(0, 20);
    const priorEvidence = this.genericEvidence.get(job.job_id) ?? [];
    this.genericEvidence.set(
      job.job_id,
      [...new Set([...priorEvidence, ...evidence])].slice(0, 20),
    );
    const candidates =
      job.access_mode === "delegated"
        ? planned.candidates.filter(
            (candidate): candidate is GenericCandidate =>
              candidate !== null &&
              typeof candidate === "object" &&
              (candidate.strategy_id === "generic-citation-pdf/1" ||
                candidate.strategy_id === "generic-article-pdf-link/1") &&
              candidate.strategy_version === "1" &&
              typeof candidate.url === "string" &&
              candidate.url.startsWith("https://"),
          )
        : [];
    if (candidates.length === 0) return false;
    await this.startGenericCandidate(job.job_id, candidates, 0);
    return true;
  }

  private async startGenericCandidate(
    jobID: string,
    candidates: GenericCandidate[],
    requestedIndex: number,
  ): Promise<void> {
    const current = findByJob(this.store, jobID);
    if (
      current === undefined ||
      !this.handoffDrives.has(jobID) ||
      current.tab_id < 0 ||
      current.status !== "accepted" ||
      current.access_mode !== "delegated"
    ) {
      return;
    }
    const state = current as ActiveJob & GenericJobState;
    if (state.generic_terminal === true) return;
    const attempted = state.generic_attempted_strategies ?? [];
    const attempts = state.generic_positive_attempts ?? 0;
    if (attempts >= 2) return;
    let index = requestedIndex;
    let candidate: GenericCandidate | undefined;
    while (index < candidates.length) {
      const next = candidates[index];
      index += 1;
      if (next !== undefined && !attempted.includes(next.strategy_id)) {
        candidate = next;
        break;
      }
    }
    if (candidate === undefined) {
      await this.emitGenericUnknown(jobID);
      return;
    }
    // Execution revalidation is separate from discovery. A changed document
    // loses authority rather than allowing a stale URL to be downloaded.
    try {
      const results = await this.deps.scripting.executeScript({
        target: { tabId: current.tab_id },
        func: planGeneric,
        args: [
          null,
          { ...(current.expected ?? {}) },
          { access_mode: "delegated" },
        ],
      });
      const fresh = results[0]?.result as GenericPlan | undefined;
      const freshCandidate = fresh?.candidates?.find(
        (entry) =>
          entry?.strategy_id === candidate!.strategy_id &&
          entry.strategy_version === candidate!.strategy_version &&
          entry.url === candidate!.url,
      );
      if (freshCandidate === undefined) {
        await this.emitGenericWrongWork(jobID, candidate.strategy_id);
        return;
      }
    } catch (error) {
      console.error(
        "papio: generic execution revalidation failed; staying assisted",
        error,
      );
      await this.emitGenericWrongWork(jobID, candidate.strategy_id);
      return;
    }
    const latest = findByJob(this.store, jobID);
    if (latest === undefined || !this.handoffDrives.has(jobID)) return;
    const priorEpoch = (latest as ActiveJob & GenericJobState)
      .generic_drive_epoch;
    const epoch: ProviderDriveEpoch | undefined =
      priorEpoch?.strategy_id === candidate.strategy_id &&
      priorEpoch.strategy === "generic"
        ? priorEpoch
        : priorEpoch?.strategy === "generic" &&
            priorEpoch.revision === candidate.strategy_version
          ? { ...priorEpoch, strategy_id: candidate.strategy_id }
          : undefined;
    if (epoch === undefined) {
      await this.emitGenericUnknown(jobID);
      return;
    }
    if (typeof epoch.revision !== "string" || epoch.revision.length === 0) {
      if (
        !this.genericCandidateAuthorized(
          this.store,
          jobID,
          epoch,
          candidate.strategy_id,
        )
      )
        return;
      await this.retainGenericCandidate(jobID, { candidates, index, epoch });
      return;
    }
    // A generic effect requires both the epoch protocol and the daemon-durable
    // permit. Without the permit capability, retain this exact candidate and
    // tuple but never send a start request that could authorize a download.
    const negotiatedFeatures = this.store.daemonFeatures ?? [];
    if (
      !negotiatedFeatures.includes(PROVIDER_DRIVE_EPOCH_FEATURE) ||
      !negotiatedFeatures.includes(EFFECT_PERMIT_FEATURE)
    ) {
      if (
        this.genericCandidateAuthorized(
          this.store,
          jobID,
          epoch,
          candidate.strategy_id,
        )
      ) {
        await this.retainGenericCandidate(jobID, { candidates, index, epoch });
      }
      return;
    }
    const effectToken = this.claimEffectGovernor(jobID);
    if (effectToken === undefined) return;
    const providerKey = this.providerKeyForJob(latest);
    const providerLeaseJob = this.providerDrainLeaseJobs.get(providerKey);
    if (providerLeaseJob !== undefined && providerLeaseJob !== jobID) {
      this.releaseEffectGovernor(jobID, effectToken);
      return;
    }
    let providerLeaseOwner = this.providerDrainLeaseOwners.get(providerKey);
    try {
      if (
        providerLeaseOwner === undefined &&
        this.currentProviderDrainLease(providerKey) === undefined
      ) {
        providerLeaseOwner = await this.claimProviderDrainLease(latest);
      }
      if (providerLeaseOwner === undefined) {
        this.releaseEffectGovernor(jobID, effectToken);
        return;
      }
    } catch {
      this.releaseEffectGovernor(jobID, effectToken);
      return;
    }
    try {
      const start = await this.requestNative(
        "provider_drive_epoch_start_request",
        {
          drive_attempt_id: epoch.drive_attempt_id,
          ordinal: epoch.ordinal,
          strategy: "generic",
          revision: epoch.revision,
        },
        "provider_drive_epoch_start_result",
        PROVIDER_DRIVE_EPOCH_FEATURE,
        true,
        jobID,
      );
      const startOutcome =
        start.kind === "response" &&
        typeof start.payload?.["outcome"] === "string"
          ? start.payload["outcome"]
          : undefined;
      const exactStarted =
        start.kind === "response" &&
        startOutcome === "started" &&
        start.payload?.["drive_attempt_id"] === epoch.drive_attempt_id &&
        start.payload?.["ordinal"] === epoch.ordinal &&
        start.payload?.["strategy"] === "generic" &&
        start.payload?.["revision"] === epoch.revision;
      if (!exactStarted) {
        if (
          this.genericCandidateAuthorized(
            this.store,
            jobID,
            epoch,
            candidate.strategy_id,
          )
        ) {
          await this.retainGenericCandidate(
            jobID,
            { candidates, index, epoch },
            startOutcome === "stale",
          );
        }
        return;
      }
      const claimedEpoch = await this.claimGenericCandidate(
        jobID,
        epoch,
        candidate,
      );
      if (claimedEpoch === undefined) return;
      // The claim update persists asynchronously. Re-read synchronously after it
      // settles so a concurrent offer downgrade cannot be followed by a download.
      if (
        !this.genericCandidateAuthorized(
          this.store,
          jobID,
          claimedEpoch,
          candidate.strategy_id,
          true,
        )
      )
        return;
      this.pendingDownloadURLs.set(candidate.url, jobID);
      try {
        const downloadID = await this.deps.downloads.download({
          url: candidate.url,
          filename: jobDownloadFilename(jobID),
          conflictAction: "uniquify",
          saveAs: false,
        });
        this.downloads.set(jobID, {
          ids: new Set([downloadID]),
          ambiguous: false,
          directOffer: false,
          generic: { candidates, index, epoch: claimedEpoch },
        });
        await this.update((s) => ({
          ...s,
          activeJobs: s.activeJobs.map((entry) =>
            entry.job_id === jobID
              ? ({
                  ...entry,
                  generic_drive_epoch: {
                    ...claimedEpoch,
                    in_flight_download_id: downloadID,
                  },
                } as ActiveJob)
              : entry,
          ),
        }));
      } catch (error) {
        console.error(
          "papio: generic download initiation failed; retaining candidate",
          error,
        );
        await this.sendGenericEpochResult(
          jobID,
          claimedEpoch,
          "unknown",
          "browser download initiation failed",
        );
        await this.update((s) => ({
          ...s,
          activeJobs: s.activeJobs.map((entry) => {
            if (entry.job_id !== jobID) return entry;
            const {
              in_flight_download_id: _inFlightDownloadID,
              ...withoutDownload
            } = claimedEpoch;
            return {
              ...entry,
              download_initiated: false,
              generic_terminal: true,
              generic_drive_epoch: withoutDownload,
            } as ActiveJob;
          }),
        }));
      } finally {
        this.pendingDownloadURLs.delete(candidate.url);
      }
    } finally {
      try {
        if (providerLeaseOwner !== undefined)
          await this.releaseProviderDrainLease(providerKey, providerLeaseOwner);
      } finally {
        this.releaseEffectGovernor(jobID, effectToken);
      }
    }
  }
  private async emitGenericWrongWork(
    jobID: string,
    strategyID: string,
  ): Promise<void> {
    const job = findByJob(this.store, jobID);
    const epoch = job?.generic_drive_epoch;
    if (epoch?.strategy !== "generic" || typeof epoch.revision !== "string")
      return;
    await this.sendGenericEpochResult(
      jobID,
      epoch,
      "wrong_work",
      `Generic strategy ${strategyID} failed identity revalidation.`,
    );
  }

  /** The hostname to report on a provider observation, or undefined.
   *
   * Fails empty on purpose. A drift with no host is merely unattributable; a
   * drift with the WRONG host is confidently misattributed, and would aim
   * adapter repair at a provider that never failed. Classification holds its
   * host across several awaits (permission checks, capture, generic
   * planning), so the tab can navigate in between. Passing `expected`
   * re-reads the tab and refuses to report anything unless it is still on
   * that host.
   */
  private async reportableHost(
    tabID: number,
    expected?: string,
  ): Promise<string | undefined> {
    if (tabID < 0) return undefined;
    let current: string;
    try {
      const tab = await this.deps.tabs.get(tabID);
      if (tab.url === undefined) return undefined;
      current = new URL(tab.url).hostname.toLowerCase();
      // A reported host must satisfy the wire grammar exactly, because `send`
      // self-validates and DROPS an invalid frame — which would suppress the
      // whole observation, not just its attribution. WHATWG hostname keeps a
      // legitimate trailing dot, so strip one; about:blank, data:, and file:
      // yield an empty hostname; an IPv6 literal keeps its brackets. Anything
      // still outside the grammar reports nothing.
      if (current.endsWith(".")) current = current.slice(0, -1);
      if (
        !/^[a-z0-9.-]{3,128}$/.test(current) ||
        current.includes("..") ||
        current.startsWith(".") ||
        // Still needed after the single strip above: "abc.." reduces to "abc."
        // and would otherwise pass, then be dropped by send's own validation.
        current.endsWith(".")
      )
        return undefined;
    } catch {
      return undefined;
    }
    if (expected !== undefined && current !== expected.toLowerCase())
      return undefined;
    return current;
  }

  private async emitGenericUnknown(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    const evidence = this.genericEvidence.get(jobID) ?? [];
    const detail =
      "No source-controlled adapter matched this provider page." +
      (evidence.length === 0
        ? ""
        : ` Generic evidence: ${evidence.join(", ")}.`);
    // Best effort, and deliberately fail-empty. See reportableHost: a wrong
    // host is worse than none, because adapter work would be aimed at the
    // wrong provider.
    const host = await this.reportableHost(job.tab_id);
    const outcomeKey = `${jobID}:ui_changed`;
    if (
      !this.handoffOutcomeSent.has(outcomeKey) &&
      this.send(
        "provider_outcome",
        { outcome: "ui_changed", detail, ...(host === undefined ? {} : { host }) },
        jobID,
      )
    ) {
      this.handoffOutcomeSent.add(outcomeKey);
      await this.settleHandoffAfterOutcome(jobID, "ui_changed");
    }
  }

  /** A non-PDF result is a daemon-owned successor decision. The extension
   * retains the exact started tuple/candidate and never increments its ordinal
   * or starts the next local candidate. */
  private async advanceGenericCandidate(
    jobID: string,
    track: GenericDownloadAttempt,
  ): Promise<void> {
    await this.retainGenericCandidate(jobID, track);
  }
  private async retainGenericCandidate(
    jobID: string,
    track: GenericDownloadAttempt,
    retryable = false,
  ): Promise<void> {
    this.downloads.delete(jobID);
    await this.update((s) => ({
      ...s,
      activeJobs: s.activeJobs.map((entry) => {
        if (entry.job_id !== jobID) return entry;
        const {
          in_flight_download_id: _inFlightDownloadID,
          ...withoutDownload
        } = track.epoch;
        return {
          ...entry,
          download_initiated: false,
          // The positive attempt and strategy remain durable. Only a fresh
          // daemon epoch may clear them and authorize another candidate.
          // "terminal" stops local execution only: daemon-initiated permit
          // reconciliation still matches generic_drive_epoch exactly.
          generic_terminal: !retryable,
          ...(retryable
            ? { generic_deferred: true }
            : { generic_deferred: undefined }),
        } as ActiveJob;
      }),
    }));
  }

  /** Settle the handoff this provider outcome ended, and decide whether the
   * job's browser-side record dies with it.
   *
   * `ui_changed` and `wrong_work` are exactly the two outcomes for which the
   * daemon opens a `manual_download` human action instead of terminating the
   * job (bridge.go's provider_outcome handler). Removing the job here — all
   * this used to do — deleted the only record `correlate` can match, so the
   * researcher's own click download could never be steered into
   * `papio/<job_id>/` and the daemon's adoption sweep never saw the file.
   * The steering window switched itself off at precisely the moment the
   * action asking the researcher to download was created.
   *
   * `no_entitlement` opens no browser action — the daemon requeues or parks
   * the job elsewhere — so the browser record dies with its parked surface. */
  private async settleHandoffAfterOutcome(
    jobID: string,
    outcome: "ui_changed" | "wrong_work" | "no_entitlement",
  ): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    if (outcome === "no_entitlement") {
      // This browser-side drive is explicitly parked and has no provider
      // effect left. `job_inactive` races the daemon's no-entitlement requeue:
      // once rediscovery opens a document-delivery action the job is active
      // again, so that disposition is refused and the navigated claim keeps
      // the institution's one sign-in slot forever. `handoff_parked` states
      // only the fact the browser owns; the daemon still vetoes it while an
      // effect permit is live. The authorized tab removal then reports
      // owner_closed and retires the binding plus its authentication lease.
      await this.removeJobWithOffer(jobID, "handoff_parked");
      return;
    }
    await this.retainForManualDownload(jobID);
  }

  /** Tear the drive down exactly as `removeJobWithOffer` does — governor slot,
   * drive timeout, provider drain lease, materialization workflow, federated
   * login claim, offer URL, managed tab — then re-insert a deliberately inert
   * correlation-only record so the researcher's own download can still be
   * claimed.
   *
   * Tab policy: the provider tab is still closed. `correlate`'s tab branch
   * cannot serve this case anyway — the researcher downloads from whatever
   * tab they open from the inbox row, not from the page papio just proved it
   * cannot drive — and the host branch needs only `provider_hosts`. Holding
   * one provider tab open per open action (27 of them on the maintainer's
   * live install) to serve a correlation none of those tabs carries is not a
   * trade worth making, and papio does not get to occupy the researcher's
   * browser on the strength of work it just failed at.
   *
   * The retained record deliberately carries no `access_mode`, so
   * `hasDelegatedAuthority` is false and every autonomous path skips it by
   * construction: the startup drive scan, `reconcileTabs`, viewer adoption,
   * resolver routing and stale redrive all gate on it. It has no tab and no
   * offer URL, so nothing can reopen one — the startup scan's tabless branch
   * requires a retained offer URL to enqueue governor work. What survives is
   * exactly `provider_hosts` plus `adapter_id`: the input to
   * `matchesManualDownloadHost`, and the input to `isFirefoxClickDownload`'s
   * refusal, which must keep working or Firefox would acknowledge a file it
   * cannot relocate.
   *
   * `awaiting_download` is the honest status for it — papio is waiting for a
   * PDF for this job and nothing else — and it is already the status the
   * download-completion path writes, so a claimed download needs no second
   * transition.
   *
   * Retirement is the daemon's call, never a timer: adoption retires it
   * through the ordinary `ack` -> `closeAfterAdoption` path, and
   * `reconcileManualDownloadWindows` drops it as soon as a complete triage
   * snapshot stops reporting the action open. */
  private async retainForManualDownload(jobID: string): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (job === undefined) return;
    const retained: ActiveJob = {
      job_id: job.job_id,
      tab_id: -1,
      offered_at: job.offered_at,
      expires_at: job.expires_at,
      status: "awaiting_download",
      provider_hosts: [...job.provider_hosts],
      ...(job.adapter_id === undefined ? {} : { adapter_id: job.adapter_id }),
      ...(job.expected === undefined ? {} : { expected: { ...job.expected } }),
    };
    await this.removeJobWithOffer(jobID);
    await this.upsertJobWithoutOffer(retained);
  }

  /** A record left behind by `retainForManualDownload`, recognised by shape
   * rather than by a worker-memory set: the set would be empty after an MV3
   * teardown while the record itself is persisted, which is the one moment
   * reconciliation has to be able to retire a stale window.
   *
   * The conjunction is exact. A live handoff has a tab or a retained offer
   * URL; a delivery has its `pendingDelivery`/`deliveryJobs` entry; a job with
   * a download in flight is in `downloads`; anything papio may still drive
   * itself carries `access_mode`. */
  private isManualDownloadWindow(job: ActiveJob): boolean {
    return (
      job.tab_id < 0 &&
      job.status === "awaiting_download" &&
      job.access_mode === undefined &&
      this.offerURLs.get(job.job_id) === undefined &&
      !this.downloads.has(job.job_id) &&
      !this.deliveryJobs.has(job.job_id) &&
      this.store.pendingDelivery?.job_id !== job.job_id
    );
  }

  /** Re-derive which manual-download correlation windows are still live from
   * the daemon's own view of what is still open.
   *
   * This is also the restart path. The window record itself is persisted, so
   * an MV3 teardown does not lose it and the startup drive scan leaves it
   * alone; what a restart does lose is any right to believe it. The first
   * complete snapshot after the worker comes back either reconfirms the
   * action — the window keeps steering — or reports it closed, in which case
   * the record is dropped rather than left to claim an unrelated download
   * from the same provider months later.
   *
   * Only a complete first page is authority. A cursored page, a page with
   * `has_more`, one the daemon could not fully render at the negotiated
   * schema, or one carrying an item this cannot read describes a subset of
   * the open actions, and retiring against a subset would drop live windows.
   * Identification is structural — schema-3+ `route_class` first, the older
   * `action_kind` field otherwise — never the action's prose detail. */
  private async reconcileManualDownloadWindows(
    snapshot: Record<string, unknown>,
    paged: boolean,
  ): Promise<void> {
    if (paged || snapshot["has_more"] !== false) return;
    if (snapshot["unsupported_items_count"] !== 0) return;
    const items = snapshot["items"];
    if (!Array.isArray(items)) return;
    const open = new Set<string>();
    for (const entry of items) {
      if (typeof entry !== "object" || entry === null) return;
      const item = entry as Record<string, unknown>;
      if (item["kind"] !== "human_action") continue;
      const jobID = item["job_id"];
      if (typeof jobID !== "string" || jobID.length === 0) continue;
      const routeClass = item["route_class"];
      const kind =
        typeof routeClass === "string" ? routeClass : item["action_kind"];
      if (kind === "manual_download") open.add(jobID);
    }
    const stale = this.store.activeJobs
      .filter(
        (job) => this.isManualDownloadWindow(job) && !open.has(job.job_id),
      )
      .map((job) => job.job_id);
    for (const jobID of stale) await this.removeJobWithOffer(jobID);
  }

  private async applyVerdict(
    jobID: string,
    spec: AdapterSpec,
    plan: Plan,
    host: string,
  ): Promise<void> {
    const job = findByJob(this.store, jobID);
    if (!job) return;
    const verdict = plan.verdict;
    const av = spec.version;
    if (verdict.kind !== "unknown" && (job.unknown_count ?? 0) !== 0) {
      // Any decisive verdict breaks the unknown streak.
      await this.update((s) => patchJob(s, jobID, { unknown_count: 0 }));
    }

    switch (verdict.kind) {
      case "article": {
        // §2.2.1 entitled_landing (Slice 3): confirms the landing is
        // actually on entitled content, not just an IdP redirect-back.
        // Latched — the daemon-side scheduler this triggers only needs to
        // hear it once per grant.
        void this.emitClaimObservation(jobID, job.tab_id, "entitled_landing", true);
        const dl = spec.download;
        // A decisive `article` verdict on a page whose adapter declares a
        // download is papio's whole job. Every way of declining it below was
        // silent, which is how a fully-rendered open-access article sat on
        // screen for days, correctly identified 15 times over, while papio
        // reported nothing to the daemon, the popup, or the console. Naming the
        // reason costs nothing on the wire and is the difference between a
        // diagnosable decline and an invisible one.
        const declineDownload = (reason: string): void => {
          console.error(
            `papio: adapter download declined for ${jobID} on ${host}: ${reason}`,
          );
        };
        if (!this.hasDelegatedAuthority(job) && dl !== undefined) {
          declineDownload(
            `job is not delegated (access_mode=${String(job.access_mode)})`,
          );
          return;
        }
        // Assisted jobs may classify and capture, but no adapter-declared
        // control or derived URL may cause a browser download.
        if (job.access_mode === "assisted" && dl !== undefined) {
          declineDownload("assisted jobs never cause a browser download");
          return;
        }
        if (dl === undefined) return;
        if (plan.method !== dl.method) {
          declineDownload(
            `plan method ${String(plan.method)} does not match the adapter's ${dl.method}`,
          );
          return;
        }
        if (plan.target_ref === null) {
          declineDownload("plan resolved no download target");
          return;
        }
        if (job.download_initiated === true) {
          // The latch is persisted while `this.downloads` is worker memory, so
          // a download that was claimed and then lost with its worker leaves
          // this true forever and every later classify skips the grab in
          // silence. Only an adapter with a terms gate had a path back
          // (maybeClassify's awaitingTermsGate); everything else was stuck.
          declineDownload(
            `a download is already latched (in flight locally: ${String(this.downloads.has(jobID))})`,
          );
          return;
        }
        if (
          dl.method === "click" &&
          this.deps.downloads.onDeterminingFilename === undefined
        ) {
          declineDownload("click downloads need onDeterminingFilename");
          return;
        }
        {
          if (
            (dl.method === "url" ||
              dl.method === "api" ||
              dl.method === "meta") &&
            dl.requiresTermsConsent === true
          ) {
            const consent = await this.deps.settings.getTermsConsent();
            if (consent !== "accept") {
              // The direct-endpoint fetch bypasses the publisher terms UI, so
              // gate it on recorded consent to auto-accept terms. Without
              // consent, prompt once and stay assisted — no fetch, no latch.
              this.send(
                "provider_outcome",
                {
                  outcome: "terms_acceptance_required",
                  adapter_id: spec.id,
                  adapter_version: av,
                },
                jobID,
              );
              if (consent === undefined) {
                await this.update((s) =>
                  patchJob(s, jobID, { needs_terms_consent: true }),
                );
              }
              return;
            }
          }
          // Re-run the planner immediately before the effect. A changed
          // target, verdict, or URL loses authority and stays assisted.
          let freshPlan: Plan | undefined;
          try {
            const results = await this.deps.scripting.executeScript({
              target: { tabId: job.tab_id },
              func: planExecution,
              args: [
                null,
                spec,
                { ...(job.expected ?? {}) },
                job.access_mode === undefined
                  ? {}
                  : { access_mode: job.access_mode },
              ],
            });
            const fresh = results[0]?.result as PlanResult | undefined;
            if (
              fresh !== undefined &&
              typeof fresh === "object" &&
              fresh !== null &&
              !("assisted" in fresh)
            ) {
              freshPlan = fresh;
            }
          } catch (e) {
            console.error(
              "papio: execution plan revalidation failed; staying assisted",
              e,
            );
            return;
          }
          if (freshPlan === undefined) {
            declineDownload("the page no longer plans");
            return;
          }
          const revalidated = freshPlan;
          const drift =
            revalidated.verdict.kind !== plan.verdict.kind
              ? "verdict"
              : revalidated.decisive_rule !== plan.decisive_rule
                ? "decisive rule"
                : revalidated.method !== plan.method
                  ? "method"
                  : revalidated.url !== plan.url
                    ? "url"
                    : JSON.stringify(revalidated.target_ref) !==
                        JSON.stringify(plan.target_ref)
                      ? "target"
                      : JSON.stringify(revalidated.expected_work) !==
                          JSON.stringify(plan.expected_work)
                        ? "work evidence"
                        : JSON.stringify(revalidated.effect_graph) !==
                            JSON.stringify(plan.effect_graph)
                          ? "effect graph"
                          : revalidated.route_origin !== plan.route_origin
                            ? "route origin"
                            : revalidated.access_mode !== plan.access_mode
                              ? "access mode"
                              : JSON.stringify(revalidated.revalidation) !==
                                  JSON.stringify(plan.revalidation)
                                ? "revalidation limits"
                                : undefined;
          if (drift !== undefined) {
            // The re-plan runs one injection round trip after the first, so a
            // page still settling can move a positional fingerprint under it.
            // Losing authority is correct; losing it silently is not.
            declineDownload(`plan revalidation drifted: ${drift}`);
            return;
          }
          if (
            (dl.method === "click" || dl.method === "api") &&
            freshPlan.target_ref === null
          ) {
            declineDownload("revalidated plan lost its target");
            return;
          }
          const currentAuthority = findByJob(this.store, jobID);
          if (!this.hasDelegatedAuthority(currentAuthority)) {
            declineDownload("job stopped being delegated during revalidation");
            return;
          }
          const freshTarget = freshPlan.target_ref;
          if (freshTarget === null) {
            declineDownload("revalidated plan has no target");
            return;
          }
          const effectToken = this.claimEffectGovernor(jobID);
          if (effectToken === undefined) {
            // Another page mutation or download owns the one-effect slot.
            // Reclassify after it settles; silently returning loses this
            // adapter/job correlation when providers share a host family.
            this.scheduleClassifyRetry(jobID, "effect");
            return;
          }
          // Do not latch or invoke page code without a concrete, validated
          // target. Click effects must reserve first because the page mutation
          // itself can open a download; URL effects reserve after their
          // page-side URL has been validated.
          let governorHeld = true;
          let claimedDownload = false;
          try {
            if (dl.method === "click") {
              claimedDownload = await this.claimDownloadInitiated(jobID);
              if (!claimedDownload) {
                declineDownload("another path already claimed the download");
                return;
              }
            }
            const result = await this.deps.scripting.executeScript({
              target: { tabId: job.tab_id },
              func: executePlannedPageEffect,
              args: [freshPlan, dl],
            });
            const effect = result[0]?.result as
              { ok?: boolean; url?: string; why?: string } | undefined;
            if (effect?.ok !== true) {
              if (claimedDownload) {
                await this.update((s) =>
                  patchJob(s, jobID, { download_initiated: false }),
                );
              }
              declineDownload(
                `the page-side effect refused: ${effect?.why ?? "no reason reported"}`,
              );
              return;
            }
            if (dl.method === "click") {
              // The click itself is the observable initiation consequence.
              // Release before a post-click reclassification so a late terms
              // gate can acquire the same global slot independently.
              this.releaseEffectGovernor(jobID, effectToken);
              governorHeld = false;
              if (
                dl.postClickWaitFor !== undefined ||
                dl.followupSelector !== undefined
              ) {
                await this.reclassifyCurrentProviderPage(jobID);
              }
              return;
            }
            const url = effect.url;
            if (typeof url !== "string" || !url.startsWith("https://")) {
              declineDownload(
                `the effect returned no https url (${typeof url})`,
              );
              return;
            }
            claimedDownload = await this.claimDownloadInitiated(jobID);
            if (!claimedDownload) {
              declineDownload("the download latch was taken concurrently");
              return;
            }
            this.pendingDownloadURLs.set(url, jobID);
            try {
              const id = await this.deps.downloads.download({
                url,
                filename: jobDownloadFilename(jobID),
                conflictAction: "uniquify",
                saveAs: false,
              });
              this.downloads.set(jobID, {
                ids: new Set([id]),
                ambiguous: false,
                directOffer: false,
              });
            } finally {
              this.pendingDownloadURLs.delete(url);
            }
          } catch (e) {
            if (claimedDownload) {
              await this.update((s) =>
                patchJob(s, jobID, { download_initiated: false }),
              );
            }
            console.error(
              "papio: adapter download initiation failed; staying assisted",
              e,
            );
          } finally {
            if (governorHeld) this.releaseEffectGovernor(jobID, effectToken);
          }
        }
        return;
      }
      case "login": {
        if (!this.hasDelegatedAuthority(job)) return;
        const effectToken = this.claimEffectGovernor(jobID);
        if (effectToken === undefined) return;
        try {
          // A provider login wall. If the adapter has a federated-login route
          // and the offer carried the institution entityID, auto-select the
          // institution by navigating the handoff tab straight to the IdP
          // (skipping the provider's picker); the human still enters
          // credentials there. Then stay auth_pending, emit nothing.
          // Prefer the autonomous account-id unlock; fall back to federated login.
          if (!(await this.maybeAppendAccountId(jobID, job, spec))) {
            await this.maybeRouteFederatedLogin(jobID, job, spec);
          }
        } finally {
          this.releaseEffectGovernor(jobID, effectToken);
        }
        return;
      }
      case "terms": {
        // Consent is observational policy, so read it before claiming the
        // effect governor. The value is then carried through the guarded
        // acceptance closure and reused for the assisted fallback below.
        const consent = await this.deps.settings.getTermsConsent();
        if (!this.hasDelegatedAuthority(job)) {
          if (job.access_mode === "assisted") {
            this.send(
              "provider_outcome",
              {
                outcome: "terms_acceptance_required",
                adapter_id: spec.id,
                adapter_version: av,
              },
              jobID,
            );
            if (consent === undefined) {
              await this.update((s) =>
                patchJob(s, jobID, { needs_terms_consent: true }),
              );
            }
          }
          return;
        }
        if (consent === "accept" && spec.termsAccept) {
          const effectToken = this.claimEffectGovernor(jobID);
          if (effectToken === undefined) {
            this.scheduleClassifyRetry(jobID, "effect");
            return;
          }
          try {
            const alreadyClaimed = job.download_initiated === true;
            const claimedDownload =
              alreadyClaimed || (await this.claimDownloadInitiated(job.job_id));
            if (!claimedDownload) return;
            const termsResult = await this.acceptTerms(job.job_id, spec);
            if (termsResult === "accepted") {
              // The accept click opens the provider PDF (often in a new viewer
              // tab), which the download / viewer-adoption path captures.
              return;
            }
            if (termsResult === "occupied") return;
            if (!alreadyClaimed && !this.downloads.has(job.job_id)) {
              await this.update((s) =>
                patchJob(s, jobID, { download_initiated: false }),
              );
            }
          } finally {
            this.releaseEffectGovernor(jobID, effectToken);
          }
        }
        this.send(
          "provider_outcome",
          {
            outcome: "terms_acceptance_required",
            adapter_id: spec.id,
            adapter_version: av,
          },
          jobID,
        );
        // First terms gate with no recorded choice: flag for the popup's
        // one-time informed-consent prompt.
        if (consent === undefined) {
          await this.update((s) =>
            patchJob(s, jobID, { needs_terms_consent: true }),
          );
        }
        return;
      }
      case "no_entitlement":
        if (
          this.send(
            "provider_outcome",
            {
              outcome: "no_entitlement",
              adapter_id: spec.id,
              adapter_version: av,
            },
            jobID,
          )
        ) {
          await this.settleHandoffAfterOutcome(jobID, "no_entitlement");
        }
        return;
      case "wrong_work":
      case "wrong_work_check": {
        // Not for the drift latch — recordProviderLatch maps wrong_work to
        // no_positive_effects and only a drift latch stores a host. This names
        // the page on the durable provider_outcome event, which is what makes
        // "papio reached a different work" diagnosable at all.
        const wrongWorkHost = await this.reportableHost(job.tab_id, host);
        if (
          this.send(
            "provider_outcome",
            {
              outcome: "wrong_work",
              adapter_id: spec.id,
              adapter_version: av,
              ...(wrongWorkHost === undefined ? {} : { host: wrongWorkHost }),
            },
            jobID,
          )
        ) {
          await this.settleHandoffAfterOutcome(jobID, "wrong_work");
        }
        return;
      }
      case "unknown": {
        const now = this.deps.now();
        const settled =
          (job.unknown_count ?? 0) >= 1 &&
          now - (job.last_unknown_ms ?? 0) >= 5000;
        await this.recordUnknown(job, host, spec, settled);
        if (!settled) return;
        const current = findByJob(this.store, jobID);
        if (
          current !== undefined &&
          (await this.runGenericOnSettledUnknown(current))
        )
          return;
        const evidence = this.genericEvidence.get(jobID) ?? [];
        const detail =
          evidence.length === 0
            ? undefined
            : `Generic evidence: ${evidence.join(", ")}.`;
        const outcomeKey = `${jobID}:ui_changed`;
        if (!this.handoffOutcomeSent.has(outcomeKey)) {
          this.handoffOutcomeSent.add(outcomeKey);
          // A real drift the daemon latches, so it needs attribution as much
          // as the deferred branch — but only if the tab is still on the page
          // that was classified.
          const reported = await this.reportableHost(job.tab_id, host);
          if (
            !this.send(
              "provider_outcome",
              {
                outcome: "ui_changed",
                adapter_id: spec.id,
                adapter_version: av,
                ...(reported === undefined ? {} : { host: reported }),
                ...(detail === undefined ? {} : { detail }),
              },
              jobID,
            )
          ) {
            this.handoffOutcomeSent.delete(outcomeKey);
          } else {
            await this.settleHandoffAfterOutcome(jobID, "ui_changed");
          }
        }
        return;
      }
    }
  }

  /** Chrome may not have populated the tab's URL by the time onActivated
   * fires (e.g. a brand-new tab still loading its first document); look it
   * up rather than trust the event payload. A tab that vanished between the
   * event and the lookup is not evidence of anything — swallow and drop. */
  private async onTabActivated(tabID: number): Promise<void> {
    const papioFocused = this.consumePapioFocusToken(tabID);
    let tab: TabInfo;
    try {
      tab = await this.deps.tabs.get(tabID);
    } catch {
      return;
    }
    this.keepaliveManager?.noteResolverActivated(tabID, tab.url);
    if (!papioFocused) {
      // Positive operator takeover, recorded where it happens rather than
      // inferred later from tab.active - which papio itself sets.
      await this.cedeOwnedTabIfOwned(tabID);
    }
    if (tab.windowId === undefined) return;
    const previous = this.lastActiveTabByWindow.get(tab.windowId);
    this.lastActiveTabByWindow.set(tab.windowId, tabID);
    if (previous !== undefined && previous !== tabID) {
      // The surface that just lost the foreground is the event-driven moment
      // a retained-because-active owned tab becomes retirable.
      await this.retireDeactivatedSurface(previous);
    }
  }

  /** Mark an owned, unceded, current-epoch record as taken over. A tab papio
   * does not own is not touched. */
  private async cedeOwnedTabIfOwned(tabID: number): Promise<void> {
    const ledger = await this.snapshotTabLedger();
    const record = ledger[String(tabID)];
    if (
      record === undefined ||
      record.ceded === true ||
      record.browser_epoch !== this.browserEpoch
    )
      return;
    await this.cedeOwnedTab(tabID, record.binding_id, "operator_activated");
  }

  /** Retire one owned surface whose job papio no longer tracks, now that it
   * is no longer the foreground tab. Same predicate reconcileOwnedTabs uses;
   * the daemon still authorizes (or refuses) the close, one use per attempt. */
  private async retireDeactivatedSurface(tabID: number): Promise<void> {
    const ledger = await this.snapshotTabLedger();
    const record = ledger[String(tabID)];
    if (
      record === undefined ||
      record.ceded === true ||
      record.job_id === undefined ||
      record.browser_epoch !== this.browserEpoch ||
      findByJob(this.store, record.job_id) !== undefined
    )
      return;
    await this.closeOwnedSurface(tabID, "job_inactive");
  }

  private async onTabRemoved(tabID: number): Promise<void> {
    await this.ready;
    this.destroyDeliveryChoiceForTab(tabID);
    this.pageNavSeq.delete(tabID);
    this.navigationErrors.delete(tabID);
    this.clearNavigationErrorMarker(tabID);
    // If pendingDelivery waiting_manual was bound to this tab, clear it — a later
    // page in the same tab_id must never revive authority.
    if (
      this.store.pendingDelivery?.status === "waiting_manual" &&
      this.store.pendingDelivery.page_identity?.tab_id === tabID
    ) {
      await this.update((s) => clearPendingDelivery(s, s.pendingDelivery?.job_id));
    }
    const pageCaptureWaiter = this.pageCaptureLoadWaiters.get(tabID);
    if (pageCaptureWaiter !== undefined) pageCaptureWaiter(false);
    this.authCountedTabs.delete(tabID);
    const wasBadgedAuthWall = this.lastBadgedAuthWallTabs.delete(tabID);
    // Serialize against any bind-response `persistClaimIdentity` already
    // queued on the ledger chain. Reading tabLedgerCache directly here can
    // race that write: a tab may close between the bind response and the
    // awaited persistence, and the job-inactive recovery branch would then
    // miss the only durable claim identity before `forgetLedgeredTab` queues
    // behind it. snapshotTabLedger waits for earlier ledger transactions and
    // still falls back to the cache when durable ledger support is absent.
    const ledgerSnapshot = await this.snapshotTabLedger();
    const ledgerRecord =
      ledgerSnapshot[String(tabID)] ??
      this.tabLedgerCache?.[String(tabID)];
    const pendingClose = ledgerRecord?.pending_close;
    const authorizedClose = pendingClose !== undefined;
    // Whether this close abandoned an institutional materialization claim. Set
    // by BOTH owner_closed reporters below — the live-grant path and the
    // restart-recovered path, which reports from the durable birth record after
    // its worker died. Read by the toast, which must offer a NEW sign-in rather
    // than a reopen for exactly this case: `owner_closed` retires the
    // authentication-entry lease and consumes the one-use close authorization,
    // so there is no reversal left to offer.
    let institutionalClaimAbandoned = false;
    // Consumed exactly once, whatever happens below: this worker removed the
    // tab itself as housekeeping, so the removal is not the operator giving up.
    const deliberate = this.deliberateRemovals.delete(tabID);
    const ownerBindingID = ledgerRecord?.binding_id;
    const ledgerJobID = ledgerRecord?.job_id;
    // Read before forgetLedgeredTab erases the record: after a worker
    // restart this is the ONLY surviving proof of which claim this surface
    // owned, and MV3 sleeps the worker after ~30s idle, so a sign-in tab
    // abandoned minutes later is the common case, not the edge one.
    const durableClaim = ledgerRecord?.claim;
    const job = findByTab(this.store, tabID);
    if (!job) {
      // job_inactive detaches browser-local job state BEFORE asking to close.
      // The tab is gone now: this is the physical evidence owner_closed
      // represents, so report it from the ledger identity instead of
      // returning early. The reducer consumes the one-use token and retires
      // the exact authentication-entry binding; releasing it at authorization
      // time would let a sibling open before this surface actually closed.
      // A claim_abandoned tombstone already has its owner_closed counterpart
      // in the outbox; only the job_inactive tombstone (or no tombstone) needs
      // this physical-loss report.
      if (
        pendingClose?.disposition !== "claim_abandoned" &&
        ledgerRecord?.ceded !== true &&
        ledgerJobID !== undefined &&
        ownerBindingID !== undefined &&
        durableClaim !== undefined
      ) {
        institutionalClaimAbandoned = true;
        this.enqueueRestartRecoveredObservation(
          {
            job_id: ledgerJobID,
            authentication_claim_id: durableClaim.authentication_claim_id,
            binding_id: ownerBindingID,
            browser_holder_generation: durableClaim.browser_holder_generation,
            gate_occurrence_id: durableClaim.gate_occurrence_id,
          },
          "owner_closed",
        );
      }
      // The observation MUST be enqueued before this deletion: after an
      // extension update this birth record is the only durable proof of the
      // claim, and a worker crash between the two must not lose the report.
      void this.forgetLedgeredTab(tabID);
      if (wasBadgedAuthWall) await this.syncConnectionBadge();
      return;
    }
    void this.forgetLedgeredTab(tabID);
    // Slice 3 owner_closed: the owning surface closed without success — a
    // deliberate daemon-authorized close (surface_close_request already told
    // the daemon under its own disposition) and a job that already reached
    // awaiting_download both report nothing here; every other loss of a
    // claim-owned tab is exactly the observation §2.2.1 defines.
    if (
      !authorizedClose &&
      !deliberate &&
      ownerBindingID !== undefined &&
      job.status !== "awaiting_download"
    ) {
      const grant = this.claimGrants.get(job.job_id);
      if (grant !== undefined) {
        institutionalClaimAbandoned = true;
        this.enqueueClaimObservation(job.job_id, ownerBindingID, "owner_closed");
        // §2.2.1's owner_closed reducer counterpart: authorize the now-gone
        // surface's abandonment. The tab is already gone, so there is nothing
        // to tombstone locally — either outcome (authorized or not_eligible)
        // leaves this job's state exactly as clearClaimGrant below sets it.
        void this.requestCloseAuthorization(
          ownerBindingID,
          "claim_abandoned",
          grant.gateOccurrenceID,
        );
      } else if (durableClaim !== undefined) {
        // Restart-recovered: the grant died with its worker, so the identity
        // comes from the birth record instead. Without this the claim keeps
        // the institution's one login slot until the daemon's lease expires,
        // and every sibling parks tablessly in the meantime — the stranded
        // sign-in this whole effort exists to prevent.
        institutionalClaimAbandoned = true;
        this.enqueueRestartRecoveredObservation(
          {
            job_id: job.job_id,
            authentication_claim_id: durableClaim.authentication_claim_id,
            binding_id: ownerBindingID,
            browser_holder_generation: durableClaim.browser_holder_generation,
            gate_occurrence_id: durableClaim.gate_occurrence_id,
          },
          "owner_closed",
        );
        void this.requestCloseAuthorization(
          ownerBindingID,
          "claim_abandoned",
          durableClaim.gate_occurrence_id,
        );
      }
    }
    this.clearClaimGrant(job.job_id);
    if (authorizedClose || deliberate) {
      // Deliberate: either a daemon-authorized close (closeOwnedSurface) or
      // this worker's own reconcile removal. Detach the job from its now-gone
      // tab without emitting provider_outcome/cancellation or tearing the job
      // down — the daemon learns the true set of live surfaces from the
      // reconcile round trip, not from a cancellation it never asked for.
      await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
      return;
    }
    this.releaseHandoffDrive(job.job_id);
    if (this.classifyRetries.delete(job.job_id)) {
      // The close has already settled the in-flight classification lifecycle;
      // retain the durable handoff for the daemon's cancellation/re-offer path
      // but never emit a second provider outcome from the dead-tab retry.
      await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
      await this.drainHandoffDriveQueue();

      return;
    }

    // ADR-0023's seventh surface, raised here and nowhere else. Placement is
    // load-bearing: it sits AFTER the deliberate/authorized-close return above,
    // because papio closing its own tab is not a loss to report, and after the
    // classify-retry return, whose lifecycle the close already settled. The
    // branches below decide what papio DOES about the loss; they do not change
    // what was lost, so one call serves all of them and `toastKindForLoss`
    // answers `undefined` for the two that lost nothing.
    void this.reportLostSurface(job, institutionalClaimAbandoned);

    // (waiting_for_session) never had a chance to sign in on its own — its
    // tab closing is not the operator abandoning the job, just losing the
    // page it was quietly waiting on. Re-enter it as an ordinary queued
    // drive (exactly reconcileTabs's dead-pre-download-tab recovery) so
    // recordFreshSessionEvidence's existing queued-release path — not a
    // second, redundant resume mechanism — is what reopens it.
    if (job.waiting_for_session === true) {
      this.beginProviderDrive(job.job_id);
      await this.update((s) =>
        patchJob(s, job.job_id, {
          tab_id: -1,
          status: "queued",
          waiting_for_session: false,
          waiting_for_session_key: undefined,
          parked_with_tab: false,
          download_initiated: false,
          unknown_count: 0,
        }),
      );
      this.scheduleQueuedHandoffRelease(job.job_id);
      return;
    }
    if (
      this.deliveryJobs.has(job.job_id) ||
      this.store.pendingDelivery?.job_id === job.job_id
    ) {
      // The browser download is independent of the source tab. Keep its exact
      // correlation and pending record alive when the operator closes the PDF.
      await this.update((s) => patchJob(s, job.job_id, { tab_id: -1 }));
      await this.drainHandoffDriveQueue();
      return;
    }
    // Once the user is past authentication (awaiting_download), a closed tab is
    // NOT a cancel: a download may be in flight or already saved into the job's
    // adoption directory, where the daemon's poll-scan adopts it. Park it, as
    // onTabRemoved would have.
    if (job.status === "awaiting_download") {
      this.completedDownloadTabs.delete(job.job_id);
      await this.removeJobWithOffer(job.job_id);
      return;
    }
    this.send("provider_outcome", { outcome: "cancelled" }, job.job_id);
    this.downloads.delete(job.job_id);
    this.completedDownloadTabs.delete(job.job_id);
    await this.removeJobWithOffer(job.job_id);
  }

  /** A DOI in the download route is stronger than a provider-host inference.
   * It lets a manual-download window survive a resolver-to-publisher hop
   * without treating every future download from that publisher as this job.
   *
   * `null` is a refusal, not "no match": conflicting URL fields, an internally
   * rejected DOI-bearing URL, no matching pending DOI, or duplicate jobs must
   * stop before the weaker host fallback can claim the file. */
  private manualDownloadJobForDOI(
    item: DownloadItemLike,
  ): ActiveJob | null | undefined {
    const windows = this.store.activeJobs.filter((job) =>
      this.isManualDownloadWindow(job),
    );
    if (windows.length === 0) return undefined;
    const observed = new Set<string>();
    for (const value of [item.referrer, item.finalUrl, item.url]) {
      if (typeof value !== "string" || value.length === 0) continue;
      const doi = doiFromURL(value);
      if (doi !== undefined) {
        observed.add(doi.toLowerCase());
        continue;
      }
      let decoded = value;
      try {
        decoded = decodeURIComponent(value);
      } catch {
        // The raw value remains safe to inspect for a DOI-shaped signal.
      }
      if (/10\.\d{4,9}(?:\/|%2f)/i.test(decoded)) return null;
    }
    if (observed.size === 0) return undefined;
    if (observed.size !== 1) return null;
    const doi = observed.values().next().value;
    if (doi === undefined) return null;
    const matches = windows.filter(
      (job) => job.expected?.doi?.trim().toLowerCase() === doi,
    );
    return matches.length === 1 ? matches[0] : null;
  }

  private correlate(item: DownloadItemLike): ActiveJob | undefined {
    // Firefox cannot relocate native/manual downloads into papio/<job>. Only
    // exact IDs/URLs registered by downloads.download are safe there; those
    // bypass this broad tab/host correlation path.
    if (this.deps.downloads.onDeterminingFilename === undefined)
      return undefined;
    const byDOI = this.manualDownloadJobForDOI(item);
    if (byDOI === null) return undefined;
    if (byDOI !== undefined) return byDOI;
    if (
      typeof item.tabId === "number" &&
      Number.isSafeInteger(item.tabId) &&
      item.tabId >= 0
    ) {
      const byTab = findByTab(this.store, item.tabId);
      if (byTab) {
        if (this.isFirefoxClickDownload(byTab)) return undefined;
        return byTab;
      }
    }
    let host: string | undefined;
    for (const value of [item.referrer, item.finalUrl, item.url]) {
      if (typeof value !== "string" || value.length === 0) continue;
      try {
        host = new URL(value).hostname;
        break;
      } catch {
        // Another browser-supplied URL field can still be valid.
      }
    }
    if (host === undefined) return undefined;
    const initiated = this.store.activeJobs.filter((job: ActiveJob) => {
      if (
        this.isFirefoxClickDownload(job) ||
        job.download_initiated !== true ||
        job.adapter_id === undefined
      )
        return false;
      const spec = this.deps.adapterSpecs.find(
        (candidate) => candidate.id === job.adapter_id,
      );
      return spec !== undefined && hostMatches(host, spec.hosts);
    });

    if (initiated.length === 1) return initiated[0];
    if (initiated.length > 1) return undefined;
    const matches = this.store.activeJobs.filter(
      (job: ActiveJob) =>
        !this.isFirefoxClickDownload(job) &&
        this.matchesManualDownloadHost(job, host),
    );
    return matches.length === 1 ? matches[0] : undefined;
  }
  private institutionalDownloadAttempt(
    job: ActiveJob,
    item: DownloadItemLike,
  ): InstitutionalDownloadAttempt | undefined {
    if (item.tabId === undefined) return undefined;
    const correlation = this.materializationCorrelation(job.job_id);
    if (
      correlation === undefined ||
      (correlation.phase !== "navigating" &&
        correlation.phase !== "navigated") ||
      correlation.tab_id !== item.tabId ||
      typeof correlation.claim_id !== "string" ||
      typeof correlation.binding_id !== "string" ||
      typeof correlation.effect_ordinal !== "number" ||
      !Number.isSafeInteger(correlation.effect_ordinal) ||
      correlation.effect_ordinal < 1 ||
      typeof correlation.institutional_request_id !== "string"
    ) {
      return undefined;
    }
    return {
      claim_id: correlation.claim_id,
      binding_id: correlation.binding_id,
      effect_ordinal: correlation.effect_ordinal,
      institutional_request_id: correlation.institutional_request_id,
    };
  }

  private async onDownloadCreated(item: DownloadItemLike): Promise<void> {
    // A download papio itself started for a job outranks any grab, and must be
    // classified first. The grab check used to run before this one and returned
    // early, so a click-adapter download whose route matched an armed grab was
    // recorded as that grab's bytes and never bound to its own job — the two
    // routes differ only in a signed query string, which `sameDownloadRoute`
    // deliberately ignores. That is the wrong-paper direction: job X's file
    // steered into grab Y's directory.
    // Chrome may call onDeterminingFilename before this async handler resumes
    // (and before downloads.download returns), so bind exact pending URLs here
    // synchronously. Cross-origin redirects then steer by ID, not stale URL
    // correlation; ambiguity parking below still applies only when there is no
    // exact binding yet.
    const syncJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    if (syncJobID !== undefined) {
      const sync = this.downloads.get(syncJobID) ?? {
        ids: new Set<number>(),
        ambiguous: false,
        directOffer: false,
      };
      sync.ids.add(item.id);
      if (sync.ids.size > 1) sync.ambiguous = true;
      this.downloads.set(syncJobID, sync);
    }
    await this.ready;
    const earlyJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    if (earlyJobID !== undefined) {
      const early = this.downloads.get(earlyJobID) ?? {
        ids: new Set<number>(),
        ambiguous: false,
        directOffer: false,
      };
      early.ids.add(item.id);
      if (early.ids.size > 1) early.ambiguous = true;
      this.downloads.set(earlyJobID, early);
    } else {
      const conflict = this.downloadGrabConflict(item);
      if (conflict !== undefined) {
        this.surfaceDownloadGrabConflict(item.id, conflict);
        return;
      }
      const pendingGrab = this.pendingGrabFor(item);
      if (pendingGrab !== undefined) {
        pendingGrab.ids.add(item.id);
        const grabID =
          this.trackedGrabFor(item.id) ?? this.pendingGrabIDFor(item);
        if (grabID !== undefined) {
          this.grabDownloads.set(grabID, pendingGrab);
        }
        return;
      }
    }
    if (
      this.store.pendingDelivery?.status === "waiting_manual" &&
      this.store.pendingDelivery.page_identity !== undefined
    ) {
      const pi = this.store.pendingDelivery.page_identity;
      // Claiming a manual download files these bytes under a paper the
      // researcher picked against one specific document. MV3 may have
      // suspended and restarted the worker since, so every check below reads
      // the browser rather than worker-local memory, and anything that cannot
      // be read refuses instead of assuming nothing moved.
      const abandonContinuation = async (): Promise<void> => {
        await this.update((s) => clearPendingDelivery(s, s.pendingDelivery?.job_id));
        this.destroyDeliveryChoiceState();
      };
      const pendingJob = findByJob(this.store, this.store.pendingDelivery.job_id);
      if (pendingJob === undefined || pendingJob.status !== "awaiting_download") {
        await abandonContinuation();
        return;
      }
      // No recorded epoch means the continuation cannot be revalidated at all:
      // the source token is origin+pathname, which two different signed
      // documents from one provider path share exactly.
      if (pi.document_id === undefined) {
        await abandonContinuation();
        return;
      }
      let liveTab: TabInfo;
      try {
        liveTab = await this.deps.tabs.get(pi.tab_id);
      } catch {
        await abandonContinuation();
        return;
      }
      if (
        typeof liveTab.url !== "string" ||
        liveTab.url.length === 0 ||
        this.pageSourceToken(liveTab.url) !== pi.source_url
      ) {
        await abandonContinuation();
        return;
      }
      const liveEpoch = await this.liveDocumentEpoch(pi.tab_id);
      if (liveEpoch === undefined || liveEpoch !== pi.document_id) {
        await abandonContinuation();
        return;
      }
      const liveNavSeq = this.pageNavSeq.get(pi.tab_id);
      if (liveNavSeq !== undefined && liveNavSeq !== pi.nav_seq) {
        await abandonContinuation();
        return;
      }
      if (typeof item.tabId === "number" && item.tabId !== pi.tab_id) {
        await abandonContinuation();
        return;
      }
    }
    const exactJobID = this.trackedJobFor(item.id) ?? this.pendingJobFor(item);
    const job =
      exactJobID === undefined
        ? this.correlate(item)
        : findByJob(this.store, exactJobID);
    if (!job) return;
    if (job.download_initiated !== true) {
      await this.update((s) =>
        patchJob(s, job.job_id, { download_initiated: true }),
      );
    }
    const track = this.downloads.get(job.job_id) ?? {
      ids: new Set<number>(),
      ambiguous: false,
      directOffer: false,
    };
    const institutional = this.institutionalDownloadAttempt(job, item);
    if (track.institutional === undefined && institutional !== undefined)
      track.institutional = institutional;
    track.ids.add(item.id);
    if (track.ids.size > 1) track.ambiguous = true;
    this.downloads.set(job.job_id, track);
  }

  /** Return the complete URL-free identity of the effect that produced this
   * download. The daemon feature gate is mandatory: older strict parsers
   * reject the additive field, and an unnegotiated identity is not authority. */
  private artifactProducer(
    track: DownloadTrack,
  ): ArtifactProducerPayload | undefined {
    if (!(this.store.daemonFeatures ?? []).includes(EFFECT_PERMIT_FEATURE))
      return undefined;
    const generic = track.generic?.epoch;
    if (
      generic !== undefined &&
      typeof generic.revision === "string" &&
      generic.revision !== ""
    ) {
      return {
        effect_kind: "generic_drive",
        drive_attempt_id: generic.drive_attempt_id,
        ordinal: generic.ordinal,
        strategy: "generic",
        revision: generic.revision,
      };
    }
    const direct = track.directEpoch;
    if (
      direct !== undefined &&
      typeof direct.route_revision === "string" &&
      direct.route_revision !== ""
    ) {
      return {
        effect_kind: "direct_get",
        drive_attempt_id: direct.drive_attempt_id,
        ordinal: direct.ordinal,
        strategy: "direct_get",
        revision: direct.route_revision,
      };
    }
    const institutional = track.institutional;
    if (institutional !== undefined) {
      return {
        effect_kind: "institutional",
        claim_id: institutional.claim_id,
        binding_id: institutional.binding_id,
        effect_ordinal: institutional.effect_ordinal,
        institutional_request_id: institutional.institutional_request_id,
      };
    }
    return undefined;
  }

  private async onDownloadChanged(delta: DownloadDeltaLike): Promise<void> {
    await this.ready;
    const state = delta.state?.current;
    const grabID = this.trackedGrabFor(delta.id);
    if (grabID !== undefined) {
      const grab = this.grabDownloads.get(grabID);
      if (grab !== undefined) {
        if (state === "interrupted") {
          const correlation = this.pdfGrabCorrelations.get(grabID);
          if (correlation !== undefined) {
            correlation.abandonPending = true;
            this.persistPdfGrabCorrelations();
            void this.finishAbandon(grabID, correlation);
          }
          return;
        }
        if (state === "complete") {
          const correlation = this.pdfGrabCorrelations.get(grabID);
          if (correlation !== undefined) {
            correlation.state = "identifying";
            this.persistPdfGrabCorrelations();
          }
          this.notifyPdfGrab(grab.scanID, grabID, "identifying");
        }
      }
    }
    if (state !== "complete") {
      if (state === "interrupted") {
        for (const job of this.store.activeJobs) {
          const track = this.downloads.get(job.job_id);
          if (track?.generic !== undefined && track.ids.has(delta.id)) {
            await this.discardDownload(job.job_id, delta.id);
            await this.sendGenericEpochResult(
              job.job_id,
              track.generic.epoch,
              "cancelled",
              "browser download interrupted",
            );
            await this.retainGenericCandidate(job.job_id, track.generic);
            return;
          }
          if (track?.delivery === true && track.ids.has(delta.id)) {
            await this.failDelivery(
              job.job_id,
              delta.id,
              "The PDF download was interrupted",
            );
            return;
          }
          if (track?.directOffer === true && track.ids.has(delta.id)) {
            if (track.directEpoch !== undefined) {
              this.send(
                "provider_direct_get_result",
                {
                  drive_attempt_id: track.directEpoch.drive_attempt_id,
                  ordinal: track.directEpoch.ordinal,
                  route_revision: track.directEpoch.route_revision ?? "",
                  outcome: "cancelled",
                  landing_class: "unknown",
                },
                job.job_id,
              );
            }
            await this.discardDownload(job.job_id, delta.id);
            if (track.directEpoch !== undefined)
              await this.clearDirectDownloadState(
                job.job_id,
                track.directEpoch,
              );
            return;
          }
        }
      }
      return;
    }
    let owner: ActiveJob | undefined;
    let track: DownloadTrack | undefined;
    for (const job of this.store.activeJobs) {
      const candidate = this.downloads.get(job.job_id);
      if (candidate && candidate.ids.has(delta.id)) {
        owner = job;
        track = candidate;
        break;
      }
    }
    if (!owner || !track) return;
    if (track.ambiguous || track.ids.size !== 1) return; // zero or multiple matches: stay with the user
    const found = await this.deps.downloads.search({ id: delta.id });
    const item = found[0];
    const mime = item?.mime?.split(";", 1)[0]?.trim().toLowerCase();
    if (track.generic !== undefined && mime !== "application/pdf") {
      await this.discardDownload(owner.job_id, delta.id);
      const clean = isCleanNonBrowserMime(mime);
      let outcome = clean ? "not_pdf" : "unknown";
      let detail = clean
        ? "generic candidate did not produce a PDF"
        : "generic candidate returned an unexpected MIME";
      try {
        const finalURL = new URL(item?.finalUrl ?? item?.url ?? "");
        const path = finalURL.pathname.toLowerCase();
        if (mime === "text/html" || mime === "application/xhtml+xml") {
          outcome = "html";
          detail = "generic candidate returned HTML";
        } else if (/(login|signin|sign-in|sso|saml)/u.test(path)) {
          outcome = "login";
          detail = "generic candidate reached a login page";
        } else if (/(term|consent|license)/u.test(path)) {
          outcome = "terms";
          detail = "generic candidate reached a terms page";
        } else if (/(challenge|captcha|verify)/u.test(path)) {
          outcome = "challenge";
          detail = "generic candidate reached a challenge page";
        } else if (/(rate[-_]?limit|too[-_]?many)/u.test(path)) {
          outcome = "rate_limited";
          detail = "generic candidate was rate limited";
        } else if (/5\d\d/u.test(path)) {
          outcome = "server_error";
          detail = "generic candidate reached a server error";
        }
      } catch {
        // Keep the bounded MIME classification.
      }
      const observation = await this.sendGenericEpochResult(
        owner.job_id,
        track.generic.epoch,
        outcome,
        detail,
      );
      const acknowledged =
        observation?.kind === "response" &&
        observation.payload?.["drive_attempt_id"] ===
          track.generic.epoch.drive_attempt_id &&
        observation.payload?.["ordinal"] === track.generic.epoch.ordinal &&
        observation.payload?.["strategy"] === "generic" &&
        observation.payload?.["revision"] === track.generic.epoch.revision &&
        observation.payload?.["outcome"] === "applied";
      if (outcome === "not_pdf" && acknowledged)
        await this.advanceGenericCandidate(owner.job_id, track.generic);
      else await this.retainGenericCandidate(owner.job_id, track.generic);
      return;
    }
    if (track.delivery === true) {
      if (mime !== "application/pdf") {
        await this.failDelivery(
          owner.job_id,
          delta.id,
          "Downloaded file was not a PDF — job stays in your inbox",
        );
        return;
      }
    } else if (track.directOffer) {
      if (mime !== "application/pdf") {
        if (track.directEpoch !== undefined) {
          const clean = isCleanNonBrowserMime(mime);
          let outcome:
            | "not_pdf"
            | "foreign"
            | "login"
            | "terms"
            | "challenge"
            | "unknown" = clean ? "not_pdf" : "unknown";
          let landing:
            "html" | "foreign" | "login" | "terms" | "challenge" | "unknown" =
            mime === "text/html" || mime === "application/xhtml+xml"
              ? "html"
              : clean
                ? "foreign"
                : "unknown";
          let finalHost = "";
          let finalPath = "";
          try {
            const finalURL = new URL(
              item?.finalUrl ?? item?.url ?? track.directURL ?? "",
            );
            finalHost = finalURL.hostname.toLowerCase();
            finalPath = finalURL.pathname;
            let expectedHost = "";
            try {
              expectedHost = new URL(
                track.directAllowedOrigin ?? "",
              ).hostname.toLowerCase();
            } catch {
              // Keep the bounded unknown classification.
            }
            const lowerPath = finalPath.toLowerCase();
            if (expectedHost !== "" && finalHost !== expectedHost) {
              outcome = "foreign";
              landing = "foreign";
            } else if (/(login|signin|sign-in|sso|saml)/u.test(lowerPath)) {
              outcome = "login";
              landing = "login";
            } else if (/(term|consent|license)/u.test(lowerPath)) {
              outcome = "terms";
              landing = "terms";
            }
          } catch {
            // Keep the bounded not_pdf/unknown observation.
          }
          this.send(
            "provider_direct_get_result",
            {
              drive_attempt_id: track.directEpoch.drive_attempt_id,
              ordinal: track.directEpoch.ordinal,
              route_revision: track.directEpoch.route_revision ?? "",
              outcome,
              landing_class: landing,
              ...(finalHost !== "" ? { final_host: finalHost } : {}),
              ...(finalPath !== "" ? { final_path: finalPath } : {}),
            },
            owner.job_id,
          );
        }
        await this.discardDownload(owner.job_id, delta.id);
        if (track.directEpoch !== undefined)
          await this.clearDirectDownloadState(owner.job_id, track.directEpoch);
        return;
      }
    } else if (mime === "text/html" || mime === "application/xhtml+xml") {
      // The provider served a web page where the PDF should be — the classic
      // no-entitlement wrapper (SAGE "get access"). Adopting it would only
      // bounce off the daemon's %PDF validation and burn a round trip, so
      // refuse here, discard the file, and tell the daemon why. The job stays
      // parked with its human actions; the tab stays for the human.
      await this.discardDownload(owner.job_id, delta.id);
      this.send(
        "error",
        {
          code: "download_not_pdf",
          message:
            "provider served HTML where a PDF was expected (likely no entitlement)",
        },
        owner.job_id,
      );
      return;
    }
    if (track.generic !== undefined) {
      const result = await this.sendGenericEpochResult(
        owner.job_id,
        track.generic.epoch,
        "success",
        "PDF download completed",
      );
      const acknowledged =
        result?.kind === "response" &&
        result.payload?.["drive_attempt_id"] ===
          track.generic.epoch.drive_attempt_id &&
        result.payload?.["ordinal"] === track.generic.epoch.ordinal &&
        result.payload?.["strategy"] === "generic" &&
        result.payload?.["revision"] === track.generic.epoch.revision &&
        result.payload?.["outcome"] === "applied";
      if (!acknowledged) {
        await this.retainGenericCandidate(owner.job_id, track.generic);
        return;
      }
      await this.update((s) =>
        patchJob(s, owner.job_id, {
          download_initiated: false,
          // Keep the opaque ID for diagnostics, but make the durable epoch
          // terminal so a second worker life cannot emit the result again.
          generic_terminal: true,
        }),
      );
    }
    if (!item) return;
    const rawName = item.filename ?? delta.filename?.current ?? "";
    const filename = rawName.split(/[\\/]/).pop() ?? "";
    const size = item.fileSize ?? item.totalBytes ?? item.bytesReceived ?? 0;
    if (filename.length === 0 || size < 1) return; // cannot form a valid frame; leave to the user

    if (track.directEpoch !== undefined) {
      let finalHost = "";
      let finalPath = "";
      try {
        const finalURL = new URL(
          item.finalUrl ?? item.url ?? track.directURL ?? "",
        );
        finalHost = finalURL.hostname.toLowerCase();
        finalPath = finalURL.pathname;
      } catch {
        // Keep the correlated result unknown and never adopt.
      }
      let outcome: "success" | "foreign" | "unknown" = "unknown";
      let landing: "pdf" | "foreign" | "unknown" = "unknown";
      let expectedHost = "";
      try {
        expectedHost = new URL(
          track.directAllowedOrigin ?? "",
        ).hostname.toLowerCase();
      } catch {
        // Keep unknown.
      }
      if (finalHost !== "" && finalPath !== "") {
        const envelopeOK =
          expectedHost !== "" &&
          expectedHost === finalHost &&
          directEnvelopePath(
            finalPath,
            track.directPathFamily,
            track.directExpectedIdentifier,
          );
        outcome = envelopeOK ? "success" : "foreign";
        landing = envelopeOK ? "pdf" : "foreign";
      }
      this.send(
        "provider_direct_get_result",
        {
          drive_attempt_id: track.directEpoch.drive_attempt_id,
          ordinal: track.directEpoch.ordinal,
          route_revision: track.directEpoch.route_revision ?? "",
          outcome,
          landing_class: landing,
          ...(finalHost !== "" ? { final_host: finalHost } : {}),
          ...(finalPath !== "" ? { final_path: finalPath } : {}),
        },
        owner.job_id,
      );
      if (outcome !== "success") {
        await this.discardDownload(owner.job_id, delta.id);
        await this.clearDirectDownloadState(owner.job_id, track.directEpoch);
        return;
      }
      await this.clearDirectDownloadState(owner.job_id, track.directEpoch);
    }
    await this.update((s) => {
      const next = this.clearAuthAttempts(
        patchJob(s, owner.job_id, { status: "awaiting_download" }),
        owner.job_id,
      );
      return track.delivery === true
        ? updatePendingDelivery(next, owner.job_id, { status: "downloaded" })
        : next;
    });
    this.authStalledReported.delete(owner.job_id);
    this.stalledAuthHandoffs.delete(owner.job_id);
    const route = this.deliveryRouteFor(owner, track);
    const sessionEvidence = this.deliveryEvidenceFor(owner, track, route);
    const pageHost = await this.deliveryPageHost(owner, item, track);
    this.send(
      "download_started",
      { download_id: delta.id, filename },
      owner.job_id,
    );
    const producer = this.artifactProducer(track);
    this.send(
      "download_complete",
      {
        download_id: delta.id,
        filename,
        size_bytes: size,
        ...(producer !== undefined ? { producer } : {}),
      },
      owner.job_id,
    );
    if (
      this.daemonNegotiated() &&
      (this.store.daemonFeatures ?? []).includes(DELIVERY_CONTEXT_FEATURE)
    ) {
      this.send(
        "delivery_context",
        {
          download_id: delta.id,
          route,
          session_evidence: sessionEvidence,
          ...(pageHost !== undefined ? { page_host: pageHost } : {}),
        },
        owner.job_id,
      );
    }
    this.completedDownloadTabs.set(owner.job_id, owner.tab_id);
    this.downloads.delete(owner.job_id);
  }
}

interface CancelRequest {
  channel: "papio";
  action: "cancel";
  job_id: string;
}

function isCancelRequest(message: unknown): message is CancelRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "cancel" &&
    "job_id" in message &&
    typeof message.job_id === "string"
  );
}

interface PageAcquireRequest {
  channel: "papio";
  action: "page_acquire";
  payload: PageAcquirePayload;
}

function isPageAcquireRequest(message: unknown): message is PageAcquireRequest {
  if (
    typeof message !== "object" ||
    message === null ||
    !("channel" in message) ||
    message.channel !== "papio" ||
    !("action" in message) ||
    message.action !== "page_acquire" ||
    !("payload" in message) ||
    typeof message.payload !== "object" ||
    message.payload === null ||
    Array.isArray(message.payload)
  ) {
    return false;
  }
  const payload = message.payload as Record<string, unknown>;
  if (
    !Object.keys(payload).every(
      (key) =>
        key === "url" || key === "doi" || key === "title" || key === "source",
    )
  ) {
    return false;
  }
  return (
    typeof payload.url === "string" &&
    (payload.doi === undefined || typeof payload.doi === "string") &&
    (payload.title === undefined || typeof payload.title === "string") &&
    (payload.source === undefined || typeof payload.source === "string")
  );
}

interface CapabilitiesRequest {
  channel: "papio";
  action: "get_capabilities";
}

function isCapabilitiesRequest(
  message: unknown,
): message is CapabilitiesRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "get_capabilities"
  );
}

interface TermsConsentRequest {
  channel: "papio";
  action: "terms_consent";
  value: "accept" | "manual";
}

function isTermsConsentRequest(
  message: unknown,
): message is TermsConsentRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    message.action === "terms_consent" &&
    "value" in message &&
    (message.value === "accept" || message.value === "manual")
  );
}

interface OrphanTabsRequest {
  channel: "papio";
  action: "orphan_tabs_status" | "orphan_tabs_cleanup";
}

function isOrphanTabsRequest(message: unknown): message is OrphanTabsRequest {
  return (
    typeof message === "object" &&
    message !== null &&
    "channel" in message &&
    message.channel === "papio" &&
    "action" in message &&
    (message.action === "orphan_tabs_status" ||
      message.action === "orphan_tabs_cleanup")
  );
}

interface InboxRuntimeSender {
  id?: string | undefined;
  url?: string | undefined;
  tab?: { id?: number | undefined } | undefined;
}

interface InboxRuntimeURLs {
  runtimeID: string;
  inboxURL: string;
  popupURL: string;
  historyURL: string;
  optionsURL: string;
  /** ADR-0019 Decision 4: addressed `?scan=<id>`, so exact-sender checks
   * compare origin+pathname only — never the full URL — for this one page. */
  pageBulkURL: string;
  /** ADR-0023's seventh surface. Its own page, so its own authorized sender:
   * the toast may consume one pending offer and act on it, and nothing else. */
  toastURL: string;
}

type InboxRuntimeReply =
  | BrokerFailure
  | { opened: true }
  | { captured: true }
  | { ok: true }
  | BrokerReply<{ snapshot: Record<string, unknown> }>
  | BrokerReply<{ counts: Record<string, unknown>; generated_at: string }>
  | BrokerReply<{ outcome: string; detail?: string }>
  | BrokerReply<{ opened: true }>
  | BrokerReply<{ waiting_jobs: Array<{ job_id: string; deadline?: number }> }>
  | BrokerReply<{ stats: Record<string, unknown> }>
  | BrokerReply<{
      available: boolean;
      pulse?: WorkPulseResponsePayload;
      received_at?: number;
      worker_epoch: string;
    }>
  | BrokerReply<{ accepted: boolean }>
  | BrokerReply<{ toast?: ToastPayload }>
  | BrokerReply<{ opened: boolean }>
  | BrokerReply<{ feature: boolean; entries: ActivityEntryPayload[] }>
  | BrokerReply<{
      state: BridgeSessionState;
      origins: KeepaliveOriginSnapshot[];
    }>
  | BrokerReply<{ scan_id: string }>
  | BrokerReply<{ snapshot: PageBulkSnapshot }>
  | BrokerReply<{ items: PageBulkStatusItem[]; truncated: boolean }>
  | BrokerReply<{
      grab_id: string;
      state: string;
      outcome?: string;
      detail?: string;
      job_id?: string;
    }>
  | BrokerReply<{
      mode: "v1" | "v2";
      processed_count: number;
      submitted: number;
      joined: number;
      already_owned: number;
      invalid: number;
      batch_id: string;
    }>
  | BrokerReply<{ allowed: boolean }>
  | BrokerReply<{ origins: string[] }>
  | BrokerReply<{
      grab_id: string;
      outcome: string;
      detail?: string;
      document_identifiers: Array<{ kind: string; value: string; source: string }>;
      suggestions: Array<{
        job_id: string;
        title?: string;
        year?: number;
        doi?: string;
        verdict: string;
        reason?: string;
        evidence: string[];
      }>;
      truncated: boolean;
    }>
  | BrokerReply<{ grab_id: string; job_id?: string; outcome: string; detail?: string }>
  | DeliveryReply;

function isObjectRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function isBareHTTPSOrigin(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > 300)
    return false;
  try {
    const parsed = new URL(value);
    return (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === "" &&
      parsed.pathname === "/" &&
      parsed.search === "" &&
      parsed.hash === "" &&
      parsed.host !== "" &&
      `${parsed.protocol}//${parsed.host}` === value
    );
  } catch {
    return false;
  }
}

function hasOnlyKeys(
  value: Record<string, unknown>,
  keys: readonly string[],
): boolean {
  return Object.keys(value).every((key) => keys.includes(key));
}

function isPositiveSafeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 1;
}

function isInboxSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  return sender.id === urls.runtimeID && sender.url === urls.inboxURL;
}

function isPopupSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  return sender.id === urls.runtimeID && sender.url === urls.popupURL;
}
function isInboxOrPopupSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  return (
    sender.id === urls.runtimeID &&
    (sender.url === urls.inboxURL || sender.url === urls.popupURL)
  );
}

/** ADR-0019 Decision 4: the workspace is addressed `?scan=<id>`, so the
 * exact-page check compares origin+pathname only, ignoring that query. */
/** The toast window is its own sender. Exact URL: unlike the page-bulk page it
 * carries no query, because its payload comes from the producer rather than
 * from its address. */
function isToastSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  return sender.id === urls.runtimeID && sender.url === urls.toastURL;
}

function isPageBulkSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  if (sender.id !== urls.runtimeID || sender.url === undefined) return false;
  try {
    const senderURL = new URL(sender.url);
    const pageURL = new URL(urls.pageBulkURL);
    return (
      senderURL.origin === pageURL.origin &&
      senderURL.pathname === pageURL.pathname
    );
  } catch {
    return false;
  }
}


// Stats is a read consumed by the popup summary and the history page as well
// as the inbox, so it accepts any of papio's own extension pages — never a
// content script or a foreign extension.
function isStatsSender(
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): boolean {
  return (
    sender.id === urls.runtimeID &&
    (sender.url === urls.inboxURL ||
      sender.url === urls.popupURL ||
      sender.url === urls.historyURL)
  );
}

function isSnapshotRuntimeRequest(value: unknown): value is {
  schema_versions: [1] | [2] | [3] | [4] | [5] | [4, 3] | [5, 4];
  limit?: number;
  cursor?: string;
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["schema_versions", "limit", "cursor"])
  )
    return false;
  const versions = value["schema_versions"];
  const validVersions =
    Array.isArray(versions) &&
    ((versions.length === 1 &&
      (versions[0] === 1 ||
        versions[0] === 2 ||
        versions[0] === 3 ||
        versions[0] === 4 ||
        versions[0] === 5)) ||
      (versions.length === 2 && versions[0] === 4 && versions[1] === 3) ||
      (versions.length === 2 && versions[0] === 5 && versions[1] === 4));
  return (
    validVersions &&
    (value["limit"] === undefined ||
      (isPositiveSafeInteger(value["limit"]) && value["limit"] <= 100)) &&
    (value["cursor"] === undefined ||
      (typeof value["cursor"] === "string" && value["cursor"].length <= 256))
  );
}
function isCountsRuntimeRequest(
  value: unknown,
): value is Record<string, never> {
  return isObjectRecord(value) && Object.keys(value).length === 0;
}

function isActivityRuntimeRequest(
  value: unknown,
): value is { limit?: number; before_seq?: string; seen_through_seq?: string } {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["limit", "before_seq", "seen_through_seq"])
  )
    return false;
  for (const key of ["before_seq", "seen_through_seq"]) {
    if (
      value[key] !== undefined &&
      (typeof value[key] !== "string" ||
        !/^[0-9]{1,64}$/u.test(value[key] as string))
    )
      return false;
  }
  return (
    value["limit"] === undefined ||
    (isPositiveSafeInteger(value["limit"]) && value["limit"] <= 50)
  );
}

function isSurfacePresenceRuntimeRequest(
  value: unknown,
): value is Omit<SurfacePresencePayload, "request_id"> {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["instance_id", "surface", "focused", "at"])
  )
    return false;
  return (
    typeof value["instance_id"] === "string" &&
    /^[A-Za-z0-9_-]{8,64}$/u.test(value["instance_id"]) &&
    (value["surface"] === "popup" || value["surface"] === "inbox") &&
    typeof value["focused"] === "boolean" &&
    typeof value["at"] === "string" &&
    value["at"].length <= 64 &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/u.test(
      value["at"],
    )
  );
}

function isDecisionRuntimeRequest(value: unknown): value is {
  item_id: string;
  op: "acquire" | "dismiss";
  watch_scope?: "all" | number[];
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["item_id", "op", "watch_scope"])
  )
    return false;
  const itemID = value["item_id"];
  const op = value["op"];
  const watchScope = value["watch_scope"];
  if (typeof itemID !== "string" || itemID.length === 0 || itemID.length > 1024)
    return false;
  if (op !== "acquire" && op !== "dismiss") return false;
  if (op === "acquire") return watchScope === undefined;
  if (watchScope === "all") return true;
  if (
    !Array.isArray(watchScope) ||
    watchScope.length < 1 ||
    watchScope.length > 100
  )
    return false;
  const ids = new Set<number>();
  for (const id of watchScope) {
    if (!isPositiveSafeInteger(id) || ids.has(id)) return false;
    ids.add(id);
  }
  return true;
}

function isResolveRuntimeRequest(value: unknown): value is {
  action_id: number;
  verdict: "accept" | "reject" | "dismiss";
  expected_revision: number;
  expected_sha256?: string;
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, [
      "action_id",
      "verdict",
      "expected_revision",
      "expected_sha256",
    ])
  ) {
    return false;
  }
  const verdict = value["verdict"];
  const expectedSHA = value["expected_sha256"];
  if (
    !isPositiveSafeInteger(value["action_id"]) ||
    !isPositiveSafeInteger(value["expected_revision"]) ||
    (verdict !== "accept" && verdict !== "reject" && verdict !== "dismiss")
  ) {
    return false;
  }
  if (verdict === "accept" && typeof expectedSHA !== "string") return false;
  return (
    expectedSHA === undefined ||
    (typeof expectedSHA === "string" && /^[a-f0-9]{64}$/.test(expectedSHA))
  );
}

function isPreviewRuntimeRequest(
  value: unknown,
): value is { action_id: number } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["action_id"]) &&
    isPositiveSafeInteger(value["action_id"])
  );
}

function isDeliveryReconcileRuntimeRequest(value: unknown): value is {
  job_id: string;
  operation: "confirm_request_exists" | "confirm_request_absent";
  provider_reference?: string;
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["job_id", "operation", "provider_reference"])
  )
    return false;
  const jobID = value["job_id"];
  const operation = value["operation"];
  const providerReference = value["provider_reference"];
  if (typeof jobID !== "string" || !/^[A-Za-z0-9_-]{8,128}$/.test(jobID))
    return false;
  if (
    operation !== "confirm_request_exists" &&
    operation !== "confirm_request_absent"
  )
    return false;
  if (operation === "confirm_request_exists") {
    return (
      typeof providerReference === "string" &&
      providerReference.length > 0 &&
      providerReference.length <= 300
    );
  }
  return !("provider_reference" in value);
}

function isPageBulkGrabStatusRuntimeRequest(
  value: unknown,
): value is { grab_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["grab_id"]) &&
    typeof value["grab_id"] === "string" &&
    value["grab_id"].length > 0 &&
    value["grab_id"].length <= 128
  );
}

function isGrabSuggestRuntimeRequest(
  value: unknown,
): value is { grab_id: string; limit?: number } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["grab_id", "limit"]))
    return false;
  const grabID = value["grab_id"];
  if (typeof grabID !== "string" || grabID.length === 0 || grabID.length > 128)
    return false;
  return (
    value["limit"] === undefined ||
    (isPositiveSafeInteger(value["limit"]) && value["limit"] <= 25)
  );
}

function isGrabConfirmRuntimeRequest(
  value: unknown,
): value is { grab_id: string; job_id: string } {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["grab_id", "job_id"]))
    return false;
  const grabID = value["grab_id"];
  const jobID = value["job_id"];
  return (
    typeof grabID === "string" &&
    grabID.length > 0 &&
    grabID.length <= 128 &&
    typeof jobID === "string" &&
    jobID.length > 0 &&
    jobID.length <= 128
  );
}

/** `expected_origin` is the bare HTTPS origin the popup bound its scan button
 * to. It is required, not optional: without it the background cannot bind the
 * scan to the page shown when the operator clicked the button. */
function isPageBulkScanRuntimeRequest(
  value: unknown,
): value is { tab_id: number; expected_origin: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["tab_id", "expected_origin"]) &&
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0 &&
    isBareHTTPSOrigin(value["expected_origin"])
  );
}

function isPageBulkRescanRuntimeRequest(
  value: unknown,
): value is { scan_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["scan_id"]) &&
    typeof value["scan_id"] === "string" &&
    value["scan_id"].length > 0 &&
    value["scan_id"].length <= 128
  );
}

function isPageBulkGrabRuntimeRequest(
  value: unknown,
): value is { tab_id: number; url?: string; title?: string; scan_id?: string } {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["tab_id", "url", "title", "scan_id"])
  )
    return false;
  const scanID = value["scan_id"];
  if (
    scanID !== undefined &&
    (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128)
  )
    return false;
  if (
    value["url"] !== undefined &&
    (typeof value["url"] !== "string" ||
      !value["url"].startsWith("https://") ||
      value["url"].length > 4000)
  )
    return false;
  return (
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0 &&
    (value["title"] === undefined || typeof value["title"] === "string")
  );
}

function isPageBulkIdentifier(value: unknown): value is PageBulkIdentifier {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["local_id", "kind", "value"])
  )
    return false;
  const kind = value["kind"];
  return (
    typeof value["local_id"] === "string" &&
    value["local_id"].length > 0 &&
    value["local_id"].length <= 128 &&
    (kind === "doi" ||
      kind === "pmid" ||
      kind === "arxiv" ||
      kind === "openalex") &&
    typeof value["value"] === "string" &&
    value["value"].length > 0 &&
    value["value"].length <= 512
  );
}

function isPageBulkStatusRuntimeRequest(value: unknown): value is {
  scan_id: string;
  identifiers: PageBulkIdentifier[];
  rendered_record_count_hint?: number;
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, [
      "scan_id",
      "identifiers",
      "rendered_record_count_hint",
    ])
  )
    return false;
  const scanID = value["scan_id"];
  const identifiers = value["identifiers"];
  if (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128)
    return false;
  if (
    !Array.isArray(identifiers) ||
    identifiers.length < 1 ||
    identifiers.length > 200
  )
    return false;
  if ("rendered_record_count_hint" in value) {
    const hint = value["rendered_record_count_hint"];
    if (typeof hint !== "number" || !Number.isInteger(hint) || hint < 0)
      return false;
  }
  return identifiers.every(isPageBulkIdentifier);
}

function isPageBulkSubmitSource(value: unknown): value is PageBulkSubmitSource {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["kind", "origin", "detector"])
  )
    return false;
  const kind = value["kind"];
  const origin = value["origin"];
  const detector = value["detector"];
  return (
    kind === "browser_page" &&
    typeof origin === "string" &&
    isBareLowercaseHTTPSOrigin(origin) &&
    typeof detector === "string" &&
    isDetectorText(detector)
  );
}

function isPageBulkSubmitRuntimeRequest(value: unknown): value is {
  scan_id: string;
  canonical_keys: string[];
  source: PageBulkSubmitSource;
} {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["scan_id", "canonical_keys", "source"])
  )
    return false;
  const scanID = value["scan_id"];
  const keys = value["canonical_keys"];
  if (typeof scanID !== "string" || scanID.length === 0 || scanID.length > 128)
    return false;
  if (!Array.isArray(keys) || keys.length < 1 || keys.length > 200)
    return false;
  const seen = new Set<string>();
  for (const key of keys) {
    if (typeof key !== "string" || !isCanonicalKey(key) || seen.has(key))
      return false;
    seen.add(key);
  }
  return isPageBulkSubmitSource(value["source"]);
}


function isHandoffOpenRuntimeRequest(
  value: unknown,
): value is { job_id: string } {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["job_id"]) &&
    typeof value["job_id"] === "string" &&
    value["job_id"].length > 0 &&
    value["job_id"].length <= 1024
  );
}
function isManualJobID(value: unknown): value is string {
  return typeof value === "string" && /^[A-Za-z0-9_-]{8,128}$/u.test(value);
}

function isSafeExternalHTTPSURL(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > 4000)
    return false;
  try {
    const parsed = new URL(value);
    return (
      parsed.protocol === "https:" &&
      parsed.username === "" &&
      parsed.password === ""
    );
  } catch {
    return false;
  }
}

function isManualOpenRuntimeRequest(
  value: unknown,
): value is ManualOpenPayload {
  return (
    isObjectRecord(value) &&
    hasOnlyKeys(value, ["job_id"]) &&
    isManualJobID(value["job_id"])
  );
}

function isSessionRetryRuntimeRequest(
  value: unknown,
): value is { job_id: string } {
  return isHandoffOpenRuntimeRequest(value);
}

function isDeliveryChoice(value: unknown): value is DeliveryChoice {
  if (!isObjectRecord(value) || !hasOnlyKeys(value, ["interaction", "job_id"]))
    return false;
  return (
    typeof value["interaction"] === "string" &&
    value["interaction"].length >= 8 &&
    value["interaction"].length <= 128 &&
    isManualJobID(value["job_id"])
  );
}

function isDeliveryStartRuntimeRequest(
  value: unknown,
): value is DeliveryStartPayload {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, ["tab_id", "url", "job_id", "doi", "title", "choice"])
  )
    return false;
  if (value["choice"] !== undefined && !isDeliveryChoice(value["choice"]))
    return false;
  return (
    typeof value["tab_id"] === "number" &&
    Number.isSafeInteger(value["tab_id"]) &&
    value["tab_id"] >= 0 &&
    typeof value["url"] === "string" &&
    value["url"].length > 0 &&
    value["url"].length <= 4000 &&
    (value["job_id"] === undefined || isManualJobID(value["job_id"])) &&
    (value["doi"] === undefined || typeof value["doi"] === "string") &&
    (value["title"] === undefined || typeof value["title"] === "string")
  );
}

// The key whitelist deliberately omits request_id. This path is the popup's
// UNSOLICITED capture (capture.ts's captureFixture), which answers no
// page_capture_request; accepting a caller-supplied correlation id here would
// let an extension page forge a binding to whatever capture the CLI is
// currently waiting on — the exact cross-binding papio-85a7420f4cd2564f is
// about, just deliberate instead of accidental. A requested capture never
// travels this way: it is sent straight from onPageCaptureRequest.
function isPageCaptureRuntimeRequest(
  value: unknown,
): value is PageCapturePayload {
  if (
    !isObjectRecord(value) ||
    !hasOnlyKeys(value, [
      "host",
      "scenario",
      "adapter_id",
      "adapter_version",
      "encoding",
      "bytes",
      "body",
    ])
  ) {
    return false;
  }
  return (
    typeof value["host"] === "string" &&
    typeof value["scenario"] === "string" &&
    (value["adapter_id"] === undefined ||
      typeof value["adapter_id"] === "string") &&
    (value["adapter_version"] === undefined ||
      typeof value["adapter_version"] === "string") &&
    typeof value["encoding"] === "string" &&
    isPositiveSafeInteger(value["bytes"]) &&
    typeof value["body"] === "string"
  );
}

/**
 * Exact extension-page authorization prevents a content script from sending
 * captured page material over native messaging.
 */
export async function handleInboxRuntimeMessage(
  bridge: Bridge,
  message: unknown,
  sender: InboxRuntimeSender,
  urls: InboxRuntimeURLs,
): Promise<InboxRuntimeReply | undefined> {
  if (!isObjectRecord(message) || typeof message["type"] !== "string")
    return undefined;
  const type = message["type"];
  if (type === "papio.page_capture") {
    if (sender.id !== urls.runtimeID || sender.url !== urls.popupURL) {
      return failure("unauthorized", "This sender cannot send page captures");
    }
    const capturePayload = message["payload"];
    if (
      !hasOnlyKeys(message, ["type", "payload"]) ||
      !isPageCaptureRuntimeRequest(capturePayload)
    ) {
      return failure("invalid_request", "Invalid page capture request");
    }
    if (!bridge.pageCaptureAvailable()) return { captured: true };
    // A refusal here is not a routine "diagnostic panel is closed" state:
    // the operator explicitly selected `terms`, so the popup's own filter
    // (which withholds `terms` from the scenario list unless the daemon
    // already advertised it) was correct when the panel loaded but the
    // daemon underneath was swapped before the click (the two-binary skew
    // AGENTS.md documents). Reporting `{ captured: true }` here previously
    // sent the operator hunting a daemon-side storage bug that does not
    // exist, when the real fix is upgrading the stale daemon.
    if (
      capturePayload.scenario === "terms" &&
      !bridge.termsCaptureAvailable()
    ) {
      return failure(
        "capture_failed",
        "The connected daemon does not support terms captures; upgrade the daemon to send this scenario",
      );
    }
    return (await bridge.sendPageCapture(capturePayload))
      ? { captured: true }
      : failure("capture_failed", "Could not send page capture");
  }
  if (type === "papio.openInbox") {
    if (!isInboxOrPopupSender(sender, urls))
      return failure("unauthorized", "This sender cannot open the inbox");
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid inbox open request");
    try {
      await bridge.openInbox(urls.inboxURL);
      return { opened: true };
    } catch {
      return failure("open_failed", "Could not open the inbox");
    }
  }
  if (type === "papio.work.pulse") {
    if (!isInboxOrPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot access the work pulse",
      );
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid work pulse request");
    return bridge.requestWorkPulse();
  }
  if (type === "papio.surface.presence") {
    if (!isInboxOrPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot report surface presence",
      );
    if (
      !hasOnlyKeys(message, ["type", "payload"]) ||
      !isSurfacePresenceRuntimeRequest(message["payload"])
    ) {
      return failure("invalid_request", "Invalid surface presence");
    }
    return bridge.sendSurfacePresence(message["payload"]);
  }
  if (type === "papio.stats") {
    if (!isStatsSender(sender, urls))
      return failure("unauthorized", "This sender cannot access papio stats");
    if (!hasOnlyKeys(message, ["type", "request"]))
      return failure("invalid_request", "Invalid stats request");
    return isCountsRuntimeRequest(message["request"])
      ? bridge.requestStats()
      : failure("invalid_request", "Invalid stats request");
  }
  if (type === "papio.handoff.open") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot access the inbox broker",
      );
    }
    if (!hasOnlyKeys(message, ["type", "request"])) {
      return failure("invalid_request", "Invalid handoff open request");
    }
    return isHandoffOpenRuntimeRequest(message["request"])
      ? bridge.openHandoff(message["request"].job_id)
      : failure("invalid_request", "Invalid handoff open request");
  }
  if (type === "papio.manual.open") {
    if (!isInboxSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot select a manual-download target",
      );
    }
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isManualOpenRuntimeRequest(message["request"])
    ) {
      return failure("invalid_request", "Invalid manual-download open request");
    }
    return bridge.openManualDownload(message["request"]);
  }
  if (type === "papio.delivery.start") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return failure("unauthorized", "This sender cannot start PDF delivery");
    }
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isDeliveryStartRuntimeRequest(message["request"])
    ) {
      return failure("invalid_request", "Invalid PDF delivery request");
    }
    return bridge.startPDFDelivery(message["request"]);
  }
  if (type === "papio.delivery.state") {
    if (!isInboxOrPopupSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot read PDF delivery state",
      );
    }
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid PDF delivery state request");
    return bridge.deliveryState();
  }
  if (type === "papio.session.state") {
    if (!isPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot access institution session state",
      );
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid institution session request");
    return {
      ok: true,
      state: await bridge.sessionStateSnapshot(),
      origins: bridge.sessionOriginStates(),
    };
  }
  if (type === "papio.session.probe") {
    if (!isPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot probe institution session state",
      );
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid institution session request");
    return {
      ok: true,
      state: await bridge.sessionStateWithProbe(),
      origins: bridge.sessionOriginStates(),
    };
  }
  if (type === "papio.session.signin") {
    if (!isPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot control institution sign-in",
      );
    if (!hasOnlyKeys(message, ["type", "origin"]))
      return failure("invalid_request", "Invalid institution sign-in request");
    const origin = message["origin"];
    if (origin === undefined) return bridge.requestSessionSignIn();
    if (!isBareHTTPSOrigin(origin))
      return failure("invalid_request", "Invalid institution sign-in request");
    return bridge.requestSessionSignIn(origin);
  }
  if (type === "papio.session.retry") {
    if (!isPopupSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot retry institution handoffs",
      );
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isSessionRetryRuntimeRequest(message["request"])
    ) {
      return failure(
        "invalid_request",
        "Invalid institution handoff retry request",
      );
    }
    return bridge.retryAuthStalled(message["request"].job_id);
  }
  if (type === "papio.pageBulk.load") {
    if (!isPageBulkSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot load a page-bulk scan",
      );
    }
    const request = message["request"];
    // Same { scan_id } shape as papio.pageBulk.rescan — a plain read of the
    // already-open workspace's snapshot, never a re-scan.
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkRescanRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid page-bulk load request");
    }
    return bridge.getPageBulkSnapshot(request.scan_id);
  }
  if (type === TOAST_PENDING_MESSAGE) {
    if (!isToastSender(sender, urls))
      return failure("unauthorized", "This sender cannot read a papio toast");
    if (!hasOnlyKeys(message, ["type"]))
      return failure("invalid_request", "Invalid toast request");
    // Answered as the payload itself, not an envelope: the page's parser
    // accepts only the closed shape and closes the window on anything else.
    return { ok: true, toast: bridge.toastPending() };
  }
  if (type === TOAST_ACTION_MESSAGE) {
    if (!isToastSender(sender, urls))
      return failure("unauthorized", "This sender cannot act on a papio toast");
    const jobID = message["job_id"];
    if (!hasOnlyKeys(message, ["type", "job_id"]) || typeof jobID !== "string" || jobID === "")
      return failure("invalid_request", "Invalid toast action");
    return { ok: true, opened: await bridge.toastAction(jobID) };
  }
  if (type === TOAST_DISMISS_MESSAGE) {
    if (!isToastSender(sender, urls))
      return failure("unauthorized", "This sender cannot dismiss a papio toast");
    const jobID = message["job_id"];
    if (
      !hasOnlyKeys(message, ["type", "job_id", "reason"]) ||
      typeof jobID !== "string" ||
      jobID === ""
    ) {
      return failure("invalid_request", "Invalid toast dismissal");
    }
    bridge.toastDismiss(jobID);
    return { ok: true };
  }
  // The injected route's own two types. Their gate is NOT `isToastSender`:
  // the sender is the researcher's own page, so `sender.url` proves nothing
  // here. Authorization is the one-use token papio minted at injection plus
  // the tab it injected into, both checked inside the bridge. `sender.id`
  // still has to be papio's own runtime, and the tab id comes from the
  // browser's sender record rather than from the message body — a page that
  // could somehow speak cannot name a tab that is not its own.
  if (type === TOAST_PAGE_ACTION_MESSAGE) {
    const jobID = message["job_id"];
    const token = message["token"];
    const tabID = sender.tab?.id;
    if (sender.id !== urls.runtimeID)
      return failure("unauthorized", "This sender cannot act on a papio toast");
    if (
      !hasOnlyKeys(message, ["type", "job_id", "token"]) ||
      typeof jobID !== "string" ||
      jobID === "" ||
      typeof token !== "string" ||
      token === "" ||
      tabID === undefined
    ) {
      return failure("invalid_request", "Invalid toast action");
    }
    return { ok: true, opened: await bridge.pageToastAction(jobID, token, tabID) };
  }
  if (type === TOAST_PAGE_DISMISS_MESSAGE) {
    const jobID = message["job_id"];
    const token = message["token"];
    const tabID = sender.tab?.id;
    if (sender.id !== urls.runtimeID) {
      return failure(
        "unauthorized",
        "This sender cannot dismiss a papio toast",
      );
    }
    if (
      !hasOnlyKeys(message, ["type", "job_id", "token", "reason"]) ||
      typeof jobID !== "string" ||
      jobID === "" ||
      typeof token !== "string" ||
      token === "" ||
      tabID === undefined
    ) {
      return failure("invalid_request", "Invalid toast dismissal");
    }
    bridge.pageToastDismiss(jobID, token, tabID);
    return { ok: true };
  }
  if (type === "papio.pageBulk.scan") {
    if (!isPopupSender(sender, urls))
      return failure("unauthorized", "This sender cannot start a page scan");
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkScanRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid page scan request");
    }
    return bridge.startPageBulkScan(
      request.tab_id,
      request.expected_origin,
      urls.pageBulkURL,
    );
  }
  if (type === "papio.pageBulk.rescan") {
    if (!isPageBulkSender(sender, urls))
      return failure("unauthorized", "This sender cannot rescan a page");
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkRescanRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid rescan request");
    }
    return bridge.requestPageBulkRescan(request.scan_id);
  }
  if (type === "papio.pageBulk.status") {
    if (!isPageBulkSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot look up page-bulk status",
      );
    }
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkStatusRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid page-bulk status request");
    }
    return bridge.requestPageBulkStatus(request);
  }
  if (type === "papio.pageBulk.submit") {
    if (!isPageBulkSender(sender, urls)) {
      return failure(
        "unauthorized",
        "This sender cannot submit a page-bulk batch",
      );
    }
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkSubmitRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid page-bulk submit request");
    }
    return bridge.requestPageBulkSubmit(request);
  }
  if (type === "papio.pageBulk.grabPdf") {
    if (!isPageBulkSender(sender, urls))
      return failure("unauthorized", "This sender cannot grab a PDF");
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkGrabRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid PDF grab request");
    }
    return bridge.requestPdfGrab({
      ...request,
      workspace_tab_id: sender.tab?.id,
    });
  }
  if (type === "papio.pageBulk.grabStatus") {
    if (!isPageBulkSender(sender, urls))
      return failure("unauthorized", "This sender cannot read PDF grab status");
    const request = message["request"];
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isPageBulkGrabStatusRuntimeRequest(request)
    ) {
      return failure("invalid_request", "Invalid PDF grab status request");
    }
    return bridge.requestPdfGrabStatus(request.grab_id);
  }
  if (type === "papio.triage.waiting") {
    if (!isInboxSender(sender, urls))
      return failure(
        "unauthorized",
        "This sender cannot access local inbox state",
      );
    if (
      !hasOnlyKeys(message, ["type", "request"]) ||
      !isCountsRuntimeRequest(message["request"])
    ) {
      return failure("invalid_request", "Invalid local inbox state request");
    }
    return {
      ok: true,
      waiting_jobs: await bridge.waitingSessionJobsSnapshot(),
    };
  }
  if (
    type !== "papio.activity" &&
    type !== "papio.triage.snapshot" &&
    type !== "papio.triage.counts" &&
    type !== "papio.triage.decide" &&
    type !== "papio.action.resolve" &&
    type !== "papio.delivery.reconcile" &&
    type !== "papio.preview" &&
    type !== "papio.grab.suggest" &&
    type !== "papio.grab.confirm"
  ) {
    return undefined;
  }
  // The popup may perform aggregate READS it renders directly: Activity for the
  // catch-up line, and counts for the pulse header's decision count, which must
  // come from turns_required. Every mutating type stays inbox-only, because the
  // popup closes on focus loss and must not own a decision whose result it
  // cannot show.
  const popupReadableTypes: readonly string[] = [
    "papio.activity",
    "papio.triage.counts",
  ];
  const senderAuthorized = popupReadableTypes.includes(type)
    ? isInboxOrPopupSender(sender, urls)
    : isInboxSender(sender, urls);
  if (!senderAuthorized) {
    return failure(
      "unauthorized",
      "This sender cannot access the inbox broker",
    );
  }
  if (!hasOnlyKeys(message, ["type", "request"])) {
    return failure("invalid_request", "Invalid inbox broker request");
  }
  const request = message["request"];
  switch (type) {
    case "papio.activity":
      return isActivityRuntimeRequest(request)
        ? bridge.requestActivity(request)
        : failure("invalid_request", "Invalid activity request");
    case "papio.triage.snapshot":
      return isSnapshotRuntimeRequest(request)
        ? bridge.requestTriageSnapshot(request)
        : failure("invalid_request", "Invalid triage snapshot request");
    case "papio.triage.counts":
      return isCountsRuntimeRequest(request)
        ? bridge.requestTriageCounts()
        : failure("invalid_request", "Invalid triage counts request");
    case "papio.triage.decide":
      return isDecisionRuntimeRequest(request)
        ? bridge.requestTriageDecision(request)
        : failure("invalid_request", "Invalid triage decision request");
    case "papio.action.resolve":
      return isResolveRuntimeRequest(request)
        ? bridge.requestActionResolve(request)
        : failure("invalid_request", "Invalid action resolution request");
    case "papio.delivery.reconcile":
      return isDeliveryReconcileRuntimeRequest(request)
        ? bridge.requestDeliveryReconcile(request)
        : failure("invalid_request", "Invalid delivery reconciliation request");
    case "papio.preview":
      return isPreviewRuntimeRequest(request)
        ? bridge.requestPreview(request)
        : failure("invalid_request", "Invalid preview request");
    // Both read-only (suggest) and holder-independent-shaped (confirm — it
    // mutates, but through the same fenced bind operator_confirm already
    // uses, never a second door): inbox-only like every other triage
    // mutation above, gated by isInboxSender via senderAuthorized.
    case "papio.grab.suggest":
      return isGrabSuggestRuntimeRequest(request)
        ? bridge.requestGrabSuggestions(request)
        : failure("invalid_request", "Invalid PDF grab suggestion request");
    case "papio.grab.confirm":
      return isGrabConfirmRuntimeRequest(request)
        ? bridge.requestGrabConfirm(request)
        : failure("invalid_request", "Invalid PDF grab confirmation request");
    default:
      return undefined;
  }
}

/** Defensive shape check for whatever chrome.storage.session actually holds
 * (foreign extension write, a stale schema from a prior version, or nothing
 * yet) before trusting it as a PageBulkScanStore. */
function isPageBulkScanStore(value: unknown): value is PageBulkScanStore {
  if (typeof value !== "object" || value === null) return false;
  if (!("order" in value) || !("byId" in value)) return false;
  const order = value.order;
  const byId = value.byId;
  return (
    Array.isArray(order) &&
    order.every((id) => typeof id === "string") &&
    typeof byId === "object" &&
    byId !== null
  );
}

function realDeps(): BridgeDeps {
  return {
    connectNative: (name) => {
      const port = chrome.runtime.connectNative(name);
      return {
        postMessage: (msg) => port.postMessage(msg),
        onMessage: {
          addListener: (cb) => port.onMessage.addListener((m) => cb(m)),
        },
        onDisconnect: {
          addListener: (cb) => port.onDisconnect.addListener(() => cb()),
        },
        disconnect: () => port.disconnect(),
      };
    },
    manifestVersion: chrome.runtime.getManifest().version,
    runtimeGetURL: (path) => chrome.runtime.getURL(path),
    randomUUID: () => crypto.randomUUID(),
    now: () => Date.now(),
    online: () => navigator.onLine,
    setTimeout: (fn, ms) => {
      setTimeout(fn, ms);
    },
    runtimeSendMessage: (message) => chrome.runtime.sendMessage(message),
    ...((
      chrome.runtime as typeof chrome.runtime & {
        getBrowserInfo?: () => Promise<{ name?: string; version?: string }>;
      }
    ).getBrowserInfo === undefined
      ? {}
      : {
          browserInfo: () =>
            (
              chrome.runtime as typeof chrome.runtime & {
                getBrowserInfo: () => Promise<{
                  name?: string;
                  version?: string;
                }>;
              }
            ).getBrowserInfo(),
        }),
    backend: chromeBackend(chrome.storage),
    tabs: {
      create: (props) => chrome.tabs.create(props),
      get: (tabID) => chrome.tabs.get(tabID),
      reload: (tabID) => chrome.tabs.reload(tabID),
      remove: (tabID) => chrome.tabs.remove(tabID),
      update: (tabID, props) => chrome.tabs.update(tabID, props),
      query: (query) => chrome.tabs.query(query),
      sendMessage: (tabID, message) => chrome.tabs.sendMessage(tabID, message),
      onUpdated: { addListener: (cb) => chrome.tabs.onUpdated.addListener(cb) },
      onRemoved: { addListener: (cb) => chrome.tabs.onRemoved.addListener(cb) },
      onActivated: {
        addListener: (cb) => chrome.tabs.onActivated.addListener(cb),
      },
      ...(typeof chrome.tabs.group === "function"
        ? {
            group: (opts: {
              tabIds: number[];
              groupId?: number;
              createProperties?: { windowId?: number };
            }) => chrome.tabs.group(opts as chrome.tabs.GroupOptions),
          }
        : {}),
    },
    // chrome.windows is present in every Chromium; guarded for other runtimes.
    ...(typeof chrome.windows !== "undefined"
      ? {
          windows: {
            create: (props: {
              url: string;
              focused: boolean;
              state?: "minimized" | "normal";
              type?: "popup";
              width?: number;
              height?: number;
              top?: number;
              left?: number;
            }) => chrome.windows.create(props) as Promise<WindowInfo>,
            // populate:true so browser-side work-window state stays observable.
            get: (windowID: number) =>
              chrome.windows.get(windowID, {
                populate: true,
              }) as Promise<WindowInfo>,
            update: (
              windowID: number,
              props: {
                focused?: boolean;
                state?: "normal" | "minimized";
                drawAttention?: boolean;
              },
            ) => chrome.windows.update(windowID, props),
            remove: (windowID: number) => chrome.windows.remove(windowID),
          },
        }
      : {}),
    // chrome.tabGroups: Chrome and Firefox 139+ (with the tabGroups permission);
    // absent on Firefox < 139 and older Chromium. Runtime-detected either way.
    ...(typeof chrome.tabGroups !== "undefined"
      ? {
          tabGroups: {
            get: (groupID: number) =>
              chrome.tabGroups.get(groupID) as Promise<TabGroupInfo>,
            update: (
              groupID: number,
              props: { collapsed?: boolean; title?: string; color?: string },
            ) =>
              chrome.tabGroups.update(
                groupID,
                props as chrome.tabGroups.UpdateProperties,
              ),
            query: (props: { title?: string }) =>
              chrome.tabGroups.query(props) as Promise<TabGroupInfo[]>,
          },
        }
      : {}),
    ...(typeof chrome.webNavigation !== "undefined"
      ? {
          webNavigation: {
            onCommitted: {
              addListener: (cb) =>
                chrome.webNavigation.onCommitted.addListener(cb as never),
            },
            onHistoryStateUpdated: {
              addListener: (cb) =>
                chrome.webNavigation.onHistoryStateUpdated.addListener(cb as never),
            },
            onReferenceFragmentUpdated: {
              addListener: (cb) =>
                chrome.webNavigation.onReferenceFragmentUpdated.addListener(cb as never),
            },
            onTabReplaced: {
              addListener: (cb) =>
                chrome.webNavigation.onTabReplaced.addListener(cb as never),
            },
            onErrorOccurred: {
              addListener: (cb) =>
                chrome.webNavigation.onErrorOccurred.addListener(cb as never),
            },
            // The live document epoch. Present on Chromium; on a browser whose
            // getFrame omits `documentId` this resolves without one and the
            // picker and manual continuation stay unavailable there by design.
            getFrame: (details) =>
              chrome.webNavigation.getFrame(details) as Promise<
                { documentId?: string } | null
              >,
          },
        }
      : {}),
    downloads: {
      removeFile: (downloadID) => chrome.downloads.removeFile(downloadID),
      erase: (query) => chrome.downloads.erase(query),
      download: (options) => chrome.downloads.download(options),
      search: (query) => chrome.downloads.search(query),
      onCreated: {
        addListener: (cb) => chrome.downloads.onCreated.addListener(cb),
      },
      onChanged: {
        addListener: (cb) => chrome.downloads.onChanged.addListener(cb),
      },
      ...(chrome.downloads.onDeterminingFilename
        ? {
            onDeterminingFilename: {
              addListener: (
                cb: (
                  item: DownloadItemLike,
                  suggest: (s: {
                    filename: string;
                    conflictAction: "uniquify";
                  }) => void,
                ) => void,
              ) => chrome.downloads.onDeterminingFilename.addListener(cb),
            },
          }
        : {}),
    },
    adapterSpecs: adapters,
    scripting: {
      executeScript: (injection) =>
        chrome.scripting.executeScript(
          injection as unknown as chrome.scripting.ScriptInjection<
            unknown[],
            unknown
          >,
        ),
    },
    captureStorage: {
      local: {
        get: (key) => chrome.storage.local.get(key),
        set: (items) => chrome.storage.local.set(items),
      },
    },
    captureConsent: {
      get: async () => {
        const got = await chrome.storage.local.get(PAGE_CAPTURE_CONSENT_KEY);
        return got[PAGE_CAPTURE_CONSENT_KEY] === true;
      },
    },
    tabLedger: {
      load: async () => {
        const got = await chrome.storage.local.get(MANAGED_TAB_LEDGER_KEY);
        return got[MANAGED_TAB_LEDGER_KEY];
      },
      save: async (entries) => {
        await chrome.storage.local.set({ [MANAGED_TAB_LEDGER_KEY]: entries });
      },
    },
    epoch: {
      getSession: async () => {
        const got = await chrome.storage.session.get(BROWSER_EPOCH_SESSION_KEY);
        const v = got[BROWSER_EPOCH_SESSION_KEY];
        return typeof v === "string" ? v : undefined;
      },
      setSession: async (value: string) => {
        await chrome.storage.session.set({ [BROWSER_EPOCH_SESSION_KEY]: value });
      },
      getLocal: async () => {
        const got = await chrome.storage.local.get(BROWSER_EPOCH_LOCAL_KEY);
        const v = got[BROWSER_EPOCH_LOCAL_KEY];
        return typeof v === "string" ? v : undefined;
      },
      setLocal: async (value: string) => {
        await chrome.storage.local.set({ [BROWSER_EPOCH_LOCAL_KEY]: value });
      },
    },
    permissions: {
      contains: (perm) => chrome.permissions.contains(perm),
    },
    settings: {
      async getTermsConsent() {
        try {
          const got = await chrome.storage.local.get(TERMS_CONSENT_KEY);
          const v = got[TERMS_CONSENT_KEY];
          return v === "accept" || v === "manual" ? v : undefined;
        } catch {
          return undefined;
        }
      },
      async setTermsConsent(value) {
        await chrome.storage.local.set({ [TERMS_CONSENT_KEY]: value });
      },
      async getHandoffSurface(): Promise<HandoffSurface> {
        try {
          const got = await chrome.storage.local.get([
            HANDOFF_SURFACE_KEY,
            WORK_WINDOW_KEY,
          ]);
          const v = got[HANDOFF_SURFACE_KEY];
          if (v === "in-window" || v === "work-window" || v === "tab-group")
            return v;
          // No explicit choice: honor the legacy boolean so upgrades are seamless.
          return got[WORK_WINDOW_KEY] === false ? "in-window" : "work-window";
        } catch {
          return "work-window";
        }
      },
      getInPageToast: () => getInPageToastEnabled(chrome.storage.local),
    },
    toolbarCount: {
      async get(): Promise<ToolbarCountMode> {
        try {
          const values = await chrome.storage.local.get(TOOLBAR_COUNT_MODE_KEY);
          const value = values[TOOLBAR_COUNT_MODE_KEY];
          return value === "all" || value === "off" ? value : "required";
        } catch {
          return "required";
        }
      },
    },
    pageBulkScans: {
      async get() {
        try {
          const got = await chrome.storage.session.get(
            PAGE_BULK_SCAN_STORAGE_KEY,
          );
          const stored = got[PAGE_BULK_SCAN_STORAGE_KEY];
          return isPageBulkScanStore(stored)
            ? stored
            : emptyPageBulkScanStore();
        } catch {
          return emptyPageBulkScanStore();
        }
      },
      async set(store) {
        await chrome.storage.session.set({
          [PAGE_BULK_SCAN_STORAGE_KEY]: store,
        });
      },
    },
    pdfGrabCorrelations: {
      async get() {
        try {
          const got = await chrome.storage.session.get(
            PDF_GRAB_CORRELATION_STORAGE_KEY,
          );
          const stored = got[PDF_GRAB_CORRELATION_STORAGE_KEY];
          if (typeof stored !== "object" || stored === null) return {};
          return stored as Record<string, PdfGrabCorrelation>;
        } catch {
          return {};
        }
      },
      async set(value) {
        await chrome.storage.session.set({
          [PDF_GRAB_CORRELATION_STORAGE_KEY]: value,
        });
      },
    },
    claimObservationOutbox: {
      async get() {
        try {
          const got = await chrome.storage.session.get(
            CLAIM_OBSERVATION_OUTBOX_STORAGE_KEY,
          );
          const stored = got[CLAIM_OBSERVATION_OUTBOX_STORAGE_KEY];
          if (typeof stored !== "object" || stored === null) return {};
          return stored as Record<string, ClaimObservationOutboxEntry>;
        } catch {
          return {};
        }
      },
      async set(value) {
        await chrome.storage.session.set({
          [CLAIM_OBSERVATION_OUTBOX_STORAGE_KEY]: value,
        });
      },
    },
    navigationErrorMarkers: {
      async get() {
        try {
          const got = await chrome.storage.session.get(
            NAVIGATION_ERROR_MARKER_STORAGE_KEY,
          );
          const stored = got[NAVIGATION_ERROR_MARKER_STORAGE_KEY];
          if (typeof stored !== "object" || stored === null) return {};
          return stored as Record<string, NavigationErrorMarkerEntry>;
        } catch {
          return {};
        }
      },
      async set(value) {
        await chrome.storage.session.set({
          [NAVIGATION_ERROR_MARKER_STORAGE_KEY]: value,
        });
      },
    },
    action: {
      setBadgeText: (details) => chrome.action.setBadgeText(details),
      setBadgeBackgroundColor: (details) =>
        chrome.action.setBadgeBackgroundColor(details),
      setTitle: (details) => chrome.action.setTitle(details),
    },
    alarms: {
      create: (name, info) => {
        if (chrome.alarms !== undefined)
          void chrome.alarms.create(
            name,
            info as chrome.alarms.AlarmCreateInfo,
          );
      },
      get: async (name) => {
        const alarm = await chrome.alarms?.get(name);
        return alarm?.name === name ? { name: alarm.name } : undefined;
      },
      onAlarm: {
        addListener: (cb) => chrome.alarms?.onAlarm?.addListener(cb),
      },
    },
    devReload: {
      installType: async () => (await chrome.management?.getSelf())?.installType ?? "",
      reload: () => chrome.runtime.reload(),
    },
  };
}

// Wiring runs only inside a real extension service worker, never under bun test.
if (typeof chrome !== "undefined" && chrome.runtime?.id) {
  const bridge = new Bridge(realDeps());
  // The broker authorizes senders by exact page URL. Derive the popup path
  // from the manifest and the inbox as its sibling so the authorized URLs
  // can never drift from the shipped page layout again.
  const declaredPopup =
    chrome.runtime.getManifest().action?.default_popup ?? POPUP_PAGE_PATH;
  const inboxRuntimeURLs: InboxRuntimeURLs = {
    runtimeID: chrome.runtime.id,
    inboxURL: chrome.runtime.getURL(
      declaredPopup.replace(/[^/]*$/, "inbox.html"),
    ),
    popupURL: chrome.runtime.getURL(declaredPopup),
    historyURL: chrome.runtime.getURL(
      declaredPopup.replace(/[^/]*$/, "history.html"),
    ),
    optionsURL: chrome.runtime.getURL(
      declaredPopup.replace(/[^/]*$/, "options.html"),
    ),
    toastURL: chrome.runtime.getURL(
      declaredPopup.replace(/[^/]*$/, "toast.html"),
    ),
    pageBulkURL: chrome.runtime.getURL(
      declaredPopup.replace(/[^/]*$/, "page-bulk.html"),
    ),
  };
  // Top-level registrations give Chrome a reason to start this worker at
  // browser launch and after install/update. Without them a cold-started
  // Chrome leaves the worker dead (and the daemon unreachable) until an
  // unrelated tab or download event happens to fire. bridge.start() already
  // ran at module top level by then; the callbacks need no body.
  chrome.runtime.onStartup.addListener(() => {
    // Remove the scanner-consent allowlist left by older extension versions.
    // This is cleanup only; no live code reads or writes the old key.
    void chrome.storage.local
      .remove("papio_scanner_allowlist_v1")
      .catch(() => {});
  });
  chrome.runtime.onInstalled.addListener(() => {});
  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (isObjectRecord(message) && isInboxRuntimeMessageType(message["type"])) {
      respondToRuntimePromise(
        handleInboxRuntimeMessage(bridge, message, _sender, inboxRuntimeURLs),
        sendResponse,
      );
      return true;
    }
    if (isCapabilitiesRequest(message)) {
      sendResponse({ page_acquire: bridge.pageAcquireAvailable() });
      return false;
    }
    if (isPageAcquireRequest(message)) {
      respondToRuntimePromise(
        bridge.requestPageAcquire(message.payload),
        sendResponse,
      );
      return true; // async native acknowledgement
    }
    if (isCancelRequest(message)) {
      respondToRuntimePromise(
        bridge.requestCancel(message.job_id).then(() => ({ ok: true })),
        sendResponse,
      );
      return true; // async sendResponse
    }
    if (isTermsConsentRequest(message)) {
      respondToRuntimePromise(
        bridge.requestTermsConsent(message.value).then(() => ({ ok: true })),
        sendResponse,
      );
      return true; // async sendResponse
    }
    if (isOrphanTabsRequest(message)) {
      respondToRuntimePromise(
        message.action === "orphan_tabs_status"
          ? bridge.orphanTabStatus()
          : bridge.cleanupOrphanTabs(),
        sendResponse,
      );
      return true; // async sendResponse
    }
    return false;
  });
  // A grant/revoke changes both resolver setup and whether a recorded provider
  // blocker remains effective; clear the cached answer before repainting.
  chrome.permissions?.onAdded?.addListener(() => {
    void bridge.onPermissionsChanged();
  });
  chrome.permissions?.onRemoved?.addListener(() => {
    void bridge.onPermissionsChanged();
  });
  // KEEPALIVE INTEGRATION
  // Constructed and attached synchronously, before bridge.start() runs (and
  // therefore before bindListeners() binds any chrome.tabs/alarms listener):
  // a navigation that WAKES this worker must never reach onTabUpdated with
  // this.keepaliveManager still undefined. initKeepalive() itself only fires
  // manager.init() without awaiting it, so hydration continues concurrently
  // with the bridge's own async startup below — neither blocks the other.
  const keepaliveManager = initKeepalive(chromeKeepaliveAPI(chrome), {
    trackedJobCount: () => bridge.trackedJobCount(),
    warmDemand: () => bridge.warmDemand(),
    latestOpenURL: () => bridge.latestOpenURL(),
    knownResolverOrigins: () => bridge.knownResolverOrigins(),
    authDemandOrigins: () => bridge.authDemandOrigins(),
    queuedAuthJobs: () => bridge.queuedAuthJobs(),
    stalledAuthJobs: () => bridge.stalledAuthJobIDs(),
    lastAuthReturnedAt: () => bridge.lastAuthReturnedAt(),
    workWindowID: () => bridge.workWindowIDForKeepalive(),
    onTabPlaced: (tabID) => bridge.foldKeepaliveTab(tabID),
    configuredOriginsReady: () => bridge.hasCurrentHello(),
    onFreshSessionEvidence: (evidence: FreshSessionEvidence) => {
      void bridge.recordFreshSessionEvidence(evidence);
    },
    onOriginAuthenticationChanged: (origin: string, authenticated: boolean) => {
      // A committed "no longer authenticated" must retract that origin's
      // release authority now, not let it idle out over AUTH_EVIDENCE_TTL_MS.
      // Signing out is exactly when papio must stop opening queued handoffs
      // into a session that will bounce them to a login wall.
      if (!authenticated) void bridge.revokeAuthEvidence(origin);
      void bridge.syncConnectionBadge();
    },
    onOriginPermissionChanged: (origin: string, granted: boolean) => {
      if (!granted) void bridge.revokeAuthEvidence(origin);
      void bridge.syncConnectionBadge();
    },
    onReauthStateChanged: (paused) => bridge.setKeepaliveReauthNeeded(paused),
    surfaceReauthTab: async (tabID) => {
      try {
        const tab = await chrome.tabs.get(tabID);
        if (tab.windowId === undefined) return;
        const win = await chrome.windows.get(tab.windowId);
        await chrome.windows.update(tab.windowId, {
          focused: true,
          ...(win.state === "minimized" ? { state: "normal" as const } : {}),
        });
      } catch {
        // Badge and popup remain the recoverable reauth signal.
      }
    },
  });
  bridge.attachKeepalive(keepaliveManager);
  void bridge.start();
}
