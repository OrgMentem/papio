// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Conformance against the SHARED corpus: the TypeScript parser must accept and
// reject exactly the browser-* fixtures the Go core does.

import { expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import {
  BROWSER_SESSION_ROLES,
  GUIDANCE_VARIANTS,
  MAX_BROWSER_MESSAGE_BYTES,
  NEXT_ACTORS,
  OPERATION_VARIANTS,
  PDF_GRAB_REFUSAL_REASONS,
  ProtocolError,
  isBareLowercaseHTTPSOrigin,
  isCanonicalKey,
  isDetectorText,
  parseBrowserMessage,
  parseBrowserMessageBytes,
  parseBrowserMessageWithLegacyInstitutionalNavigation,
} from "../src/protocol";
import type { BrowserMessageType } from "../src/protocol";

const corpusRoot = join(import.meta.dir, "..", "..", "testdata", "protocol");

test("valid browser corpus parses", () => {
  const fixtures = readdirSync(join(corpusRoot, "valid")).filter((name) =>
    name.startsWith("browser-"),
  );
  expect(fixtures.length).toBeGreaterThanOrEqual(5);
  for (const name of fixtures) {
    const text = readFileSync(join(corpusRoot, "valid", name), "utf8");
    const msg = parseBrowserMessageBytes(text);
    expect(msg.protocol).toBe("papio-browser/1");
  }
});
test("activity request and response round-trip through the shared corpus", () => {
  const request = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-activity-request.json"),
      "utf8",
    ),
  );
  expect(request.type).toBe("activity_request");
  expect(request.payload).toEqual({
    request_id: "request-activity-1",
    limit: 12,
  });
  const defaultRequest = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-activity-request-default.json"),
      "utf8",
    ),
  );
  expect(defaultRequest.payload).toEqual({ request_id: "request-activity-2" });

  const response = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-activity-response.json"),
      "utf8",
    ),
  );
  expect(response.type).toBe("activity_response");
  expect(
    response.payload["entries"] as Array<Record<string, unknown>>,
  ).toHaveLength(2);
  expect(
    (response.payload["entries"] as Array<Record<string, unknown>>)[0]?.[
      "text"
    ],
  ).toBe("Download complete (paper.pdf, 1.2 MB)");

  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(
        join(
          corpusRoot,
          "invalid",
          "browser-activity-request-missing-request-id.json",
        ),
        "utf8",
      ),
    ),
  ).toThrow(ProtocolError);
});

test("page capture request and result round-trip through the shared corpus", () => {
  const request = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-page-capture-request.json"),
      "utf8",
    ),
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
    readFileSync(
      join(corpusRoot, "valid", "browser-page-capture-request-result.json"),
      "utf8",
    ),
  );
  expect(result.type).toBe("page_capture_request_result");
  expect(result.payload).toEqual({
    request_id: "capture-request-001",
    outcome: "captured",
  });
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(
        join(corpusRoot, "invalid", "browser-page-capture-request-http.json"),
        "utf8",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(
        join(
          corpusRoot,
          "invalid",
          "browser-page-capture-request-result-outcome.json",
        ),
        "utf8",
      ),
    ),
  ).toThrow(ProtocolError);
});

test("counts schema negotiation and session evidence round-trip", () => {
  const v1 = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-triage-counts-response.json"),
      "utf8",
    ),
  );
  expect(v1.payload["counts"]).not.toHaveProperty("actions_requires_auth");
  const requestV2 = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-triage-counts-request-v2.json"),
      "utf8",
    ),
  );
  expect(requestV2.payload).toEqual({
    request_id: "request-0003",
    schema_versions: [2],
  });
  const v2 = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-triage-counts-response-v2.json"),
      "utf8",
    ),
  );
  expect(
    (v2.payload["counts"] as Record<string, unknown>)["actions_requires_auth"],
  ).toBe(1);
  const evidence = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-session-evidence.json"),
      "utf8",
    ),
  );
  expect(evidence.payload).toEqual({
    evidence: "warm_verified",
    origin_hint: "https://resolver.example.edu",
    at: "2026-08-03T12:00:00Z",
  });
  expect(() =>
    parseBrowserMessageBytes(
      readFileSync(
        join(
          corpusRoot,
          "invalid",
          "browser-session-evidence-missing-evidence.json",
        ),
        "utf8",
      ),
    ),
  ).toThrow(ProtocolError);
});

test("handoff_focus is an empty job-scoped frame listed by the shared schema", () => {
  const text = readFileSync(
    join(corpusRoot, "valid", "browser-handoff-focus.json"),
    "utf8",
  );
  const msg = parseBrowserMessageBytes(text);
  expect(msg.type).toBe("handoff_focus");
  expect(msg.job_id).toBe("job_focus_001");
  expect(msg.payload).toEqual({});

  const schema = JSON.parse(
    readFileSync(
      join(import.meta.dir, "..", "..", "protocol", "browser-v1.schema.json"),
      "utf8",
    ),
  ) as {
    properties: { type: { enum: string[] } };
  };
  expect(schema.properties.type.enum).toContain("handoff_focus");

  const frame = JSON.parse(text) as Record<string, unknown>;
  const withoutJobID = { ...frame };
  delete withoutJobID["job_id"];
  expect(() => parseBrowserMessage(withoutJobID)).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({ ...frame, payload: { unexpected: true } }),
  ).toThrow(ProtocolError);
});

test("provider drive epoch frames require job scope and closed result outcomes", () => {
  const base = {
    drive_attempt_id: "epoch-attempt-001",
    ordinal: 0,
    strategy: "generic",
    revision: "1",
  };
  const frame = (
    type: string,
    payload: Record<string, unknown>,
    jobID?: string,
  ) => ({
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
    [
      "provider_drive_epoch_result_request",
      { ...base, outcome: "strategy_outcome" },
    ],
    ["provider_drive_epoch_result", { ...base, outcome: "applied" }],
  ] as const;
  for (const [type, payload] of valid) {
    expect(
      parseBrowserMessage(frame(type, payload, "job-epoch-001")).job_id,
    ).toBe("job-epoch-001");
    expect(() => parseBrowserMessage(frame(type, payload))).toThrow(
      ProtocolError,
    );
    expect(() => parseBrowserMessage(frame(type, payload, ""))).toThrow(
      ProtocolError,
    );
  }
  expect(() =>
    parseBrowserMessage(
      frame(
        "provider_drive_epoch_start_result",
        { ...base, outcome: "applied" },
        "job-epoch-001",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame(
        "provider_drive_epoch_result",
        { ...base, outcome: "started" },
        "job-epoch-001",
      ),
    ),
  ).toThrow(ProtocolError);
});

test("handoff link correlation and URL text stay wire-strict", () => {
  const frame = (
    type: "handoff_link_request" | "handoff_link_result",
    payload: Record<string, unknown>,
  ) => ({
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
    parseBrowserMessage(
      frame("handoff_link_request", {
        request_id: "",
        job_id: "job_handoff_0001",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("handoff_link_result", {
        request_id: "",
        outcome: "unavailable",
        detail: "not available",
      }),
    ),
  ).toThrow(ProtocolError);
});

test("triage schema 1 keeps the locked action shape while schema 2 carries access classification", () => {
  const text = readFileSync(
    join(corpusRoot, "valid", "browser-triage-snapshot-response.json"),
    "utf8",
  );
  expect(JSON.stringify(parseBrowserMessageBytes(text).payload)).toContain(
    '"requires_auth":true,"blocked_by":"paywall"',
  );

  const schema1 = text
    .replace('"schema":2', '"schema":1')
    .replace(',"requires_auth":true,"blocked_by":"paywall"', "");
  expect(parseBrowserMessageBytes(schema1).protocol).toBe("papio-browser/1");

  const invalidSchema1 = text.replace('"schema":2', '"schema":1');
  expect(() => parseBrowserMessageBytes(invalidSchema1)).toThrow(ProtocolError);

  const invalid = text.replace(
    '"blocked_by":"paywall"',
    '"blocked_by":"captcha"',
  );
  expect(() => parseBrowserMessageBytes(invalid)).toThrow(ProtocolError);
});

test("triage schema 3 requires attention/route_class/auth_requirement and gates delivery to document_delivery", () => {
  const text = readFileSync(
    join(corpusRoot, "valid", "browser-triage-snapshot-response-v3.json"),
    "utf8",
  );
  const parsed = parseBrowserMessageBytes(text);
  expect(parsed.payload["schema"]).toBe(3);
  const items = parsed.payload["items"] as Array<Record<string, unknown>>;
  const delivery = items.find(
    (item) => item["action_kind"] === "document_delivery",
  );
  expect(delivery?.["attention"]).toBe("required");
  expect(delivery?.["route_class"]).toBe("document_delivery");
  expect(delivery?.["auth_requirement"]).toBe("unknown");
  expect(delivery?.["delivery"]).toEqual({
    provider: "illiad",
    provider_reference: "TN-42",
    state: "unknown_outcome",
  });
  expect(delivery?.["ops"]).toEqual([
    "open_request_history",
    "confirm_request_exists",
    "confirm_request_absent",
  ]);

  const missingAttention = JSON.parse(text);
  delete missingAttention.payload.items[0].attention;
  expect(() => parseBrowserMessage(missingAttention)).toThrow(ProtocolError);

  const missingRouteClass = JSON.parse(text);
  delete missingRouteClass.payload.items[1].route_class;
  expect(() => parseBrowserMessage(missingRouteClass)).toThrow(ProtocolError);

  const deliveryOnWrongKind = JSON.parse(text);
  deliveryOnWrongKind.payload.items[1].delivery = {
    provider: "illiad",
    state: "pending",
  };
  expect(() => parseBrowserMessage(deliveryOnWrongKind)).toThrow(ProtocolError);

  // A schema-2 frame must never carry a schema-3-only blocked_by value —
  // new values are v3-only, never overloading a v2 value's meaning.
  const v2Text = readFileSync(
    join(corpusRoot, "valid", "browser-triage-snapshot-response.json"),
    "utf8",
  );
  const v2WithV3BlockedBy = v2Text.replace(
    '"blocked_by":"paywall"',
    '"blocked_by":"login"',
  );
  expect(() => parseBrowserMessageBytes(v2WithV3BlockedBy)).toThrow(
    ProtocolError,
  );
});

test("delivery offered state parses while unknown state is rejected", () => {
  const text = readFileSync(
    join(corpusRoot, "valid", "browser-triage-snapshot-response-v3.json"),
    "utf8",
  );
  const offered = JSON.parse(text) as {
    payload: { items: Array<Record<string, unknown>> };
  };
  const offeredDelivery = offered.payload.items.find(
    (item) => item["action_kind"] === "document_delivery",
  )?.["delivery"];
  expect(offeredDelivery).toBeDefined();
  (offeredDelivery as Record<string, unknown>)["state"] = "offered";
  expect(parseBrowserMessage(offered).protocol).toBe("papio-browser/1");

  const unknown = JSON.parse(text) as {
    payload: { items: Array<Record<string, unknown>> };
  };
  const unknownDelivery = unknown.payload.items.find(
    (item) => item["action_kind"] === "document_delivery",
  )?.["delivery"];
  expect(unknownDelivery).toBeDefined();
  (unknownDelivery as Record<string, unknown>)["state"] =
    "not_a_delivery_state";
  expect(() => parseBrowserMessage(unknown)).toThrow(ProtocolError);
});

test("downloads_access_required route_class parses and rejects a route_class outside the closed vocabulary", () => {
  const text = readFileSync(
    join(
      corpusRoot,
      "valid",
      "browser-triage-snapshot-downloads-access-required.json",
    ),
    "utf8",
  );
  const parsed = parseBrowserMessageBytes(text);
  const items = parsed.payload["items"] as Array<Record<string, unknown>>;
  const action = items.find(
    (item) => item["action_kind"] === "downloads_access_required",
  );
  expect(action?.["attention"]).toBe("required");
  expect(action?.["route_class"]).toBe("downloads_access_required");
  expect(action?.["auth_requirement"]).toBe("unknown");
  const detail = (
    action?.["facts"] as Array<{ label: string; text: string }>
  ).find((fact) => fact.label === "Detail");
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
    payload: {
      request_id: "request-delivery-0001",
      job_id: "job_delivery_0001",
      operation: "confirm_request_exists",
      provider_reference: "TN-42",
    },
  };
  expect(parseBrowserMessage(base).type).toBe("delivery_reconcile_request");

  const missingReference = {
    ...base,
    payload: { ...base.payload, provider_reference: undefined },
  };
  delete (missingReference.payload as Record<string, unknown>)[
    "provider_reference"
  ];
  expect(() => parseBrowserMessage(missingReference)).toThrow(ProtocolError);

  const absentWithReference = {
    ...base,
    payload: { ...base.payload, operation: "confirm_request_absent" },
  };
  expect(() => parseBrowserMessage(absentWithReference)).toThrow(ProtocolError);

  const absentWithoutReference = {
    ...base,
    payload: {
      request_id: base.payload.request_id,
      job_id: base.payload.job_id,
      operation: "confirm_request_absent",
    },
  };
  expect(parseBrowserMessage(absentWithoutReference).type).toBe(
    "delivery_reconcile_request",
  );

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
  const fixtures = readdirSync(join(corpusRoot, "invalid")).filter((name) =>
    name.startsWith("browser-"),
  );
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
    payload: {
      download_id: downloadId,
      filename: "paper.pdf",
      size_bytes: 100,
    },
  });
  const deliveryContext = (downloadId: number) => ({
    protocol: "papio-browser/1",
    type: "delivery_context",
    msg_id: "m_dctx_floor01",
    job_id: "job_dctx_floor01",
    seq: 1,
    payload: {
      download_id: downloadId,
      route: "direct",
      session_evidence: "none",
    },
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
    payload: {
      download_id: 1,
      route: "direct",
      session_evidence: "none",
      page_host: pageHost,
    },
  });
  for (const host of [".abc", "abc.", "a..b"]) {
    expect(() => parseBrowserMessage(frame(host)), host).toThrow(ProtocolError);
  }
  expect(
    parseBrowserMessage(frame("publisher.example.edu")).payload["page_host"],
  ).toBe("publisher.example.edu");
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
    payload: {
      evidence: "warm_verified",
      origin_hint: originHint,
      at: "2026-08-03T12:00:00Z",
    },
  });
  expect(() => parseBrowserMessage(frame("https://EXAMPLE.com"))).toThrow(
    ProtocolError,
  );
  expect(
    parseBrowserMessage(frame("https://resolver.example.edu")).payload[
      "origin_hint"
    ],
  ).toBe("https://resolver.example.edu");
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
    payload: {
      evidence: "warm_verified",
      origin_hint: originHint,
      at: "2026-08-03T12:00:00Z",
    },
  });
  for (const hint of [
    "https://library",
    "https://localhost",
    "https://localhost:8443",
    "https://127.0.0.1",
  ]) {
    expect(parseBrowserMessage(frame(hint)).payload["origin_hint"], hint).toBe(
      hint,
    );
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
  expect(
    parseBrowserMessage(
      frame({
        daemon_version: "0.1.0",
        features: ["browser_handoff"],
      }),
    ).payload,
  ).toEqual({
    daemon_version: "0.1.0",
    features: ["browser_handoff"],
  });
  expect(() => parseBrowserMessage(frame({ features: [null] }))).toThrow(
    ProtocolError,
  );
  expect(() =>
    parseBrowserMessage(frame({ daemon_version: "v".repeat(51) })),
  ).toThrow(ProtocolError);
  expect(
    parseBrowserMessage(
      frame({ resolver_origins: ["https://onesearch.library.example.edu"] }),
    ).payload,
  ).toEqual({ resolver_origins: ["https://onesearch.library.example.edu"] });
  expect(() =>
    parseBrowserMessage(frame({ resolver_origins: [null] })),
  ).toThrow(ProtocolError);
  for (const bad of [
    "http://insecure.example.edu",
    "https://example.edu/path",
    "https://example.edu?x=1",
    "ftp://example.edu",
  ]) {
    expect(() =>
      parseBrowserMessage(frame({ resolver_origins: [bad] })),
    ).toThrow(ProtocolError);
  }
  // role: the acknowledged session's slot state. An absent role means holder —
  // an older daemon only ever acknowledged the session it had just slotted — so
  // the empty-payload case above must keep parsing.
  for (const role of ["holder", "pending"]) {
    expect(parseBrowserMessage(frame({ role })).payload).toEqual({ role });
  }
  for (const bad of [
    "observer",
    "Holder",
    "pending ",
    "primary",
    "",
    null,
    1,
  ]) {
    expect(
      () => parseBrowserMessage(frame({ role: bad })),
      `role ${bad}`,
    ).toThrow(ProtocolError);
  }
});

test("hello_ack tolerates a wider feature set from a mixed-version-ahead daemon", () => {
  // Oracle review finding 7 (dev/scratch/oracle/20260818T202529Z-lifecycle-endtoend/
  // answer3.md lines 139-154): every daemon still emits at most 32 features
  // (internal/browser/bridge.go's `required` literal, pinned exactly by
  // bridge_test.go's TestHelloAckAnnouncesDaemonVersion), but a manually
  // upgraded daemon that has NOT yet had its emit cap raised must not be able
  // to lock a currently-shipped extension out of the whole native session by
  // simply advertising a longer list. Stage 1 of the migration widens only the
  // accept side (HELLO_ACK_FEATURES_ACCEPT_CAP = 64 in protocol.ts) while the
  // daemon's emitted cap is untouched.
  const frame = (features: string[]) => ({
    protocol: "papio-browser/1",
    type: "hello_ack",
    msg_id: "daemon-ack-mixed-version",
    seq: 1,
    payload: { features },
  });

  // A future daemon with the emit cap eventually raised to 33 (feature 33
  // from the review) must still negotiate against today's extension: the
  // extra, unrecognized feature name is simply never matched by any
  // `slices.Contains`-style capability check, not a parse failure.
  const thirtyThree = Array.from(
    { length: 33 },
    (_, i) => `future_feature_${i}`,
  );
  const parsed33 = parseBrowserMessage(frame(thirtyThree));
  expect(parsed33.payload["features"]).toEqual(thirtyThree);

  // The widened bound is itself finite: 64 entries parse, 65 fail closed,
  // exactly mirroring the old 32/33 boundary one order of magnitude up.
  const sixtyFour = Array.from({ length: 64 }, (_, i) => `wide_feature_${i}`);
  expect(
    parseBrowserMessage(frame(sixtyFour)).payload["features"],
  ).toHaveLength(64);
  const sixtyFive = Array.from({ length: 65 }, (_, i) => `wide_feature_${i}`);
  expect(() => parseBrowserMessage(frame(sixtyFive))).toThrow(ProtocolError);

  // An old daemon that never learned about the widened bound — advertising
  // only its small legacy list — must keep negotiating exactly as before.
  const legacy = ["browser_handoff", "session_roles_v1"];
  expect(parseBrowserMessage(frame(legacy)).payload["features"]).toEqual(
    legacy,
  );
});

test("pdf_grab_result.reason is a closed classifier confined to the two refusals", () => {
  // The popup switches on reason to pick its own copy, so an unclassified value
  // must never reach it, and reason must stay confined to the refusal outcomes —
  // anywhere else it would imply a failure that did not happen. Absent on a
  // refusal stays legal: an older daemon classified nothing.
  const requestID = "pdf-grab-request-frame";
  const grabID = "grab_0123456789abcdef01234567";
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "pdf_grab_result",
    msg_id: "pdf-grab-result-reason",
    seq: 31,
    payload,
  });

  // session_elsewhere is deliberately absent: a grab is user-initiated and
  // self-routing, so holdership never refuses one.
  expect(PDF_GRAB_REFUSAL_REASONS).toEqual([
    "no_session",
    "extension_outdated",
    "daemon_unsupported",
    "busy",
    "not_configured",
    "adoption_unhealthy",
    "tab_unusable",
    "internal",
  ]);
  expect(BROWSER_SESSION_ROLES).toEqual(["holder", "pending"]);

  for (const outcome of ["unavailable", "not_supported"]) {
    for (const reason of PDF_GRAB_REFUSAL_REASONS) {
      expect(
        parseBrowserMessage(
          frame({ request_id: requestID, grab_id: grabID, outcome, reason }),
        ).payload["reason"],
        `${outcome}/${reason}`,
      ).toBe(reason);
    }
    expect(
      parseBrowserMessage(frame({ request_id: requestID, outcome })).payload,
    ).toEqual({ request_id: requestID, outcome });
  }

  for (const bad of [
    "session_elsewhere",
    "Busy",
    "unknown",
    "busy ",
    "",
    null,
    1,
  ]) {
    expect(
      () =>
        parseBrowserMessage(
          frame({ request_id: requestID, outcome: "unavailable", reason: bad }),
        ),
      `reason ${bad}`,
    ).toThrow(ProtocolError);
  }

  const nonRefusals: Array<Record<string, unknown>> = [
    {
      request_id: requestID,
      grab_id: grabID,
      outcome: "steering",
      steering_path: `papio/grabs/${grabID}/`,
      reason: "busy",
    },
    {
      request_id: requestID,
      grab_id: grabID,
      outcome: "existing",
      reason: "busy",
    },
    { grab_id: grabID, outcome: "job_created", reason: "internal" },
    { grab_id: grabID, outcome: "already_owned", reason: "internal" },
    { grab_id: grabID, outcome: "needs_identifier", reason: "internal" },
    { grab_id: grabID, outcome: "failed_validation", reason: "internal" },
    { grab_id: grabID, outcome: "abandoned", reason: "internal" },
  ];
  for (const payload of nonRefusals) {
    expect(
      () => parseBrowserMessage(frame(payload)),
      `reason on ${payload["outcome"]}`,
    ).toThrow(ProtocolError);
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
  expect(
    parseBrowserMessage(
      frame({
        extension_version: "0.14.0",
        features: ["institutional_materialization_v1", "future_capability"],
      }),
    ).payload["features"],
  ).toEqual(["institutional_materialization_v1", "future_capability"]);
  expect(
    parseBrowserMessage(frame({ extension_version: "0.14.0" })).payload,
  ).toEqual({
    extension_version: "0.14.0",
  });
  expect(() =>
    parseBrowserMessage(frame({ extension_version: "0.14.0", unknown: true })),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame({
        extension_version: "0.14.0",
        features: ["future_capability", "future_capability"],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame({ extension_version: "0.14.0", features: ["Future"] }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame({
        extension_version: "0.14.0",
        features: Array.from({ length: 33 }, (_, i) => `future_${i}`),
      }),
    ),
  ).toThrow(ProtocolError);
});

test("page_acquire messages parse strictly", () => {
  const frame = (
    type: "page_acquire" | "page_acquire_ack",
    payload: Record<string, unknown>,
  ) => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "page-acquire-001",
    seq: 1,
    payload,
  });

  expect(
    parseBrowserMessage(
      frame("page_acquire", {
        url: "https://publisher.example.edu/article/42",
        doi: "10.1000/example.42",
        title: "An Example Paper",
        source: "popup",
      }),
    ).payload,
  ).toEqual({
    url: "https://publisher.example.edu/article/42",
    doi: "10.1000/example.42",
    title: "An Example Paper",
    source: "popup",
  });
  expect(
    parseBrowserMessage(
      frame("page_acquire_ack", {
        job_id: "job_page_acquire_001",
        duplicate: true,
      }),
    ).payload,
  ).toEqual({ job_id: "job_page_acquire_001", duplicate: true });
  expect(
    parseBrowserMessage(
      frame("page_acquire_ack", {
        error: "page has no DOI",
      }),
    ).payload,
  ).toEqual({ error: "page has no DOI" });

  for (const payload of [
    {},
    { url: "ftp://publisher.example.edu/article/42" },
    { url: "https://publisher.example.edu/article/42", doi: "d".repeat(513) },
    { url: null },
    { url: "https://publisher.example.edu/article/42", unexpected: true },
    { url: "https://publisher.example.edu/article/\0" },
    {
      url: "https://publisher.example.edu/article/42",
      doi: "10.1000/\0example",
    },
    {
      url: "https://publisher.example.edu/article/42",
      title: "Example\0 Paper",
    },
    { url: "https://publisher.example.edu/article/42", source: "pop\0up" },
  ]) {
    expect(() => parseBrowserMessage(frame("page_acquire", payload))).toThrow(
      ProtocolError,
    );
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
    expect(() =>
      parseBrowserMessage(frame("page_acquire_ack", payload)),
    ).toThrow(ProtocolError);
  }
});

test("page_capture messages parse strictly before echoed frames reach the inbound ignore path", () => {
  // A valid echo must pass parsing so onInbound reaches its extension-only
  // default instead of disconnecting the native session before it can ignore it.
  const text = readFileSync(
    join(corpusRoot, "valid", "browser-page-capture.json"),
    "utf8",
  );
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
    expect(() => parseBrowserMessage({ ...frame, payload: invalid })).toThrow(
      ProtocolError,
    );
  }

  // Optional: an unsolicited capture omits it, a requested one echoes the id
  // it answers. The daemon binds on that presence, so both shapes must parse
  // (papio-85a7420f4cd2564f).
  expect(
    parseBrowserMessage({
      ...frame,
      payload: { ...payload, request_id: "DRA6SOdBEB1ZgMIRV8qfqQ" },
    }).type,
  ).toBe("page_capture");
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
  expect(() =>
    parseBrowserMessageBytes(`{"protocol":"papio-browser/1"}${pad}`),
  ).toThrow(/exceeds cap/);
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
    readFileSync(
      join(corpusRoot, "valid", "browser-page-bulk-status-request.json"),
      "utf8",
    ),
  );
  expect(statusRequest.type).toBe("page_bulk_status_request");
  expect((statusRequest.payload["identifiers"] as unknown[]).length).toBe(4);
  expect(statusRequest.payload["rendered_record_count_hint"]).toBe(12);

  const statusResult = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-page-bulk-status-result.json"),
      "utf8",
    ),
  );
  expect(statusResult.type).toBe("page_bulk_status_result");
  const items = statusResult.payload["items"] as Array<Record<string, unknown>>;
  expect(items).toHaveLength(4);
  expect(items[2]).toEqual({
    local_id: "row-3",
    canonical_key: "work-key-queued-1",
    status: "queued",
    ownership_complete: false,
    job_id: "job_bulk_00001",
  });
  expect(items[3]).toEqual({
    local_id: "row-4",
    status: "invalid",
    ownership_complete: false,
  });

  const submitRequest = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-page-bulk-submit-request.json"),
      "utf8",
    ),
  );
  expect(submitRequest.type).toBe("page_bulk_submit_request");
  expect(submitRequest.payload).toEqual({
    request_id: "request-bulk-0002",
    scan_id: "scan-bulk-0001",
    canonical_keys: [
      "work-key-doi-10-1000-example-42",
      "work-key-pmid-12345678",
    ],
    source: {
      kind: "browser_page",
      origin: "https://scholar.example.edu",
      detector: "generic-identifiers/1",
    },
  });

  const submitResult = parseBrowserMessageBytes(
    readFileSync(
      join(corpusRoot, "valid", "browser-page-bulk-submit-result.json"),
      "utf8",
    ),
  );
  expect(submitResult.type).toBe("page_bulk_submit_result");
  expect(submitResult.payload).toEqual({
    request_id: "request-bulk-0002",
    scan_id: "scan-bulk-0001",
    submitted: 1,
    joined: 1,
    already_owned: 0,
    invalid: 0,
    batch_id: "batch_bulk_00001",
  });

  for (const name of [
    "browser-page-bulk-status-request-too-many-identifiers.json",
    "browser-page-bulk-status-request-bad-kind.json",
    "browser-page-bulk-status-request-negative-hint.json",
    "browser-page-bulk-submit-request-too-many-keys.json",
    "browser-page-bulk-submit-request-origin-with-path.json",
    "browser-page-bulk-submit-request-origin-uppercase-host.json",
  ]) {
    expect(
      () =>
        parseBrowserMessageBytes(
          readFileSync(join(corpusRoot, "invalid", name), "utf8"),
        ),
      name,
    ).toThrow(ProtocolError);
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
      source: {
        kind: "browser_page",
        origin,
        detector: "generic-identifiers/1",
      },
    },
  });
  expect(() =>
    parseBrowserMessage(frame("https://Scholar.Example.EDU")),
  ).toThrow(ProtocolError);
  expect(parseBrowserMessage(frame("https://scholar.example.edu")).type).toBe(
    "page_bulk_submit_request",
  );
});

test("page_bulk_status_request rejects malformed identifiers", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_status_request",
    msg_id: "page-bulk-status-req-01",
    seq: 1,
    payload,
  });
  const validIdentifier = {
    local_id: "row-1",
    kind: "doi",
    value: "10.1000/example.42",
  };
  const openalexIdentifier = {
    local_id: "row-2",
    kind: "openalex",
    value: "W2741809807",
  };

  expect(
    parseBrowserMessage(
      frame({
        request_id: "request-bulk-0001",
        scan_id: "scan-bulk-0001",
        identifiers: [validIdentifier, openalexIdentifier],
      }),
    ).payload,
  ).toEqual({
    request_id: "request-bulk-0001",
    scan_id: "scan-bulk-0001",
    identifiers: [validIdentifier, openalexIdentifier],
  });

  for (const identifiers of [
    [],
    Array.from({ length: 201 }, (_, i) => ({
      local_id: `row-${i}`,
      kind: "doi",
      value: `10.1000/x${i}`,
    })),
    [{ local_id: "row-1", kind: "isbn", value: "9780000000002" }],
    [validIdentifier, { ...validIdentifier }],
    [{ local_id: "row-1", kind: "doi", value: "" }],
    [{ local_id: "", kind: "doi", value: "10.1000/example.42" }],
  ]) {
    expect(() =>
      parseBrowserMessage(
        frame({
          request_id: "request-bulk-0001",
          scan_id: "scan-bulk-0001",
          identifiers,
        }),
      ),
    ).toThrow(ProtocolError);
  }
  expect(() =>
    parseBrowserMessage(
      frame({
        request_id: "request-bulk-0001",
        scan_id: "short",
        identifiers: [validIdentifier],
      }),
    ),
  ).toThrow(ProtocolError);
});

test("page_bulk_status_result enforces the closed status vocabulary and canonical_key/job_id invariants", () => {
  const frame = (payload: Record<string, unknown>) => ({
    protocol: "papio-browser/1",
    type: "page_bulk_status_result",
    msg_id: "page-bulk-status-res-01",
    seq: 1,
    payload,
  });
  const base = {
    request_id: "request-bulk-0001",
    scan_id: "scan-bulk-0001",
    truncated: false,
  };

  expect(
    parseBrowserMessage(
      frame({
        ...base,
        items: [
          { local_id: "row-1", status: "invalid", ownership_complete: false },
        ],
      }),
    ).payload,
  ).toEqual({
    ...base,
    items: [
      { local_id: "row-1", status: "invalid", ownership_complete: false },
    ],
  });

  for (const items of [
    [
      {
        local_id: "row-1",
        canonical_key: "wk1",
        status: "invalid",
        ownership_complete: false,
      },
    ],
    [{ local_id: "row-1", status: "eligible", ownership_complete: false }],
    [
      {
        local_id: "row-1",
        canonical_key: "wk1",
        status: "unexpected",
        ownership_complete: false,
      },
    ],
    [
      {
        local_id: "row-1",
        canonical_key: "wk1",
        status: "eligible",
        ownership_complete: false,
        job_id: "job_bulk_00001",
      },
    ],
    [
      {
        local_id: "row-1",
        canonical_key: "wk1",
        status: "queued",
        ownership_complete: false,
        job_id: "short",
      },
    ],
    Array.from({ length: 201 }, (_, i) => ({
      local_id: `row-${i}`,
      canonical_key: "wk",
      status: "eligible",
      ownership_complete: false,
    })),
  ]) {
    expect(() => parseBrowserMessage(frame({ ...base, items }))).toThrow(
      ProtocolError,
    );
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
  const validSource = {
    kind: "browser_page",
    origin: "https://scholar.example.edu",
    detector: "generic-identifiers/1",
  };

  expect(
    parseBrowserMessage(
      frame({
        request_id: "request-bulk-0002",
        scan_id: "scan-bulk-0001",
        canonical_keys: ["wk1"],
        source: validSource,
      }),
    ).payload,
  ).toEqual({
    request_id: "request-bulk-0002",
    scan_id: "scan-bulk-0001",
    canonical_keys: ["wk1"],
    source: validSource,
  });

  for (const payload of [
    { canonical_keys: [], source: validSource },
    {
      canonical_keys: Array.from({ length: 51 }, (_, i) => `wk${i}`),
      source: validSource,
    },
    { canonical_keys: ["wk1", "wk1"], source: validSource },
    {
      canonical_keys: ["wk1"],
      source: { ...validSource, origin: "https://scholar.example.edu/path" },
    },
    {
      canonical_keys: ["wk1"],
      source: { ...validSource, origin: "https://scholar.example.edu?x=1" },
    },
    {
      canonical_keys: ["wk1"],
      source: { ...validSource, origin: "http://scholar.example.edu" },
    },
    { canonical_keys: ["wk1"], source: { ...validSource, detector: "" } },
    { canonical_keys: ["wk1"], source: { ...validSource, kind: "extension" } },
  ]) {
    expect(() =>
      parseBrowserMessage(
        frame({
          request_id: "request-bulk-0002",
          scan_id: "scan-bulk-0001",
          ...payload,
        }),
      ),
    ).toThrow(ProtocolError);
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
  const base = {
    request_id: "request-bulk-0003",
    scan_id: "scan-bulk-0001",
    submitted: 1,
    joined: 0,
    already_owned: 0,
    invalid: 0,
  };

  expect(
    parseBrowserMessage(frame({ ...base, batch_id: "batch_bulk_00001" }))
      .payload,
  ).toEqual({
    ...base,
    batch_id: "batch_bulk_00001",
  });
  expect(() =>
    parseBrowserMessage(
      frame({ ...base, submitted: -1, batch_id: "batch_bulk_00001" }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(frame({ ...base, batch_id: "short" })),
  ).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(frame(base))).toThrow(ProtocolError);
});

test("institutional materialization families are strict, bounded, and job-scoped", () => {
  type InstitutionalMessageType = Extract<
    BrowserMessageType,
    `institutional_${string}`
  >;
  const envelope = (
    type: InstitutionalMessageType,
    payload: Record<string, unknown>,
    job = "job-inst-001",
  ) => ({
    protocol: "papio-browser/1",
    type,
    msg_id: "msg-inst-001",
    seq: 1,
    ...(job === "" ? {} : { job_id: job }),
    payload: { request_id: "request-inst-001", ...payload },
  });
  const ids = {
    candidate_id: "candidate-001",
    claim_id: "claim-001",
    binding_id: "binding-001",
  };
  const valid: Array<
    readonly [InstitutionalMessageType, Record<string, unknown>, string]
  > = [
    [
      "institutional_claim_request",
      { candidate_id: ids.candidate_id, materialization_kind: "browser_tab" },
      "job-inst-001",
    ],
    [
      "institutional_claim_response",
      {
        outcome: "claimed",
        ...ids,
        browser_holder_generation: 1,
        lease_until: "2026-08-11T12:00:00Z",
      },
      "job-inst-001",
    ],
    [
      "institutional_bind_request",
      { claim_id: ids.claim_id, binding_id: ids.binding_id, tab_id: 7 },
      "job-inst-001",
    ],
    [
      "institutional_bind_response",
      { outcome: "bound", claim_id: ids.claim_id, binding_id: ids.binding_id },
      "job-inst-001",
    ],
    [
      "institutional_route_request",
      {
        claim_id: ids.claim_id,
        binding_id: ids.binding_id,
        expected_effect_ordinal: 0,
        institutional_request_id: "institutional-request-001",
      },
      "job-inst-001",
    ],
    [
      "institutional_route_response",
      {
        outcome: "issued",
        claim_id: ids.claim_id,
        binding_id: ids.binding_id,
        route_issuance_ordinal: 1,
        effect_ordinal: 1,
        institutional_request_id: "institutional-request-001",
        url: "https://resolver.example.edu/open",
      },
      "job-inst-001",
    ],
    [
      "institutional_navigated_request",
      {
        claim_id: ids.claim_id,
        binding_id: ids.binding_id,
        route_issuance_ordinal: 1,
        effect_ordinal: 1,
        institutional_request_id: "institutional-request-001",
        tab_id: 7,
      },
      "job-inst-001",
    ],
    [
      "institutional_navigated_response",
      {
        outcome: "acknowledged",
        claim_id: ids.claim_id,
        binding_id: ids.binding_id,
      },
      "job-inst-001",
    ],
    [
      "institutional_reconcile_request",
      {
        bindings: [{ binding_id: ids.binding_id, tab_id: 7 }],
      },
      "",
    ],
    [
      "institutional_reconcile_response",
      {
        outcome: "reconciled",
        claims: [
          {
            claim_id: ids.claim_id,
            binding_id: ids.binding_id,
            candidate_id: ids.candidate_id,
            phase: "bound",
            tab_id: 7,
          },
        ],
      },
      "",
    ],
  ];
  for (const [type, payload, job] of valid) {
    expect(parseBrowserMessage(envelope(type, payload, job)).type).toBe(type);
  }

  const disabled = [
    [
      "institutional_claim_response",
      { outcome: "feature_disabled", detail: "dark" },
    ],
    [
      "institutional_bind_response",
      { outcome: "feature_disabled", detail: "dark" },
    ],
    [
      "institutional_route_response",
      { outcome: "feature_disabled", detail: "dark" },
    ],
    [
      "institutional_navigated_response",
      { outcome: "feature_disabled", detail: "dark" },
    ],
    [
      "institutional_reconcile_response",
      { outcome: "feature_disabled", detail: "dark" },
    ],
  ] as const;
  for (const [type, payload] of disabled) {
    const job = type.startsWith("institutional_reconcile")
      ? ""
      : "job-inst-001";
    expect(parseBrowserMessage(envelope(type, payload, job)).type).toBe(type);
  }

  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_claim_request",
        { candidate_id: ids.candidate_id, materialization_kind: "browser_tab" },
        "",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_reconcile_request",
        { bindings: [{ binding_id: ids.binding_id, tab_id: 1 }], extra: true },
        "",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_route_response",
        {
          outcome: "stale",
          detail: "late",
          url: "https://resolver.example.edu/x",
        },
        "",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_route_response",
        {
          outcome: "issued",
          claim_id: ids.claim_id,
          binding_id: ids.binding_id,
          route_issuance_ordinal: 0,
        },
        "",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_bind_request",
        {
          claim_id: ids.claim_id,
          binding_id: ids.binding_id,
          tab_id: Number.MAX_SAFE_INTEGER + 1,
        },
        "job-inst-001",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_reconcile_request",
        {
          bindings: Array.from({ length: 33 }, (_, i) => ({
            binding_id: `binding-${String(i).padStart(3, "0")}`,
            tab_id: i,
          })),
        },
        "",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_claim_request",
        { candidate_id: "x".repeat(129), materialization_kind: "browser_tab" },
        "job-inst-001",
      ),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      envelope(
        "institutional_bind_request",
        { claim_id: ids.claim_id, binding_id: ids.binding_id, tab_id: -1 },
        "job-inst-001",
      ),
    ),
  ).toThrow(ProtocolError);
  const missingRequestID = envelope("institutional_claim_request", {
    candidate_id: ids.candidate_id,
    materialization_kind: "browser_tab",
  });
  delete (missingRequestID.payload as Record<string, unknown>).request_id;
  expect(() => parseBrowserMessage(missingRequestID)).toThrow(ProtocolError);
});

test("pre-permit institutional navigation is explicit cleanup-only wire", () => {
  const frame = {
    protocol: "papio-browser/1",
    type: "institutional_navigated_request",
    msg_id: "legacy-nav-001",
    seq: 1,
    job_id: "job-inst-legacy",
    payload: {
      request_id: "request-inst-legacy",
      claim_id: "claim-inst-legacy",
      binding_id: "binding-inst-legacy",
      route_issuance_ordinal: 2,
      tab_id: 7,
    },
  };
  expect(() => parseBrowserMessage(frame)).toThrow(ProtocolError);
  expect(
    parseBrowserMessageWithLegacyInstitutionalNavigation(frame, true).payload,
  ).toEqual(frame.payload);
  expect(() =>
    parseBrowserMessageWithLegacyInstitutionalNavigation(
      {
        ...frame,
        payload: { ...frame.payload, effect_ordinal: 1 },
      },
      true,
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessageWithLegacyInstitutionalNavigation(
      {
        ...frame,
        payload: {
          ...frame.payload,
          effect_ordinal: 1,
          institutional_request_id: "institutional-request-current",
        },
      },
      true,
    ),
  ).toThrow(ProtocolError);
});

test("institutional_candidate_offer is URL-free, browser-tab-only, job-scoped, and bounded", () => {
  const envelope = (payload: Record<string, unknown>, job?: string) => ({
    protocol: "papio-browser/1",
    type: "institutional_candidate_offer",
    msg_id: "candidate-offer-001",
    seq: 1,
    ...(job === undefined
      ? { job_id: "job-inst-001" }
      : job === ""
        ? {}
        : { job_id: job }),
    payload,
  });
  const valid = {
    candidate_id: "candidate-001",
    materialization_kind: "browser_tab",
    expires_at: "2026-08-11T12:00:00Z",
    provider_hosts: ["resolver.example.edu"],
    expected: { doi: "10.1234/example", title: "Example work" },
    access_mode: "delegated",
    login_entity_id: "https://idp.example/entity",
    proquest_account_id: "12345",
    requires_auth: true,
    drive_attempt_id: "attempt-001",
    drive_ordinal: 0,
    drive_strategy: "generic",
    drive_revision: "rev-1",
  };
  expect(parseBrowserMessage(envelope(valid)).payload).toEqual(valid);
  expect(parseBrowserMessage(envelope(valid, "job-inst-001")).type).toBe(
    "institutional_candidate_offer",
  );

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
  expect(() =>
    parseBrowserMessage(envelope({ ...valid, extra: true })),
  ).toThrow(ProtocolError);

  const oversized = JSON.stringify(
    envelope({ ...valid, extra: "x".repeat(MAX_BROWSER_MESSAGE_BYTES) }),
  );
  expect(() => parseBrowserMessageBytes(oversized)).toThrow(/exceeds cap/);
});
const protocolFrame = (
  type: BrowserMessageType,
  payload: Record<string, unknown>,
) => ({
  protocol: "papio-browser/1",
  type,
  msg_id: "new-frame-msg-001",
  seq: 1,
  payload,
});

const pulseEpisode = (
  key = "episode-001",
  since = "2026-01-01T00:00:00Z",
  count = 1,
) => ({
  episode_key: key,
  cause_kind: "execution_lease_overdue",
  public_label: "Execution lease overdue",
  since,
  count,
});

const pulsePayload = (): Record<string, unknown> => ({
  request_id: "request-pulse-001",
  schema: 1,
  generated_at: "2026-01-01T00:00:00Z",
  nonterminal_total: 10,
  projection_complete: true,
  in_flight: 2,
  continuing: 4,
  scheduled: 1,
  waiting_required: 2,
  stalled: 1,
  effect_capacity: { busy: 1, limit: 2, waiting: 1 },
  effect_permits: [
    { permit_id: "permit-001", status: "held", since: "2026-01-01T00:00:00Z" },
  ],
  human_surface_capacity: { busy: 1, limit: 2, waiting_claims: 2 },
  last_forward_at: "2026-01-01T00:00:01Z",
  stall_episodes: [pulseEpisode()],
  stall_episodes_truncated: false,
  last_finished_at: "2025-12-31T23:59:59Z",
  next_action: {
    at: "2026-01-01T00:01:00Z",
    kind: "retry",
    source: "OpenAlex",
    count: 2,
  },
  gates: [
    {
      kind: "source_budget",
      source: "OpenAlex",
      until: "2026-01-01T01:00:00Z",
      count: 2,
    },
  ],
  latest_batch: {
    batch_id: "batch-001",
    label: "January browser submission",
    started_at: "2026-01-01T00:00:00Z",
    settled_at: "2026-01-01T00:02:00Z",
    membership: "complete",
    projection_complete: true,
    total: 8,
    settled: 3,
    nonterminal_total: 5,
    in_flight: 1,
    continuing: 1,
    scheduled: 1,
    waiting_required: 1,
    stalled: 1,
    unavailable: 1,
  },
});

const countsV3 = (): Record<string, unknown> => ({
  pending_total: 1,
  watch_hits: 0,
  actions: 1,
  retractions: 0,
  jobs_working: 0,
  jobs_needs_review: 0,
  failure_groups_7d: 0,
  turns_required: 1,
  turns_working: 0,
  family_breakdown_complete: true,
  family_runs: [
    {
      run_key: "run-001",
      first_rank: 0,
      route_class: "manual_download",
      action_kind: "manual_download",
      next_actor: "researcher",
      guidance_variant: "manual_download",
      operation_variant: "open_and_dismiss",
      count: 1,
    },
  ],
  required_turns_complete: true,
  required_turns: [
    {
      item_id: "action-001",
      item_kind: "human_action",
      action_id: 1,
      job_id: "job-0001",
      route_class: "manual_download",
      gate_claim_id: "gate-001",
      dependent_jobs: 2,
    },
  ],
});

const activityEntry = (seq: number): Record<string, unknown> => ({
  seq,
  at: "2026-01-01T00:00:00Z",
  job_id: "job-activity-001",
  kind: "watch.alert",
  text: "new",
  title: "Watch hit",
});

const bulkSource = {
  kind: "browser_page",
  origin: "https://scholar.example.edu:8443",
  detector: "generic-identifiers/1",
};

test("new protocol frames parse with all optional fields and round-trip", () => {
  const frames: Array<[BrowserMessageType, Record<string, unknown>]> = [
    [
      "surface_presence",
      {
        request_id: "request-presence-001",
        instance_id: "instance-001",
        surface: "popup",
        focused: true,
        at: "2026-01-01T00:00:00Z",
      },
    ],
    [
      "surface_presence_ack",
      { request_id: "request-presence-001", accepted: true },
    ],
    [
      "work_pulse_request",
      { request_id: "request-pulse-001", schema_versions: [1] },
    ],
    ["work_pulse_response", pulsePayload()],
    [
      "activity_page_request",
      {
        request_id: "request-activity-page-001",
        limit: 50,
        before_seq: "41",
        seen_through_seq: "40",
      },
    ],
    [
      "activity_page_response",
      {
        request_id: "request-activity-page-001",
        generated_at: "2026-01-01T00:00:00Z",
        entries: [activityEntry(42)],
        has_more: true,
        cursor: "43",
        latest_seq: 43,
        new_count_since: 1,
        gap: false,
      },
    ],
    [
      "page_bulk_submit_v2_request",
      {
        request_id: "request-bulk-v2-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        source: bulkSource,
        cohort_total: 1,
        chunk_index: 0,
        final_chunk: true,
        canonical_keys: ["work-key-1"],
      },
    ],
    [
      "page_bulk_submit_v2_result",
      {
        request_id: "request-bulk-v2-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        chunk_index: 0,
        final_chunk: true,
        batch_id: "batch-001",
        membership: "complete",
        cohort_total: 1,
        persisted_members: 1,
        submitted: 1,
        joined: 0,
        already_owned: 0,
        invalid: 0,
      },
    ],
  ];
  for (const [type, payload] of frames) {
    const frame = protocolFrame(type, payload);
    const parsed = parseBrowserMessage(frame);
    expect(parsed.type).toBe(type);
    expect(parsed.payload).toEqual(payload);
    expect(JSON.parse(JSON.stringify(parsed.payload))).toEqual(payload);
  }
});

test("new protocol frames reject unknown and null fields", () => {
  const payloads: Array<[BrowserMessageType, Record<string, unknown>]> = [
    [
      "surface_presence",
      {
        request_id: "request-001",
        instance_id: "instance-001",
        surface: "popup",
        focused: true,
        at: "2026-01-01T00:00:00Z",
      },
    ],
    ["surface_presence_ack", { request_id: "request-001", accepted: true }],
    ["work_pulse_request", { request_id: "request-001", schema_versions: [1] }],
    ["work_pulse_response", pulsePayload()],
    ["activity_page_request", { request_id: "request-001" }],
    [
      "activity_page_response",
      {
        request_id: "request-001",
        generated_at: "2026-01-01T00:00:00Z",
        entries: [],
        has_more: false,
        latest_seq: 0,
      },
    ],
    [
      "page_bulk_submit_v2_request",
      {
        request_id: "request-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        source: bulkSource,
        cohort_total: 1,
        chunk_index: 0,
        final_chunk: true,
        canonical_keys: ["key"],
      },
    ],
    [
      "page_bulk_submit_v2_result",
      {
        request_id: "request-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        chunk_index: 0,
        final_chunk: true,
        batch_id: "batch-001",
        membership: "complete",
        persisted_members: 1,
        submitted: 1,
        joined: 0,
        already_owned: 0,
        invalid: 0,
      },
    ],
  ];
  for (const [type, payload] of payloads) {
    expect(() =>
      parseBrowserMessage(
        protocolFrame(type, { ...payload, unexpected: true }),
      ),
    ).toThrow(ProtocolError);
    const key = Object.keys(payload)[0]!;
    expect(() =>
      parseBrowserMessage(protocolFrame(type, { ...payload, [key]: null })),
    ).toThrow(ProtocolError);
  }
});

test("new protocol closed vocabularies reject unknown values", () => {
  expect(() =>
    parseBrowserMessage(
      protocolFrame("surface_presence", {
        request_id: "request-001",
        instance_id: "instance-001",
        surface: "tab",
        focused: true,
        at: "2026-01-01T00:00:00Z",
      }),
    ),
  ).toThrow(ProtocolError);
  for (const [field, value] of [
    ["cause_kind", "other"],
    ["next_action.kind", "other"],
    ["gates[].kind", "other"],
    ["latest_batch.membership", "other"],
  ] as const) {
    const payload = pulsePayload();
    if (field === "cause_kind")
      (payload.stall_episodes as Array<Record<string, unknown>>)[0]![
        "cause_kind"
      ] = value;
    if (field === "next_action.kind")
      (payload.next_action as Record<string, unknown>)["kind"] = value;
    if (field === "gates[].kind")
      (payload.gates as Array<Record<string, unknown>>)[0]!["kind"] = value;
    if (field === "latest_batch.membership")
      (payload.latest_batch as Record<string, unknown>)["membership"] = value;
    expect(() =>
      parseBrowserMessage(protocolFrame("work_pulse_response", payload)),
    ).toThrow(ProtocolError);
  }
  expect(() =>
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_result", {
        request_id: "request-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        chunk_index: 0,
        final_chunk: true,
        batch_id: "batch-001",
        membership: "other",
        persisted_members: 1,
        submitted: 1,
        joined: 0,
        already_owned: 0,
        invalid: 0,
      }),
    ),
  ).toThrow(ProtocolError);
});

test("new protocol IDs, timestamps, strings, and public labels are bounded", () => {
  expect(() =>
    parseBrowserMessage(
      protocolFrame("surface_presence", {
        request_id: "bad id",
        instance_id: "instance-001",
        surface: "popup",
        focused: true,
        at: "2026-01-01T00:00:00Z",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("surface_presence", {
        request_id: "request-001",
        instance_id: "instance-001",
        surface: "popup",
        focused: true,
        at: "not-time",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("work_pulse_response", {
        ...pulsePayload(),
        request_id: "x".repeat(65),
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("work_pulse_response", {
        ...pulsePayload(),
        stall_episodes: [{ ...pulseEpisode(), public_label: "x".repeat(65) }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("work_pulse_response", {
        ...pulsePayload(),
        stall_episodes: [{ ...pulseEpisode(), public_label: "bad\nlabel" }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("work_pulse_response", {
        ...pulsePayload(),
        effect_permits: [
          {
            permit_id: "x".repeat(65),
            status: "held",
            since: "2026-01-01T00:00:00Z",
          },
        ],
      }),
    ),
  ).toThrow(ProtocolError);
});

test("work pulse bounds, algebra, capacities, episodes, and gates match the wire contract", () => {
  const withPulse = (mutate: (payload: Record<string, unknown>) => void) => {
    const payload = pulsePayload();
    mutate(payload);
    expect(() =>
      parseBrowserMessage(protocolFrame("work_pulse_response", payload)),
    ).toThrow(ProtocolError);
  };
  withPulse((p) => {
    p.nonterminal_total = 11;
  });
  withPulse((p) => {
    (p.effect_capacity as Record<string, unknown>).busy = 3;
  });
  withPulse((p) => {
    p.stalled = 1;
    p.stall_episodes = [];
  });
  withPulse((p) => {
    p.stall_episodes = [pulseEpisode("a", "2026-01-01T00:00:00Z", 2)];
  });
  const validTruncated = pulsePayload();
  validTruncated.stall_episodes_truncated = true;
  validTruncated.stall_episodes = [
    pulseEpisode("a", "2026-01-01T00:00:00Z", 1),
  ];
  validTruncated.stalled = 2;
  validTruncated.nonterminal_total = 11;
  expect(
    parseBrowserMessage(protocolFrame("work_pulse_response", validTruncated))
      .payload,
  ).toBeDefined();
  withPulse((p) => {
    p.stall_episodes_truncated = true;
    p.stall_episodes = [pulseEpisode("a", "2026-01-01T00:00:00Z", 2)];
  });
  withPulse((p) => {
    p.stall_episodes = Array.from({ length: 17 }, (_, i) =>
      pulseEpisode(
        `episode-${String(i).padStart(3, "0")}`,
        `2026-01-01T00:${String(i).padStart(2, "0")}:00Z`,
      ),
    );
  });
  withPulse((p) => {
    p.stall_episodes = [
      pulseEpisode("duplicate"),
      pulseEpisode("duplicate", "2026-01-01T00:01:00Z"),
    ];
  });
  withPulse((p) => {
    p.stall_episodes = [
      pulseEpisode("z", "2026-01-01T00:01:00Z"),
      pulseEpisode("a", "2026-01-01T00:00:00Z"),
    ];
  });
  withPulse((p) => {
    p.effect_permits = Array.from({ length: 5 }, (_, i) => ({
      permit_id: `permit-${i}`,
      status: "held",
      since: "2026-01-01T00:00:00Z",
    }));
  });
  withPulse((p) => {
    p.effect_permits = [
      {
        permit_id: "permit-1",
        status: "settled",
        since: "2026-01-01T00:00:00Z",
      },
    ];
  });
  withPulse((p) => {
    p.effect_permits = [
      { permit_id: "permit-1", status: "held", since: "2026-01-01T00:00:00Z" },
      { permit_id: "permit-1", status: "held", since: "2026-01-01T00:00:00Z" },
    ];
  });
  withPulse((p) => {
    p.gates = Array.from({ length: 17 }, (_, i) => ({
      kind: "source_budget",
      source: `source-${i}`,
      until: "2026-01-01T01:00:00Z",
      count: 1,
    }));
  });
  withPulse((p) => {
    p.gates = [
      {
        kind: "source_budget",
        source: "OpenAlex",
        until: "2026-01-01T01:00:00Z",
        count: 1,
      },
      {
        kind: "source_budget",
        source: "OpenAlex",
        until: "2026-01-01T02:00:00Z",
        count: 1,
      },
    ];
  });
  withPulse((p) => {
    p.stalled = 1_000_001;
  });
  withPulse((p) => {
    (p.stall_episodes as Array<Record<string, unknown>>)[0]!["count"] = 0;
  });
  withPulse((p) => {
    p.next_action = { at: "2026-01-01T00:01:00Z", kind: "other" };
  });
});

test("activity page bounds, entries, cursor algebra, and gap semantics match the wire contract", () => {
  const base = {
    request_id: "request-activity-page-001",
    generated_at: "2026-01-01T00:00:00Z",
    entries: [activityEntry(1)],
    has_more: false,
    latest_seq: 1,
  };
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        entries: Array.from({ length: 51 }, (_, i) => activityEntry(i + 1)),
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        entries: [{ ...activityEntry(1), seq: -1 }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        entries: [{ ...activityEntry(1), at: "not-time" }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        entries: [{ ...activityEntry(1), kind: "" }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        entries: [{ ...activityEntry(1), text: "" }],
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", { ...base, has_more: true }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        has_more: false,
        cursor: "2",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_response", {
        ...base,
        gap: true,
        new_count_since: 1,
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("activity_page_request", {
        request_id: "request-001",
        limit: 51,
      }),
    ),
  ).toThrow(ProtocolError);
});

test("page bulk v2 enforces cohort, chunk, canonical-key, and result-count bounds", () => {
  const request = (changes: Record<string, unknown> = {}) =>
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_request", {
        request_id: "request-bulk-001",
        scan_id: "scan-001",
        cohort_id: "cohort-001",
        source: bulkSource,
        cohort_total: 1,
        chunk_index: 0,
        final_chunk: true,
        canonical_keys: ["key"],
        ...changes,
      }),
    );
  expect(() => request({ cohort_total: 0 })).toThrow(ProtocolError);
  expect(() => request({ cohort_total: 201 })).toThrow(ProtocolError);
  expect(() =>
    request({
      cohort_total: 51,
      chunk_index: 4,
      final_chunk: true,
      canonical_keys: ["key"],
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    request({
      cohort_total: 51,
      chunk_index: 0,
      final_chunk: false,
      canonical_keys: Array.from({ length: 51 }, (_, i) => `key-${i}`),
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    request({ cohort_total: 2, canonical_keys: ["duplicate", "duplicate"] }),
  ).toThrow(ProtocolError);
  const result = {
    request_id: "request-bulk-001",
    scan_id: "scan-001",
    cohort_id: "cohort-001",
    chunk_index: 0,
    final_chunk: true,
    batch_id: "batch-001",
    membership: "complete",
    cohort_total: 1,
    persisted_members: 1,
    submitted: 1,
    joined: 0,
    already_owned: 0,
    invalid: 0,
  };
  expect(() =>
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_result", {
        ...result,
        chunk_index: 4,
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_result", {
        ...result,
        submitted: 1_000_001,
      }),
    ),
  ).toThrow(ProtocolError);
});

test("page bulk v2 accepts cumulative persisted members across two chunks", () => {
  const base = {
    request_id: "request-bulk-001",
    scan_id: "scan-001",
    cohort_id: "cohort-001",
    batch_id: "batch-001",
    membership: "open",
    cohort_total: 100,
    persisted_members: 0,
    submitted: 50,
    joined: 0,
    already_owned: 0,
    invalid: 0,
  };
  expect(
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_result", {
        ...base,
        chunk_index: 0,
        final_chunk: false,
      }),
    ).payload,
  ).toBeDefined();
  expect(
    parseBrowserMessage(
      protocolFrame("page_bulk_submit_v2_result", {
        ...base,
        chunk_index: 1,
        final_chunk: true,
        persisted_members: 50,
      }),
    ).payload,
  ).toBeDefined();
  for (const changes of [
    { chunk_index: 0, final_chunk: false, submitted: 0 },
    { chunk_index: 1, final_chunk: true, persisted_members: 50, submitted: 49 },
  ]) {
    expect(() =>
      parseBrowserMessage(
        protocolFrame("page_bulk_submit_v2_result", { ...base, ...changes }),
      ),
    ).toThrow(ProtocolError);
  }
});

test("counts schema negotiation accepts only the locked versions", () => {
  for (const versions of [[1], [2], [3]]) {
    expect(
      parseBrowserMessage(
        protocolFrame("triage_counts_request", {
          request_id: "request-001",
          schema_versions: versions,
        }),
      ).payload,
    ).toEqual({
      request_id: "request-001",
      schema_versions: versions,
    });
  }
  for (const schema_versions of [[4], [3, 2]]) {
    expect(() =>
      parseBrowserMessage(
        protocolFrame("triage_counts_request", {
          request_id: "request-001",
          schema_versions,
        }),
      ),
    ).toThrow(ProtocolError);
  }
  for (const schema_versions of [[1], [2], [3], [4], [5], [4, 3], [5, 4]]) {
    expect(
      parseBrowserMessage(
        protocolFrame("triage_snapshot_request", {
          request_id: "request-001",
          schema_versions,
        }),
      ).payload,
    ).toEqual({
      request_id: "request-001",
      schema_versions,
    });
  }
  for (const schema_versions of [[6], [5, 3], [3, 4]]) {
    expect(() =>
      parseBrowserMessage(
        protocolFrame("triage_snapshot_request", {
          request_id: "request-001",
          schema_versions,
        }),
      ),
    ).toThrow(ProtocolError);
  }
});

test("counts v3 fields, family runs, and required-turn item kinds are strict", () => {
  const valid = (counts: Record<string, unknown>) =>
    parseBrowserMessage(
      protocolFrame("triage_counts_response", {
        request_id: "request-001",
        counts,
      }),
    );
  expect(valid(countsV3()).payload).toBeDefined();
  expect(() => valid({ ...countsV3(), turns_required: 1_000_001 })).toThrow(
    ProtocolError,
  );
  expect(() =>
    valid({
      ...countsV3(),
      family_runs: Array.from({ length: 129 }, (_, i) => ({
        ...(countsV3().family_runs instanceof Array
          ? (countsV3().family_runs as Array<Record<string, unknown>>)[0]
          : {}),
        run_key: `run-${i}`,
      })),
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    valid({
      ...countsV3(),
      required_turns: Array.from({ length: 1_025 }, (_, i) => ({
        ...(countsV3().required_turns as Array<Record<string, unknown>>)[0],
        item_id: `item-${i}`,
      })),
    }),
  ).toThrow(ProtocolError);
  for (const field of [
    "next_actor",
    "guidance_variant",
    "operation_variant",
  ] as const) {
    const counts = countsV3();
    (counts.family_runs as Array<Record<string, unknown>>)[0]![field] =
      "unknown";
    expect(() => valid(counts)).toThrow(ProtocolError);
  }
  const wrongItemKind = countsV3();
  (
    wrongItemKind.required_turns as Array<Record<string, unknown>>
  )[0]!.item_kind = "other";
  expect(() => valid(wrongItemKind)).toThrow(ProtocolError);
  const pdf = countsV3();
  pdf.required_turns = [
    {
      item_id: "grab-001",
      item_kind: "pdf_grab",
      grab_id: "grab-001",
      route_class: "pdf_identifier_needed",
      dependent_jobs: 0,
    },
  ];
  pdf.turns_required = 1;
  expect(valid(pdf).payload).toBeDefined();
  const invalidPDF = {
    ...pdf,
    required_turns: [
      {
        item_id: "grab-001",
        item_kind: "pdf_grab",
        grab_id: "grab-001",
        action_id: 1,
        route_class: "pdf_identifier_needed",
        dependent_jobs: 0,
      },
    ],
  };
  expect(() => valid(invalidPDF)).toThrow(ProtocolError);
  const invalidHuman = {
    ...countsV3(),
    required_turns: [
      {
        item_id: "action-001",
        item_kind: "human_action",
        route_class: "manual_download",
        dependent_jobs: 0,
      },
    ],
  };
  expect(() => valid(invalidHuman)).toThrow(ProtocolError);
  const belowV3 = { ...countsV3() };
  delete belowV3.turns_required;
  delete belowV3.turns_working;
  delete belowV3.family_breakdown_complete;
  delete belowV3.family_runs;
  delete belowV3.required_turns_complete;
  delete belowV3.required_turns;
  expect(valid(belowV3).payload).toBeDefined();
  const belowV3Frame = protocolFrame("triage_snapshot_response", {
    request_id: "request-001",
    schema: 2,
    generated_at: "2026-01-01T00:00:00Z",
    counts: countsV3(),
    items: [],
    has_more: false,
    unsupported_items_count: 0,
  });
  expect(() => parseBrowserMessage(belowV3Frame)).toThrow(ProtocolError);
});

test("triage snapshot schema v5 quartet is all-or-none and kind-gated", () => {
  const item: Record<string, unknown> = {
    kind: "human_action",
    id: "action-001",
    rank: 0,
    title: "Download",
    facts: [],
    links: [],
    ops: ["open", "dismiss"],
    attention: "required",
    action_id: 1,
    job_id: "job-0001",
    action_kind: "manual_download",
    job_state: "awaiting_human",
    revision: 1,
    sha256: "",
    size_bytes: 0,
    route_class: "manual_download",
    auth_requirement: "false",
    run_key: "run-001",
    next_actor: "researcher",
    guidance_variant: "manual_download",
    operation_variant: "open_and_dismiss",
  };
  const response = (
    schema: number,
    itemOverride: Record<string, unknown> = {},
  ) =>
    protocolFrame("triage_snapshot_response", {
      request_id: "request-001",
      schema,
      generated_at: "2026-01-01T00:00:00Z",
      counts: {
        pending_total: 1,
        watch_hits: 0,
        actions: 1,
        retractions: 0,
        jobs_working: 0,
        jobs_needs_review: 0,
        failure_groups_7d: 0,
      },
      items: [{ ...item, ...itemOverride }],
      has_more: false,
      unsupported_items_count: 0,
    });
  expect(parseBrowserMessage(response(5)).payload).toBeDefined();
  for (const field of [
    "run_key",
    "next_actor",
    "guidance_variant",
    "operation_variant",
  ]) {
    const partial = { ...item };
    delete partial[field];
    const frame = response(5);
    (frame.payload as Record<string, unknown>)["items"] = [partial];
    expect(() => parseBrowserMessage(frame)).toThrow(ProtocolError);
  }
  expect(() => parseBrowserMessage(response(4))).toThrow(ProtocolError);
  const watch = {
    kind: "watch_hit",
    id: "watch-001",
    rank: 0,
    title: "Watch",
    facts: [],
    links: [],
    ops: [],
    attention: "advisory",
    work: {
      doi: "10.1/x",
      title: "Watch",
      authors: "A",
      year: 2026,
      is_oa: true,
    },
    abstract: "x",
    watches: [{ id: 1, label: "w" }],
    first_seen_at: "2026-01-01T00:00:00Z",
    run_key: "run-001",
    next_actor: "reference",
    guidance_variant: "manual_download",
    operation_variant: "none",
  };
  expect(() => parseBrowserMessage(response(5, watch))).toThrow(ProtocolError);
  const pdfItem: Record<string, unknown> = {
    kind: "pdf_grab",
    label: "Provide PDF identifier",
    grab: { grab_id: "grab-001", state: "awaiting_file" },
    route_class: "pdf_identifier_needed",
    blocked_by: "identifier_missing",
    attention: "required",
    ops: ["provide_identifier", "dismiss"],
    run_key: "run-001",
    next_actor: "researcher",
    guidance_variant: "pdf_identifier",
    operation_variant: "provide_identifier_or_dismiss",
  };
  const pdfFrame = protocolFrame("triage_snapshot_response", {
    request_id: "request-001",
    schema: 5,
    generated_at: "2026-01-01T00:00:00Z",
    counts: {
      pending_total: 1,
      watch_hits: 0,
      actions: 0,
      retractions: 0,
      jobs_working: 0,
      jobs_needs_review: 0,
      failure_groups_7d: 0,
    },
    items: [pdfItem],
    has_more: false,
    unsupported_items_count: 0,
  });
  expect(parseBrowserMessage(pdfFrame).payload).toBeDefined();
  const partialPDF = { ...pdfItem };
  delete partialPDF["operation_variant"];
  (pdfFrame.payload as Record<string, unknown>)["items"] = [partialPDF];
  expect(() => parseBrowserMessage(pdfFrame)).toThrow(ProtocolError);
  const belowV5 = response(4);
  (belowV5.payload as Record<string, unknown>)["items"] = [{ ...item }];
  expect(() => parseBrowserMessage(belowV5)).toThrow(ProtocolError);
});

test("shared protocol validators and triage vocabularies are locked", () => {
  expect(isBareLowercaseHTTPSOrigin("https://example.edu")).toBe(true);
  expect(isBareLowercaseHTTPSOrigin("https://example.edu:8443")).toBe(true);
  for (const origin of [
    "https://EXAMPLE.edu",
    "https://example.edu/path",
    "https://example.edu?x=1",
    "https://example.edu#x",
    "https://user@example.edu",
    "http://example.edu",
    `https://${"a".repeat(301)}`,
  ]) {
    expect(isBareLowercaseHTTPSOrigin(origin)).toBe(false);
  }
  expect(isCanonicalKey("")).toBe(false);
  expect(isCanonicalKey("key\0with-nul")).toBe(false);
  expect(isCanonicalKey("x".repeat(301))).toBe(false);
  expect(isCanonicalKey("x".repeat(300))).toBe(true);
  expect(isDetectorText("")).toBe(false);
  expect(isDetectorText("x".repeat(129))).toBe(false);
  expect(isDetectorText("x".repeat(128))).toBe(true);
  expect(NEXT_ACTORS).toEqual(["papio", "researcher", "reference"]);
  expect(GUIDANCE_VARIANTS).toEqual([
    "manual_download",
    "manual_download_adapter_missing",
    "manual_download_page_undriveable",
    "manual_download_rejected_file",
    "manual_download_wrong_work",
    "institution_sign_in",
    "open_page",
    "verify_identity",
    "document_delivery",
    "downloads_access",
    "terms_acceptance",
    "security_challenge",
    "pdf_identifier",
    "papio_continuing",
  ]);
  expect(OPERATION_VARIANTS).toEqual([
    "none",
    "dismiss_only",
    "open_and_dismiss",
    "accept_reject",
    "accept_reject_open",
    "delivery_reconcile",
    "provide_identifier_or_dismiss",
  ]);
});

test("effect_permit reconcile request/response cover every kind and reject cross-kind forbidden fields", () => {
  const job = "job-effect-0001";
  const permit = "permit-effect-0001";
  const mk = (
    payload: Record<string, unknown>,
    type: BrowserMessageType = "effect_permit_reconcile_request",
  ) => protocolFrame(type, payload);
  const mkResponse = (payload: Record<string, unknown>) =>
    mk(payload, "effect_permit_reconcile_response");
  const base = (kind: string, extra: Record<string, unknown>) => ({
    request_id: "request-effect-0001",
    permit_id: permit,
    effect_kind: kind,
    ...extra,
  });
  // Every kind has an explicit envelope scope: PDF is jobless; all others are job-scoped.
  const generics = base("generic_drive", {
    drive_attempt_id: "drive-attempt-0001",
    ordinal: 0,
    strategy: "fallback",
    revision: "1",
  });
  expect(parseBrowserMessage({ ...mk(generics), job_id: job }).type).toBe(
    "effect_permit_reconcile_request",
  );
  expect(() => parseBrowserMessage(mk(generics))).toThrow(ProtocolError);
  const direct = base("direct_get", {
    drive_attempt_id: "drive-attempt-0001",
    ordinal: 1,
    strategy: "direct_get",
    revision: "r2",
  });
  expect(parseBrowserMessage({ ...mk(direct), job_id: job }).type).toBe(
    "effect_permit_reconcile_request",
  );
  const pdfGrab = base("pdf_grab", { grab_id: "grab_effect_0001" });
  expect(parseBrowserMessage(mk(pdfGrab)).type).toBe(
    "effect_permit_reconcile_request",
  );
  expect(() => parseBrowserMessage({ ...mk(pdfGrab), job_id: job })).toThrow(
    ProtocolError,
  );
  const terms = base("terms", { terms_occurrence_id: "terms-occurrence-0001" });
  expect(parseBrowserMessage({ ...mk(terms), job_id: job }).type).toBe(
    "effect_permit_reconcile_request",
  );
  const institutional = base("institutional", {
    claim_id: "claim-effect-0001",
    binding_id: "binding-effect-0001",
    effect_ordinal: 1,
    institutional_request_id: "institutional-request-0001",
  });
  expect(parseBrowserMessage({ ...mk(institutional), job_id: job }).type).toBe(
    "effect_permit_reconcile_request",
  );
  // institutional may carry optional tab_id, non-institutional must not.
  const withTab = base("institutional", {
    claim_id: "claim-effect-0001",
    binding_id: "binding-effect-0001",
    effect_ordinal: 2,
    institutional_request_id: "institutional-request-0002",
    tab_id: 7,
  });
  expect(parseBrowserMessage({ ...mk(withTab), job_id: job }).type).toBe(
    "effect_permit_reconcile_request",
  );
  expect(() =>
    parseBrowserMessage(
      mk({ ...pdfGrab, tab_id: 1 } as Record<string, unknown>),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...terms, tab_id: 1 } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...generics, tab_id: 1 } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Cross-kind forbidden fields.
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...generics, grab_id: "grab-x" } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      mk({ ...pdfGrab, drive_attempt_id: "drive-attempt-0001" } as Record<
        string,
        unknown
      >),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      mk({ ...pdfGrab, ordinal: 0 } as Record<string, unknown>),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...terms, claim_id: "claim-x" } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...institutional, grab_id: "grab-x" } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...institutional, terms_occurrence_id: "terms-x" } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Strategy rules: direct_get requires direct_get, generic_drive forbids it.
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("direct_get", {
          drive_attempt_id: "drive-attempt-0001",
          ordinal: 0,
          strategy: "fallback",
          revision: "1",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("generic_drive", {
          drive_attempt_id: "drive-attempt-0001",
          ordinal: 0,
          strategy: "direct_get",
          revision: "1",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Missing required per-kind.
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("generic_drive", {
          ordinal: 0,
          strategy: "fallback",
          revision: "1",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() => parseBrowserMessage(mk(base("pdf_grab", {})))).toThrow(
    ProtocolError,
  );
  expect(() =>
    parseBrowserMessage({ ...mk(base("terms", {})), job_id: job }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("institutional", {
          claim_id: "claim-effect-0001",
          binding_id: "binding-effect-0001",
          institutional_request_id: "institutional-request-0001",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Safe integers: drive ordinals/tab IDs are nonnegative; institutional effect ordinals start at one.
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("generic_drive", {
          drive_attempt_id: "drive-attempt-0001",
          ordinal: -1,
          strategy: "fallback",
          revision: "1",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("generic_drive", {
          drive_attempt_id: "drive-attempt-0001",
          ordinal: Number.MAX_SAFE_INTEGER + 1,
          strategy: "fallback",
          revision: "1",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk(
        base("institutional", {
          claim_id: "claim-effect-0001",
          binding_id: "binding-effect-0001",
          effect_ordinal: 0,
          institutional_request_id: "institutional-request-0001",
        }),
      ),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...institutional, tab_id: -1 } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...institutional, tab_id: Number.MAX_SAFE_INTEGER + 1 } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Unknown and null fields fail closed.
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...generics, unknown_extra: 1 } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mk({ ...generics, drive_attempt_id: null } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  // Response: closed outcome set, booleans, no URL/provider text.
  const respBase: Record<string, unknown> = {
    request_id: "request-effect-0002",
    permit_id: permit,
    outcome: "recorded",
    dispatched: true,
    download_present: false,
    acknowledged: true,
    tab_present: false,
  };
  for (const outcome of [
    "recorded",
    "settled",
    "stale",
    "duplicate",
    "error",
  ] as const) {
    expect(
      parseBrowserMessage({
        ...mkResponse({ ...respBase, outcome }),
        job_id: job,
      }).type,
    ).toBe("effect_permit_reconcile_response");
  }
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, outcome: "unknown" } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, dispatched: "true" } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, download_present: null } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, url: "https://example.com" } as Record<
        string,
        unknown
      >),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, path: "/tmp/x" } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, detail: null } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({ ...respBase, extra: 1 } as Record<string, unknown>),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
  expect(
    parseBrowserMessage(mkResponse(respBase as Record<string, unknown>)).type,
  ).toBe("effect_permit_reconcile_response");
  // Reconciliation never carries arbitrary provider or diagnostic text.
  expect(() =>
    parseBrowserMessage({
      ...mkResponse({
        ...respBase,
        detail: "provider returned /tmp/private.pdf",
      }),
      job_id: job,
    }),
  ).toThrow(ProtocolError);
});
test("effect_permit reconcile permits jobless pdf_grab only", () => {
  const pdf = protocolFrame("effect_permit_reconcile_request", {
    request_id: "request-pdf-0001",
    permit_id: "permit_pdf_0001",
    effect_kind: "pdf_grab",
    grab_id: "grab_pdf_0001",
  });
  expect(parseBrowserMessage(pdf).job_id).toBeUndefined();

  const generic = protocolFrame("effect_permit_reconcile_request", {
    request_id: "request-generic-0001",
    permit_id: "permit-generic-0001",
    effect_kind: "generic_drive",
    drive_attempt_id: "drive-attempt-0001",
    ordinal: 0,
    strategy: "fallback",
    revision: "rev-1",
  });
  expect(() => parseBrowserMessage(generic)).toThrow(ProtocolError);

  const response = protocolFrame("effect_permit_reconcile_response", {
    request_id: "request-pdf-0002",
    permit_id: "permit-pdf-0001",
    outcome: "recorded",
    dispatched: false,
    download_present: false,
    acknowledged: false,
    tab_present: false,
  });
  expect(parseBrowserMessage(response).job_id).toBeUndefined();
});

test("effect_permit reconcile pdf_grab grab_id is a correlation id capped at 64 chars", () => {
  const permit = "permit-effect-0001";
  const valid = "a".repeat(64);
  const invalid = "a".repeat(65);
  expect(
    parseBrowserMessage(
      protocolFrame("effect_permit_reconcile_request", {
        request_id: "request-effect-0001",
        permit_id: permit,
        effect_kind: "pdf_grab",
        grab_id: valid,
      }),
    ).type,
  ).toBe("effect_permit_reconcile_request");
  expect(() =>
    parseBrowserMessage(
      protocolFrame("effect_permit_reconcile_request", {
        request_id: "request-effect-0001",
        permit_id: permit,
        effect_kind: "pdf_grab",
        grab_id: invalid,
      }),
    ),
  ).toThrow(ProtocolError);
});

test("provider drive epoch start-result still parses unsupported without treating it as started", () => {
  const base = {
    drive_attempt_id: "epoch-attempt-001",
    ordinal: 0,
    strategy: "generic",
    revision: "1",
  };
  const mkFrame = (outcome: string) => {
    const payload: Record<string, unknown> = { ...base, outcome };
    if (outcome === "unsupported") payload["detail"] = "need effect_permit_v1";
    return {
      protocol: "papio-browser/1" as const,
      type: "provider_drive_epoch_start_result" as const,
      msg_id: "epoch-msg-002",
      seq: 2,
      job_id: "job-epoch-002",
      payload,
    };
  };
  expect(parseBrowserMessage(mkFrame("started")).payload["outcome"]).toBe(
    "started",
  );
  expect(parseBrowserMessage(mkFrame("unsupported")).payload["outcome"]).toBe(
    "unsupported",
  );
  expect(
    parseBrowserMessage(mkFrame("unsupported")).payload["outcome"],
  ).not.toBe("started");
  // Extension must not treat unsupported as authorizing: only started authorizes.
  const isAuthorizing = (outcome: string) => outcome === "started";
  expect(isAuthorizing("unsupported")).toBe(false);
  expect(isAuthorizing("started")).toBe(true);
  for (const outcome of [
    "started",
    "duplicate",
    "stale",
    "unsupported",
    "error",
  ] as const) {
    expect(parseBrowserMessage(mkFrame(outcome)).payload["outcome"]).toBe(
      outcome,
    );
  }
  expect(() => parseBrowserMessage(mkFrame("applied"))).toThrow(ProtocolError);
});

test("surface_close_request/response round-trip and enforce §2.3 field rules", () => {
  const frame = (
    type: "surface_close_request" | "surface_close_response",
    payload: Record<string, unknown>,
  ) => ({
    protocol: "papio-browser/1" as const,
    type,
    msg_id: "msg-close-001",
    seq: 1,
    payload,
  });

  // Not job-scoped: a job_id-less envelope parses fine for both directions.
  for (const disposition of [
    "scaffold_idle",
    "materialization_settled",
  ] as const) {
    const msg = parseBrowserMessage(
      frame("surface_close_request", {
        request_id: "close-request-001",
        binding_id: "binding-close-001",
        browser_holder_generation: 1,
        disposition,
      }),
    );
    expect(msg.type).toBe("surface_close_request");
    expect(msg.job_id).toBeUndefined();
  }

  // gate_occurrence_id is optional even on claim_abandoned.
  expect(
    parseBrowserMessage(
      frame("surface_close_request", {
        request_id: "close-request-002",
        binding_id: "binding-close-001",
        browser_holder_generation: 1,
        disposition: "claim_abandoned",
      }),
    ).type,
  ).toBe("surface_close_request");
  expect(
    parseBrowserMessage(
      frame("surface_close_request", {
        request_id: "close-request-003",
        binding_id: "binding-close-001",
        browser_holder_generation: 1,
        disposition: "claim_abandoned",
        gate_occurrence_id: "gate-occurrence-001",
      }),
    ).payload,
  ).toEqual({
    request_id: "close-request-003",
    binding_id: "binding-close-001",
    browser_holder_generation: 1,
    disposition: "claim_abandoned",
    gate_occurrence_id: "gate-occurrence-001",
  });

  // gate_occurrence_id is forbidden on any disposition other than claim_abandoned.
  for (const disposition of [
    "scaffold_idle",
    "materialization_settled",
  ] as const) {
    expect(() =>
      parseBrowserMessage(
        frame("surface_close_request", {
          request_id: "close-request-004",
          binding_id: "binding-close-001",
          browser_holder_generation: 1,
          disposition,
          gate_occurrence_id: "gate-occurrence-001",
        }),
      ),
    ).toThrow(ProtocolError);
  }

  // §parity: Go's GateOccurrenceID field is a plain string validated via
  // `!= ""`, not key-presence — an explicit empty string round-trips the
  // same as an absent field even on a disposition that forbids it.
  for (const disposition of [
    "scaffold_idle",
    "materialization_settled",
  ] as const) {
    expect(
      parseBrowserMessage(
        frame("surface_close_request", {
          request_id: "close-request-004b",
          binding_id: "binding-close-001",
          browser_holder_generation: 1,
          disposition,
          gate_occurrence_id: "",
        }),
      ).type,
    ).toBe("surface_close_request");
  }

  // Unknown disposition value rejected.
  expect(() =>
    parseBrowserMessage(
      frame("surface_close_request", {
        request_id: "close-request-005",
        binding_id: "binding-close-001",
        browser_holder_generation: 1,
        disposition: "orphaned",
      }),
    ),
  ).toThrow(ProtocolError);

  // Every required request field is enforced.
  for (const key of [
    "request_id",
    "binding_id",
    "browser_holder_generation",
    "disposition",
  ]) {
    const payload: Record<string, unknown> = {
      request_id: "close-request-006",
      binding_id: "binding-close-001",
      browser_holder_generation: 1,
      disposition: "scaffold_idle",
    };
    delete payload[key];
    expect(() =>
      parseBrowserMessage(frame("surface_close_request", payload)),
    ).toThrow(ProtocolError);
  }

  // Unknown field fails closed.
  expect(() =>
    parseBrowserMessage(
      frame("surface_close_request", {
        request_id: "close-request-007",
        binding_id: "binding-close-001",
        browser_holder_generation: 1,
        disposition: "scaffold_idle",
        extra: true,
      }),
    ),
  ).toThrow(ProtocolError);

  // Response: authorized requires and carries exactly the token triple.
  expect(
    parseBrowserMessage(
      frame("surface_close_response", {
        request_id: "close-request-001",
        outcome: "authorized",
        close_authorization_id: "close-auth-001",
        nonce: "close-nonce-001",
        browser_holder_generation: 1,
      }),
    ).payload,
  ).toEqual({
    request_id: "close-request-001",
    outcome: "authorized",
    close_authorization_id: "close-auth-001",
    nonce: "close-nonce-001",
    browser_holder_generation: 1,
  });
  expect(() =>
    parseBrowserMessage(
      frame("surface_close_response", {
        request_id: "close-request-001",
        outcome: "authorized",
        close_authorization_id: "close-auth-001",
        nonce: "close-nonce-001",
        browser_holder_generation: 1,
        detail: "should not be here",
      }),
    ),
  ).toThrow(ProtocolError);
  for (const key of [
    "close_authorization_id",
    "nonce",
    "browser_holder_generation",
  ]) {
    const payload: Record<string, unknown> = {
      request_id: "close-request-001",
      outcome: "authorized",
      close_authorization_id: "close-auth-001",
      nonce: "close-nonce-001",
      browser_holder_generation: 1,
    };
    delete payload[key];
    expect(() =>
      parseBrowserMessage(frame("surface_close_response", payload)),
    ).toThrow(ProtocolError);
  }

  // Every non-authorized outcome forbids the token triple and accepts detail.
  for (const outcome of ["stale", "not_eligible", "busy", "error"] as const) {
    expect(
      parseBrowserMessage(
        frame("surface_close_response", {
          request_id: "close-request-001",
          outcome,
          detail: "cannot close",
        }),
      ).type,
    ).toBe("surface_close_response");
    for (const key of [
      "close_authorization_id",
      "nonce",
      "browser_holder_generation",
    ]) {
      const forbidden: Record<string, unknown> = {
        request_id: "close-request-001",
        outcome,
        [key]: key === "browser_holder_generation" ? 1 : "close-value-0001",
      };
      expect(() =>
        parseBrowserMessage(frame("surface_close_response", forbidden)),
      ).toThrow(ProtocolError);
    }
  }

  // Unknown outcome value rejected.
  expect(() =>
    parseBrowserMessage(
      frame("surface_close_response", {
        request_id: "close-request-001",
        outcome: "parked",
      }),
    ),
  ).toThrow(ProtocolError);

  // detail is bounded to 1000 chars on non-authorized outcomes.
  expect(() =>
    parseBrowserMessage(
      frame("surface_close_response", {
        request_id: "close-request-001",
        outcome: "error",
        detail: "x".repeat(1001),
      }),
    ),
  ).toThrow(ProtocolError);
});

// The shared-corpus half of this contract lives in testdata/protocol/{valid,
// invalid}/browser-surface-close-*.json, added by the sibling Go daemon
// agent implementing internal/protocol and internal/browser/bridge.go. The
// blanket "valid browser corpus parses"/"invalid browser corpus fails
// closed" tests above already pick up any browser-*.json fixture with no
// per-type allowlist, so no wiring change is needed here; this test adds a
// clearly-named, skip-if-absent check specific to this message family so a
// failure names surface_close rather than an anonymous corpus fixture.
test("surface_close corpus fixtures parse when the sibling daemon slice has landed", () => {
  const validDir = join(corpusRoot, "valid");
  const invalidDir = join(corpusRoot, "invalid");
  const validFixtures = existsSync(validDir)
    ? readdirSync(validDir).filter((name) =>
        name.startsWith("browser-surface-close"),
      )
    : [];
  const invalidFixtures = existsSync(invalidDir)
    ? readdirSync(invalidDir).filter((name) =>
        name.startsWith("browser-surface-close"),
      )
    : [];
  for (const name of validFixtures) {
    const text = readFileSync(join(validDir, name), "utf8");
    expect(() => parseBrowserMessageBytes(text), name).not.toThrow();
  }
  for (const name of invalidFixtures) {
    const text = readFileSync(join(invalidDir, name), "utf8");
    expect(() => parseBrowserMessageBytes(text), name).toThrow(ProtocolError);
  }
});

test("authentication_claim_request/response round-trip and enforce §2.1 field rules", () => {
  const frame = (
    type: "authentication_claim_request" | "authentication_claim_response",
    payload: Record<string, unknown>,
  ) => ({
    protocol: "papio-browser/1" as const,
    type,
    msg_id: "msg-auth-claim-001",
    seq: 1,
    job_id: "job-auth-claim-00001",
    payload,
  });

  // Job-scoped: a job_id-less envelope is rejected for both directions.
  expect(() =>
    parseBrowserMessage({
      protocol: "papio-browser/1",
      type: "authentication_claim_request",
      msg_id: "msg-auth-claim-001",
      seq: 1,
      payload: {
        request_id: "auth-claim-request-001",
        candidate_id: "candidate-00000001",
        materialization_kind: "browser_tab",
        trigger: "automatic",
      },
    }),
  ).toThrow(ProtocolError);

  // Round-trip for both materialization_kind and trigger values.
  for (const materializationKind of ["browser_tab", "direct_download"] as const) {
    for (const trigger of ["automatic", "explicit"] as const) {
      const msg = parseBrowserMessage(
        frame("authentication_claim_request", {
          request_id: "auth-claim-request-001",
          candidate_id: "candidate-00000001",
          materialization_kind: materializationKind,
          trigger,
        }),
      );
      expect(msg.type).toBe("authentication_claim_request");
      expect(msg.job_id).toBe("job-auth-claim-00001");
    }
  }

  // Every required request field is enforced.
  for (const key of [
    "request_id",
    "candidate_id",
    "materialization_kind",
    "trigger",
  ]) {
    const payload: Record<string, unknown> = {
      request_id: "auth-claim-request-001",
      candidate_id: "candidate-00000001",
      materialization_kind: "browser_tab",
      trigger: "automatic",
    };
    delete payload[key];
    expect(() =>
      parseBrowserMessage(frame("authentication_claim_request", payload)),
    ).toThrow(ProtocolError);
  }

  // Unknown field, invalid materialization_kind, invalid trigger all fail closed.
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_request", {
        request_id: "auth-claim-request-001",
        candidate_id: "candidate-00000001",
        materialization_kind: "browser_tab",
        trigger: "automatic",
        extra: true,
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_request", {
        request_id: "auth-claim-request-001",
        candidate_id: "candidate-00000001",
        materialization_kind: "pdf_download",
        trigger: "automatic",
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_request", {
        request_id: "auth-claim-request-001",
        candidate_id: "candidate-00000001",
        materialization_kind: "browser_tab",
        trigger: "curious",
      }),
    ),
  ).toThrow(ProtocolError);

  // Response: each of the four operational outcomes requires
  // authentication_claim_id/browser_holder_generation/gate_occurrence_id
  // and forbids detail.
  const operationalBase = {
    request_id: "auth-claim-request-001",
    authentication_claim_id: "auth-claim-00000001",
    browser_holder_generation: 7,
    gate_occurrence_id: "gate-occurrence-00001",
  };
  for (const outcome of [
    "navigate_existing",
    "open_new",
    "focus_owner",
    "park",
  ] as const) {
    for (const key of [
      "authentication_claim_id",
      "browser_holder_generation",
      "gate_occurrence_id",
    ]) {
      const payload: Record<string, unknown> = {
        ...operationalBase,
        outcome,
        ...(outcome === "park"
          ? { dependent_count: 0 }
          : { lease_until: "2026-08-18T00:00:00Z" }),
        ...(outcome === "navigate_existing" || outcome === "focus_owner"
          ? { owner_binding_id: "binding-owner-0001" }
          : {}),
      };
      delete payload[key];
      expect(() =>
        parseBrowserMessage(frame("authentication_claim_response", payload)),
      ).toThrow(ProtocolError);
    }
    expect(() =>
      parseBrowserMessage(
        frame("authentication_claim_response", {
          ...operationalBase,
          outcome,
          detail: "should not be here",
          ...(outcome === "park"
            ? { dependent_count: 0 }
            : { lease_until: "2026-08-18T00:00:00Z" }),
          ...(outcome === "navigate_existing" || outcome === "focus_owner"
            ? { owner_binding_id: "binding-owner-0001" }
            : {}),
        }),
      ),
    ).toThrow(ProtocolError);
    // §parity: Go's Detail field is a plain string validated via `!= ""`,
    // not key-presence — an explicit empty string round-trips the same as
    // an absent field even though the operational outcomes forbid it.
    expect(
      parseBrowserMessage(
        frame("authentication_claim_response", {
          ...operationalBase,
          outcome,
          detail: "",
          ...(outcome === "park"
            ? { dependent_count: 0 }
            : { lease_until: "2026-08-18T00:00:00Z" }),
          ...(outcome === "navigate_existing" || outcome === "focus_owner"
            ? { owner_binding_id: "binding-owner-0001" }
            : {}),
        }),
      ).type,
    ).toBe("authentication_claim_response");
  }

  // navigate_existing: lease_until + owner_binding_id required, owner_tab_hint optional.
  expect(
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "navigate_existing",
        lease_until: "2026-08-18T00:00:00Z",
        owner_binding_id: "binding-owner-0001",
        owner_tab_hint: 42,
      }),
    ).payload,
  ).toEqual({
    ...operationalBase,
    outcome: "navigate_existing",
    lease_until: "2026-08-18T00:00:00Z",
    owner_binding_id: "binding-owner-0001",
    owner_tab_hint: 42,
  });
  expect(
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "navigate_existing",
        lease_until: "2026-08-18T00:00:00Z",
        owner_binding_id: "binding-owner-0001",
      }),
    ).type,
  ).toBe("authentication_claim_response");
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "navigate_existing",
        owner_binding_id: "binding-owner-0001",
      }),
    ),
  ).toThrow(ProtocolError); // missing lease_until
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "navigate_existing",
        lease_until: "2026-08-18T00:00:00Z",
      }),
    ),
  ).toThrow(ProtocolError); // missing owner_binding_id

  // open_new: lease_until required, but no binding_id/owner_tab_hint/dependent_count.
  expect(
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "open_new",
        lease_until: "2026-08-18T00:00:00Z",
      }),
    ).type,
  ).toBe("authentication_claim_response");
  for (const key of ["owner_binding_id", "owner_tab_hint", "dependent_count"]) {
    expect(() =>
      parseBrowserMessage(
        frame("authentication_claim_response", {
          ...operationalBase,
          outcome: "open_new",
          lease_until: "2026-08-18T00:00:00Z",
          [key]: key === "owner_binding_id" ? "binding-owner-0001" : 1,
        }),
      ),
    ).toThrow(ProtocolError);
  }

  // focus_owner: same shape as navigate_existing.
  expect(
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "focus_owner",
        lease_until: "2026-08-18T00:00:00Z",
        owner_binding_id: "binding-owner-0001",
      }),
    ).type,
  ).toBe("authentication_claim_response");

  // park: dependent_count required, lease_until/owner_binding_id/owner_tab_hint forbidden.
  expect(
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "park",
        dependent_count: 3,
      }),
    ).payload,
  ).toEqual({
    ...operationalBase,
    outcome: "park",
    dependent_count: 3,
  });
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_response", {
        ...operationalBase,
        outcome: "park",
      }),
    ),
  ).toThrow(ProtocolError); // missing dependent_count
  for (const key of ["lease_until", "owner_binding_id", "owner_tab_hint"]) {
    expect(() =>
      parseBrowserMessage(
        frame("authentication_claim_response", {
          ...operationalBase,
          outcome: "park",
          dependent_count: 3,
          [key]: key === "lease_until" ? "2026-08-18T00:00:00Z" : "x",
        }),
      ),
    ).toThrow(ProtocolError);
  }

  // The four non-operational outcomes forbid every operational-only field
  // and accept only detail.
  for (const outcome of [
    "feature_disabled",
    "not_eligible",
    "busy",
    "error",
  ] as const) {
    expect(
      parseBrowserMessage(
        frame("authentication_claim_response", {
          request_id: "auth-claim-request-001",
          outcome,
          detail: "cannot arbitrate",
        }),
      ).type,
    ).toBe("authentication_claim_response");
    for (const key of [
      "authentication_claim_id",
      "browser_holder_generation",
      "gate_occurrence_id",
      "lease_until",
      "dependent_count",
      "owner_binding_id",
      "owner_tab_hint",
    ]) {
      const forbidden: Record<string, unknown> = {
        request_id: "auth-claim-request-001",
        outcome,
        [key]: key === "browser_holder_generation" || key === "dependent_count" || key === "owner_tab_hint"
          ? 1
          : key === "lease_until"
            ? "2026-08-18T00:00:00Z"
            : "auth-claim-00000001",
      };
      expect(() =>
        parseBrowserMessage(frame("authentication_claim_response", forbidden)),
      ).toThrow(ProtocolError);
    }
  }

  // detail is bounded to 1000 chars on non-operational outcomes.
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_response", {
        request_id: "auth-claim-request-001",
        outcome: "error",
        detail: "x".repeat(1001),
      }),
    ),
  ).toThrow(ProtocolError);

  // Unknown outcome value rejected.
  expect(() =>
    parseBrowserMessage(
      frame("authentication_claim_response", {
        request_id: "auth-claim-request-001",
        outcome: "granted",
      }),
    ),
  ).toThrow(ProtocolError);
});

test("claim_observation/claim_observation_ack round-trip and enforce §2.2 field rules", () => {
  const frame = (
    type: "claim_observation" | "claim_observation_ack",
    payload: Record<string, unknown>,
  ) => ({
    protocol: "papio-browser/1" as const,
    type,
    msg_id: "msg-claim-obs-001",
    seq: 1,
    job_id: "job-claim-obs-00001",
    payload,
  });

  const observationBase = {
    request_id: "claim-obs-request-001",
    authentication_claim_id: "auth-claim-00000001",
    binding_id: "binding-obs-00000001",
    browser_holder_generation: 4,
    gate_occurrence_id: "gate-occurrence-00001",
    observation_id: "observation-0000001",
    event_ordinal: 0,
  };

  // Job-scoped: a job_id-less envelope is rejected.
  expect(() =>
    parseBrowserMessage({
      protocol: "papio-browser/1",
      type: "claim_observation",
      msg_id: "msg-claim-obs-001",
      seq: 1,
      payload: { ...observationBase, event_kind: "wall_observed" },
    }),
  ).toThrow(ProtocolError);

  // Round-trip for every event_kind value, with and without the optional
  // materialization_claim_id.
  for (const eventKind of [
    "wall_observed",
    "login_started",
    "mfa",
    "challenge",
    "auth_returned",
    "entitled_landing",
    "owner_closed",
    "navigation_error",
  ] as const) {
    expect(
      parseBrowserMessage(
        frame("claim_observation", {
          ...observationBase,
          event_kind: eventKind,
        }),
      ).type,
    ).toBe("claim_observation");
  }
  expect(
    parseBrowserMessage(
      frame("claim_observation", {
        ...observationBase,
        materialization_claim_id: "materialization-claim-001",
        event_kind: "wall_observed",
      }),
    ).payload,
  ).toEqual({
    ...observationBase,
    materialization_claim_id: "materialization-claim-001",
    event_kind: "wall_observed",
  });

  // Every required field is enforced (materialization_claim_id excluded — optional).
  for (const key of Object.keys(observationBase)) {
    const payload: Record<string, unknown> = {
      ...observationBase,
      event_kind: "wall_observed",
    };
    delete payload[key];
    expect(() =>
      parseBrowserMessage(frame("claim_observation", payload)),
    ).toThrow(ProtocolError);
  }
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation", { ...observationBase }),
    ),
  ).toThrow(ProtocolError); // missing event_kind

  // Unknown field and invalid event_kind both fail closed.
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation", {
        ...observationBase,
        event_kind: "wall_observed",
        extra: true,
      }),
    ),
  ).toThrow(ProtocolError);
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation", {
        ...observationBase,
        event_kind: "auth_pending",
      }),
    ),
  ).toThrow(ProtocolError);

  // Negative event_ordinal rejected.
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation", {
        ...observationBase,
        event_ordinal: -1,
        event_kind: "wall_observed",
      }),
    ),
  ).toThrow(ProtocolError);

  // Ack: gate_occurrence_id and browser_holder_generation are always
  // required; detail is forbidden on applied/duplicate.
  const ackBase = {
    request_id: "claim-obs-request-001",
    gate_occurrence_id: "gate-occurrence-00002",
    browser_holder_generation: 5,
  };
  for (const outcome of ["applied", "duplicate"] as const) {
    expect(
      parseBrowserMessage(
        frame("claim_observation_ack", { ...ackBase, outcome }),
      ).type,
    ).toBe("claim_observation_ack");
    expect(() =>
      parseBrowserMessage(
        frame("claim_observation_ack", {
          ...ackBase,
          outcome,
          detail: "should not be here",
        }),
      ),
    ).toThrow(ProtocolError);
  }
  for (const outcome of ["stale", "rejected", "error"] as const) {
    expect(
      parseBrowserMessage(
        frame("claim_observation_ack", {
          ...ackBase,
          outcome,
          detail: "not applied",
        }),
      ).type,
    ).toBe("claim_observation_ack");
  }

  // applied: lease_until is optional (present for the four renewing kinds,
  // absent otherwise — the ack cannot see the request's event_kind).
  expect(
    parseBrowserMessage(
      frame("claim_observation_ack", {
        ...ackBase,
        outcome: "applied",
        lease_until: "2026-08-18T00:05:00Z",
      }),
    ).payload,
  ).toEqual({
    ...ackBase,
    outcome: "applied",
    lease_until: "2026-08-18T00:05:00Z",
  });
  expect(
    parseBrowserMessage(
      frame("claim_observation_ack", { ...ackBase, outcome: "applied" }),
    ).payload,
  ).toEqual({ ...ackBase, outcome: "applied" });

  // lease_until is forbidden on every other outcome.
  for (const outcome of ["duplicate", "stale", "rejected", "error"] as const) {
    expect(() =>
      parseBrowserMessage(
        frame("claim_observation_ack", {
          ...ackBase,
          outcome,
          lease_until: "2026-08-18T00:05:00Z",
        }),
      ),
    ).toThrow(ProtocolError);
  }

  // gate_occurrence_id and browser_holder_generation required for every outcome.
  for (const key of ["gate_occurrence_id", "browser_holder_generation"]) {
    const payload: Record<string, unknown> = { ...ackBase, outcome: "applied" };
    delete payload[key];
    expect(() =>
      parseBrowserMessage(frame("claim_observation_ack", payload)),
    ).toThrow(ProtocolError);
  }

  // Unknown outcome value rejected.
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation_ack", { ...ackBase, outcome: "accepted" }),
    ),
  ).toThrow(ProtocolError);

  // detail is bounded to 1000 chars on outcomes that permit it.
  expect(() =>
    parseBrowserMessage(
      frame("claim_observation_ack", {
        ...ackBase,
        outcome: "error",
        detail: "x".repeat(1001),
      }),
    ),
  ).toThrow(ProtocolError);
});

// The shared-corpus half of this contract lives in testdata/protocol/{valid,
// invalid}/browser-authentication-claim-*.json and browser-claim-
// observation-*.json, added by the sibling Go daemon agent implementing
// internal/protocol and internal/browser/bridge.go. The blanket "valid
// browser corpus parses"/"invalid browser corpus fails closed" tests above
// already pick up any browser-*.json fixture with no per-type allowlist, so
// no wiring change is needed here; this test adds a clearly-named,
// skip-if-absent check specific to this message family so a failure names
// the claim-observation protocol rather than an anonymous corpus fixture.
test("authentication_claim/claim_observation corpus fixtures parse when the sibling daemon slice has landed", () => {
  const validDir = join(corpusRoot, "valid");
  const invalidDir = join(corpusRoot, "invalid");
  const prefixes = ["browser-authentication-claim", "browser-claim-observation"];
  const validFixtures = existsSync(validDir)
    ? readdirSync(validDir).filter((name) =>
        prefixes.some((prefix) => name.startsWith(prefix)),
      )
    : [];
  const invalidFixtures = existsSync(invalidDir)
    ? readdirSync(invalidDir).filter((name) =>
        prefixes.some((prefix) => name.startsWith(prefix)),
      )
    : [];
  for (const name of validFixtures) {
    const text = readFileSync(join(validDir, name), "utf8");
    expect(() => parseBrowserMessageBytes(text), name).not.toThrow();
  }
  for (const name of invalidFixtures) {
    const text = readFileSync(join(invalidDir, name), "utf8");
    expect(() => parseBrowserMessageBytes(text), name).toThrow(ProtocolError);
  }
});
