// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// papio-browser/1 — the locked extension/native-host contract. This parser
// MUST accept and reject exactly the same corpus as the
// Go core (testdata/protocol/valid and testdata/protocol/invalid): unknown
// fields, unknown types, oversized frames, and out-of-bounds values are
// errors, never warnings. auth_pending/auth_returned payloads structurally
// cannot carry URLs or titles — identity-provider addresses never leave the
// browser.

export const BROWSER_PROTOCOL_VERSION = "papio-browser/1";
export const MAX_BROWSER_MESSAGE_BYTES = 256 * 1024;
export const MAX_BROWSER_INTEGER = Number.MAX_SAFE_INTEGER;
export const MsgPageCapture = "page_capture" as const;
export const MsgPageCaptureRequest = "page_capture_request" as const;
export const MsgPageCaptureRequestResult = "page_capture_request_result" as const;

export type BrowserMessageType =
  | "hello"
  | "hello_ack"
  | "page_acquire"
  | "page_acquire_ack"
  | "page_capture"
  | "page_capture_request"
  | "page_capture_request_result"
  | "job_offer"
  | "handoff_outcome"
  | "job_accept"
  | "job_reject"
  | "auth_pending"
  | "auth_returned"
  | "session_evidence"
  | "download_started"
  | "download_complete"
  | "delivery_context"
  | "provider_outcome"
  | "cancel"
  | "handoff_focus"
  | "ack"
  | "error"
  | "triage_snapshot_request"
  | "triage_snapshot_response"
  | "triage_counts_request"
  | "triage_counts_response"
  | "triage_decide"
  | "triage_decide_result"
  | "human_action_resolve"
  | "human_action_resolve_result"
  | "review_preview_request"
  | "review_preview_result"
  | "stats_request"
  | "stats_response"
  | "activity_request"
  | "activity_response";

export interface HelloPayload {
  extension_version: string;
  adapter_versions?: Record<string, string>;
}

export interface HelloAckPayload {
  daemon_version?: string;
  features?: string[];
  /** https origins of the daemon's configured OpenURL resolvers. The extension
   * requests a host permission for each so it can steer that resolver's menu. */
  resolver_origins?: string[];
}

export interface PageAcquirePayload {
  url: string;
  /** Optional on the wire for forward evolution; this daemon currently requires it. */
  doi?: string;
  title?: string;
  source?: string;
}

export interface PageAcquireAckPayload {
  job_id?: string;
  duplicate?: boolean;
  error?: string;
}

export interface PageCapturePayload {
  host: string;
  scenario: "observed" | "success" | "login-return" | "no-entitlement" | "drift" | "terms";
  adapter_id?: string;
  adapter_version?: string;
  encoding: "gzip+base64";
  bytes: number;
  body: string;
}

export interface PageCaptureRequestPayload {
  request_id: string;
  url: string;
  provider: string;
  scenario: "success" | "login-return" | "no-entitlement" | "drift" | "terms";
  settle_ms?: number;
}

export interface PageCaptureRequestResultPayload {
  request_id: string;
  outcome: "captured" | "nav_failed" | "timeout" | "not_permitted" | "busy";
  detail?: string;
}

export interface JobOfferExpected {
  doi?: string;
  title?: string;
}

export interface JobOfferPayload {
  openurl: string;
  provider_hosts: string[];
  expected?: JobOfferExpected;
  access_mode: "assisted" | "delegated";
  expires_at: string;
  /** The institution's Shibboleth IdP entityID, when the daemon knows it.
   * Lets an adapter's federated-login route auto-select the institution on a
   * provider login wall. Optional; an https URL when present. */
  login_entity_id?: string;
  /** The institution's ProQuest account id, when the daemon knows it. Lets the
   * ProQuest adapter unlock the openurl link-resolver by appending
   * ?accountid=<id>. Optional; digits when present. */
  proquest_account_id?: string;
  /** True when the handoff needs an authenticated institutional session; false
   * or absent means the URL is publicly reachable (open access). */
  requires_auth?: boolean;
}

/** Reports that a handoff tab terminated on an identity-provider failure
 * page. final_host is a bare hostname; no path, query, or page content ever
 * crosses the bridge. */
export interface HandoffOutcomePayload {
  outcome: "stale_sso" | "auth_error";
  final_host: string;
}

/** Timing only — no URL/host/title/query/fragment fields exist by design. */
export interface AuthPayload {
  elapsed_ms?: number;
}

/** Timing-only evidence that an institutional resolver session is available.
 * origin_hint is a bare resolver origin and never an IdP path or query. */
export interface SessionEvidencePayload {
  evidence: "warm_verified" | "auth_returned";
  origin_hint?: string;
  at: string;
}

export interface DownloadStartedPayload {
  download_id: number;
  filename: string;
}

export interface DownloadCompletePayload {
  download_id: number;
  filename: string;
  size_bytes: number;
}

export type DeliveryRoute = "resolver" | "direct" | "oa";
export type DeliverySessionEvidence = "fresh_auth" | "warm" | "none";

export interface DeliveryContextPayload {
  download_id: number;
  route: DeliveryRoute;
  page_host?: string;
  session_evidence: DeliverySessionEvidence;
}

export type ProviderOutcome =
  | "no_entitlement"
  | "document_delivery_available"
  | "wrong_work"
  | "ui_changed"
  | "rate_limited"
  | "terms_acceptance_required"
  | "human_auth_required"
  | "cancelled";

export interface ProviderOutcomePayload {
  outcome: ProviderOutcome;
  adapter_version?: string;
  detail?: string;
}

export interface ErrorPayload {
  code: string;
  message: string;
}

export interface TriageCounts {
  pending_total: number;
  watch_hits: number;
  actions: number;
  /** Present only in the negotiated triage counts response schema v2. */
  actions_requires_auth?: number;
  retractions: number;
  jobs_working: number;
  jobs_needs_review: number;
  failure_groups_7d: number;
}

export interface TriageSnapshotRequestPayload {
  request_id: string;
  schema_versions: [1];
  limit?: number;
  cursor?: string;
}

export interface TriageSnapshotResponsePayload {
  request_id: string;
  schema: 1;
  generated_at: string;
  counts: TriageCounts;
  items: TriageSnapshotItem[];
  cursor?: string;
  has_more: boolean;
  unsupported_items_count: number;
}

export interface TriageFact {
  label: string;
  text: string;
}

export interface TriageLink {
  rel: "doi" | "arxiv" | "openalex" | "landing" | "preview";
  url: string;
}

export interface TriageSnapshotItem {
  kind: "watch_hit" | "human_action" | "retraction";
  id: string;
  rank: number;
  title: string;
  facts: TriageFact[];
  links: TriageLink[];
  ops: Array<"acquire" | "dismiss" | "accept" | "reject" | "open" | "retry">;
  work?: { doi: string; title: string; authors: string; year: number; is_oa: boolean };
  abstract?: string;
  watches?: Array<{ id: number; label: string }>;
  first_seen_at?: string;
  action_id?: number;
  job_id?: string;
  action_kind?: string;
  job_state?: string;
  revision?: number;
  sha256?: string;
  size_bytes?: number;
  requires_auth?: boolean;
  blocked_by?: "anti_bot" | "paywall" | "landing_page";
  doi?: string;
  nature?: "retraction" | "correction" | "concern";
  noticed_at?: string;
  notice_doi?: string;
}

export interface TriageCountsRequestPayload {
  request_id: string;
  schema_versions?: [1] | [2];
}

export interface TriageCountsResponsePayload {
  request_id: string;
  counts: TriageCounts;
}

export interface TriageDecidePayload {
  request_id: string;
  item_id: string;
  op: "acquire" | "dismiss";
  watch_scope?: "all" | number[];
}

export interface TriageDecideResultPayload {
  request_id: string;
  outcome: "applied" | "already_applied" | "conflict" | "error";
  detail?: string;
}

export interface HumanActionResolvePayload {
  request_id: string;
  action_id: number;
  verdict: "accept" | "reject" | "dismiss";
  expected_revision: number;
  expected_sha256?: string;
}

export type HumanActionResolveResultPayload = TriageDecideResultPayload;

export interface ReviewPreviewRequestPayload {
  request_id: string;
  action_id: number;
}

export interface ReviewPreviewResultPayload {
  request_id: string;
  outcome: "ok" | "error";
  detail?: string;
  url?: string;
  sha256?: string;
  size_bytes?: number;
  expires_at?: string;
}

export interface StatsRequestPayload {
  request_id: string;
}

export interface StatsAccess {
  open_access: number;
  institutional: number;
  licensed_api: number;
  other: number;
}

export interface StatsBucket {
  period_start: string;
  acquired: number;
}

export interface StatsResponsePayload {
  request_id: string;
  generated_at: string;
  acquired_total: number;
  failed_total: number;
  handoffs_required: number;
  access: StatsAccess;
  series: StatsBucket[];
}

export interface ActivityRequestPayload {
  request_id: string;
  limit?: number;
}

export interface ActivityEntryPayload {
  seq: number;
  at: string;
  job_id?: string;
  kind: string;
  text: string;
  title?: string;
}

export interface ActivityResponsePayload {
  request_id: string;
  generated_at: string;
  entries: ActivityEntryPayload[];
}

export interface BrowserMessage {
  protocol: typeof BROWSER_PROTOCOL_VERSION;
  type: BrowserMessageType;
  msg_id: string;
  job_id?: string;
  seq: number;
  payload: Record<string, unknown>;
}

export class ProtocolError extends Error {
  override name = "ProtocolError";
}

const MSG_TYPES: Record<string, true> = {
  hello: true,
  hello_ack: true,
  page_acquire: true,
  page_acquire_ack: true,
  page_capture: true,
  page_capture_request: true,
  page_capture_request_result: true,
  job_offer: true,
  handoff_outcome: true,
  job_accept: true,
  job_reject: true,
  auth_pending: true,
  auth_returned: true,
  session_evidence: true,
  download_started: true,
  download_complete: true,
  delivery_context: true,
  provider_outcome: true,
  cancel: true,
  handoff_focus: true,
  ack: true,
  error: true,
  triage_snapshot_request: true,
  triage_snapshot_response: true,
  triage_counts_request: true,
  triage_counts_response: true,
  triage_decide: true,
  triage_decide_result: true,
  human_action_resolve: true,
  human_action_resolve_result: true,
  review_preview_request: true,
  review_preview_result: true,
  stats_request: true,
  stats_response: true,
  activity_request: true,
  activity_response: true,
};

const JOB_SCOPED: Record<string, true> = {
  job_offer: true,
  handoff_outcome: true,
  job_accept: true,
  job_reject: true,
  auth_pending: true,
  auth_returned: true,
  download_started: true,
  download_complete: true,
  delivery_context: true,
  provider_outcome: true,
  cancel: true,
  handoff_focus: true,
};

const OUTCOMES: Record<string, true> = {
  no_entitlement: true,
  document_delivery_available: true,
  wrong_work: true,
  ui_changed: true,
  rate_limited: true,
  terms_acceptance_required: true,
  human_auth_required: true,
  cancelled: true,
};

const MSG_ID_RE = /^[A-Za-z0-9_-]{8,64}$/;
const JOB_ID_RE = /^[A-Za-z0-9_-]{8,128}$/;
const HOST_RE = /^[a-z0-9.-]{3,253}$/;
const ERROR_CODE_RE = /^[a-z0-9_]{2,50}$/;
const FILENAME_RE = /^[^/\\]{1,255}$/u;
const RFC3339_RE =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|([+-])(\d{2}):(\d{2}))$/;
const BASE64_RE = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

function fail(msg: string): never {
  throw new ProtocolError(msg);
}

function asRecord(v: unknown, what: string): Record<string, unknown> {
  if (typeof v !== "object" || v === null || Array.isArray(v)) fail(`${what} must be an object`);
  return v as Record<string, unknown>;
}

function requireKeys(obj: Record<string, unknown>, what: string, required: string[], optional: string[] = []): void {
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(obj)) {
    if (!allowed.has(key)) fail(`${what}: unknown field ${JSON.stringify(key)} (fail closed)`);
  }
  for (const key of required) {
    if (!(key in obj)) fail(`${what}: missing required field ${JSON.stringify(key)}`);
  }
}
type FieldSpec<T> = { [K in keyof T & string]?: "required" | "optional" };

function requireFields<T>(obj: Record<string, unknown>, what: string, spec: FieldSpec<T>): void {
  for (const key of Object.keys(obj)) {
    if (!(key in spec)) fail(`${what}: unknown field ${JSON.stringify(key)} (fail closed)`);
  }
  for (const key of Object.keys(spec)) {
    if (spec[key as keyof FieldSpec<T>] === "required" && !(key in obj)) {
      fail(`${what}: missing required field ${JSON.stringify(key)}`);
    }
  }
}

function str(obj: Record<string, unknown>, key: string, what: string, max = 1000): string {
  const v = obj[key];
  if (typeof v !== "string") fail(`${what}.${key} must be a string`);
  if (Array.from(v).length > max) fail(`${what}.${key} exceeds ${max} chars`);
  return v;
}

function rejectNUL(value: string, what: string): void {
  if (value.includes("\0")) fail(`${what} cannot contain NUL`);
}

function int(obj: Record<string, unknown>, key: string, what: string, min: number): number {
  const v = obj[key];
  if (typeof v !== "number" || !Number.isInteger(v)) fail(`${what}.${key} must be an integer`);
  if (v < min) fail(`${what}.${key} must be >= ${min}`);
  if (v > MAX_BROWSER_INTEGER) fail(`${what}.${key} exceeds ${MAX_BROWSER_INTEGER}`);
  return v;
}

function isRFC3339(value: string): boolean {
  const match = RFC3339_RE.exec(value);
  if (match === null) return false;
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  const hour = Number(match[4]);
  const minute = Number(match[5]);
  const second = Number(match[6]);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) return false;
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const daysInMonth = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (day < 1 || day > daysInMonth[month - 1]!) return false;
  if (match[7] !== "Z") {
    const offsetHour = Number(match[9]);
    const offsetMinute = Number(match[10]);
    if (offsetHour > 23 || offsetMinute > 59) return false;
  }
  return true;
}

function triageText(obj: Record<string, unknown>, key: string, what: string, max: number): string {
  const value = str(obj, key, what, max);
  rejectNUL(value, `${what}.${key}`);
  return value;
}

function correlationID(obj: Record<string, unknown>, key: string, what: string): string {
  const value = triageText(obj, key, what, 64);
  if (!MSG_ID_RE.test(value)) fail(`${what}.${key} must match the msg_id charset`);
  return value;
}

function triageTime(obj: Record<string, unknown>, key: string, what: string): string {
  const value = triageText(obj, key, what, 64);
  if (!isRFC3339(value)) fail(`${what}.${key} must be RFC3339`);
  return value;
}

function triageURL(value: string, what: string, scheme: "http:" | "https:"): URL {
  rejectNUL(value, what);
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== scheme || parsed.host === "") fail(`${what} must be a ${scheme} URL`);
    return parsed;
  } catch (e) {
    if (e instanceof ProtocolError) throw e;
    fail(`${what} must be a ${scheme} URL`);
  }
}

function triageCounts(raw: unknown, what: string, allowAuth = false): void {
  const counts = asRecord(raw, what);
  const fields = [
    "pending_total",
    "watch_hits",
    "actions",
    "retractions",
    "jobs_working",
    "jobs_needs_review",
    "failure_groups_7d",
  ];
  requireFields<TriageCounts>(counts, what, {
    pending_total: "required",
    watch_hits: "required",
    actions: "required",
    ...(allowAuth ? { actions_requires_auth: "optional" as const } : {}),
    retractions: "required",
    jobs_working: "required",
    jobs_needs_review: "required",
    failure_groups_7d: "required",
  });
  const pending = int(counts, "pending_total", what, 0);
  const visible = int(counts, "watch_hits", what, 0) + int(counts, "actions", what, 0) + int(counts, "retractions", what, 0);
  for (const key of fields.slice(4)) int(counts, key, what, 0);
  if (allowAuth && "actions_requires_auth" in counts) int(counts, "actions_requires_auth", what, 0);
  if (pending !== visible) fail(`${what}.pending_total must equal visible item counts`);
}

function triageItem(raw: unknown, schema: 1 | 2): void {
  const item = asRecord(raw, "triage item");
  const core = ["kind", "id", "rank", "title", "facts", "links", "ops"];
  const kind = triageText(item, "kind", "triage item", 50);
  let extra: string[];
  switch (kind) {
    case "watch_hit":
      extra = ["work", "abstract", "watches", "first_seen_at"];
      break;
    case "human_action":
      extra = ["action_id", "job_id", "action_kind", "job_state", "revision", "sha256", "size_bytes"];
      break;
    case "retraction":
      extra = ["doi", "nature", "noticed_at"];
      break;
    default:
      fail(`unsupported triage item kind ${JSON.stringify(kind)}`);
  }
  const optional = kind === "human_action" && schema === 2
    ? ["requires_auth", "blocked_by"]
    : kind === "retraction" ? ["notice_doi"] : [];
  requireKeys(item, `triage item ${kind}`, [...core, ...extra], optional);
  if (triageText(item, "id", `triage item ${kind}`, 1024) === "") fail("triage item.id is required");
  int(item, "rank", `triage item ${kind}`, 0);
  triageText(item, "title", `triage item ${kind}`, 500);
  const facts = item["facts"];
  if (!Array.isArray(facts) || facts.length > 8) fail("triage item.facts must have at most 8 entries");
  for (const rawFact of facts) {
    const fact = asRecord(rawFact, "triage fact");
    requireKeys(fact, "triage fact", ["label", "text"]);
    triageText(fact, "label", "triage fact", 40);
    triageText(fact, "text", "triage fact", 400);
  }
  const links = item["links"];
  if (!Array.isArray(links) || links.length > 16) fail("triage item.links must have at most 16 entries");
  for (const rawLink of links) {
    const link = asRecord(rawLink, "triage link");
    requireKeys(link, "triage link", ["rel", "url"]);
    const rel = triageText(link, "rel", "triage link", 50);
    if (!["doi", "arxiv", "openalex", "landing", "preview"].includes(rel)) fail(`invalid triage link rel ${JSON.stringify(rel)}`);
    triageURL(triageText(link, "url", "triage link", 4000), "triage link.url", "https:");
  }
  const ops = item["ops"];
  if (!Array.isArray(ops)) fail("triage item.ops must be an array");
  const seenOps = new Set<string>();
  for (const rawOp of ops) {
    if (typeof rawOp !== "string" || !["acquire", "dismiss", "accept", "reject", "open", "retry"].includes(rawOp) || seenOps.has(rawOp)) {
      fail("triage item.ops contains an invalid or repeated operation");
    }
    seenOps.add(rawOp);
  }
  if (kind === "watch_hit") {
    const work = asRecord(item["work"], "watch_hit.work");
    requireKeys(work, "watch_hit.work", ["doi", "title", "authors", "year", "is_oa"]);
    triageText(work, "doi", "watch_hit.work", 300);
    triageText(work, "title", "watch_hit.work", 500);
    triageText(work, "authors", "watch_hit.work", 200);
    int(work, "year", "watch_hit.work", 0);
    if (typeof work["is_oa"] !== "boolean") fail("watch_hit.work.is_oa must be a boolean");
    triageText(item, "abstract", "watch_hit", 2000);
    triageTime(item, "first_seen_at", "watch_hit");
    const watches = item["watches"];
    if (!Array.isArray(watches) || watches.length < 1 || watches.length > 100) fail("watch_hit.watches must have 1..100 entries");
    const seenWatches = new Set<number>();
    for (const rawWatch of watches) {
      const watch = asRecord(rawWatch, "watch_hit.watch");
      requireKeys(watch, "watch_hit.watch", ["id", "label"]);
      const id = int(watch, "id", "watch_hit.watch", 1);
      if (seenWatches.has(id)) fail("watch_hit.watches IDs must be unique");
      seenWatches.add(id);
      triageText(watch, "label", "watch_hit.watch", 500);
    }
  } else if (kind === "human_action") {
    int(item, "action_id", "human_action", 1);
    const jobID = triageText(item, "job_id", "human_action", 128);
    if (!JOB_ID_RE.test(jobID)) fail("human_action.job_id is invalid");
    if (triageText(item, "action_kind", "human_action", 100) === "") fail("human_action.action_kind is required");
    if (triageText(item, "job_state", "human_action", 50) === "") fail("human_action.job_state is required");
    int(item, "revision", "human_action", 1);
    const sha = triageText(item, "sha256", "human_action", 64);
    if (sha !== "" && !/^[a-f0-9]{64}$/.test(sha)) fail("human_action.sha256 must be lowercase SHA-256");
    int(item, "size_bytes", "human_action", 0);
    if (schema === 2) {
      if (("requires_auth" in item) !== ("blocked_by" in item)) {
        fail("human_action.requires_auth and blocked_by must be present together");
      }
      if ("requires_auth" in item && typeof item["requires_auth"] !== "boolean") {
        fail("human_action.requires_auth must be a boolean");
      }
      if ("blocked_by" in item) {
        const blockedBy = triageText(item, "blocked_by", "human_action", 50);
        if (!["anti_bot", "paywall", "landing_page"].includes(blockedBy)) {
          fail("human_action.blocked_by is invalid");
        }
      }
    }
  } else {
    if (triageText(item, "doi", "retraction", 300) === "") fail("retraction.doi is required");
    const nature = triageText(item, "nature", "retraction", 50);
    if (!["retraction", "correction", "concern"].includes(nature)) fail("invalid retraction.nature");
    triageTime(item, "noticed_at", "retraction");
    if ("notice_doi" in item) triageText(item, "notice_doi", "retraction", 300);
  }
}

function triageResult(p: Record<string, unknown>, what: string): void {
  requireFields<TriageDecideResultPayload>(p, what, { request_id: "required", outcome: "required", detail: "optional" });
  correlationID(p, "request_id", what);
  const outcome = triageText(p, "outcome", what, 50);
  if (!["applied", "already_applied", "conflict", "error"].includes(outcome)) fail(`${what}.outcome is invalid`);
  if ("detail" in p) triageText(p, "detail", what, 1000);
}

/** Parse one decoded JSON value as a bridge message, failing closed. */
export function parseBrowserMessage(raw: unknown): BrowserMessage {
  const env = asRecord(raw, "message");
  requireKeys(env, "message", ["protocol", "type", "msg_id", "seq", "payload"], ["job_id"]);
  if (env["protocol"] !== BROWSER_PROTOCOL_VERSION) {
    fail(`protocol ${JSON.stringify(env["protocol"])}, want ${BROWSER_PROTOCOL_VERSION}`);
  }
  const type = str(env, "type", "message", 50);
  if (MSG_TYPES[type] !== true) fail(`unknown type ${JSON.stringify(type)} (fail closed)`);
  const msgID = str(env, "msg_id", "message", 64);
  if (!MSG_ID_RE.test(msgID)) fail(`invalid msg_id ${JSON.stringify(msgID)}`);
  const seq = int(env, "seq", "message", 0);
  let jobID: string | undefined;
  if ("job_id" in env) {
    jobID = str(env, "job_id", "message", 128);
    if (!JOB_ID_RE.test(jobID)) fail(`invalid job_id ${JSON.stringify(jobID)}`);
  }
  if (JOB_SCOPED[type] === true && jobID === undefined) fail(`type ${type} requires job_id`);
  const payload = asRecord(env["payload"], "payload");
  validatePayload(type as BrowserMessageType, payload);

  const msg: BrowserMessage = {
    protocol: BROWSER_PROTOCOL_VERSION,
    type: type as BrowserMessageType,
    msg_id: msgID,
    seq,
    payload,
  };
  if (jobID !== undefined) msg.job_id = jobID;
  return msg;
}

/** Parse a wire string, enforcing the encoded-size cap before JSON.parse. */
export function parseBrowserMessageBytes(text: string): BrowserMessage {
  if (new TextEncoder().encode(text).byteLength > MAX_BROWSER_MESSAGE_BYTES) {
    fail(`frame exceeds cap of ${MAX_BROWSER_MESSAGE_BYTES} bytes`);
  }
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (e) {
    fail(`invalid JSON: ${String(e)}`);
  }
  return parseBrowserMessage(doc);
}

function validatePayload(type: BrowserMessageType, p: Record<string, unknown>): void {
  switch (type) {
    case "hello": {
      requireFields<HelloPayload>(p, "hello", { extension_version: "required", adapter_versions: "optional" });
      const v = str(p, "extension_version", "hello", 50);
      if (v.length === 0) fail("hello.extension_version required");
      if ("adapter_versions" in p) {
        const av = asRecord(p["adapter_versions"], "hello.adapter_versions");
        const keys = Object.keys(av);
        if (keys.length > 50) fail("hello.adapter_versions capped at 50");
        for (const k of keys) {
          const value = av[k];
          if (typeof value !== "string" || Array.from(value).length > 50) {
            fail(`hello.adapter_versions.${k} must be a short string`);
          }
        }
      }
      break;
    }
    case "page_acquire": {
      requireFields<PageAcquirePayload>(p, "page_acquire", { url: "required", doi: "optional", title: "optional", source: "optional" });
      const pageURL = str(p, "url", "page_acquire", 4000);
      rejectNUL(pageURL, "page_acquire.url");
      let validURL = false;
      try {
        const u = new URL(pageURL);
        validURL = (u.protocol === "http:" || u.protocol === "https:") && u.host !== "";
      } catch {
        validURL = false;
      }
      if (!validURL) fail("page_acquire.url must be a parseable http(s) URL");
      if ("doi" in p) rejectNUL(str(p, "doi", "page_acquire", 512), "page_acquire.doi");
      if ("title" in p) rejectNUL(str(p, "title", "page_acquire", 1024), "page_acquire.title");
      if ("source" in p) rejectNUL(str(p, "source", "page_acquire", 1024), "page_acquire.source");
      break;
    }
    case "page_acquire_ack": {
      requireFields<PageAcquireAckPayload>(p, "page_acquire_ack", { job_id: "optional", duplicate: "optional", error: "optional" });
      const jobID = "job_id" in p ? str(p, "job_id", "page_acquire_ack", 128) : "";
      if ("job_id" in p && jobID === "") fail("page_acquire_ack.job_id must be non-empty");
      if (jobID !== "" && !JOB_ID_RE.test(jobID)) {
        fail("page_acquire_ack.job_id is invalid");
      }
      const error = "error" in p ? str(p, "error", "page_acquire_ack", 1000) : "";
      if ("error" in p && error === "") fail("page_acquire_ack.error must be non-empty");
      rejectNUL(error, "page_acquire_ack.error");
      if ((jobID !== "") === (error !== "")) {
        fail("page_acquire_ack requires exactly one of job_id or error");
      }
      const duplicate = p["duplicate"];
      if (duplicate !== undefined && typeof duplicate !== "boolean") {
        fail("page_acquire_ack.duplicate must be a boolean");
      }
      if (duplicate === true && jobID === "") {
        fail("page_acquire_ack.duplicate requires job_id");
      }
      break;
    }
    case "page_capture": {
      requireFields<PageCapturePayload>(p, "page_capture", {
        host: "required",
        scenario: "required",
        adapter_id: "optional",
        adapter_version: "optional",
        encoding: "required",
        bytes: "required",
        body: "required",
      });
      const host = str(p, "host", "page_capture", 253);
      if (!HOST_RE.test(host)) fail("page_capture.host must be a hostname");
      const scenario = str(p, "scenario", "page_capture", 50);
      if (!["observed", "success", "login-return", "no-entitlement", "drift", "terms"].includes(scenario)) {
        fail("page_capture.scenario is invalid");
      }
      if ("adapter_id" in p && !/^[A-Za-z0-9_-]{1,64}$/.test(str(p, "adapter_id", "page_capture", 64))) {
        fail("page_capture.adapter_id must use the id charset");
      }
      if ("adapter_version" in p) str(p, "adapter_version", "page_capture", 50);
      if (str(p, "encoding", "page_capture", 20) !== "gzip+base64") {
        fail("page_capture.encoding must be gzip+base64");
      }
      if (int(p, "bytes", "page_capture", 1) > 2 * 1024 * 1024) {
        fail("page_capture.bytes exceeds 2097152");
      }
      const body = str(p, "body", "page_capture", MAX_BROWSER_MESSAGE_BYTES);
      if (!BASE64_RE.test(body)) fail("page_capture.body must be canonical base64");
      break;
    }
    case "page_capture_request": {
      requireFields<PageCaptureRequestPayload>(p, "page_capture_request", {
        request_id: "required",
        url: "required",
        provider: "required",
        scenario: "required",
        settle_ms: "optional",
      });
      correlationID(p, "request_id", "page_capture_request");
      const pageURL = str(p, "url", "page_capture_request", 4000);
      if (!pageURL.startsWith("https://")) fail("page_capture_request.url must be https");
      try {
        const parsed = new URL(pageURL);
        if (parsed.protocol !== "https:" || parsed.host === "") fail("page_capture_request.url must be https");
      } catch {
        fail("page_capture_request.url must be https");
      }
      if (!/^[A-Za-z0-9_-]{1,64}$/.test(str(p, "provider", "page_capture_request", 64))) {
        fail("page_capture_request.provider must use the id charset");
      }
      const scenario = str(p, "scenario", "page_capture_request", 50);
      if (!["success", "login-return", "no-entitlement", "drift", "terms"].includes(scenario)) {
        fail("page_capture_request.scenario is invalid");
      }
      if ("settle_ms" in p && int(p, "settle_ms", "page_capture_request", 0) > 10_000) {
        fail("page_capture_request.settle_ms must be <= 10000");
      }
      break;
    }
    case "page_capture_request_result": {
      requireFields<PageCaptureRequestResultPayload>(p, "page_capture_request_result", {
        request_id: "required",
        outcome: "required",
        detail: "optional",
      });
      correlationID(p, "request_id", "page_capture_request_result");
      const outcome = str(p, "outcome", "page_capture_request_result", 20);
      if (!["captured", "nav_failed", "timeout", "not_permitted", "busy"].includes(outcome)) {
        fail("page_capture_request_result.outcome is invalid");
      }
      if ("detail" in p) triageText(p, "detail", "page_capture_request_result", 1000);
      break;
    }
    case "job_offer": {
      requireFields<JobOfferPayload>(p, "job_offer", {
        openurl: "required",
        provider_hosts: "required",
        expected: "optional",
        access_mode: "required",
        expires_at: "required",
        login_entity_id: "optional",
        proquest_account_id: "optional",
        requires_auth: "optional",
      });
      const openurl = str(p, "openurl", "job_offer", 4000);
      if (!openurl.startsWith("https://")) fail("job_offer.openurl must be https");
      const hosts = p["provider_hosts"];
      if (!Array.isArray(hosts) || hosts.length < 1 || hosts.length > 20) {
        fail("job_offer.provider_hosts must have 1..20 entries");
      }
      for (const h of hosts) {
        if (typeof h !== "string" || !HOST_RE.test(h)) fail(`invalid provider host ${JSON.stringify(h)}`);
      }
      const mode = str(p, "access_mode", "job_offer", 20);
      if (mode !== "assisted" && mode !== "delegated") fail(`invalid access_mode ${JSON.stringify(mode)}; expected "assisted" or "delegated"`);
      const expires = str(p, "expires_at", "job_offer", 64);
      if (!isRFC3339(expires)) fail("job_offer.expires_at must be RFC3339");
      if ("expected" in p) {
        const ex = asRecord(p["expected"], "job_offer.expected");
        requireFields<JobOfferExpected>(ex, "job_offer.expected", { doi: "optional", title: "optional" });
        if ("doi" in ex) str(ex, "doi", "job_offer.expected", 300);
        if ("title" in ex) str(ex, "title", "job_offer.expected", 500);
      }
      if ("login_entity_id" in p) {
        const entity = str(p, "login_entity_id", "job_offer", 4000);
        if (!entity.startsWith("https://")) fail("job_offer.login_entity_id must be https");
      }
      if ("proquest_account_id" in p) {
        const acct = str(p, "proquest_account_id", "job_offer", 64);
        if (!/^[0-9]+$/.test(acct)) fail("job_offer.proquest_account_id must be digits");
      }
      if ("requires_auth" in p && typeof p["requires_auth"] !== "boolean") {
        fail("job_offer.requires_auth must be a boolean");
      }
      break;
    }
    case "handoff_outcome": {
      requireFields<HandoffOutcomePayload>(p, "handoff_outcome", { outcome: "required", final_host: "required" });
      const outcome = str(p, "outcome", "handoff_outcome", 20);
      if (outcome !== "stale_sso" && outcome !== "auth_error") {
        fail(`invalid handoff outcome ${JSON.stringify(outcome)}`);
      }
      const host = str(p, "final_host", "handoff_outcome", 253);
      if (!HOST_RE.test(host)) fail("handoff_outcome.final_host must be a hostname");
      break;
    }
    case "auth_pending":
    case "auth_returned": {
      // Structural privacy invariant: timing only.
      requireFields<AuthPayload>(p, type, { elapsed_ms: "optional" });
      if ("elapsed_ms" in p) int(p, "elapsed_ms", type, 0);
      break;
    }
    case "session_evidence": {
      requireFields<SessionEvidencePayload>(p, "session_evidence", {
        evidence: "required",
        origin_hint: "optional",
        at: "required",
      });
      const evidence = str(p, "evidence", "session_evidence", 20);
      if (evidence !== "warm_verified" && evidence !== "auth_returned") {
        fail(`session_evidence.evidence is invalid: ${JSON.stringify(evidence)}`);
      }
      triageTime(p, "at", "session_evidence");
      if ("origin_hint" in p) {
        const origin = str(p, "origin_hint", "session_evidence", 300);
        try {
          const parsed = new URL(origin);
          if (parsed.protocol !== "https:" || parsed.host === "" || `${parsed.protocol}//${parsed.host}` !== origin) {
            fail("session_evidence.origin_hint must be a bare https origin");
          }
        } catch (e) {
          if (e instanceof ProtocolError) throw e;
          fail("session_evidence.origin_hint must be a bare https origin");
        }
      }
      break;
    }
    case "download_started": {
      requireFields<DownloadStartedPayload>(p, "download_started", { download_id: "required", filename: "required" });
      int(p, "download_id", "download_started", 0);
      if (!FILENAME_RE.test(str(p, "filename", "download_started", 255))) {
        fail("download_started.filename must be a bare name without path separators");
      }
      break;
    }
    case "download_complete": {
      requireFields<DownloadCompletePayload>(p, "download_complete", { download_id: "required", filename: "required", size_bytes: "required" });
      int(p, "download_id", "download_complete", 0);
      if (!FILENAME_RE.test(str(p, "filename", "download_complete", 255))) {
        fail("download_complete.filename must be a bare name without path separators");
      }
      int(p, "size_bytes", "download_complete", 1);
      break;
    }
    case "delivery_context": {
      requireFields<DeliveryContextPayload>(p, "delivery_context", {
        download_id: "required",
        route: "required",
        page_host: "optional",
        session_evidence: "required",
      });
      int(p, "download_id", "delivery_context", 0);
      const route = str(p, "route", "delivery_context", 20);
      if (route !== "resolver" && route !== "direct" && route !== "oa") {
        fail(`delivery_context.route is invalid: ${JSON.stringify(route)}`);
      }
      const evidence = str(p, "session_evidence", "delivery_context", 20);
      if (evidence !== "fresh_auth" && evidence !== "warm" && evidence !== "none") {
        fail(`delivery_context.session_evidence is invalid: ${JSON.stringify(evidence)}`);
      }
      if (route === "oa" && evidence !== "none") {
        fail("delivery_context.route oa requires session_evidence none");
      }
      if ("page_host" in p) {
        const pageHost = str(p, "page_host", "delivery_context", 128);
        if (!HOST_RE.test(pageHost) || pageHost.includes("..") || pageHost.startsWith(".") || pageHost.endsWith(".")) {
          fail("delivery_context.page_host must be a bounded lowercase registrable hostname");
        }
      }
      break;
    }
    case "provider_outcome": {
      requireFields<ProviderOutcomePayload>(p, "provider_outcome", { outcome: "required", adapter_version: "optional", detail: "optional" });
      const outcome = str(p, "outcome", "provider_outcome", 50);
      if (OUTCOMES[outcome] !== true) fail(`invalid outcome ${JSON.stringify(outcome)}`);
      if ("adapter_version" in p) str(p, "adapter_version", "provider_outcome", 50);
      if ("detail" in p) str(p, "detail", "provider_outcome", 500);
      break;
    }
    case "error": {
      requireFields<ErrorPayload>(p, "error", { code: "required", message: "required" });
      if (!ERROR_CODE_RE.test(str(p, "code", "error", 50))) fail("invalid error code");
      const message = str(p, "message", "error", 1000);
      if (message.length === 0) fail("error.message required");
      break;
    }
    case "hello_ack": {
      requireFields<HelloAckPayload>(p, "hello_ack", { daemon_version: "optional", features: "optional", resolver_origins: "optional" });
      if ("daemon_version" in p) str(p, "daemon_version", "hello_ack", 50);
      if ("features" in p) {
        const features = p["features"];
        if (!Array.isArray(features) || features.length > 32) {
          fail("hello_ack.features must be an array with at most 32 entries");
        }
        for (const feature of features) {
          if (typeof feature !== "string" || Array.from(feature).length === 0 || Array.from(feature).length > 64) {
            fail("hello_ack.features entries must be non-empty strings with at most 64 chars");
          }
        }
      }
      if ("resolver_origins" in p) {
        const origins = p["resolver_origins"];
        if (!Array.isArray(origins) || origins.length > 32) {
          fail("hello_ack.resolver_origins must be an array with at most 32 entries");
        }
        for (const origin of origins) {
          let ok = typeof origin === "string" && origin.length <= 300 && origin.startsWith("https://");
          if (ok) {
            try {
              const u = new URL(origin as string);
              ok = u.protocol === "https:" && u.host !== "" && `${u.protocol}//${u.host}` === origin;
            } catch {
              ok = false;
            }
          }
          if (!ok) fail("hello_ack.resolver_origins entries must be bounded https origins");
        }
      }
      break;
    }
    case "triage_snapshot_request": {
      requireFields<TriageSnapshotRequestPayload>(p, "triage_snapshot_request", { request_id: "required", schema_versions: "required", limit: "optional", cursor: "optional" });
      correlationID(p, "request_id", "triage_snapshot_request");
      const versions = p["schema_versions"];
      if (!Array.isArray(versions) || versions.length !== 1 || (versions[0] !== 1 && versions[0] !== 2)) {
        fail("triage_snapshot_request.schema_versions must be [1] or [2]");
      }
      if ("limit" in p) int(p, "limit", "triage_snapshot_request", 1);
      if ("limit" in p && (p["limit"] as number) > 100) fail("triage_snapshot_request.limit must be <= 100");
      if ("cursor" in p) triageText(p, "cursor", "triage_snapshot_request", 256);
      break;
    }
    case "triage_snapshot_response": {
      requireFields<TriageSnapshotResponsePayload>(p, "triage_snapshot_response", {
        request_id: "required",
        schema: "required",
        generated_at: "required",
        counts: "required",
        items: "required",
        cursor: "optional",
        has_more: "required",
        unsupported_items_count: "required",
      });
      correlationID(p, "request_id", "triage_snapshot_response");
      if (p["schema"] !== 1 && p["schema"] !== 2) fail("triage_snapshot_response.schema must be 1 or 2");
      const schema = p["schema"] as 1 | 2;
      triageTime(p, "generated_at", "triage_snapshot_response");
      triageCounts(p["counts"], "triage_snapshot_response.counts");
      const items = p["items"];
      if (!Array.isArray(items) || items.length > 100) fail("triage_snapshot_response.items must have at most 100 entries");
      for (const item of items) triageItem(item, schema);
      if (typeof p["has_more"] !== "boolean") fail("triage_snapshot_response.has_more must be boolean");
      int(p, "unsupported_items_count", "triage_snapshot_response", 0);
      if (p["has_more"] === true && !("cursor" in p)) fail("triage_snapshot_response.cursor required when has_more");
      if (p["has_more"] === false && "cursor" in p) fail("triage_snapshot_response.cursor must be omitted when not has_more");
      if ("cursor" in p) triageText(p, "cursor", "triage_snapshot_response", 256);
      break;
    }
    case "triage_counts_request": {
      requireFields<TriageCountsRequestPayload>(p, "triage_counts_request", {
        request_id: "required",
        schema_versions: "optional",
      });
      correlationID(p, "request_id", "triage_counts_request");
      if ("schema_versions" in p) {
        const versions = p["schema_versions"];
        if (!Array.isArray(versions) || versions.length !== 1 || (versions[0] !== 1 && versions[0] !== 2)) {
          fail("triage_counts_request.schema_versions must be [1] or [2]");
        }
      }
      break;
    }
    case "triage_counts_response": {
      requireFields<TriageCountsResponsePayload>(p, "triage_counts_response", { request_id: "required", counts: "required" });
      correlationID(p, "request_id", "triage_counts_response");
      triageCounts(p["counts"], "triage_counts_response.counts", true);
      break;
    }
    case "triage_decide": {
      requireFields<TriageDecidePayload>(p, "triage_decide", { request_id: "required", item_id: "required", op: "required", watch_scope: "optional" });
      correlationID(p, "request_id", "triage_decide");
      if (triageText(p, "item_id", "triage_decide", 1024) === "") fail("triage_decide.item_id is required");
      const op = triageText(p, "op", "triage_decide", 20);
      if (op !== "acquire" && op !== "dismiss") fail("triage_decide.op must be acquire or dismiss");
      if (op === "acquire" && "watch_scope" in p) fail("triage_decide.watch_scope is only valid for dismiss");
      if (op === "dismiss") {
        if (!("watch_scope" in p)) fail("triage_decide.watch_scope is required for dismiss");
        const scope = p["watch_scope"];
        if (scope !== "all") {
          if (!Array.isArray(scope) || scope.length < 1 || scope.length > 100) fail("triage_decide.watch_scope must be all or 1..100 watch IDs");
          const seen = new Set<number>();
          for (const id of scope) {
            if (typeof id !== "number" || !Number.isInteger(id) || id < 1 || id > MAX_BROWSER_INTEGER || seen.has(id)) {
              fail("triage_decide.watch_scope contains an invalid watch ID");
            }
            seen.add(id);
          }
        }
      }
      break;
    }
    case "triage_decide_result":
      triageResult(p, "triage_decide_result");
      break;
    case "human_action_resolve": {
      requireFields<HumanActionResolvePayload>(p, "human_action_resolve", { request_id: "required", action_id: "required", verdict: "required", expected_revision: "required", expected_sha256: "optional" });
      correlationID(p, "request_id", "human_action_resolve");
      int(p, "action_id", "human_action_resolve", 1);
      int(p, "expected_revision", "human_action_resolve", 1);
      const verdict = triageText(p, "verdict", "human_action_resolve", 20);
      if (verdict !== "accept" && verdict !== "reject" && verdict !== "dismiss") fail("human_action_resolve.verdict must be accept, reject, or dismiss");
      if (verdict === "accept" && !("expected_sha256" in p)) fail("human_action_resolve.expected_sha256 is required for accept");
      if ("expected_sha256" in p) {
        const sha = triageText(p, "expected_sha256", "human_action_resolve", 64);
        if (!/^[a-f0-9]{64}$/.test(sha)) fail("human_action_resolve.expected_sha256 must be lowercase SHA-256");
      }
      break;
    }
    case "human_action_resolve_result":
      triageResult(p, "human_action_resolve_result");
      break;
    case "review_preview_request": {
      requireFields<ReviewPreviewRequestPayload>(p, "review_preview_request", { request_id: "required", action_id: "required" });
      correlationID(p, "request_id", "review_preview_request");
      int(p, "action_id", "review_preview_request", 1);
      break;
    }
    case "review_preview_result": {
      requireFields<ReviewPreviewResultPayload>(p, "review_preview_result", {
        request_id: "required",
        outcome: "required",
        detail: "optional",
        url: "optional",
        sha256: "optional",
        size_bytes: "optional",
        expires_at: "optional",
      });
      correlationID(p, "request_id", "review_preview_result");
      const outcome = triageText(p, "outcome", "review_preview_result", 10);
      if (outcome !== "ok" && outcome !== "error") fail("review_preview_result.outcome must be ok or error");
      if ("detail" in p) triageText(p, "detail", "review_preview_result", 1000);
      const hasCapability = "url" in p || "sha256" in p || "size_bytes" in p || "expires_at" in p;
      if (outcome === "error") {
        if (hasCapability) fail("review_preview_result: error outcome must not carry capability fields");
        break;
      }
      if ("detail" in p) fail("review_preview_result: ok outcome must not carry a detail");
      if (!("url" in p) || !("sha256" in p) || !("size_bytes" in p) || !("expires_at" in p)) {
        fail("review_preview_result: ok outcome requires url, sha256, size_bytes, expires_at");
      }
      const preview = triageURL(triageText(p, "url", "review_preview_result", 4000), "review_preview_result.url", "http:");
      if (preview.hostname !== "127.0.0.1" || preview.port === "" || !preview.pathname.startsWith("/p/") || preview.search !== "" || preview.hash !== "") {
        fail("review_preview_result.url must be a loopback capability URL");
      }
      if (!/^[a-f0-9]{64}$/.test(triageText(p, "sha256", "review_preview_result", 64))) {
        fail("review_preview_result.sha256 must be lowercase SHA-256");
      }
      int(p, "size_bytes", "review_preview_result", 0);
      triageTime(p, "expires_at", "review_preview_result");
      break;
    }
    case "stats_request": {
      requireFields<StatsRequestPayload>(p, "stats_request", { request_id: "required" });
      correlationID(p, "request_id", "stats_request");
      break;
    }
    case "stats_response": {
      requireFields<StatsResponsePayload>(p, "stats_response", {
        request_id: "required",
        generated_at: "required",
        acquired_total: "required",
        failed_total: "required",
        handoffs_required: "required",
        access: "required",
        series: "required",
      });
      correlationID(p, "request_id", "stats_response");
      triageTime(p, "generated_at", "stats_response");
      int(p, "acquired_total", "stats_response", 0);
      int(p, "failed_total", "stats_response", 0);
      int(p, "handoffs_required", "stats_response", 0);
      const access = asRecord(p["access"], "stats_response.access");
      requireFields<StatsAccess>(access, "stats_response.access", { open_access: "required", institutional: "required", licensed_api: "required", other: "required" });
      for (const key of ["open_access", "institutional", "licensed_api", "other"]) {
        int(access, key, "stats_response.access", 0);
      }
      const series = p["series"];
      if (!Array.isArray(series) || series.length > 60) fail("stats_response.series must have at most 60 entries");
      for (const rawBucket of series) {
        const bucket = asRecord(rawBucket, "stats_response.series");
        requireFields<StatsBucket>(bucket, "stats_response.series", { period_start: "required", acquired: "required" });
        triageTime(bucket, "period_start", "stats_response.series");
        int(bucket, "acquired", "stats_response.series", 0);
      }
      break;
    }
    case "activity_request": {
      requireFields<ActivityRequestPayload>(p, "activity_request", { request_id: "required", limit: "optional" });
      correlationID(p, "request_id", "activity_request");
      if ("limit" in p) {
        const limit = int(p, "limit", "activity_request", 1);
        if (limit > 50) fail("activity_request.limit must be <= 50");
      }
      break;
    }
    case "activity_response": {
      requireFields<ActivityResponsePayload>(p, "activity_response", {
        request_id: "required",
        generated_at: "required",
        entries: "required",
      });
      correlationID(p, "request_id", "activity_response");
      triageTime(p, "generated_at", "activity_response");
      const entries = p["entries"];
      if (!Array.isArray(entries) || entries.length > 50) fail("activity_response.entries must have at most 50 entries");
      for (const rawEntry of entries) {
        const entry = asRecord(rawEntry, "activity_response.entry");
        requireFields<ActivityEntryPayload>(entry, "activity_response.entry", {
          seq: "required",
          at: "required",
          job_id: "optional",
          kind: "required",
          text: "required",
          title: "optional",
        });
        int(entry, "seq", "activity_response.entry", 0);
        triageTime(entry, "at", "activity_response.entry");
        if ("job_id" in entry && !JOB_ID_RE.test(str(entry, "job_id", "activity_response.entry", 128))) {
          fail("activity_response.entry.job_id is invalid");
        }
        if (triageText(entry, "kind", "activity_response.entry", 100) === "") {
          fail("activity_response.entry.kind is required");
        }
        triageText(entry, "text", "activity_response.entry", 160);
        if ("title" in entry) triageText(entry, "title", "activity_response.entry", 500);
      }
      break;
    }
    case "ack":
    case "job_accept":
    case "job_reject":
    case "cancel":
    case "handoff_focus": {
      requireKeys(p, type, []);
      break;
    }
  }
}
