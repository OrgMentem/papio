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

export type BrowserMessageType =
  | "hello"
  | "hello_ack"
  | "job_offer"
  | "job_accept"
  | "job_reject"
  | "auth_pending"
  | "auth_returned"
  | "download_started"
  | "download_complete"
  | "provider_outcome"
  | "cancel"
  | "ack"
  | "error";

export interface HelloPayload {
  extension_version: string;
  adapter_versions?: Record<string, string>;
}

export interface HelloAckPayload {
  daemon_version?: string;
  features?: string[];
}


export interface JobOfferExpected {
  doi?: string;
  title?: string;
}

export interface JobOfferPayload {
  openurl: string;
  provider_hosts: string[];
  expected?: JobOfferExpected;
  access_mode: "assisted" | "maximal";
  expires_at: string;
  /** The institution's Shibboleth IdP entityID, when the daemon knows it.
   * Lets an adapter's federated-login route auto-select the institution on a
   * provider login wall. Optional; an https URL when present. */
  login_entity_id?: string;
  /** The institution's ProQuest account id, when the daemon knows it. Lets the
   * ProQuest adapter unlock the openurl link-resolver by appending
   * ?accountid=<id>. Optional; digits when present. */
  proquest_account_id?: string;
}

/** Timing only — no URL/host/title/query/fragment fields exist by design. */
export interface AuthPayload {
  elapsed_ms?: number;
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
  job_offer: true,
  job_accept: true,
  job_reject: true,
  auth_pending: true,
  auth_returned: true,
  download_started: true,
  download_complete: true,
  provider_outcome: true,
  cancel: true,
  ack: true,
  error: true,
};

const JOB_SCOPED: Record<string, true> = {
  job_offer: true,
  job_accept: true,
  job_reject: true,
  auth_pending: true,
  auth_returned: true,
  download_started: true,
  download_complete: true,
  provider_outcome: true,
  cancel: true,
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

function str(obj: Record<string, unknown>, key: string, what: string, max = 1000): string {
  const v = obj[key];
  if (typeof v !== "string") fail(`${what}.${key} must be a string`);
  if (Array.from(v).length > max) fail(`${what}.${key} exceeds ${max} chars`);
  return v;
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
      requireKeys(p, "hello", ["extension_version"], ["adapter_versions"]);
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
    case "job_offer": {
      requireKeys(p, "job_offer", ["openurl", "provider_hosts", "access_mode", "expires_at"], ["expected", "login_entity_id", "proquest_account_id"]);
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
      if (mode !== "assisted" && mode !== "maximal") fail(`invalid access_mode ${JSON.stringify(mode)}`);
      const expires = str(p, "expires_at", "job_offer", 64);
      if (!isRFC3339(expires)) fail("job_offer.expires_at must be RFC3339");
      if ("expected" in p) {
        const ex = asRecord(p["expected"], "job_offer.expected");
        requireKeys(ex, "job_offer.expected", [], ["doi", "title"]);
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
      break;
    }
    case "auth_pending":
    case "auth_returned": {
      // Structural privacy invariant: timing only.
      requireKeys(p, type, [], ["elapsed_ms"]);
      if ("elapsed_ms" in p) int(p, "elapsed_ms", type, 0);
      break;
    }
    case "download_started": {
      requireKeys(p, "download_started", ["download_id", "filename"]);
      int(p, "download_id", "download_started", 0);
      if (!FILENAME_RE.test(str(p, "filename", "download_started", 255))) {
        fail("download_started.filename must be a bare name without path separators");
      }
      break;
    }
    case "download_complete": {
      requireKeys(p, "download_complete", ["download_id", "filename", "size_bytes"]);
      int(p, "download_id", "download_complete", 0);
      if (!FILENAME_RE.test(str(p, "filename", "download_complete", 255))) {
        fail("download_complete.filename must be a bare name without path separators");
      }
      int(p, "size_bytes", "download_complete", 1);
      break;
    }
    case "provider_outcome": {
      requireKeys(p, "provider_outcome", ["outcome"], ["adapter_version", "detail"]);
      const outcome = str(p, "outcome", "provider_outcome", 50);
      if (OUTCOMES[outcome] !== true) fail(`invalid outcome ${JSON.stringify(outcome)}`);
      if ("adapter_version" in p) str(p, "adapter_version", "provider_outcome", 50);
      if ("detail" in p) str(p, "detail", "provider_outcome", 500);
      break;
    }
    case "error": {
      requireKeys(p, "error", ["code", "message"]);
      if (!ERROR_CODE_RE.test(str(p, "code", "error", 50))) fail("invalid error code");
      const message = str(p, "message", "error", 1000);
      if (message.length === 0) fail("error.message required");
      break;
    }
    case "hello_ack": {
      requireKeys(p, "hello_ack", [], ["daemon_version", "features"]);
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
      break;
    }
    case "ack":
    case "job_accept":
    case "job_reject":
    case "cancel": {
      requireKeys(p, type, []);
      break;
    }
  }
}
