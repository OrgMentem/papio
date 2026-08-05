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
  ]) {
    expect(() => parseBrowserMessage({ ...frame, payload: invalid })).toThrow(ProtocolError);
  }
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
