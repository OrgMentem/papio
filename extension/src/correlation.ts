// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import {
  EFFECT_PERMIT_FEATURE,
  AGENT_NAVIGATION_FEATURE,
  NATIVE_CLICK_ADOPTION_FEATURE,
  INSTITUTIONAL_AUTHENTICATION_CLAIM_FEATURE,
  SURFACE_CLOSE_FEATURE,
  type BrowserMessage,
  type BrowserMessageType,
} from "./protocol";
import { PDF_GRAB_FEATURE, PDF_GRAB_SUGGEST_FEATURE } from "./deliver";

const REQUEST_TIMEOUT_MS = 15_000;

/**
 * The reply types a correlated request may name. `BrowserMessageType` is too
 * wide: `responseType: "hello_ack"` would compile, and `handleInbound` would
 * then consume the handshake frame before `background.ts` could run its own
 * hello branch, leaving features and hello waiters permanently unset. Reply
 * frames carry a `_response`, `_result`, `_result_v1`, or `_ack` suffix.
 * `hello_ack` is the one such frame that is never a correlated reply.
 */
type CorrelatedReplyType = Exclude<
  Extract<
    BrowserMessageType,
    `${string}_response` | `${string}_result` | `${string}_result_v1` | `${string}_ack`
  >,
  "hello_ack"
>;

type RequestPolicy = {
  responseType: CorrelatedReplyType;
  feature: string;
  timeoutMs?: number;
} &
  (
    | { operation: "read"; transport: "retry_once" | "single_attempt" }
    | { operation: "mutation"; transport: "single_attempt" }
  );

/**
 * The complete policy for native requests that wait for a correlated reply.
 * Callers select a request kind. They cannot combine a reply, feature, or retry
 * rule that the inbound router cannot satisfy.
 *
 * This table is the ONE registry. `CorrelatedRequestKind` is its `keyof`, and
 * the accepted-reply set `handleInbound` consults is derived from its
 * `responseType` values — so a kind cannot exist without a registered reply,
 * and an unregistered kind is a compile error at the call site.
 *
 * That single-source property is load-bearing, not tidiness. The predecessor
 * design held the senders' expected types and the accepted-reply registry as
 * two hand-maintained lists. A sender whose reply type was missing from the
 * registry threw an uncoded Error before any connection or feature check, and
 * `respondToRuntimePromise` swallowed it: the page showed a generic "could not
 * complete that request" while nothing reached the service-worker console or
 * the daemon. The page-bulk bridge shipped exactly that way (2026-08-07,
 * ADR-0019 phase B) and bulk acquisition never worked once — `page_bulk_runs`
 * held six opened runs with zero submissions. Do not reintroduce a second
 * list, and do not widen `request`'s parameter from this table's `keyof` to a
 * bare string. `internal/browser/dispatch_exhaustive_test.go` proves the same
 * class on the daemon's inbound dispatch.
 */
const REQUEST_POLICIES = {
  native_download_rebind_request_v1: {
    responseType: "native_download_rebind_result_v1", feature: AGENT_NAVIGATION_FEATURE,
    operation: "mutation", transport: "single_attempt", timeoutMs: 45_000,
  },
  native_download_arm_request_v1: {
    responseType: "native_download_arm_result_v1", feature: NATIVE_CLICK_ADOPTION_FEATURE,
    operation: "mutation", transport: "single_attempt", timeoutMs: 15_000,
  },
  native_download_import_request_v1: {
    responseType: "native_download_import_result_v1", feature: NATIVE_CLICK_ADOPTION_FEATURE,
    operation: "mutation", transport: "single_attempt", timeoutMs: 60_000,
  },
  agent_decide_request_v1: {
    responseType: "agent_decide_result_v1",
    feature: "agent_fallback_v1",
    operation: "mutation",
    transport: "single_attempt",
    timeoutMs: 45_000,
  },
  surface_close_request: {
    responseType: "surface_close_response",
    feature: SURFACE_CLOSE_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  authentication_claim_request: {
    responseType: "authentication_claim_response",
    feature: INSTITUTIONAL_AUTHENTICATION_CLAIM_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  claim_observation: {
    responseType: "claim_observation_ack",
    feature: INSTITUTIONAL_AUTHENTICATION_CLAIM_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  handoff_link_request: {
    responseType: "handoff_link_result",
    feature: "handoff_link_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
  triage_snapshot_request: {
    responseType: "triage_snapshot_response",
    feature: "triage_snapshot_v1",
    operation: "read",
    transport: "retry_once",
  },
  triage_counts_request: {
    responseType: "triage_counts_response",
    feature: "triage_snapshot_v1",
    operation: "read",
    transport: "retry_once",
  },
  stats_request: {
    responseType: "stats_response",
    feature: "browser_stats_v1",
    operation: "read",
    transport: "retry_once",
  },
  work_pulse_request: {
    responseType: "work_pulse_response",
    feature: "work_pulse_v1",
    operation: "read",
    transport: "retry_once",
  },
  surface_presence: {
    responseType: "surface_presence_ack",
    feature: "surface_presence_v1",
    operation: "read",
    transport: "single_attempt",
  },
  activity_page_request: {
    responseType: "activity_page_response",
    feature: "activity_page_v1",
    operation: "read",
    transport: "retry_once",
  },
  activity_request: {
    responseType: "activity_response",
    feature: "activity_feed_v1",
    operation: "read",
    transport: "retry_once",
  },
  pdf_grab_request: {
    responseType: "pdf_grab_result",
    feature: PDF_GRAB_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  pdf_grab_status_request: {
    responseType: "pdf_grab_status_result",
    feature: PDF_GRAB_FEATURE,
    operation: "read",
    transport: "retry_once",
  },
  pdf_grab_abandon_request: {
    responseType: "pdf_grab_abandon_result",
    feature: PDF_GRAB_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  page_bulk_status_request: {
    responseType: "page_bulk_status_result",
    feature: "page_bulk_acquire_v1",
    operation: "read",
    transport: "retry_once",
  },
  page_bulk_submit_request: {
    responseType: "page_bulk_submit_result",
    feature: "page_bulk_acquire_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
  page_bulk_submit_v2_request: {
    responseType: "page_bulk_submit_v2_result",
    feature: "page_bulk_cohort_v2",
    operation: "mutation",
    transport: "single_attempt",
  },
  triage_decide: {
    responseType: "triage_decide_result",
    feature: "triage_mutations_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
  human_action_resolve: {
    responseType: "human_action_resolve_result",
    feature: "triage_mutations_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
  delivery_reconcile_request: {
    responseType: "delivery_reconcile_result",
    feature: "triage_snapshot_schema_v3",
    operation: "mutation",
    transport: "single_attempt",
  },
  review_preview_request: {
    responseType: "review_preview_result",
    feature: "review_preview_v1",
    operation: "read",
    transport: "retry_once",
  },
  pdf_grab_suggest_request: {
    responseType: "pdf_grab_suggest_response",
    feature: PDF_GRAB_SUGGEST_FEATURE,
    operation: "read",
    transport: "retry_once",
  },
  pdf_grab_confirm_request: {
    responseType: "pdf_grab_confirm_response",
    feature: PDF_GRAB_SUGGEST_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  terms_effect_result_request: {
    responseType: "terms_effect_result",
    feature: EFFECT_PERMIT_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  terms_effect_start_request: {
    responseType: "terms_effect_start_result",
    feature: EFFECT_PERMIT_FEATURE,
    operation: "mutation",
    transport: "single_attempt",
  },
  provider_drive_epoch_result_request: {
    responseType: "provider_drive_epoch_result",
    feature: "provider_drive_epoch_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
  provider_drive_epoch_start_request: {
    responseType: "provider_drive_epoch_start_result",
    feature: "provider_drive_epoch_v1",
    operation: "mutation",
    transport: "single_attempt",
  },
} as const satisfies Partial<Record<BrowserMessageType, RequestPolicy>>;

export type CorrelatedRequestKind = keyof typeof REQUEST_POLICIES;
export type NativeRequestKind = "response" | "transport" | "timeout";

export interface NativeRequestResult {
  kind: NativeRequestKind;
  payload?: Record<string, unknown>;
  code?: string;
  message?: string;
}

export interface CorrelatedRequestOptions {
  jobID?: string | undefined;
  requestID?: string | undefined;
}

export type CorrelationInboundDisposition =
  | "handled"
  | "unmatched_error"
  | "unhandled";

interface PendingRequest {
  expectedType: BrowserMessageType;
  resolve(result: NativeRequestResult): void;
}

interface CorrelationDeps {
  randomUUID(): string;
  setTimeout(fn: () => void, ms: number): void;
  ensureConnected(): Promise<boolean>;
  connectionFailure(): NativeRequestResult;
  supportsFeature(feature: string): boolean;
  send(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    jobID?: string,
  ): boolean;
  reconnect(): void;
}

export class NativeRequestCorrelation {
  private readonly pending = new Map<string, PendingRequest>();
  private readonly responseTypes = Object.fromEntries(
    Object.values(REQUEST_POLICIES).map((policy) => [
      policy.responseType,
      true,
    ]),
  ) as Readonly<Partial<Record<BrowserMessageType, true>>>;
  private requestIDSequence = 0;

  constructor(private readonly deps: CorrelationDeps) {}

  createRequestID(): string {
    const random = this.deps.randomUUID().replace(/-/g, "");
    const suffix = `_${this.requestIDSequence++}`;
    return random.length + suffix.length <= 64 ? `${random}${suffix}` : random;
  }

  async request(
    kind: CorrelatedRequestKind,
    payload: Record<string, unknown>,
    options: CorrelatedRequestOptions = {},
  ): Promise<NativeRequestResult> {
    const policy = REQUEST_POLICIES[kind];
    const attempts = policy.transport === "retry_once" ? 2 : 1;
    for (let attempt = 0; attempt < attempts; attempt += 1) {
      if (!(await this.deps.ensureConnected())) {
        return this.deps.connectionFailure();
      }
      if (!this.deps.supportsFeature(policy.feature)) {
        return {
          kind: "response",
          code: "feature_unavailable",
          message: "This daemon does not support the requested inbox feature",
        };
      }
      const result = await this.send(kind, payload, policy, options);
      if (result.kind !== "transport" || attempt + 1 === attempts) return result;
    }
    return {
      kind: "transport",
      code: "connection_lost",
      message: "The daemon is unavailable",
    };
  }

  handleInbound(message: BrowserMessage): CorrelationInboundDisposition {
    if (message.type === "error") return this.handleError(message);
    if (this.responseTypes[message.type] !== true) return "unhandled";

    const requestID = message.payload["request_id"];
    if (typeof requestID !== "string") return "handled";
    const pending = this.pending.get(requestID);
    if (pending === undefined || pending.expectedType !== message.type) {
      console.debug(
        "papio: dropping unknown or late correlated response",
        message.type,
        requestID,
      );
      return "handled";
    }
    this.pending.delete(requestID);
    pending.resolve({ kind: "response", payload: message.payload });
    return "handled";
  }

  failAll(code: string, message: string): void {
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const request of pending) {
      request.resolve({ kind: "transport", code, message });
    }
  }

  private send(
    kind: CorrelatedRequestKind,
    payload: Record<string, unknown>,
    policy: RequestPolicy,
    options: CorrelatedRequestOptions,
  ): Promise<NativeRequestResult> {
    const requestID = options.requestID ?? this.createRequestID();
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
    if (this.pending.has(requestID)) {
      return Promise.resolve({
        kind: "transport",
        code: "duplicate_request_id",
        message: "A request with this id is already pending",
      });
    }

    return new Promise<NativeRequestResult>((resolve) => {
      const pending: PendingRequest = {
        expectedType: policy.responseType,
        resolve,
      };
      this.pending.set(requestID, pending);
      this.deps.setTimeout(() => {
        if (this.pending.get(requestID) !== pending) return;
        this.pending.delete(requestID);
        resolve({ kind: "timeout" });
      }, policy.timeoutMs ?? REQUEST_TIMEOUT_MS);
      if (!this.deps.send(kind, { ...payload, request_id: requestID }, options.jobID)) {
        this.pending.delete(requestID);
        resolve({
          kind: "transport",
          code: "connection_lost",
          message: "The daemon connection was lost before the request was sent",
        });
        this.deps.reconnect();
      }
    });
  }

  private handleError(message: BrowserMessage): CorrelationInboundDisposition {
    const requestID = message.payload["request_id"];
    if (typeof requestID !== "string") return "unmatched_error";
    const pending = this.pending.get(requestID);
    if (pending === undefined) return "unmatched_error";

    this.pending.delete(requestID);
    pending.resolve({
      kind: "transport",
      code:
        typeof message.payload["code"] === "string"
          ? message.payload["code"]
          : "daemon_error",
      message:
        typeof message.payload["message"] === "string"
          ? message.payload["message"]
          : "The daemon rejected the request",
    });
    return "handled";
  }
}
