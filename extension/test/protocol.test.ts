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
