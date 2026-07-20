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
