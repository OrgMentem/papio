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
