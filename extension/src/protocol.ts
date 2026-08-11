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

export const MsgHandoffLinkRequest = "handoff_link_request" as const;
export const MsgHandoffLinkResult = "handoff_link_result" as const;
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
  | "provider_direct_get_request"
  | "provider_direct_get_result"
  | "provider_drive_epoch_start_request"
  | "provider_drive_epoch_start_result"
  | "provider_drive_epoch_result_request"
  | "provider_drive_epoch_result"
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
  | "delivery_reconcile_request"
  | "delivery_reconcile_result"
  | "handoff_link_request"
  | "handoff_link_result"
  | "review_preview_request"
  | "review_preview_result"
  | "stats_request"
  | "stats_response"
  | "activity_request"
  | "activity_response"
  | "page_bulk_status_request"
  | "page_bulk_status_result"
  | "page_bulk_submit_request"
  | "page_bulk_submit_result"
  | "pdf_grab_request"
  | "pdf_grab_result"
  | "pdf_grab_status_request"
  | "pdf_grab_status_result"
  | "pdf_grab_abandon_request"
  | "pdf_grab_abandon_result"
  | "institutional_candidate_offer"
  | "institutional_claim_request"
  | "institutional_claim_response"
  | "institutional_bind_request"
  | "institutional_bind_response"
  | "institutional_route_request"
  | "institutional_route_response"
  | "institutional_navigated_request"
  | "institutional_navigated_response"
  | "institutional_reconcile_request"
  | "institutional_reconcile_response";

export interface HelloPayload {
  extension_version: string;
  adapter_versions?: Record<string, string>;
  features?: string[];
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
  /** Echoes the page_capture_request this capture answers. Absent on an
   * unsolicited capture (the developer panel's captureFixture), which is what
   * lets the daemon tell the two apart — see papio-85a7420f4cd2564f. */
  request_id?: string;
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
  access_mode?: "assisted" | "delegated";
  expires_at: string;
  /** The institution's Shibboleth IdP entityID, when the daemon knows it.
   * Lets an adapter's federated-login route auto-select the institution on a
   * provider login wall. Optional; an https URL when present. */
  login_entity_id?: string;
  /** The institution's ProQuest account id, when the daemon knows it. Lets the
   * ProQuest adapter unlock the openurl link-resolver by appending
   * ?accountid=<id>. Optional; digits when present. */
  proquest_account_id?: string;
  requires_auth?: boolean;
  drive_attempt_id?: string;
  drive_ordinal?: number;
  drive_strategy?: string;
  drive_revision?: string;
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
  adapter_id?: string;
  adapter_version?: string;
  detail?: string;
}
export interface ProviderDirectGetRequestPayload {
  drive_attempt_id: string;
  ordinal: number;
  route_revision: string;
  expected_identifier: string;
  url: string;
  allowed_origin: string;
  path_family: string;
  terms_policy: "none" | "durable_consent";
}

export type ProviderDirectGetOutcome =
  | "success" | "not_pdf" | "foreign" | "login" | "terms" | "challenge"
  | "cancelled" | "timeout" | "network" | "rate_limited" | "server_error" | "unknown";

export interface ProviderDirectGetResultPayload {
  drive_attempt_id: string;
  ordinal: number;
  route_revision: string;
  outcome: ProviderDirectGetOutcome;
  final_host?: string;
  final_path?: string;
  landing_class: "pdf" | "html" | "login" | "terms" | "challenge" | "foreign" | "unknown";
  detail?: string;
}
export interface ProviderDriveEpochTuple {
  request_id?: string;
  drive_attempt_id: string;
  ordinal: number;
  strategy: string;
  revision: string;
}
export interface ProviderDriveEpochStartRequestPayload extends ProviderDriveEpochTuple {}
export interface ProviderDriveEpochStartResultPayload extends ProviderDriveEpochTuple {
  outcome: "started" | "stale" | "unsupported" | "error";
  detail?: string;
}
export interface ProviderDriveEpochResultRequestPayload extends ProviderDriveEpochTuple {
  outcome: string;
  detail?: string;
}
export interface ProviderDriveEpochResultPayload extends ProviderDriveEpochTuple {
  outcome: "applied" | "stale" | "duplicate" | "unsupported" | "error";
  detail?: string;
}

export interface ErrorPayload {
  code: string;
  message: string;
  request_id?: string;
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
  schema_versions: [1] | [2] | [3] | [4] | [4, 3];
  limit?: number;
  cursor?: string;
}

export interface TriageSnapshotResponsePayload {
  request_id: string;
  schema: 1 | 2 | 3 | 4;
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
  kind: "watch_hit" | "human_action" | "retraction" | "pdf_grab";
  id: string;
  rank: number;
  title: string;
  facts: TriageFact[];
  links: TriageLink[];
  ops: Array<
    | "acquire"
    | "dismiss"
    | "accept"
    | "reject"
    | "open"
    | "retry"
    | "open_request_history"
    | "confirm_request_exists"
    | "confirm_request_absent"
    | "provide_identifier"
  >;
  /** Required on every schema-3 item, forbidden below (triage-snapshot/3). */
  attention?: "working" | "required" | "advisory";
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
  blocked_by?: "anti_bot" | "paywall" | "landing_page" | "login" | "terms" | "delivery_outcome" | "identity_review" | "unknown" | "identifier_missing";
  /** Required on schema-3 human_action items, forbidden below/elsewhere:
   * the existing action-kind vocabulary formalized into a fixed enum, plus
   * document_delivery, decoupled from action_kind's open one (ADR-0016
   * Decision 4). */
  route_class?:
    | "openurl_handoff"
    | "manual_download"
    | "verify_identity"
    | "openurl_available"
    | "human_auth_required"
    | "document_delivery"
    | "downloads_access_required"
    | "terms_acceptance_required"
    | "pdf_identifier_needed";
  /** ADR-0016 Decision 4's tri-state auth carrier as a string enum (never a
   * bare bool): required on schema-3 human_action items, forbidden below.
   * requires_auth stays the narrow execution gate; only this may drive
   * presentation copy. */
  auth_requirement?: "true" | "false" | "unknown";
  /** Present only on a document_delivery human_action item (schema 3). */
  delivery?: TriageDelivery;
  doi?: string;
  label?: string;
  grab?: {
    grab_id: string;
    state: "awaiting_file" | "quarantined" | "identified" | "job_created" | "parked_no_identifier" | "failed_validation";
  };
  nature?: "retraction" | "correction" | "concern";
  noticed_at?: string;
  notice_doi?: string;
}

/** triage-snapshot/3's document_delivery sub-object: the observed provider
 * request a document_delivery human_action item is reconciling. "offered"
 * means papio created the request but has not submitted it to the provider.
 * "fulfilled" means the provider supplied the document — never that papio
 * holds trusted bytes yet (ADR-0017). */
export interface TriageDelivery {
  provider: string;
  provider_reference?: string;
  state: "offered" | "submitted" | "pending" | "fulfilled" | "declined" | "cancelled" | "unknown_outcome";
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

/** Asks the daemon to perform one of triage-snapshot/3's document_delivery
 * reconciliation mutations (ADR-0017 Decision 4) against a job's open
 * document_delivery human action. A new message rather than a widened
 * human_action_resolve — that payload's verdict vocabulary is closed to
 * accept/reject/dismiss against a CAS candidate binding.
 * open_request_history is deliberately absent: it never mutates anything,
 * so the inbox renders it from the item's own delivery sub-object instead
 * of a round trip. */
export interface DeliveryReconcilePayload {
  request_id: string;
  job_id: string;
  operation: "confirm_request_exists" | "confirm_request_absent";
  provider_reference?: string;
}

export interface DeliveryReconcileResultPayload {
  request_id: string;
  outcome: "applied" | "already_applied" | "conflict" | "error";
  detail?: string;
}
export interface HandoffLinkRequestPayload {
  request_id?: string;
  job_id: string;
}

export type HandoffLinkOutcome = "opened" | "job_gone" | "not_open_action" | "not_openurl" | "unavailable";

export interface HandoffLinkResultPayload {
  request_id?: string;
  outcome: HandoffLinkOutcome;
  url?: string;
  detail?: string;
}

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

export interface PageBulkIdentifier {
  local_id: string;
  kind: "doi" | "pmid" | "arxiv" | "openalex";
  value: string;
}

/** rendered_record_count_hint is an honest structural denominator: set only
 * when page-scan.ts recognizes the page's result-list shape (definition-list
 * rows, repeated card containers, reference-list items) and counts the
 * visible records without reading their contents. Omitted, never null, when
 * no shape is recognized (dev/post-build-followups.md item 3). */
export interface PageBulkStatusRequestPayload {
  request_id: string;
  scan_id: string;
  identifiers: PageBulkIdentifier[];
  rendered_record_count_hint?: number;
}

export type PageBulkStatus =
  | "eligible"
  | "owned_with_pdf"
  | "owned_missing_pdf"
  | "queued"
  | "previously_unavailable"
  | "ownership_incomplete"
  | "ownership_unknown"
  | "invalid"
  | "frame_too_large";

export interface PageBulkStatusItem {
  local_id: string;
  /** Omitted when status is "invalid" or "frame_too_large". */
  canonical_key?: string;
  status: PageBulkStatus;
  ownership_complete: boolean;
  /** Present only when status is "queued". */
  job_id?: string;
  /** Present only when status is "owned_missing_pdf" and the match came
   * from a zotio library lookup — the existing Zotero parent item key. */
  zotio_item_key?: string;
}

export interface PageBulkStatusResultPayload {
  request_id: string;
  scan_id: string;
  items: PageBulkStatusItem[];
  truncated: boolean;
}

/** Per-source provenance on the created batch manifest, distinct from the
 * daemon-assigned consumer. origin is the bare scheme+host only — never
 * path, query, fragment, or page title (ADR-0019 Decision 6). */
export interface PageBulkSubmitSource {
  kind: "browser_page";
  origin: string;
  detector: string;
}

export interface PageBulkSubmitRequestPayload {
  request_id: string;
  scan_id: string;
  canonical_keys: string[];
  source: PageBulkSubmitSource;
}

export interface PageBulkSubmitResultPayload {
  request_id: string;
  scan_id: string;
  submitted: number;
  joined: number;
  already_owned: number;
  invalid: number;
  batch_id: string;
}
/** ADR-0020: browser → daemon request to grab an open PDF tab. The
 * extension keeps the full tab URL local; only the bare host crosses the wire. */
export interface PdfGrabRequestPayload {
  request_id: string;
  host: string;
  title?: string;
}

export interface PdfGrabStatusRequestPayload {
  request_id: string;
  grab_id: string;
}

export interface PdfGrabResultPayload {
  request_id?: string;
  grab_id?: string;
  outcome:
    | "steering"
    | "existing"
    | "not_supported"
    | "unavailable"
    | "job_created"
    | "already_owned"
    | "needs_identifier"
    | "failed_validation"
    | "abandoned";
  steering_path?: string;
  detail?: string;
}

export interface PdfGrabStatusResultPayload {
  request_id: string;
  grab_id: string;
  state: "awaiting_file" | "quarantined" | "identified" | "job_created" | "parked_no_identifier" | "failed_validation" | "abandoned" | "";
  outcome?: "not_found" | "unavailable" | "job_created" | "already_owned" | "needs_identifier" | "failed_validation" | "abandoned";
  detail?: string;
  job_id?: string;
}

export interface InstitutionalCandidateOfferPayload {
  candidate_id: string;
  materialization_kind: "browser_tab";
  expires_at: string;
  provider_hosts: string[];
  expected?: JobOfferExpected;
  access_mode?: "assisted" | "delegated";
  login_entity_id?: string;
  proquest_account_id?: string;
  requires_auth?: boolean;
  drive_attempt_id?: string;
  drive_ordinal?: number;
  drive_strategy?: string;
  drive_revision?: string;
}

export interface InstitutionalClaimRequestPayload {
  request_id: string;
  candidate_id: string;
  materialization_kind: "browser_tab" | "direct_download";
}
export interface InstitutionalClaimResponsePayload {
  request_id: string;
  outcome: InstitutionalMaterializationOutcome;
  detail?: string;
  candidate_id?: string;
  claim_id?: string;
  binding_id?: string;
  browser_holder_generation?: number;
  lease_until?: string;
}
export interface InstitutionalBindRequestPayload {
  request_id: string;
  claim_id: string;
  binding_id: string;
  tab_id: number;
}
export interface InstitutionalBindResponsePayload {
  request_id: string;
  outcome: InstitutionalMaterializationOutcome;
  detail?: string;
  claim_id?: string;
  binding_id?: string;
}
export interface InstitutionalRouteRequestPayload {
  request_id: string;
  claim_id: string;
  binding_id: string;
}
export interface InstitutionalRouteResponsePayload {
  outcome: InstitutionalMaterializationOutcome;
  request_id: string;
  detail?: string;
  claim_id?: string;
  binding_id?: string;
  route_issuance_ordinal?: number;
  url?: string;
}
export interface InstitutionalNavigatedRequestPayload {
  claim_id: string;
  request_id: string;
  binding_id: string;
  route_issuance_ordinal: number;
  tab_id: number;
}
export interface InstitutionalNavigatedResponsePayload {
  outcome: InstitutionalMaterializationOutcome;
  request_id: string;
  detail?: string;
  claim_id?: string;
  binding_id?: string;
}

export interface InstitutionalBindingPair {
  binding_id: string;
  tab_id: number;
}

export interface InstitutionalClaimStatus {
  claim_id: string;
  binding_id: string;
  candidate_id: string;
  phase: "claimed" | "bound" | "route_issued" | "navigated" | "settled" | "abandoned";
  tab_id?: number;
}
export interface InstitutionalReconcileRequestPayload {
  bindings: InstitutionalBindingPair[];
  request_id: string;
}
export interface InstitutionalReconcileResponsePayload {
  outcome: InstitutionalMaterializationOutcome;
  request_id: string;
  detail?: string;
  claims?: InstitutionalClaimStatus[];
}

export type InstitutionalMaterializationOutcome =
  | "feature_disabled"
  | "claimed"
  | "bound"
  | "issued"
  | "acknowledged"
  | "reconciled"
  | "stale"
  | "not_eligible"
  | "busy"
  | "error";

export interface PdfGrabAbandonRequestPayload {
  request_id: string;
  grab_id: string;
}

export interface PdfGrabAbandonResultPayload {
  request_id: string;
  grab_id: string;
  state: "awaiting_file" | "quarantined" | "identified" | "job_created" | "parked_no_identifier" | "failed_validation" | "abandoned" | "";
  outcome?: "abandoned" | "not_found" | "unavailable" | "conflict";
  detail?: string;
}

export type PdfGrabDisplayState = "idle" | "grabbed" | "identifying" | "job_created" | "already_owned" | "needs_identifier" | "failed" | "abandoned";

export function durablePdfGrabState(value: unknown): PdfGrabDisplayState | null {
  if (value === "awaiting_file") return "grabbed";
  if (value === "quarantined" || value === "identified") return "identifying";
  if (value === "job_created") return "job_created";
  if (value === "parked_no_identifier") return "needs_identifier";
  if (value === "failed_validation") return "failed";
  if (value === "abandoned") return "abandoned";
  if (value === "idle" || value === "grabbed" || value === "identifying" || value === "already_owned" || value === "needs_identifier" || value === "failed") return value;
  return null;
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

const MSG_TYPES: Record<BrowserMessageType, true> = {
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
  provider_direct_get_request: true,
  provider_direct_get_result: true,
  provider_drive_epoch_start_request: true,
  provider_drive_epoch_start_result: true,
  provider_drive_epoch_result_request: true,
  provider_drive_epoch_result: true,
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
  delivery_reconcile_request: true,
  delivery_reconcile_result: true,
  handoff_link_request: true,
  handoff_link_result: true,
  review_preview_request: true,
  review_preview_result: true,
  stats_request: true,
  stats_response: true,
  activity_request: true,
  activity_response: true,
  page_bulk_status_request: true,
  page_bulk_status_result: true,
  page_bulk_submit_request: true,
  page_bulk_submit_result: true,
  pdf_grab_request: true,
  pdf_grab_result: true,
  pdf_grab_status_request: true,
  pdf_grab_status_result: true,
  pdf_grab_abandon_request: true,
  pdf_grab_abandon_result: true,
  institutional_candidate_offer: true,
  institutional_claim_request: true,
  institutional_claim_response: true,
  institutional_bind_request: true,
  institutional_bind_response: true,
  institutional_route_request: true,
  institutional_route_response: true,
  institutional_navigated_request: true,
  institutional_navigated_response: true,
  institutional_reconcile_request: true,
  institutional_reconcile_response: true,
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
  provider_direct_get_request: true,
  provider_direct_get_result: true,
  provider_drive_epoch_start_request: true,
  provider_drive_epoch_start_result: true,
  provider_drive_epoch_result_request: true,
  provider_drive_epoch_result: true,
  cancel: true,
  handoff_focus: true,
  institutional_candidate_offer: true,
  institutional_claim_request: true,
  institutional_claim_response: true,
  institutional_bind_request: true,
  institutional_bind_response: true,
  institutional_route_request: true,
  institutional_route_response: true,
  institutional_navigated_request: true,
  institutional_navigated_response: true,
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
const CLIENT_FEATURE_RE = /^[a-z0-9_]+$/;
const JOB_ID_RE = /^[A-Za-z0-9_-]{8,128}$/;
// ZOTERO_KEY_RE must stay byte-identical to zoteroKeyRE in
// internal/protocol/protocol.go.
const ZOTERO_KEY_RE = /^[A-Za-z0-9]{1,32}$/;
const HOST_RE = /^[a-z0-9.-]{3,253}$/;
// ORIGIN_HOST_RE is the resolver-origin host grammar used ONLY by
// session_evidence.origin_hint (see the validation block below for why it
// exists alongside HOST_RE rather than reusing it): lowercase RFC 1035
// labels (alnum first/last character, hyphens interior only, 1-63 chars per
// label) joined by single dots — ONE label or several. Must stay
// byte-identical to originHostRE in internal/protocol/protocol.go and the
// host portion of session_evidence.origin_hint's pattern in
// protocol/browser-v1.schema.json — label count is deliberately
// unconstrained; see the validation block below for why.
const ORIGIN_HOST_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/;
const ERROR_CODE_RE = /^[a-z0-9_]{2,50}$/;
const FILENAME_RE = /^[^/\\]{1,255}$/u;
const RFC3339_RE =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|([+-])(\d{2}):(\d{2}))$/;
const BASE64_RE = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
// Must stay byte-identical to the steering-path pattern
// internal/protocol/protocol.go's pdf_grab_result validation enforces.
const PDF_GRAB_STEERING_PATH_RE = /^papio\/grabs\/[A-Za-z0-9_-]{8,64}\/$/;
function escapeDirectGetIdentifier(identifier: string): string {
  const hex = "0123456789ABCDEF";
  const encoder = new TextEncoder();
  const encoded: string[] = [];
  for (const segment of identifier.split("/")) {
    let value = "";
    const bytes = encoder.encode(segment);
    const dotSegment = segment === "." || segment === "..";
    for (const byte of bytes) {
      const unreserved =
        !dotSegment &&
        ((byte >= 0x41 && byte <= 0x5a) ||
          (byte >= 0x61 && byte <= 0x7a) ||
          (byte >= 0x30 && byte <= 0x39) ||
          byte === 0x2d ||
          byte === 0x2e ||
          byte === 0x5f ||
          byte === 0x7e);
      value += unreserved ? String.fromCharCode(byte) : `%${hex[byte >> 4]}${hex[byte & 0x0f]}`;
    }
    encoded.push(value);
  }
  return encoded.join("/");
}
function directGetIdentifierUnsafe(value: string): boolean {
  for (const char of value) {
    const code = char.codePointAt(0)!;
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true;
  }
  return false;
}

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
/** Every field of the payload type must be listed, so a field added to the
 * interface without a matching parser disposition is a typecheck error.
 * `forbidden` names a field the wire contract defines but this schema version
 * must reject (e.g. a v2-only counter seen on a v1 frame). */
type FieldSpec<T> = { [K in keyof T & string]-?: "required" | "optional" | "forbidden" };

function requireFields<T>(obj: Record<string, unknown>, what: string, spec: FieldSpec<T>): void {
  for (const key of Object.keys(obj)) {
    if (!(key in spec)) fail(`${what}: unknown field ${JSON.stringify(key)} (fail closed)`);
  }
  for (const key of Object.keys(spec)) {
    const disposition = spec[key as keyof FieldSpec<T>];
    if (disposition === "required" && !(key in obj)) {
      fail(`${what}: missing required field ${JSON.stringify(key)}`);
    }
    if (disposition === "forbidden" && key in obj) {
      fail(`${what}: unknown field ${JSON.stringify(key)} (fail closed)`);
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

const RFC3986_URI_TEXT_RE = /^[A-Za-z][A-Za-z0-9+.-]*:[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]*$/;
const INVALID_URI_ESCAPE_RE = /%(?![0-9A-Fa-f]{2})/;

function triageURL(value: string, what: string, scheme: "http:" | "https:"): URL {
  rejectNUL(value, what);
  if (!RFC3986_URI_TEXT_RE.test(value) || INVALID_URI_ESCAPE_RE.test(value)) {
    fail(`${what} must be an RFC 3986 URI`);
  }
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== scheme || parsed.host === "") fail(`${what} must be a ${scheme} URL`);
    return parsed;
  } catch (e) {
    if (e instanceof ProtocolError) throw e;
    fail(`${what} must be a ${scheme} URL`);
  }
}

function triageCounts(raw: unknown, what: string, allowAuth = false, additionalPending = 0, allowPartial = false): void {
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
    actions_requires_auth: allowAuth ? "optional" : "forbidden",
    retractions: "required",
    jobs_working: "required",
    jobs_needs_review: "required",
    failure_groups_7d: "required",
  });
  const pending = int(counts, "pending_total", what, 0);
  const visible = int(counts, "watch_hits", what, 0) + int(counts, "actions", what, 0) + int(counts, "retractions", what, 0);
  const expected = visible + additionalPending;
  if (allowPartial ? pending < expected : pending !== expected) {
    fail(`${what}.pending_total must ${allowPartial ? "be at least" : "equal"} visible items plus pdf grabs`);
  }
  for (const key of fields.slice(4)) int(counts, key, what, 0);
  if (allowAuth && "actions_requires_auth" in counts) int(counts, "actions_requires_auth", what, 0);
}
const ROUTE_CLASSES = [
  "openurl_handoff",
  "manual_download",
  "verify_identity",
  "openurl_available",
  "human_auth_required",
  "terms_acceptance_required",
  "document_delivery",
  "downloads_access_required",
];

// blockedByV2 is schema 2's exact closed set, shipped and locked: a
// schema-2 frame must never carry a value outside it. blockedByV3 is
// schema 3's strict superset; identifier_missing is reserved for v4 grabs.
const BLOCKED_BY_V2 = ["anti_bot", "paywall", "landing_page"];
const BLOCKED_BY_V3 = [...BLOCKED_BY_V2, "login", "terms", "delivery_outcome", "identity_review", "unknown"];

function triageItem(raw: unknown, schema: 1 | 2 | 3 | 4): void {
  const item = asRecord(raw, "triage item");
  const kind = triageText(item, "kind", "triage item", 50);
  if (kind === "pdf_grab") {
    if (schema !== 4) fail("pdf_grab items require triage-snapshot/4");
    requireKeys(item, "triage item pdf_grab", ["kind", "label", "grab", "route_class", "blocked_by", "attention", "ops"]);
    triageText(item, "label", "triage item pdf_grab", 500);
    const grab = asRecord(item["grab"], "triage item pdf_grab.grab");
    requireKeys(grab, "triage item pdf_grab.grab", ["grab_id", "state"]);
    if (triageText(item, "label", "triage item pdf_grab", 500) === "") fail("pdf_grab.label is required");
    const state = triageText(grab, "state", "triage item pdf_grab.grab", 50);
    if (!["awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation"].includes(state)) {
      fail("pdf_grab.state is invalid");
    }
    const grabID = triageText(grab, "grab_id", "triage item pdf_grab.grab", 128);
    if (!/^[A-Za-z0-9_-]+$/.test(grabID)) fail("pdf_grab.grab_id is invalid");
    if (triageText(item, "route_class", "triage item pdf_grab", 100) !== "pdf_identifier_needed") fail("pdf_grab.route_class is invalid");
    if (triageText(item, "blocked_by", "triage item pdf_grab", 50) !== "identifier_missing") fail("pdf_grab.blocked_by is invalid");
    if (triageText(item, "attention", "triage item pdf_grab", 20) !== "required") fail("pdf_grab.attention is invalid");
    const ops = item["ops"];
    if (!Array.isArray(ops) || ops.length !== 2 || ops[0] !== "provide_identifier" || ops[1] !== "dismiss") fail("pdf_grab.ops is invalid");
    return;
  }
  const core = ["kind", "id", "rank", "title", "facts", "links", "ops"];
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
  // attention is required on every schema-3 item and forbidden below;
  // route_class/auth_requirement are required on schema-3 human_action
  // items and forbidden below (triage-snapshot/3). Putting them in the
  // required list for schema 3 and leaving them out entirely otherwise
  // makes requireKeys enforce both directions: present-and-required, or
  // absent-because-unknown-field.
  if (schema >= 3) {
    extra = [...extra, "attention"];
    if (kind === "human_action") extra = [...extra, "route_class", "auth_requirement"];
  }
  const optional =
    kind === "human_action" && schema >= 2
      ? ["requires_auth", "blocked_by", ...(schema >= 3 ? ["delivery"] : [])]
      : kind === "retraction"
        ? ["notice_doi"]
        : [];
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
    if (
      typeof rawOp !== "string" ||
      !["acquire", "dismiss", "accept", "reject", "open", "retry", "open_request_history", "confirm_request_exists", "confirm_request_absent"].includes(rawOp) ||
      seenOps.has(rawOp)
    ) {
      fail("triage item.ops contains an invalid or repeated operation");
    }
    seenOps.add(rawOp);
  }
  if (schema >= 3) {
    const attention = triageText(item, "attention", "triage item", 20);
    if (!["working", "required", "advisory"].includes(attention)) fail("triage item.attention is invalid");
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
    const actionKind = triageText(item, "action_kind", "human_action", 100);
    if (actionKind === "") fail("human_action.action_kind is required");
    if (triageText(item, "job_state", "human_action", 50) === "") fail("human_action.job_state is required");
    int(item, "revision", "human_action", 1);
    const sha = triageText(item, "sha256", "human_action", 64);
    if (sha !== "" && !/^[a-f0-9]{64}$/.test(sha)) fail("human_action.sha256 must be lowercase SHA-256");
    int(item, "size_bytes", "human_action", 0);
    if (schema >= 2) {
      if (("requires_auth" in item) !== ("blocked_by" in item)) {
        fail("human_action.requires_auth and blocked_by must be present together");
      }
      if ("requires_auth" in item && typeof item["requires_auth"] !== "boolean") {
        fail("human_action.requires_auth must be a boolean");
      }
      if ("blocked_by" in item) {
        const blockedBy = triageText(item, "blocked_by", "human_action", 50);
        const allowedBlockedBy = schema >= 3 ? BLOCKED_BY_V3 : BLOCKED_BY_V2;
        if (!allowedBlockedBy.includes(blockedBy)) {
          fail("human_action.blocked_by is invalid");
        }
      }
    }
    if (schema >= 3) {
      const routeClass = triageText(item, "route_class", "human_action", 100);
      if (!ROUTE_CLASSES.includes(routeClass)) fail("human_action.route_class is invalid");
      const authRequirement = triageText(item, "auth_requirement", "human_action", 10);
      if (!["true", "false", "unknown"].includes(authRequirement)) fail("human_action.auth_requirement is invalid");
      if ("delivery" in item) {
        if (actionKind !== "document_delivery") fail("human_action.delivery is only valid for document_delivery items");
        const delivery = asRecord(item["delivery"], "human_action.delivery");
        requireKeys(delivery, "human_action.delivery", ["provider", "state"], ["provider_reference"]);
        if (triageText(delivery, "provider", "human_action.delivery", 100) === "") {
          fail("human_action.delivery.provider is required");
        }
        if ("provider_reference" in delivery) triageText(delivery, "provider_reference", "human_action.delivery", 300);
        const state = triageText(delivery, "state", "human_action.delivery", 20);
        if (!["offered", "submitted", "pending", "fulfilled", "declined", "cancelled", "unknown_outcome"].includes(state)) {
          fail("human_action.delivery.state is invalid");
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
function institutionalID(obj: Record<string, unknown>, key: string, what: string): string {
  const value = str(obj, key, what, 128);
  if (!JOB_ID_RE.test(value)) fail(`${what}.${key} must be an opaque bounded ID`);
  return value;
}

function institutionalOutcome(obj: Record<string, unknown>, what: string): InstitutionalMaterializationOutcome {
  const outcome = str(obj, "outcome", what, 32) as InstitutionalMaterializationOutcome;
  if (!["feature_disabled", "claimed", "bound", "issued", "acknowledged", "reconciled", "stale", "not_eligible", "busy", "error"].includes(outcome)) {
    fail(`${what}.outcome is invalid`);
  }
  return outcome;
}

function institutionalFailure(p: Record<string, unknown>, what: string, outcome: InstitutionalMaterializationOutcome, success: InstitutionalMaterializationOutcome): void {
  if (outcome === success && "detail" in p) fail(`${what}.${success} must not carry detail`);
  if ("detail" in p) triageText(p, "detail", what, 1000);
}
function institutionalOutcomeOneOf(
  outcome: InstitutionalMaterializationOutcome,
  what: string,
  allowed: readonly InstitutionalMaterializationOutcome[],
): void {
  if (!allowed.includes(outcome)) fail(`${what}.outcome is invalid`);
}
function institutionalTabID(obj: Record<string, unknown>, key: string, what: string): void {
  const value = obj[key];
  if (typeof value !== "number" || !Number.isInteger(value) || !Number.isSafeInteger(value) || value < 0) {
    fail(`${what}.${key} must be a safe nonnegative integer`);
  }
}

/** Parse one decoded JSON value as a bridge message, failing closed. */
export function parseBrowserMessage(raw: unknown): BrowserMessage {
  const env = asRecord(raw, "message");
  requireKeys(env, "message", ["protocol", "type", "msg_id", "seq", "payload"], ["job_id"]);
  if (env["protocol"] !== BROWSER_PROTOCOL_VERSION) {
    fail(`protocol ${JSON.stringify(env["protocol"])}, want ${BROWSER_PROTOCOL_VERSION}`);
  }
  const type = str(env, "type", "message", 50);
  if (!Object.prototype.hasOwnProperty.call(MSG_TYPES, type)) fail(`unknown type ${JSON.stringify(type)} (fail closed)`);
  const msgID = str(env, "msg_id", "message", 64);
  if (!MSG_ID_RE.test(msgID)) fail(`invalid msg_id ${JSON.stringify(msgID)}`);
  const seq = int(env, "seq", "message", 0);
  let jobID: string | undefined;
  if ("job_id" in env) {
    jobID = str(env, "job_id", "message", 128);
    if (!JOB_ID_RE.test(jobID)) fail(`invalid job_id ${JSON.stringify(jobID)}`);
  }
  if ((type === "institutional_reconcile_request" || type === "institutional_reconcile_response") && jobID !== undefined) {
    fail(`type ${type} must not carry job_id`);
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
      requireFields<HelloPayload>(p, "hello", {
        extension_version: "required",
        adapter_versions: "optional",
        features: "optional",
      });
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
      if ("features" in p) {
        const rawFeatures = p["features"];
        if (!Array.isArray(rawFeatures)) fail("hello.features must be an array");
        if (rawFeatures.length > 32) fail("hello.features capped at 32");
        const seen = new Set<string>();
        for (const rawFeature of rawFeatures) {
          if (typeof rawFeature !== "string" || Array.from(rawFeature).length < 1 || Array.from(rawFeature).length > 64 || !CLIENT_FEATURE_RE.test(rawFeature)) {
            fail("hello.features entries must match [a-z0-9_]{1,64}");
          }
          if (seen.has(rawFeature)) fail(`hello.features contains duplicate ${JSON.stringify(rawFeature)}`);
          seen.add(rawFeature);
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
        request_id: "optional",
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
      if ("request_id" in p) correlationID(p, "request_id", "page_capture");
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
        access_mode: "optional",
        expires_at: "required",
        login_entity_id: "optional",
        proquest_account_id: "optional",
        requires_auth: "optional",
        drive_attempt_id: "optional",
        drive_ordinal: "optional",
        drive_strategy: "optional",
        drive_revision: "optional",
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
      if ("access_mode" in p) {
        const mode = str(p, "access_mode", "job_offer", 20);
        if (mode !== "assisted" && mode !== "delegated") fail(`invalid access_mode ${JSON.stringify(mode)}; expected "assisted" or "delegated"`);
      }
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
      if ("drive_attempt_id" in p) correlationID(p, "drive_attempt_id", "job_offer");
      if ("drive_ordinal" in p) int(p, "drive_ordinal", "job_offer", 0);
      if ("drive_strategy" in p) str(p, "drive_strategy", "job_offer", 128);
      if ("drive_revision" in p) str(p, "drive_revision", "job_offer", 128);
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
        // Two checks, each catching a different way a value can fail to be
        // a bare, lowercase resolver origin:
        //
        // 1. The round-trip equality below is what actually rejects a
        //    mixed-case host such as "https://EXAMPLE.com": the WHATWG URL
        //    parser lowercases the host of a special scheme internally, so
        //    `${parsed.protocol}//${parsed.host}` no longer equals the raw
        //    uppercase input once parsed. That also happens to reject any
        //    path/query/fragment/userinfo the raw string carried, since
        //    those are stripped out of `parsed.host`. It does NOT check
        //    host charset on its own — every legal WHATWG host round-trips
        //    cleanly and would pass this check alone.
        // 2. ORIGIN_HOST_RE therefore separately enforces the charset.
        //    Label count is deliberately NOT constrained: an earlier
        //    version of this rule required two-or-more labels on the
        //    unverified assumption that "a single-label host is never a
        //    real institutional resolver origin". That assumption was
        //    checked against internal/config/config.go's
        //    validateOpenURLBase (only an https scheme and a non-empty
        //    host — no FQDN, no label count) and found false:
        //    browser.openurl_base_url = "https://library" is a valid
        //    config today for an intranet resolver, and this module's
        //    resolverOriginHint/latestResolverOrigin derive origin_hint
        //    straight from that configured origin. Rejecting a value a
        //    legitimate config can produce is a release blocker, not a
        //    safety margin — Bridge.send self-validates every outbound
        //    frame and silently drops an invalid one, so an over-strict
        //    host grammar here permanently starves that institution's
        //    session_evidence signal, and the same value is a fatal
        //    inbound decode under version skew. Do not re-add a label-count
        //    or minimum-length bound without first tightening
        //    validateOpenURLBase to match. Case is rejected rather than
        //    normalized even though internal/config/config.go's
        //    ResolverProfileForOrigin already lowercases and
        //    case-insensitively matches the decoded hint, so case alone
        //    cannot currently misroute a match — that downstream leniency
        //    isn't a reason to leave the wire format ambiguous, and
        //    uppercase is unreachable from a genuine producer (origin_hint
        //    is always derived via `new URL(...)`, which lowercases the
        //    host for https) so rejecting it costs nothing real. A
        //    bracketed IPv6 literal is rejected too, but not by a dedicated
        //    check: ORIGIN_HOST_RE's charset has no room for '[', ']', or
        //    ':'. This is the one deliberate gap against "never reject
        //    what a valid config can produce": validateOpenURLBase
        //    (internal/config/config.go) does not exclude an IPv6-literal
        //    openurl_base_url either. It is accepted anyway, scoped
        //    narrowly, because the two runtimes cannot even agree on what
        //    string to test: this parser's `.hostname` keeps the brackets
        //    ("[::1]") while Go's Hostname() strips them ("::1"), AND —
        //    unlike a DNS host, which both runtimes lowercase identically
        //    before this validator ever sees it — Go's net/url does NOT
        //    lowercase IPv6 hex digits while the WHATWG URL parser does.
        //    Accepting IPv6 correctly would need a second,
        //    bracket-and-case-aware code path duplicated three ways
        //    instead of one shared regex — exactly the drift this function
        //    exists to close. No observed browser.resolvers.* origin is an
        //    IPv6 literal, so the safer near-term call is to reject the
        //    one host shape the three implementations cannot trivially
        //    agree on. Revisit with an explicit bracket-aware rule in all
        //    three places together if a real deployment ever needs it.
        //    This matches internal/protocol/protocol.go's
        //    validateResolverOriginHint over the DNS-shaped subset a
        //    genuine producer emits, not byte-for-byte in every corner:
        //    this round-trip check also folds a purely-numeric final host
        //    label into an IPv4 address (WHATWG's "host ends in a number"
        //    rule) and drops a port's leading zeros before comparing, so
        //    "https://123" and "https://library:08443" — which Go and the
        //    schema both accept — fail here. Neither shape is reachable
        //    from a genuine producer; see validateResolverOriginHint's doc
        //    comment for the full accepted-gap list and why closing them
        //    isn't worth duplicating WHATWG's parser in two more places.
        try {
          const parsed = new URL(origin);
          if (
            parsed.protocol !== "https:" ||
            parsed.host === "" ||
            `${parsed.protocol}//${parsed.host}` !== origin ||
            !ORIGIN_HOST_RE.test(parsed.hostname)
          ) {
            fail("session_evidence.origin_hint must be a bare https origin with a lowercase resolver host");
          }
        } catch (e) {
          if (e instanceof ProtocolError) throw e;
          fail("session_evidence.origin_hint must be a bare https origin with a lowercase resolver host");
        }
      }
      break;
    }
    // download_id must be >= 1, not >= 0: it is the correlation key half of
    // browserDownloadKey{JobID, DownloadID} in internal/browser/bridge.go, and
    // two downloads reported as download_id 0 for one job collide on that key
    // (download_complete overwrites the first pending entry's CandidateID, and
    // delivery_context then applies the first download's access_basis to the
    // second, unrelated candidate). chrome.downloads allocates ids starting at
    // 1, so a genuine extension never sends 0 — this only closes the gap for a
    // buggy or compromised client.
    case "download_started": {
      requireFields<DownloadStartedPayload>(p, "download_started", { download_id: "required", filename: "required" });
      int(p, "download_id", "download_started", 1);
      if (!FILENAME_RE.test(str(p, "filename", "download_started", 255))) {
        fail("download_started.filename must be a bare name without path separators");
      }
      break;
    }
    case "download_complete": {
      requireFields<DownloadCompletePayload>(p, "download_complete", { download_id: "required", filename: "required", size_bytes: "required" });
      int(p, "download_id", "download_complete", 1);
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
      int(p, "download_id", "delivery_context", 1);
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
      requireFields<ProviderOutcomePayload>(p, "provider_outcome", {
        outcome: "required",
        adapter_id: "optional",
        adapter_version: "optional",
        detail: "optional",
      });
      const outcome = str(p, "outcome", "provider_outcome", 50);
      if (OUTCOMES[outcome] !== true) fail(`invalid outcome ${JSON.stringify(outcome)}`);
      if ("adapter_id" in p && !/^[A-Za-z0-9_-]{1,64}$/.test(str(p, "adapter_id", "provider_outcome", 64))) {
        fail("provider_outcome.adapter_id must use the id charset");
      }
      if ("adapter_version" in p) str(p, "adapter_version", "provider_outcome", 50);
      if ("detail" in p) str(p, "detail", "provider_outcome", 500);
      break;
      }
    case "provider_direct_get_request": {
      requireFields<ProviderDirectGetRequestPayload>(p, "provider_direct_get_request", {
        drive_attempt_id: "required", ordinal: "required", route_revision: "required",
        expected_identifier: "required", url: "required", allowed_origin: "required", path_family: "required",
        terms_policy: "required",
      });
      correlationID(p, "drive_attempt_id", "provider_direct_get_request");
      int(p, "ordinal", "provider_direct_get_request", 0);
      const revision = str(p, "route_revision", "provider_direct_get_request", 128);
      if (!revision.includes("/")) fail("provider_direct_get_request.route_revision is invalid");
      const expected = str(p, "expected_identifier", "provider_direct_get_request", 256);
      const family = str(p, "path_family", "provider_direct_get_request", 512);
      if (/[?#{}\\@\s\u0000\r\n]/u.test(expected) || /[?#\u0000\r\n\\]/u.test(family) || directGetIdentifierUnsafe(expected)) fail("provider_direct_get_request envelope text is invalid");
      const termsPolicy = str(p, "terms_policy", "provider_direct_get_request", 32);
      if (termsPolicy !== "none" && termsPolicy !== "durable_consent") {
        fail("provider_direct_get_request.terms_policy is invalid");
      }
      try {
        const target = new URL(str(p, "url", "provider_direct_get_request", 2048));
        const origin = new URL(str(p, "allowed_origin", "provider_direct_get_request", 300));
        if (target.protocol !== "https:" || origin.protocol !== "https:" || target.username || target.password ||
            target.hash || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash ||
            target.host !== origin.host || (target.search !== "" && target.search !== "?download=true")) {
          fail("provider_direct_get_request URL is outside its declared envelope");
        }
        const split = expected.indexOf(":");
        if (split < 1 || split === expected.length - 1) fail("provider_direct_get_request.expected_identifier is invalid");
        const placeholder = `{${expected.slice(0, split)}}`;
        const openCount = family.split("{").length - 1;
        const closeCount = family.split("}").length - 1;
        const marker = family.indexOf(placeholder);
        const escapedIdentifier = escapeDirectGetIdentifier(expected.slice(split + 1));
        if (
          /[{}]/u.test(expected.slice(0, split)) ||
          openCount !== 1 ||
          closeCount !== 1 ||
          marker < 1 ||
          target.pathname !== `${family.slice(0, marker)}${escapedIdentifier}${family.slice(marker + placeholder.length)}`
        ) {
          fail("provider_direct_get_request URL path does not match exactly one path_family placeholder");
        }
      } catch {
        fail("provider_direct_get_request URL is invalid");
      }
      break;
    }
    case "provider_drive_epoch_start_request": {
      requireFields<ProviderDriveEpochStartRequestPayload>(p, type, {
        request_id: "optional",
        drive_attempt_id: "required",
        ordinal: "required",
        strategy: "required",
        revision: "required",
      });
      correlationID(p, "drive_attempt_id", type);
      int(p, "ordinal", type, 0);
      const strategy = str(p, "strategy", type, 128);
      const revision = str(p, "revision", type, 128);
      if (/[\u0000\r\n]/u.test(strategy + revision)) fail(`${type} tuple text is invalid`);
      break;
    }
    case "provider_drive_epoch_start_result": {
      requireFields<ProviderDriveEpochStartResultPayload>(p, type, {
        request_id: "optional",
        drive_attempt_id: "required",
        ordinal: "required",
        strategy: "required",
        revision: "required",
        outcome: "required",
        detail: "optional",
      });
      correlationID(p, "drive_attempt_id", type);
      int(p, "ordinal", type, 0);
      const strategy = str(p, "strategy", type, 128);
      const revision = str(p, "revision", type, 128);
      if (/[\u0000\r\n]/u.test(strategy + revision)) fail(`${type} tuple text is invalid`);
      const outcome = str(p, "outcome", type, 64);
      if (!["started", "stale", "unsupported", "error"].includes(outcome)) fail(`${type}.outcome is invalid`);
      if ("detail" in p) str(p, "detail", type, 500);
      break;
    }
    case "provider_drive_epoch_result_request": {
      requireFields<ProviderDriveEpochResultRequestPayload>(p, type, {
        request_id: "optional",
        drive_attempt_id: "required",
        ordinal: "required",
        strategy: "required",
        revision: "required",
        outcome: "required",
        detail: "optional",
      });
      correlationID(p, "drive_attempt_id", type);
      int(p, "ordinal", type, 0);
      const strategy = str(p, "strategy", type, 128);
      const revision = str(p, "revision", type, 128);
      if (/[\u0000\r\n]/u.test(strategy + revision)) fail(`${type} tuple text is invalid`);
      str(p, "outcome", type, 64);
      if ("detail" in p) str(p, "detail", type, 500);
      break;
    }
    case "provider_drive_epoch_result": {
      requireFields<ProviderDriveEpochResultPayload>(p, type, {
        request_id: "optional",
        drive_attempt_id: "required",
        ordinal: "required",
        strategy: "required",
        revision: "required",
        outcome: "required",
        detail: "optional",
      });
      correlationID(p, "drive_attempt_id", type);
      int(p, "ordinal", type, 0);
      const strategy = str(p, "strategy", type, 128);
      const revision = str(p, "revision", type, 128);
      if (/[\u0000\r\n]/u.test(strategy + revision)) fail(`${type} tuple text is invalid`);
      const outcome = str(p, "outcome", type, 64);
      if (!["applied", "stale", "duplicate", "unsupported", "error"].includes(outcome)) fail(`${type}.outcome is invalid`);
      if ("detail" in p) str(p, "detail", type, 500);
      break;
    }
    case "provider_direct_get_result": {
      requireFields<ProviderDirectGetResultPayload>(p, "provider_direct_get_result", {
        drive_attempt_id: "required", ordinal: "required", route_revision: "required", outcome: "required",
        final_host: "optional", final_path: "optional", landing_class: "required", detail: "optional",
      });
      correlationID(p, "drive_attempt_id", "provider_direct_get_result");
      int(p, "ordinal", "provider_direct_get_result", 0);
      const outcome = str(p, "outcome", "provider_direct_get_result", 50);
      if (!(["success", "not_pdf", "foreign", "login", "terms", "challenge", "cancelled", "timeout", "network", "rate_limited", "server_error", "unknown"] as string[]).includes(outcome)) fail("provider_direct_get_result.outcome is invalid");
      const landing = str(p, "landing_class", "provider_direct_get_result", 20);
      if (!(["pdf", "html", "login", "terms", "challenge", "foreign", "unknown"] as string[]).includes(landing)) fail("provider_direct_get_result.landing_class is invalid");
      if ("final_host" in p) {
        const host = str(p, "final_host", "provider_direct_get_result", 253);
        if (!HOST_RE.test(host) || host !== host.toLowerCase()) fail("provider_direct_get_result.final_host is invalid");
      }
      if ("final_path" in p) {
        const path = str(p, "final_path", "provider_direct_get_result", 1000);
        if (!path.startsWith("/") || /[?#\u0000\r\n]/u.test(path)) fail("provider_direct_get_result.final_path is invalid");
      }
      if (outcome === "success" && (landing !== "pdf" || !("final_host" in p) || !("final_path" in p))) fail("provider_direct_get_result success requires final envelope");
      if ("detail" in p) str(p, "detail", "provider_direct_get_result", 500);
      break;
    }
    case "error": {
      requireFields<ErrorPayload>(p, "error", { code: "required", message: "required", request_id: "optional" });
      if (!ERROR_CODE_RE.test(str(p, "code", "error", 50))) fail("invalid error code");
      const message = str(p, "message", "error", 1000);
      if (message.length === 0) fail("error.message required");
      if ("request_id" in p) correlationID(p, "request_id", "error");
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
      if (
        !Array.isArray(versions) ||
        !(
          (versions.length === 1 && (versions[0] === 1 || versions[0] === 2 || versions[0] === 3 || versions[0] === 4)) ||
          (versions.length === 2 && versions[0] === 4 && versions[1] === 3)
        )
      ) {
        fail("triage_snapshot_request.schema_versions must be [1], [2], [3], [4], or [4,3]");
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
      if (p["schema"] !== 1 && p["schema"] !== 2 && p["schema"] !== 3 && p["schema"] !== 4) fail("triage_snapshot_response.schema must be 1, 2, 3, or 4");
      const schema = p["schema"] as 1 | 2 | 3 | 4;
      const items = p["items"];
      if (!Array.isArray(items) || items.length > 100) fail("triage_snapshot_response.items must have at most 100 entries");
      const pdfGrabCount = schema === 4 ? items.filter((item) => typeof item === "object" && item !== null && (item as Record<string, unknown>)["kind"] === "pdf_grab").length : 0;
      const allowFloor = schema === 4;
      triageCounts(p["counts"], "triage_snapshot_response.counts", false, pdfGrabCount, allowFloor);
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
    case "delivery_reconcile_request": {
      requireFields<DeliveryReconcilePayload>(p, "delivery_reconcile_request", {
        request_id: "required",
        job_id: "required",
        operation: "required",
        provider_reference: "optional",
      });
      correlationID(p, "request_id", "delivery_reconcile_request");
      const jobID = str(p, "job_id", "delivery_reconcile_request", 128);
      if (!JOB_ID_RE.test(jobID)) fail("delivery_reconcile_request.job_id is invalid");
      const operation = triageText(p, "operation", "delivery_reconcile_request", 30);
      if (operation !== "confirm_request_exists" && operation !== "confirm_request_absent") {
        fail("delivery_reconcile_request.operation must be confirm_request_exists or confirm_request_absent");
      }
      if (operation === "confirm_request_exists") {
        if (!("provider_reference" in p) || triageText(p, "provider_reference", "delivery_reconcile_request", 300) === "") {
          fail("delivery_reconcile_request.provider_reference is required for confirm_request_exists");
        }
      } else if ("provider_reference" in p) {
        fail("delivery_reconcile_request.provider_reference is only valid for confirm_request_exists");
      }
      break;
    }
    case "delivery_reconcile_result":
      triageResult(p, "delivery_reconcile_result");
      break;
    case "handoff_link_request": {
      requireFields<HandoffLinkRequestPayload>(p, "handoff_link_request", { request_id: "optional", job_id: "required" });
      const jobID = str(p, "job_id", "handoff_link_request", 128);
      if (!JOB_ID_RE.test(jobID)) fail("handoff_link_request.job_id is invalid");
      if ("request_id" in p) correlationID(p, "request_id", "handoff_link_request");
      break;
    }
    case "handoff_link_result": {
      requireFields<HandoffLinkResultPayload>(p, "handoff_link_result", {
        request_id: "optional",
        outcome: "required",
        url: "optional",
        detail: "optional",
      });
      if ("request_id" in p) correlationID(p, "request_id", "handoff_link_result");
      const outcome = str(p, "outcome", "handoff_link_result", 30) as HandoffLinkOutcome;
      if (!["opened", "job_gone", "not_open_action", "not_openurl", "unavailable"].includes(outcome)) {
        fail("handoff_link_result.outcome is invalid");
      }
      if (outcome === "opened") {
        if (!("url" in p) || "detail" in p) fail("handoff_link_result.opened requires url and forbids detail");
        triageURL(str(p, "url", "handoff_link_result", 4000), "handoff_link_result.url", "https:");
      } else {
        if ("url" in p) fail(`handoff_link_result.${outcome} must not carry url`);
        if (!("detail" in p)) fail(`handoff_link_result.${outcome} requires detail`);
        if (triageText(p, "detail", "handoff_link_result", 1000) === "") fail(`handoff_link_result.${outcome} requires detail`);
      }
      break;
    }
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
    case "page_bulk_status_request": {
      requireFields<PageBulkStatusRequestPayload>(p, "page_bulk_status_request", {
        request_id: "required",
        scan_id: "required",
        identifiers: "required",
        rendered_record_count_hint: "optional",
      });
      correlationID(p, "request_id", "page_bulk_status_request");
      correlationID(p, "scan_id", "page_bulk_status_request");
      const identifiers = p["identifiers"];
      if (!Array.isArray(identifiers) || identifiers.length < 1 || identifiers.length > 200) {
        fail("page_bulk_status_request.identifiers must have 1..200 entries");
      }
      const seenIDs = new Set<string>();
      for (const rawIdentifier of identifiers) {
        const identifier = asRecord(rawIdentifier, "page_bulk_status_request.identifiers");
        requireFields<PageBulkIdentifier>(identifier, "page_bulk_status_request.identifiers", {
          local_id: "required",
          kind: "required",
          value: "required",
        });
        const localID = triageText(identifier, "local_id", "page_bulk_status_request.identifiers", 128);
        if (localID === "") fail("page_bulk_status_request.identifiers.local_id is required");
        if (seenIDs.has(localID)) fail(`page_bulk_status_request.identifiers.local_id ${JSON.stringify(localID)} is duplicated`);
        seenIDs.add(localID);
        const kind = str(identifier, "kind", "page_bulk_status_request.identifiers", 10);
        if (kind !== "doi" && kind !== "pmid" && kind !== "arxiv" && kind !== "openalex") fail("page_bulk_status_request.identifiers.kind is invalid");
        const value = triageText(identifier, "value", "page_bulk_status_request.identifiers", 512);
        if (value === "") fail("page_bulk_status_request.identifiers.value is required");
      }
      if ("rendered_record_count_hint" in p) int(p, "rendered_record_count_hint", "page_bulk_status_request", 0);
      break;
    }
    case "page_bulk_status_result": {
      requireFields<PageBulkStatusResultPayload>(p, "page_bulk_status_result", {
        request_id: "required",
        scan_id: "required",
        items: "required",
        truncated: "required",
      });
      correlationID(p, "request_id", "page_bulk_status_result");
      correlationID(p, "scan_id", "page_bulk_status_result");
      const items = p["items"];
      if (!Array.isArray(items) || items.length > 200) fail("page_bulk_status_result.items must have at most 200 entries");
      const seenItemIDs = new Set<string>();
      for (const rawItem of items) {
        const item = asRecord(rawItem, "page_bulk_status_result.items");
        requireFields<PageBulkStatusItem>(item, "page_bulk_status_result.items", {
          local_id: "required",
          canonical_key: "optional",
          status: "required",
          ownership_complete: "required",
          job_id: "optional",
          zotio_item_key: "optional",
        });
        const localID = triageText(item, "local_id", "page_bulk_status_result.items", 128);
        if (localID === "") fail("page_bulk_status_result.items.local_id is required");
        if (seenItemIDs.has(localID)) fail(`page_bulk_status_result.items.local_id ${JSON.stringify(localID)} is duplicated`);
        seenItemIDs.add(localID);
        const status = str(item, "status", "page_bulk_status_result.items", 30);
        if (
          ![
            "eligible",
            "owned_with_pdf",
            "owned_missing_pdf",
            "queued",
            "previously_unavailable",
            "ownership_incomplete",
            "ownership_unknown",
            "invalid",
            "frame_too_large",
          ].includes(status)
        ) {
          fail("page_bulk_status_result.items.status is invalid");
        }
        // An identifier that never resolved, or a result refused because it
        // could not fit the response, has no canonical work identity.
        if (status === "invalid" || status === "frame_too_large") {
          if ("canonical_key" in item) fail(`page_bulk_status_result.items.canonical_key must be omitted for ${status}`);
        } else {
          if (!("canonical_key" in item)) fail("page_bulk_status_result.items.canonical_key is required");
          if (triageText(item, "canonical_key", "page_bulk_status_result.items", 300) === "") {
            fail("page_bulk_status_result.items.canonical_key is required");
          }
        }
        if (typeof item["ownership_complete"] !== "boolean") fail("page_bulk_status_result.items.ownership_complete must be a boolean");
        if ("job_id" in item) {
          if (status !== "queued") fail("page_bulk_status_result.items.job_id is only valid for queued");
          if (!JOB_ID_RE.test(str(item, "job_id", "page_bulk_status_result.items", 128))) {
            fail("page_bulk_status_result.items.job_id is invalid");
          }
        }
        if ("zotio_item_key" in item) {
          if (status !== "owned_missing_pdf") fail("page_bulk_status_result.items.zotio_item_key is only valid for owned_missing_pdf");
          if (!ZOTERO_KEY_RE.test(str(item, "zotio_item_key", "page_bulk_status_result.items", 32))) {
            fail("page_bulk_status_result.items.zotio_item_key is invalid");
          }
        }
      }
      if (typeof p["truncated"] !== "boolean") fail("page_bulk_status_result.truncated must be a boolean");
      break;
    }
    case "page_bulk_submit_request": {
      requireFields<PageBulkSubmitRequestPayload>(p, "page_bulk_submit_request", {
        request_id: "required",
        scan_id: "required",
        canonical_keys: "required",
        source: "required",
      });
      correlationID(p, "request_id", "page_bulk_submit_request");
      correlationID(p, "scan_id", "page_bulk_submit_request");
      const keys = p["canonical_keys"];
      if (!Array.isArray(keys) || keys.length < 1 || keys.length > 50) {
        fail("page_bulk_submit_request.canonical_keys must have 1..50 entries");
      }
      const seenKeys = new Set<string>();
      for (const rawKey of keys) {
        if (typeof rawKey !== "string" || rawKey.length === 0 || Array.from(rawKey).length > 300) {
          fail("page_bulk_submit_request.canonical_keys entries must be non-empty bounded strings");
        }
        rejectNUL(rawKey, "page_bulk_submit_request.canonical_keys");
        if (seenKeys.has(rawKey)) fail(`page_bulk_submit_request.canonical_keys contains a duplicate ${JSON.stringify(rawKey)}`);
        seenKeys.add(rawKey);
      }
      const source = asRecord(p["source"], "page_bulk_submit_request.source");
      requireFields<PageBulkSubmitSource>(source, "page_bulk_submit_request.source", {
        kind: "required",
        origin: "required",
        detector: "required",
      });
      if (str(source, "kind", "page_bulk_submit_request.source", 20) !== "browser_page") {
        fail("page_bulk_submit_request.source.kind must be browser_page");
      }
      // Bare https scheme+host only — never path, query, fragment, or page
      // title (ADR-0019 Decision 6), the same round-trip shape
      // hello_ack.resolver_origins already validates above.
      const origin = str(source, "origin", "page_bulk_submit_request.source", 300);
      let originOK = origin.startsWith("https://");
      if (originOK) {
        try {
          const parsed = new URL(origin);
          originOK = parsed.protocol === "https:" && parsed.host !== "" && `${parsed.protocol}//${parsed.host}` === origin;
        } catch {
          originOK = false;
        }
      }
      if (!originOK) fail("page_bulk_submit_request.source.origin must be a bare https scheme://host origin");
      if (triageText(source, "detector", "page_bulk_submit_request.source", 128) === "") {
        fail("page_bulk_submit_request.source.detector is required");
      }
      break;
    }
    case "page_bulk_submit_result": {
      requireFields<PageBulkSubmitResultPayload>(p, "page_bulk_submit_result", {
        request_id: "required",
        scan_id: "required",
        submitted: "required",
        joined: "required",
        already_owned: "required",
        invalid: "required",
        batch_id: "required",
      });
      correlationID(p, "request_id", "page_bulk_submit_result");
      correlationID(p, "scan_id", "page_bulk_submit_result");
      int(p, "submitted", "page_bulk_submit_result", 0);
      int(p, "joined", "page_bulk_submit_result", 0);
      int(p, "already_owned", "page_bulk_submit_result", 0);
      int(p, "invalid", "page_bulk_submit_result", 0);
      if (!JOB_ID_RE.test(str(p, "batch_id", "page_bulk_submit_result", 128))) {
        fail("page_bulk_submit_result.batch_id is invalid");
      }
      break;
    }
    case "pdf_grab_request": {
      requireFields<PdfGrabRequestPayload>(p, "pdf_grab_request", {
        request_id: "required",
        host: "required",
        title: "optional",
      });
      correlationID(p, "request_id", "pdf_grab_request");
      const host = str(p, "host", "pdf_grab_request", 253);
      if (!/^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$/.test(host)) {
        fail("pdf_grab_request.host must be a bare hostname");
      }
      if ("title" in p) triageText(p, "title", "pdf_grab_request", 500);
      break;
    }
    case "pdf_grab_result": {
      requireFields<PdfGrabResultPayload>(p, "pdf_grab_result", {
        request_id: "optional",
        grab_id: "optional",
        outcome: "required",
        steering_path: "optional",
        detail: "optional",
      });
      const outcome = str(p, "outcome", "pdf_grab_result", 30);
      if ("grab_id" in p) correlationID(p, "grab_id", "pdf_grab_result");
      if (
        !["steering", "existing", "not_supported", "unavailable", "job_created", "already_owned", "needs_identifier", "failed_validation", "abandoned"].includes(
          outcome,
        )
      ) {
        fail("pdf_grab_result.outcome is invalid");
      }
      if (outcome !== "not_supported" && outcome !== "unavailable" && !("grab_id" in p)) {
        fail(`pdf_grab_result: ${outcome} outcome requires grab_id`);
      }
      const requestID = "request_id" in p ? correlationID(p, "request_id", "pdf_grab_result") : "";
      if ("steering_path" in p && !PDF_GRAB_STEERING_PATH_RE.test(str(p, "steering_path", "pdf_grab_result", 128))) {
        fail("pdf_grab_result.steering_path is invalid");
      }
      if ("detail" in p) triageText(p, "detail", "pdf_grab_result", 1000);
      if (outcome === "steering") {
        if (requestID === "") fail("pdf_grab_result: steering outcome requires request_id");
        if (!("steering_path" in p)) fail("pdf_grab_result: steering outcome requires steering_path");
      } else if (outcome === "existing") {
        if (requestID === "" || !("grab_id" in p)) fail("pdf_grab_result: existing outcome requires request_id and grab_id");
        if ("steering_path" in p) fail("pdf_grab_result: existing outcome must not carry steering_path");
      } else if (outcome === "not_supported" || outcome === "unavailable") {
        if (requestID === "") fail(`pdf_grab_result: ${outcome} outcome requires request_id`);
        if ("steering_path" in p) fail(`pdf_grab_result: ${outcome} outcome must not carry steering_path`);
      } else {
        if (requestID !== "") fail(`pdf_grab_result: ${outcome} outcome must not carry request_id`);
        if ("steering_path" in p) fail(`pdf_grab_result: ${outcome} outcome must not carry steering_path`);
      }
      break;
    }
    case "institutional_candidate_offer": {
      requireFields<InstitutionalCandidateOfferPayload>(p, type, {
        candidate_id: "required",
        materialization_kind: "required",
        expires_at: "required",
        provider_hosts: "required",
        expected: "optional",
        access_mode: "optional",
        login_entity_id: "optional",
        proquest_account_id: "optional",
        requires_auth: "optional",
        drive_attempt_id: "optional",
        drive_ordinal: "optional",
        drive_strategy: "optional",
        drive_revision: "optional",
      });
      institutionalID(p, "candidate_id", type);
      if (str(p, "materialization_kind", type, 32) !== "browser_tab") fail(`${type}.materialization_kind is invalid`);
      triageTime(p, "expires_at", type);
      const hosts = p["provider_hosts"];
      if (!Array.isArray(hosts) || hosts.length < 1 || hosts.length > 20) fail(`${type}.provider_hosts must have 1..20 entries`);
      for (const host of hosts) {
        if (typeof host !== "string" || !HOST_RE.test(host)) fail(`${type}.provider_hosts contains an invalid host`);
      }
      if ("expected" in p) {
        const expected = asRecord(p["expected"], `${type}.expected`);
        requireFields<JobOfferExpected>(expected, `${type}.expected`, { doi: "optional", title: "optional" });
        if ("doi" in expected) str(expected, "doi", `${type}.expected`, 300);
        if ("title" in expected) str(expected, "title", `${type}.expected`, 500);
      }
      if ("access_mode" in p) {
        const mode = str(p, "access_mode", type, 20);
        if (mode !== "assisted" && mode !== "delegated") fail(`${type}.access_mode is invalid`);
      }
      if ("login_entity_id" in p) {
        const entity = str(p, "login_entity_id", type, 4000);
        if (!entity.startsWith("https://")) fail(`${type}.login_entity_id must be https`);
      }
      if ("proquest_account_id" in p && !/^[0-9]+$/.test(str(p, "proquest_account_id", type, 64))) {
        fail(`${type}.proquest_account_id must be digits`);
      }
      if ("requires_auth" in p && typeof p["requires_auth"] !== "boolean") fail(`${type}.requires_auth must be a boolean`);
      if ("drive_attempt_id" in p) correlationID(p, "drive_attempt_id", type);
      if ("drive_ordinal" in p) int(p, "drive_ordinal", type, 0);
      if ("drive_strategy" in p) str(p, "drive_strategy", type, 128);
      if ("drive_revision" in p) str(p, "drive_revision", type, 128);
      break;
    }
    case "institutional_claim_request": {
      requireFields<InstitutionalClaimRequestPayload>(p, type, {
        request_id: "required",
        candidate_id: "required",
        materialization_kind: "required",
      });
      institutionalID(p, "candidate_id", type);
      institutionalID(p, "request_id", type);
      const kind = str(p, "materialization_kind", type, 32);
      if (kind !== "browser_tab" && kind !== "direct_download") fail(`${type}.materialization_kind is invalid`);
      break;
    }
    case "institutional_claim_response": {
      requireFields<InstitutionalClaimResponsePayload>(p, type, {
        request_id: "required",
        outcome: "required",
        detail: "optional",
        candidate_id: "optional",
        claim_id: "optional",
        binding_id: "optional",
        browser_holder_generation: "optional",
        lease_until: "optional",
      });
      const outcome = institutionalOutcome(p, type);
      institutionalID(p, "request_id", type);
      institutionalOutcomeOneOf(outcome, type, ["feature_disabled", "claimed", "stale", "not_eligible", "busy", "error"]);
      if (outcome === "claimed") {
        for (const key of ["candidate_id", "claim_id", "binding_id"]) institutionalID(p, key, type);
        int(p, "browser_holder_generation", type, 0);
        triageTime(p, "lease_until", type);
        if ("detail" in p) fail(`${type}.claimed must not carry detail`);
      } else {
        for (const key of ["candidate_id", "claim_id", "binding_id", "browser_holder_generation", "lease_until"]) {
          if (key in p) fail(`${type}.${outcome} must not carry ${key}`);
        }
        institutionalFailure(p, type, outcome, "claimed");
      }
      break;
    }
    case "institutional_bind_request": {
      requireFields<InstitutionalBindRequestPayload>(p, type, { request_id: "required", claim_id: "required", binding_id: "required", tab_id: "required" });
      institutionalID(p, "claim_id", type);
      institutionalID(p, "request_id", type);
      institutionalID(p, "binding_id", type);
      institutionalTabID(p, "tab_id", type);
      break;
    }
    case "institutional_bind_response": {
      requireFields<InstitutionalBindResponsePayload>(p, type, { request_id: "required", outcome: "required", detail: "optional", claim_id: "optional", binding_id: "optional" });
      const outcome = institutionalOutcome(p, type);
      institutionalID(p, "request_id", type);
      institutionalOutcomeOneOf(outcome, type, ["feature_disabled", "bound", "stale", "not_eligible", "error"]);
      if (outcome === "bound") {
        institutionalID(p, "claim_id", type);
        institutionalID(p, "binding_id", type);
        if ("detail" in p) fail(`${type}.bound must not carry detail`);
      } else {
        if ("claim_id" in p || "binding_id" in p) fail(`${type}.${outcome} must not carry claim_id or binding_id`);
        institutionalFailure(p, type, outcome, "bound");
      }
      break;
    }
    case "institutional_route_request": {
      requireFields<InstitutionalRouteRequestPayload>(p, type, { request_id: "required", claim_id: "required", binding_id: "required" });
      institutionalID(p, "claim_id", type);
      institutionalID(p, "request_id", type);
      institutionalID(p, "binding_id", type);
      break;
    }
    case "institutional_route_response": {
      requireFields<InstitutionalRouteResponsePayload>(p, type, {
        request_id: "required",
        outcome: "required",
        detail: "optional",
        claim_id: "optional",
        binding_id: "optional",
        route_issuance_ordinal: "optional",
        url: "optional",
      });
      const outcome = institutionalOutcome(p, type);
      institutionalOutcomeOneOf(outcome, type, ["feature_disabled", "issued", "stale", "not_eligible", "busy", "error"]);
      institutionalID(p, "request_id", type);
      if (outcome === "issued") {
        institutionalID(p, "claim_id", type);
        institutionalID(p, "binding_id", type);
        int(p, "route_issuance_ordinal", type, 0);
        triageURL(str(p, "url", type, 4000), `${type}.url`, "https:");
        if ("detail" in p) fail(`${type}.issued must not carry detail`);
      } else {
        for (const key of ["claim_id", "binding_id", "route_issuance_ordinal", "url"]) {
          if (key in p) fail(`${type}.${outcome} must not carry ${key}`);
        }
        institutionalFailure(p, type, outcome, "issued");
      }
      break;
    }
    case "institutional_navigated_request": {
      requireFields<InstitutionalNavigatedRequestPayload>(p, type, {
        request_id: "required",
        claim_id: "required",
        binding_id: "required",
        route_issuance_ordinal: "required",
        tab_id: "required",
      });
      institutionalID(p, "claim_id", type);
      institutionalID(p, "binding_id", type);
      int(p, "route_issuance_ordinal", type, 0);
      institutionalTabID(p, "tab_id", type);
      institutionalID(p, "request_id", type);
      break;
    }
    case "institutional_navigated_response": {
      requireFields<InstitutionalNavigatedResponsePayload>(p, type, { request_id: "required", outcome: "required", detail: "optional", claim_id: "optional", binding_id: "optional" });
      const outcome = institutionalOutcome(p, type);
      institutionalID(p, "request_id", type);
      institutionalOutcomeOneOf(outcome, type, ["feature_disabled", "acknowledged", "stale", "not_eligible", "error"]);
      if (outcome === "acknowledged") {
        institutionalID(p, "claim_id", type);
        institutionalID(p, "binding_id", type);
        if ("detail" in p) fail(`${type}.acknowledged must not carry detail`);
      } else {
        if ("claim_id" in p || "binding_id" in p) fail(`${type}.${outcome} must not carry claim_id or binding_id`);
        institutionalFailure(p, type, outcome, "acknowledged");
      }
      break;
    }
    case "institutional_reconcile_request": {
      requireFields<InstitutionalReconcileRequestPayload>(p, type, { request_id: "required", bindings: "required" });
      const bindings = p["bindings"];
      institutionalID(p, "request_id", type);
      if (!Array.isArray(bindings) || bindings.length > 32) fail(`${type}.bindings must have at most 32 entries`);
      const seen = new Set<string>();
      for (const rawBinding of bindings) {
        const binding = asRecord(rawBinding, `${type}.bindings`);
        requireFields<InstitutionalBindingPair>(binding, `${type}.bindings`, { binding_id: "required", tab_id: "required" });
        const id = institutionalID(binding, "binding_id", `${type}.bindings`);
        if (seen.has(id)) fail(`${type}.bindings contains a duplicate binding_id`);
        seen.add(id);
        institutionalTabID(binding, "tab_id", `${type}.bindings`);
      }
      break;
    }
    case "institutional_reconcile_response": {
      requireFields<InstitutionalReconcileResponsePayload>(p, type, { request_id: "required", outcome: "required", detail: "optional", claims: "optional" });
      const outcome = institutionalOutcome(p, type);
      institutionalID(p, "request_id", type);
      institutionalOutcomeOneOf(outcome, type, ["feature_disabled", "reconciled", "error"]);
      if (outcome === "reconciled") {
        if ("detail" in p) fail(`${type}.reconciled must not carry detail`);
        if ("claims" in p) {
          const claims = p["claims"];
          if (!Array.isArray(claims) || claims.length > 32) fail(`${type}.claims must have at most 32 entries`);
          for (const rawClaim of claims) {
            const claim = asRecord(rawClaim, `${type}.claims`);
            requireFields<InstitutionalClaimStatus>(claim, `${type}.claims`, {
              claim_id: "required",
              binding_id: "required",
              candidate_id: "required",
              phase: "required",
              tab_id: "optional",
            });
            institutionalID(claim, "claim_id", `${type}.claims`);
            institutionalID(claim, "binding_id", `${type}.claims`);
            institutionalID(claim, "candidate_id", `${type}.claims`);
            const phase = str(claim, "phase", `${type}.claims`, 32);
            if (!["claimed", "bound", "route_issued", "navigated", "settled", "abandoned"].includes(phase)) fail(`${type}.claims.phase is invalid`);
            if ("tab_id" in claim) institutionalTabID(claim, "tab_id", `${type}.claims`);
          }
        }
      } else {
        if ("claims" in p) fail(`${type}.${outcome} must not carry claims`);
        institutionalFailure(p, type, outcome, "reconciled");
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
    case "pdf_grab_status_request": {
      requireFields<PdfGrabStatusRequestPayload>(p, "pdf_grab_status_request", {
        request_id: "required",
        grab_id: "required",
      });
      correlationID(p, "request_id", "pdf_grab_status_request");
      correlationID(p, "grab_id", "pdf_grab_status_request");
      break;
    }
    case "pdf_grab_status_result": {
      requireFields<PdfGrabStatusResultPayload>(p, "pdf_grab_status_result", {
        request_id: "required",
        grab_id: "required",
        state: "required",
        outcome: "optional",
        detail: "optional",
        job_id: "optional",
      });
      const state = str(p, "state", "pdf_grab_status_result", 30);
      correlationID(p, "request_id", "pdf_grab_status_result");
      if (state !== "" && !["awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation", "abandoned"].includes(state)) {
        fail("pdf_grab_status_result.state is invalid");
      }
      if ("outcome" in p) {
        const outcome = str(p, "outcome", "pdf_grab_status_result", 30);
        if (!["not_found", "unavailable", "job_created", "already_owned", "needs_identifier", "failed_validation", "abandoned"].includes(outcome)) {
          fail("pdf_grab_status_result.outcome is invalid");
        }
        if (state === "" && outcome !== "not_found" && outcome !== "unavailable") fail("pdf_grab_status_result.state required");
      } else if (state === "") {
        fail("pdf_grab_status_result.state required");
      }
      if ("detail" in p) triageText(p, "detail", "pdf_grab_status_result", 1000);
      if ("job_id" in p) correlationID(p, "job_id", "pdf_grab_status_result");
      break;
    }
    case "pdf_grab_abandon_request": {
      requireFields<PdfGrabAbandonRequestPayload>(p, "pdf_grab_abandon_request", {
        request_id: "required",
        grab_id: "required",
      });
      correlationID(p, "request_id", "pdf_grab_abandon_request");
      correlationID(p, "grab_id", "pdf_grab_abandon_request");
      break;
    }
    case "pdf_grab_abandon_result": {
      requireFields<PdfGrabAbandonResultPayload>(p, "pdf_grab_abandon_result", {
        request_id: "required",
        grab_id: "required",
        state: "required",
        outcome: "optional",
        detail: "optional",
      });
      correlationID(p, "request_id", "pdf_grab_abandon_result");
      correlationID(p, "grab_id", "pdf_grab_abandon_result");
      const state = str(p, "state", "pdf_grab_abandon_result", 30);
      if (state !== "" && !["awaiting_file", "quarantined", "identified", "job_created", "parked_no_identifier", "failed_validation", "abandoned"].includes(state)) fail("pdf_grab_abandon_result.state is invalid");
      const outcome = "outcome" in p ? str(p, "outcome", "pdf_grab_abandon_result", 30) : "";
      if (outcome !== "" && !["abandoned", "not_found", "unavailable", "conflict"].includes(outcome)) fail("pdf_grab_abandon_result.outcome is invalid");
      if (state === "" && outcome !== "not_found" && outcome !== "unavailable") fail("pdf_grab_abandon_result.state required");
      if (outcome === "abandoned" && state !== "abandoned") fail("pdf_grab_abandon_result.abandoned state required");
      break;
    }
    }
  }
