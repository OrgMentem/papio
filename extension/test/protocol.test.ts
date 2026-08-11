// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Conformance against the SHARED corpus: the TypeScript parser must accept and
// reject exactly the browser-* fixtures the Go core does.

import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import {
  MAX_BROWSER_MESSAGE_BYTES,
  ProtocolError,
  parseBrowserMessage,
  parseBrowserMessageBytes,
} from "../src/protocol";
import type { BrowserMessageType } from "../src/protocol";

const corpusRoot = join(import.meta.dir, "..", "..", "testdata", "protocol");

test("valid browser corpus parses", () => {
  const fixtures = readdirSync(join(corpusRoot, "valid")).filter((name) => name.startsWith("browser-"));
  expect(fixtures.length).toBeGreaterThanOrEqual(5);
  for (const name of fixtures) {
    const text = readFileSync(join(corpusRoot, "valid", name), "utf8");
    const msg = parseBrowserMessageBytes(text);
    expect(msg.protocol).toBe("papio-browser/1");
  }
});
test("activity request and response round-trip through the shared corpus", () => {
  const request = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-activity-request.json"), "utf8"),
  );
  expect(request.type).toBe("activity_request");
  expect(request.payload).toEqual({ request_id: "request-activity-1", limit: 12 });
  const defaultRequest = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-activity-request-default.json"), "utf8"),
  );
  expect(defaultRequest.payload).toEqual({ request_id: "request-activity-2" });


  const response = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-activity-response.json"), "utf8"),
  );
  expect(response.type).toBe("activity_response");
  expect((response.payload["entries"] as Array<Record<string, unknown>>)).toHaveLength(2);
  expect((response.payload["entries"] as Array<Record<string, unknown>>)[0]?.["text"]).toBe(
    "Download complete (paper.pdf, 1.2 MB)",
  );

  expect(() => parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "invalid", "browser-activity-request-missing-request-id.json"), "utf8"),
  )).toThrow(ProtocolError);
});

test("page capture request and result round-trip through the shared corpus", () => {
  const request = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-capture-request.json"), "utf8"),
  );
  expect(request.type).toBe("page_capture_request");
  expect(request.payload).toEqual({
    request_id: "capture-request-001",
    url: "https://journals.example.org/article/42",
    provider: "example_provider",
    scenario: "success",
    settle_ms: 2500,
  });
  const result = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-capture-request-result.json"), "utf8"),
  );
  expect(result.type).toBe("page_capture_request_result");
  expect(result.payload).toEqual({ request_id: "capture-request-001", outcome: "captured" });
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(join(corpusRoot, "invalid", "browser-page-capture-request-http.json"), "utf8"),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(join(corpusRoot, "invalid", "browser-page-capture-request-result-outcome.json"), "utf8"),
    ),
  ).toThrow(ProtocolError);
});

test("counts schema negotiation and session evidence round-trip", () => {
  const v1 = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-triage-counts-response.json"), "utf8"),
  );
  expect(v1.payload["counts"]).not.toHaveProperty("actions_requires_auth");
  const requestV2 = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-triage-counts-request-v2.json"), "utf8"),
  );
  expect(requestV2.payload).toEqual({ request_id: "request-0003", schema_versions: [2] });
  const v2 = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-triage-counts-response-v2.json"), "utf8"),
  );
  expect((v2.payload["counts"] as Record<string, unknown>)["actions_requires_auth"]).toBe(1);
  const evidence = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-session-evidence.json"), "utf8"),
  );
  expect(evidence.payload).toEqual({
    evidence: "warm_verified",
    origin_hint: "https://resolver.example.edu",
    at: "2026-08-03T12:00:00Z",
  });
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(join(corpusRoot, "invalid", "browser-session-evidence-missing-evidence.json"), "utf8"),
    ),
  ).toThrow(ProtocolError);
});


test("handoff_focus is an empty job-scoped frame listed by the shared schema", () => {
  const text = readFileSync(join(corpusRoot, "valid", "browser-handoff-focus.json"), "utf8");
  const msg = parseBrowserMessageBytes(text);
  expect(msg.type).toBe("handoff_focus");
  expect(msg.job_id).toBe("job_focus_001");
  expect(msg.payload).toEqual({});

  const schema = JSON.parse(readFileSync(join(import.meta.dir, "..", "..", "protocol", "browser-v1.schema.json"), "utf8")) as {
    properties: { type: { enum: string[] } };
  };
  expect(schema.properties.type.enum).toContain("handoff_focus");

  const frame = JSON.parse(text) as Record<string, unknown>;
  const withoutJobID = { ...frame };
  delete withoutJobID["job_id"];
  expect(() => parseBrowserMessage(withoutJobID)).toThrow(ProtocolError);
  expect(() => parseBrowserMessage({ ...frame, payload: { unexpected: true } })).toThrow(ProtocolError);
});

test("provider drive epoch frames require job scope and closed result outcomes", () => {
  const base = {
    drive_attempt_id: "epoch-attempt-001",
    ordinal: 0,
    strategy: "generic",
    revision: "1",
  };
  const frame = (type: string, payload: Record<string, unknown>, jobID?: string) => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "epoch-msg-001",
    seq: 1,
    ...(jobID === undefined ? {} : { job_id: jobID }),
    payload,
  });
  const valid = [
    ["provider_drive_epoch_start_request", base],
    ["provider_drive_epoch_start_result", { ...base, outcome: "started" }],
    ["provider_drive_epoch_result_request", { ...base, outcome: "strategy_outcome" }],
    ["provider_drive_epoch_result", { ...base, outcome: "applied" }],
  ] as const;
  for (const [type, payload] of valid) {
    expect(parseBrowserMessage(frame(type, payload, "job-epoch-001")).job_id).toBe("job-epoch-001");
    expect(() => parseBrowserMessage(frame(type, payload))).toThrow(ProtocolError);
    expect(() => parseBrowserMessage(frame(type, payload, ""))).toThrow(ProtocolError);
  }
  expect(() =>
    parseBrowserMessage(frame("provider_drive_epoch_start_result", { ...base, outcome: "applied" }, "job-epoch-001")),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(frame("provider_drive_epoch_result", { ...base, outcome: "started" }, "job-epoch-001")),
  ).toThrow(ProtocolError);
});

test("handoff link correlation and URL text stay wire-strict", () => {
  const frame = (type: "handoff_link_request" | "handoff_link_result", payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "handoff-link-strict-001",
    seq: 1,
    payload,
  });
  expect(
    parseBrowserMessage(
      frame("handoff_link_result", {
        request_id: "handoff-request-001",
        outcome: "opened",
        url: "https://resolver.example.edu/openurl?title=A%20Paper",
      }),
    ).payload["outcome"],
  ).toBe("opened");
  expect(() =>
    parseBrowserMessage(
      frame("handoff_link_result", {
        request_id: "handoff-request-001",
        outcome: "opened",
        url: "https://resolver.example.edu/openurl?title=A Paper",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("handoff_link_result", {
        request_id: "handoff-request-001",
        outcome: "opened",
        url: "https://resolver.example.edu/openurl?title=%zz",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(frame("handoff_link_request", { request_id: "", job_id: "job_handoff_0001" })),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("handoff_link_result", { request_id: "", outcome: "unavailable", detail: "not available" }),
    ),
  ).toThrow(ProtocolError);
});

test("triage schema 1 keeps the locked action shape while schema 2 carries access classification", () => {
  const text = readFileSync(join(corpusRoot, "valid", "browser-triage-snapshot-response.json"), "utf8");
  expect(JSON.stringify(parseBrowserMessageBytes(text).payload))
    .toContain('"requires_auth":true,"blocked_by":"paywall"');

  const schema1 = text
    .replace('"schema":2', '"schema":1')
    .replace(',"requires_auth":true,"blocked_by":"paywall"', "");
  expect(parseBrowserMessageBytes(schema1).protocol).toBe("papio-browser/1");

  const invalidSchema1 = text.replace('"schema":2', '"schema":1');
  expect(() => parseBrowserMessageBytes(invalidSchema1)).toThrow(ProtocolError);

  const invalid = text.replace('"blocked_by":"paywall"', '"blocked_by":"captcha"');
  expect(() => parseBrowserMessageBytes(invalid)).toThrow(ProtocolError);
});

test("triage schema 3 requires attention/route_class/auth_requirement and gates delivery to document_delivery", () => {
  const text = readFileSync(join(corpusRoot, "valid", "browser-triage-snapshot-response-v3.json"), "utf8");
  const parsed = parseBrowserMessageBytes(text);
  expect(parsed.payload["schema"]).toBe(3);
  const items = parsed.payload["items"] as Array<Record<string, unknown>>;
  const delivery = items.find((item) => item["action_kind"] === "document_delivery");
  expect(delivery?.["attention"]).toBe("required");
  expect(delivery?.["route_class"]).toBe("document_delivery");
  expect(delivery?.["auth_requirement"]).toBe("unknown");
  expect(delivery?.["delivery"]).toEqual({ provider: "illiad", provider_reference: "TN-42", state: "unknown_outcome" });
  expect(delivery?.["ops"]).toEqual(["open_request_history", "confirm_request_exists", "confirm_request_absent"]);

  const missingAttention = JSON.parse(text);
  delete missingAttention.payload.items[0].attention;
  expect(() => parseBrowserMessage(missingAttention)).toThrow(ProtocolError);

  const missingRouteClass = JSON.parse(text);
  delete missingRouteClass.payload.items[1].route_class;
  expect(() => parseBrowserMessage(missingRouteClass)).toThrow(ProtocolError);

  const deliveryOnWrongKind = JSON.parse(text);
  deliveryOnWrongKind.payload.items[1].delivery = { provider: "illiad", state: "pending" };
  expect(() => parseBrowserMessage(deliveryOnWrongKind)).toThrow(ProtocolError);

  // A schema-2 frame must never carry a schema-3-only blocked_by value —
  // new values are v3-only, never overloading a v2 value's meaning.
  const v2Text = readFileSync(join(corpusRoot, "valid", "browser-triage-snapshot-response.json"), "utf8");
  const v2WithV3BlockedBy = v2Text.replace('"blocked_by":"paywall"', '"blocked_by":"login"');
  expect(() => parseBrowserMessageBytes(v2WithV3BlockedBy)).toThrow(ProtocolError);
});

test("delivery offered state parses while unknown state is rejected", () => {
  const text = readFileSync(join(corpusRoot, "valid", "browser-triage-snapshot-response-v3.json"), "utf8");
  const offered = JSON.parse(text) as { payload: { items: Array<Record<string, unknown>> } };
  const offeredDelivery = offered.payload.items.find((item) => item["action_kind"] === "document_delivery")?.["delivery"];
  expect(offeredDelivery).toBeDefined();
  (offeredDelivery as Record<string, unknown>)["state"] = "offered";
  expect(parseBrowserMessage(offered).protocol).toBe("papio-browser/1");

  const unknown = JSON.parse(text) as { payload: { items: Array<Record<string, unknown>> } };
  const unknownDelivery = unknown.payload.items.find((item) => item["action_kind"] === "document_delivery")?.["delivery"];
  expect(unknownDelivery).toBeDefined();
  (unknownDelivery as Record<string, unknown>)["state"] = "not_a_delivery_state";
  expect(() => parseBrowserMessage(unknown)).toThrow(ProtocolError);
});

test("downloads_access_required route_class parses and rejects a route_class outside the closed vocabulary", () => {
  const text = readFileSync(join(corpusRoot, "valid", "browser-triage-snapshot-downloads-access-required.json"), "utf8");
  const parsed = parseBrowserMessageBytes(text);
  const items = parsed.payload["items"] as Array<Record<string, unknown>>;
  const action = items.find((item) => item["action_kind"] === "downloads_access_required");
  expect(action?.["attention"]).toBe("required");
  expect(action?.["route_class"]).toBe("downloads_access_required");
  expect(action?.["auth_requirement"]).toBe("unknown");
  const detail = (action?.["facts"] as Array<{ label: string; text: string }>).find((fact) => fact.label === "Detail");
  expect(detail?.text).toBe("/Users/example/Downloads/papio");

  const unknownRouteClass = JSON.parse(text);
  unknownRouteClass.payload.items[0].route_class = "downloads_access_pending";
  expect(() => parseBrowserMessage(unknownRouteClass)).toThrow(ProtocolError);
});

test("delivery_reconcile_request/result round-trip and validate operation-specific provider_reference rules", () => {
  const base = {
    protocol: "papio-browser/1",
    type: "delivery_reconcile_request",
    msg_id: "delivery-reconcile-request-0001",
    seq: 1,
    payload: { request_id: "request-delivery-0001", job_id: "job_delivery_0001", operation: "confirm_request_exists", provider_reference: "TN-42" },
  };
  expect(parseBrowserMessage(base).type).toBe("delivery_reconcile_request");

  const missingReference = { ...base, payload: { ...base.payload, provider_reference: undefined } };
  delete (missingReference.payload as Record<string, unknown>)["provider_reference"];
  expect(() => parseBrowserMessage(missingReference)).toThrow(ProtocolError);

  const absentWithReference = { ...base, payload: { ...base.payload, operation: "confirm_request_absent" } };
  expect(() => parseBrowserMessage(absentWithReference)).toThrow(ProtocolError);

  const absentWithoutReference = { ...base, payload: { request_id: base.payload.request_id, job_id: base.payload.job_id, operation: "confirm_request_absent" } };
  expect(parseBrowserMessage(absentWithoutReference).type).toBe("delivery_reconcile_request");

  const result = {
    protocol: "papio-browser/1",
    type: "delivery_reconcile_result",
    msg_id: "delivery-reconcile-result-0001",
    seq: 2,
    payload: { request_id: "request-delivery-0001", outcome: "applied" },
  };
  expect(parseBrowserMessage(result).type).toBe("delivery_reconcile_result");
});

test("invalid browser corpus fails closed", () => {
  const fixtures = readdirSync(join(corpusRoot, "invalid")).filter((name) => name.startsWith("browser-"));
  expect(fixtures.length).toBeGreaterThanOrEqual(4);
  for (const name of fixtures) {
    const text = readFileSync(join(corpusRoot, "invalid", name), "utf8");
    expect(() => parseBrowserMessageBytes(text), name).toThrow(ProtocolError);
  }
});

test("download_id must be >= 1: the correlation key half of browserDownloadKey{JobID, DownloadID} collides at 0", () => {
  // chrome.downloads allocates ids starting at 1, so a genuine extension never
  // sends 0 — this pins fail-closed hardening, not a live-traffic fix. See
  // testdata/protocol/invalid/browser-download-id-zero.json and
  // browser-delivery-context-download-id-zero.json for the shared-corpus half
  // of this contract.
  const started = (downloadId: number) => ({
    protocol: "papio-browser/1",
    type: "download_started",
    msg_id: "m_dls_floor01",
    job_id: "job_dls_floor01",
    seq: 1,
    payload: { download_id: downloadId, filename: "paper.pdf" },
  });
  const complete = (downloadId: number) => ({
    protocol: "papio-browser/1",
    type: "download_complete",
    msg_id: "m_dlc_floor01",
    job_id: "job_dlc_floor01",
    seq: 1,
    payload: { download_id: downloadId, filename: "paper.pdf", size_bytes: 100 },
  });
  const deliveryContext = (downloadId: number) => ({
    protocol: "papio-browser/1",
    type: "delivery_context",
    msg_id: "m_dctx_floor01",
    job_id: "job_dctx_floor01",
    seq: 1,
    payload: { download_id: downloadId, route: "direct", session_evidence: "none" },
  });

  for (const frame of [started, complete, deliveryContext]) {
    expect(() => parseBrowserMessage(frame(0))).toThrow(ProtocolError);
    expect(() => parseBrowserMessage(frame(-1))).toThrow(ProtocolError);
    expect(parseBrowserMessage(frame(1)).payload["download_id"]).toBe(1);
  }
});

test("delivery_context.page_host rejects a leading dot, a trailing dot, and a '..' run", () => {
  // papio-a82ab8e6906fda25: the published pattern
  // `^[a-z0-9.-]{3,128}$` alone silently admitted these three shapes, even
  // though this parser and internal/protocol/protocol.go already rejected
  // them explicitly — the schema documented a contract neither executable
  // parser honoured. See testdata/protocol/invalid/browser-delivery-context-
  // page-host-{leading,trailing,double}-dot.json for the shared-corpus half
  // of this contract, and protocol/browser-v1.schema.json's new "not" clause
  // for the schema half.
  const frame = (pageHost: string) => ({
    protocol: "papio-browser/1",
    type: "delivery_context",
    msg_id: "m_dctx_host_case",
    job_id: "job_dctx_host_case",
    seq: 1,
    payload: { download_id: 1, route: "direct", session_evidence: "none", page_host: pageHost },
  });
  for (const host of [".abc", "abc.", "a..b"]) {
    expect(() => parseBrowserMessage(frame(host)), host).toThrow(ProtocolError);
  }
  expect(parseBrowserMessage(frame("publisher.example.edu")).payload["page_host"]).toBe("publisher.example.edu");
});

test("session_evidence.origin_hint rejects a mixed-case host", () => {
  // papio-26fa531528e29798: Go, this parser, and the schema previously
  // disagreed on this shape. "https://EXAMPLE.com" was accepted by Go's
  // validateBareRoute (which never compared case) and rejected here only as
  // a side effect of `new URL()` lowercasing the host of a special scheme
  // before the round-trip equality check below — an accident, not a stated
  // rule. ORIGIN_HOST_RE now makes the rejection explicit and identical
  // across internal/protocol/protocol.go, this file, and
  // protocol/browser-v1.schema.json. See testdata/protocol/invalid/browser-
  // session-evidence-origin-hint-uppercase-host.json for the shared-corpus
  // half of this contract. The single-label counterpart of this test
  // (papio-26fa531528e29798's other half) was reverted: a single-label host
  // is a legitimate origin_hint value, not an invalid one — see the
  // "accepts legitimate hosts" test below and
  // testdata/protocol/valid/browser-session-evidence-origin-hint-single-label.json.
  const frame = (originHint: string) => ({
    protocol: "papio-browser/1",
    type: "session_evidence",
    msg_id: "m_origin_case",
    seq: 1,
    payload: { evidence: "warm_verified", origin_hint: originHint, at: "2026-08-03T12:00:00Z" },
  });
  expect(() => parseBrowserMessage(frame("https://EXAMPLE.com"))).toThrow(ProtocolError);
  expect(parseBrowserMessage(frame("https://resolver.example.edu")).payload["origin_hint"]).toBe(
    "https://resolver.example.edu",
  );
});

test("session_evidence.origin_hint accepts every host shape a valid config can produce", () => {
  // Release blocker: a single-label intranet resolver, localhost (with and
  // without a port), and an IPv4 literal are all values
  // internal/config/config.go's validateOpenURLBase accepts (only an https
  // scheme and a non-empty host — no FQDN, no label count), and this
  // module's resolverOriginHint/latestResolverOrigin derive origin_hint
  // straight from that configured origin. Rejecting any of these here
  // silently drops the whole session_evidence frame outbound (Bridge.send
  // self-validates and discards an invalid frame) and fatally kills the
  // native-messaging session inbound under version skew. See
  // ORIGIN_HOST_RE's doc comment for the full incident.
  const frame = (originHint: string) => ({
    protocol: "papio-browser/1",
    type: "session_evidence",
    msg_id: "m_origin_legit",
    seq: 1,
    payload: { evidence: "warm_verified", origin_hint: originHint, at: "2026-08-03T12:00:00Z" },
  });
  for (const hint of ["https://library", "https://localhost", "https://localhost:8443", "https://127.0.0.1"]) {
    expect(parseBrowserMessage(frame(hint)).payload["origin_hint"], hint).toBe(hint);
  }
});

test("hello_ack accepts optional daemon details and rejects invalid members", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "hello_ack",
    msg_id: "daemon-ack-001",
    seq: 1,
    payload,
  });

  expect(parseBrowserMessage(frame({})).payload).toEqual({});
  expect(parseBrowserMessage(frame({
    daemon_version: "0.1.0",
    features: ["browser_handoff"],
  })).payload).toEqual({
    daemon_version: "0.1.0",
    features: ["browser_handoff"],
  });
  expect(() => parseBrowserMessage(frame({ features: [null] }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame({ daemon_version: "v".repeat(51) }))).toThrow(ProtocolError);
  expect(
    parseBrowserMessage(frame({ resolver_origins: ["https://onesearch.library.example.edu"] })).payload,
  ).toEqual({ resolver_origins: ["https://onesearch.library.example.edu"] });
  expect(() => parseBrowserMessage(frame({ resolver_origins: [null] }))).toThrow(ProtocolError);
  for (const bad of [
    "http://insecure.example.edu",
    "https://example.edu/path",
    "https://example.edu?x=1",
    "ftp://example.edu",
  ]) {
    expect(() => parseBrowserMessage(frame({ resolver_origins: [bad] }))).toThrow(ProtocolError);
  }
});

test("hello features are optional but strict, unique, and bounded", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "hello",
    msg_id: "client-hello-1",
    seq: 0,
    payload,
  });
  expect(parseBrowserMessage(frame({
    extension_version: "0.14.0",
    features: ["institutional_materialization_v1", "future_capability"],
  })).payload["features"]).toEqual(["institutional_materialization_v1", "future_capability"]);
  expect(parseBrowserMessage(frame({ extension_version: "0.14.0" })).payload).toEqual({
    extension_version: "0.14.0",
  });
  expect(() => parseBrowserMessage(frame({ extension_version: "0.14.0", unknown: true }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame({
    extension_version: "0.14.0",
    features: ["future_capability", "future_capability"],
  }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame({ extension_version: "0.14.0", features: ["Future"] }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame({
    extension_version: "0.14.0",
    features: Array.from({ length: 33 }, (_, i) => `future_${i}`),
  }))).toThrow(ProtocolError);
});

test("page_acquire messages parse strictly", () => {
  const frame = (type: "page_acquire" | "page_acquire_ack", payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "page-acquire-001",
    seq: 1,
    payload,
  });

  expect(parseBrowserMessage(frame("page_acquire", {
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  })).payload).toEqual({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  });
  expect(parseBrowserMessage(frame("page_acquire_ack", {
    job_id: "job_page_acquire_001",
    duplicate: true,
  })).payload).toEqual({ job_id: "job_page_acquire_001", duplicate: true });
  expect(parseBrowserMessage(frame("page_acquire_ack", {
    error: "page has no DOI",
  })).payload).toEqual({ error: "page has no DOI" });


  for (const payload of [
    {},
    { url: "ftp://publisher.example.edu/article/42" },
    { url: "https://publisher.example.edu/article/42", doi: "d".repeat(513) },
    { url: null },
    { url: "https://publisher.example.edu/article/42", unexpected: true },
    { url: "https://publisher.example.edu/article/\0" },
    { url: "https://publisher.example.edu/article/42", doi: "10.1000/\0example" },
    { url: "https://publisher.example.edu/article/42", title: "Example\0 Paper" },
    { url: "https://publisher.example.edu/article/42", source: "pop\0up" },
  ]) {
    expect(() => parseBrowserMessage(frame("page_acquire", payload))).toThrow(ProtocolError);
  }
  for (const payload of [
    { job_id: null },
    { duplicate: "yes" },
    { error: null },
    { unexpected: true },
    {},
    { duplicate: true },
    { job_id: "job_page_acquire_001", error: "already queued" },
    { error: "bad\0error" },
    { error: "" },
    { job_id: "", error: "page has no DOI" },
  ]) {
    expect(() => parseBrowserMessage(frame("page_acquire_ack", payload))).toThrow(ProtocolError);
  }
});

test("page_capture messages parse strictly before echoed frames reach the inbound ignore path", () => {
  // A valid echo must pass parsing so onInbound reaches its extension-only
  // default instead of disconnecting the native session before it can ignore it.
  const text = readFileSync(join(corpusRoot, "valid", "browser-page-capture.json"), "utf8");
  const frame = JSON.parse(text) as Record<string, unknown>;
  expect(parseBrowserMessage(frame).type).toBe("page_capture");

  const unscoped = { ...frame };
  delete unscoped["job_id"];
  expect(parseBrowserMessage(unscoped).job_id).toBeUndefined();

  const payload = frame["payload"] as Record<string, unknown>;
  for (const invalid of [
    { ...payload, encoding: "base64" },
    { ...payload, bytes: 2 * 1024 * 1024 + 1 },
    { ...payload, host: "https://journals.sagepub.com" },
    { ...payload, scenario: "unexpected" },
    { ...payload, body: "not base64!" },
    { ...payload, request_id: "short" },
    { ...payload, request_id: "has spaces in it" },
    { ...payload, request_id: null },
  ]) {
    expect(() => parseBrowserMessage({ ...frame, payload: invalid })).toThrow(ProtocolError);
  }

  // Optional: an unsolicited capture omits it, a requested one echoes the id
  // it answers. The daemon binds on that presence, so both shapes must parse
  // (papio-85a7420f4cd2564f).
  expect(parseBrowserMessage({ ...frame, payload: { ...payload, request_id: "DRA6SOdBEB1ZgMIRV8qfqQ" } }).type).toBe(
    "page_capture",
  );
  expect("request_id" in payload).toBe(false);
});
test("auth payloads structurally reject URLs", () => {
  expect(() =>
    parseBrowserMessage({
      protocol: "papio-browser/1",
      type: "auth_returned",
      msg_id: "m_auth_ret1",
      job_id: "job_0002_tyler",
      seq: 5,
      payload: { url: "https://idp.example.edu/sso?token=SECRET" },
    }),
  ).toThrow(/unknown field "url"/);
});

test("oversized frames are rejected before parsing", () => {
  const pad = " ".repeat(MAX_BROWSER_MESSAGE_BYTES);
  expect(() => parseBrowserMessageBytes(`{"protocol":"papio-browser/1"}${pad}`)).toThrow(/exceeds cap/);
});

test("unknown envelope fields fail closed", () => {
  expect(() =>
    parseBrowserMessage({
      protocol: "papio-browser/1",
      type: "ack",
      msg_id: "m_ack_00001",
      seq: 0,
      payload: {},
      debug_cookie: "session=abc",
    }),
  ).toThrow(/unknown field "debug_cookie"/);
});

test("page bulk status and submit messages round-trip through the shared corpus", () => {
  const statusRequest = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-bulk-status-request.json"), "utf8"),
  );
  expect(statusRequest.type).toBe("page_bulk_status_request");
  expect((statusRequest.payload["identifiers"] as unknown[]).length).toBe(4);
  expect(statusRequest.payload["rendered_record_count_hint"]).toBe(12);

  const statusResult = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-bulk-status-result.json"), "utf8"),
  );
  expect(statusResult.type).toBe("page_bulk_status_result");
  const items = statusResult.payload["items"] as Array<Record<string, unknown>>;
  expect(items).toHaveLength(4);
  expect(items[2]).toEqual({
    local_id: "row-3", canonical_key: "work-key-queued-1", status: "queued",
    ownership_complete: false, job_id: "job_bulk_00001",
  });
  expect(items[3]).toEqual({ local_id: "row-4", status: "invalid", ownership_complete: false });

  const submitRequest = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-bulk-submit-request.json"), "utf8"),
  );
  expect(submitRequest.type).toBe("page_bulk_submit_request");
  expect(submitRequest.payload).toEqual({
    request_id: "request-bulk-0002", scan_id: "scan-bulk-0001",
    canonical_keys: ["work-key-doi-10-1000-example-42", "work-key-pmid-12345678"],
    source: { kind: "browser_page", origin: "https://scholar.example.edu", detector: "generic-identifiers/1" },
  });

  const submitResult = parseBrowserMessageBytes(
    readFileSync(join(corpusRoot, "valid", "browser-page-bulk-submit-result.json"), "utf8"),
  );
  expect(submitResult.type).toBe("page_bulk_submit_result");
  expect(submitResult.payload).toEqual({
    request_id: "request-bulk-0002", scan_id: "scan-bulk-0001",
    submitted: 1, joined: 1, already_owned: 0, invalid: 0, batch_id: "batch_bulk_00001",
  });

  for (const name of [
    "browser-page-bulk-status-request-too-many-identifiers.json",
    "browser-page-bulk-status-request-bad-kind.json",
    "browser-page-bulk-status-request-negative-hint.json",
    "browser-page-bulk-submit-request-too-many-keys.json",
    "browser-page-bulk-submit-request-origin-with-path.json",
    "browser-page-bulk-submit-request-origin-uppercase-host.json",
  ]) {
    expect(() => parseBrowserMessageBytes(readFileSync(join(corpusRoot, "invalid", name), "utf8")), name).toThrow(ProtocolError);
  }
});

test("page_bulk_submit_request.source.origin rejects a mixed-case host", () => {
  // Go's PageBulkSubmitSource.validate() used to call the permissive
  // validResolverOrigin (no host-case comparison at all), so
  // "https://Scholar.Example.EDU" decoded there while this parser's
  // round-trip check and protocol/browser-v1.schema.json's lowercase-only
  // pattern already rejected it — the same divergence direction
  // session_evidence.origin_hint hit before (see the "rejects a mixed-case
  // host" test above). Go now reuses validateBareLowercaseOrigin for
  // source.origin instead of a third copy of the rule. See
  // testdata/protocol/invalid/browser-page-bulk-submit-request-origin-
  // uppercase-host.json for the shared-corpus half of this contract.
  const frame = (origin: string) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_submit_request",
    msg_id: "m_bulk_submit_origin_case",
    seq: 1,
    payload: {
      request_id: "request-bulk-0002",
      scan_id: "scan-bulk-0001",
      canonical_keys: ["work-key-1"],
      source: { kind: "browser_page", origin, detector: "generic-identifiers/1" },
    },
  });
  expect(() => parseBrowserMessage(frame("https://Scholar.Example.EDU"))).toThrow(ProtocolError);
  expect(parseBrowserMessage(frame("https://scholar.example.edu")).type).toBe("page_bulk_submit_request");
});

test("page_bulk_status_request rejects malformed identifiers", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_status_request",
    msg_id: "page-bulk-status-req-01",
    seq: 1,
    payload,
  });
  const validIdentifier = { local_id: "row-1", kind: "doi", value: "10.1000/example.42" };
  const openalexIdentifier = { local_id: "row-2", kind: "openalex", value: "W2741809807" };

  expect(parseBrowserMessage(frame({
    request_id: "request-bulk-0001", scan_id: "scan-bulk-0001", identifiers: [validIdentifier, openalexIdentifier],
  })).payload).toEqual({
    request_id: "request-bulk-0001", scan_id: "scan-bulk-0001", identifiers: [validIdentifier, openalexIdentifier],
  });

  for (const identifiers of [
    [],
    Array.from({ length: 201 }, (_, i) => ({ local_id: `row-${i}`, kind: "doi", value: `10.1000/x${i}` })),
    [{ local_id: "row-1", kind: "isbn", value: "9780000000002" }],
    [validIdentifier, { ...validIdentifier }],
    [{ local_id: "row-1", kind: "doi", value: "" }],
    [{ local_id: "", kind: "doi", value: "10.1000/example.42" }],
  ]) {
    expect(() => parseBrowserMessage(frame({
      request_id: "request-bulk-0001", scan_id: "scan-bulk-0001", identifiers,
    }))).toThrow(ProtocolError);
  }
  expect(() => parseBrowserMessage(frame({
    request_id: "request-bulk-0001", scan_id: "short", identifiers: [validIdentifier],
  }))).toThrow(ProtocolError);
});

test("page_bulk_status_result enforces the closed status vocabulary and canonical_key/job_id invariants", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_status_result",
    msg_id: "page-bulk-status-res-01",
    seq: 1,
    payload,
  });
  const base = { request_id: "request-bulk-0001", scan_id: "scan-bulk-0001", truncated: false };

  expect(parseBrowserMessage(frame({
    ...base,
    items: [{ local_id: "row-1", status: "invalid", ownership_complete: false }],
  })).payload).toEqual({
    ...base,
    items: [{ local_id: "row-1", status: "invalid", ownership_complete: false }],
  });

  for (const items of [
    [{ local_id: "row-1", canonical_key: "wk1", status: "invalid", ownership_complete: false }],
    [{ local_id: "row-1", status: "eligible", ownership_complete: false }],
    [{ local_id: "row-1", canonical_key: "wk1", status: "unexpected", ownership_complete: false }],
    [{ local_id: "row-1", canonical_key: "wk1", status: "eligible", ownership_complete: false, job_id: "job_bulk_00001" }],
    [{ local_id: "row-1", canonical_key: "wk1", status: "queued", ownership_complete: false, job_id: "short" }],
    Array.from({ length: 201 }, (_, i) => ({ local_id: `row-${i}`, canonical_key: "wk", status: "eligible", ownership_complete: false })),
  ]) {
    expect(() => parseBrowserMessage(frame({ ...base, items }))).toThrow(ProtocolError);
  }
});

test("page_bulk_submit_request requires a bare https origin and bounds canonical_keys", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_submit_request",
    msg_id: "page-bulk-submit-req-01",
    seq: 1,
    payload,
  });
  const validSource = { kind: "browser_page", origin: "https://scholar.example.edu", detector: "generic-identifiers/1" };

  expect(parseBrowserMessage(frame({
    request_id: "request-bulk-0002", scan_id: "scan-bulk-0001", canonical_keys: ["wk1"], source: validSource,
  })).payload).toEqual({
    request_id: "request-bulk-0002", scan_id: "scan-bulk-0001", canonical_keys: ["wk1"], source: validSource,
  });

  for (const payload of [
    { canonical_keys: [], source: validSource },
    { canonical_keys: Array.from({ length: 51 }, (_, i) => `wk${i}`), source: validSource },
    { canonical_keys: ["wk1", "wk1"], source: validSource },
    { canonical_keys: ["wk1"], source: { ...validSource, origin: "https://scholar.example.edu/path" } },
    { canonical_keys: ["wk1"], source: { ...validSource, origin: "https://scholar.example.edu?x=1" } },
    { canonical_keys: ["wk1"], source: { ...validSource, origin: "http://scholar.example.edu" } },
    { canonical_keys: ["wk1"], source: { ...validSource, detector: "" } },
    { canonical_keys: ["wk1"], source: { ...validSource, kind: "extension" } },
  ]) {
    expect(() => parseBrowserMessage(frame({
      request_id: "request-bulk-0002", scan_id: "scan-bulk-0001", ...payload,
    }))).toThrow(ProtocolError);
  }
});

test("page_bulk_submit_result rejects negative counts and a malformed batch_id", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_submit_result",
    msg_id: "page-bulk-submit-res-01",
    seq: 1,
    payload,
  });
  const base = { request_id: "request-bulk-0003", scan_id: "scan-bulk-0001", submitted: 1, joined: 0, already_owned: 0, invalid: 0 };

  expect(parseBrowserMessage(frame({ ...base, batch_id: "batch_bulk_00001" })).payload).toEqual({
    ...base, batch_id: "batch_bulk_00001",
  });
  expect(() => parseBrowserMessage(frame({ ...base, submitted: -1, batch_id: "batch_bulk_00001" }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame({ ...base, batch_id: "short" }))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame(base))).toThrow(ProtocolError);
});

 
test("institutional materialization families are strict, bounded, and job-scoped", () => {
  type InstitutionalMessageType = Extract<BrowserMessageType, `institutional_${string}`>;
  const envelope = (type: InstitutionalMessageType, payload: Record<string, unknown>, job = "job-inst-001") => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "msg-inst-001",
    seq: 1,
    ...(job === "" ? {} : { job_id: job }),
    payload: { request_id: "request-inst-001", ...payload },
  });
  const ids = { candidate_id: "candidate-001", claim_id: "claim-001", binding_id: "binding-001" };
  const valid: Array<readonly [InstitutionalMessageType, Record<string, unknown>, string]> = [
    ["institutional_claim_request", { candidate_id: ids.candidate_id, materialization_kind: "browser_tab" }, "job-inst-001"],
    ["institutional_claim_response", {
      outcome: "claimed", ...ids, browser_holder_generation: 1, lease_until: "2026-08-11T12:00:00Z",
    }, "job-inst-001"],
    ["institutional_bind_request", { claim_id: ids.claim_id, binding_id: ids.binding_id, tab_id: 7 }, "job-inst-001"],
    ["institutional_bind_response", { outcome: "bound", claim_id: ids.claim_id, binding_id: ids.binding_id }, "job-inst-001"],
    ["institutional_route_request", { claim_id: ids.claim_id, binding_id: ids.binding_id }, "job-inst-001"],
    ["institutional_route_response", {
      outcome: "issued", claim_id: ids.claim_id, binding_id: ids.binding_id, route_issuance_ordinal: 0,
      url: "https://resolver.example.edu/open",
    }, "job-inst-001"],
    ["institutional_navigated_request", {
      claim_id: ids.claim_id, binding_id: ids.binding_id, route_issuance_ordinal: 0, tab_id: 7,
    }, "job-inst-001"],
    ["institutional_navigated_response", { outcome: "acknowledged", claim_id: ids.claim_id, binding_id: ids.binding_id }, "job-inst-001"],
    ["institutional_reconcile_request", {
      bindings: [{ binding_id: ids.binding_id, tab_id: 7 }],
    }, ""],
    ["institutional_reconcile_response", {
      outcome: "reconciled",
      claims: [{ claim_id: ids.claim_id, binding_id: ids.binding_id, candidate_id: ids.candidate_id, phase: "bound", tab_id: 7 }],
    }, ""],
  ];
  for (const [type, payload, job] of valid) {
    expect(parseBrowserMessage(envelope(type, payload, job)).type).toBe(type);
  }

  const disabled = [
    ["institutional_claim_response", { outcome: "feature_disabled", detail: "dark" }],
    ["institutional_bind_response", { outcome: "feature_disabled", detail: "dark" }],
    ["institutional_route_response", { outcome: "feature_disabled", detail: "dark" }],
    ["institutional_navigated_response", { outcome: "feature_disabled", detail: "dark" }],
    ["institutional_reconcile_response", { outcome: "feature_disabled", detail: "dark" }],
  ] as const;
  for (const [type, payload] of disabled) {
    const job = type.startsWith("institutional_reconcile") ? "" : "job-inst-001";
    expect(parseBrowserMessage(envelope(type, payload, job)).type).toBe(type);
  }

  expect(() => parseBrowserMessage(envelope(
    "institutional_claim_request", { candidate_id: ids.candidate_id, materialization_kind: "browser_tab" }, "",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_reconcile_request", { bindings: [{ binding_id: ids.binding_id, tab_id: 1 }], extra: true }, "",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_route_response", { outcome: "stale", detail: "late", url: "https://resolver.example.edu/x" }, "",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_route_response", { outcome: "issued", claim_id: ids.claim_id, binding_id: ids.binding_id, route_issuance_ordinal: 0 }, "",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_bind_request", { claim_id: ids.claim_id, binding_id: ids.binding_id, tab_id: Number.MAX_SAFE_INTEGER + 1 }, "job-inst-001",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_reconcile_request", {
      bindings: Array.from({ length: 33 }, (_, i) => ({ binding_id: `binding-${String(i).padStart(3, "0")}`, tab_id: i })),
    }, "",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_claim_request", { candidate_id: "x".repeat(129), materialization_kind: "browser_tab" }, "job-inst-001",
  ))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope(
    "institutional_bind_request", { claim_id: ids.claim_id, binding_id: ids.binding_id, tab_id: -1 }, "job-inst-001",
  ))).toThrow(ProtocolError);
  const missingRequestID = envelope("institutional_claim_request", {
    candidate_id: ids.candidate_id,
    materialization_kind: "browser_tab",
  });
  delete (missingRequestID.payload as Record<string, unknown>).request_id;
  expect(() => parseBrowserMessage(missingRequestID)).toThrow(ProtocolError);
});

test("institutional_candidate_offer is URL-free, browser-tab-only, job-scoped, and bounded", () => {
  const envelope = (payload: Record<string, unknown>, job?: string) => ({
    protocol: "papio-browser/1",
    type: "institutional_candidate_offer",
    msg_id: "candidate-offer-001",
    seq: 1,
    ...(job === undefined ? { job_id: "job-inst-001" } : job === "" ? {} : { job_id: job }),
    payload,
  });
  const valid = {
    candidate_id: "candidate-001",
    materialization_kind: "browser_tab",
    expires_at: "2026-08-11T12:00:00Z",
  };
  expect(parseBrowserMessage(envelope(valid)).payload).toEqual(valid);
  expect(parseBrowserMessage(envelope(valid, "job-inst-001")).type).toBe("institutional_candidate_offer");

  for (const payload of [
    { ...valid, url: "https://resolver.example.edu/open" },
    { ...valid, materialization_kind: "direct_download" },
    { ...valid, candidate_id: "short" },
    { ...valid, candidate_id: "x".repeat(129) },
    { ...valid, expires_at: "not-a-time" },
  ]) {
    expect(() => parseBrowserMessage(envelope(payload))).toThrow(ProtocolError);
  }
  expect(() => parseBrowserMessage(envelope(valid, ""))).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(envelope({ ...valid, extra: true }))).toThrow(ProtocolError);

  const oversized = JSON.stringify(envelope({ ...valid, extra: "x".repeat(MAX_BROWSER_MESSAGE_BYTES) }));
  expect(() => parseBrowserMessageBytes(oversized)).toThrow(/exceeds cap/);
});