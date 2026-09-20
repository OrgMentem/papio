import { expect, test } from "bun:test";

import { NativeRequestCorrelation } from "../src/correlation";
import type { BrowserMessage, BrowserMessageType } from "../src/protocol";

interface CorrelationHarness {
  correlation: NativeRequestCorrelation;
  preparedFeatures: string[];
  reconnects: { count: number };
  sent: Array<{
    type: BrowserMessageType;
    payload: Record<string, unknown>;
    jobID?: string | undefined;
  }>;
  sendResults: boolean[];
  timers: Array<() => void>;
  timerDelays: number[];
}

function makeHarness(sendResults: boolean[] = [], featureAvailable = true): CorrelationHarness {
  const preparedFeatures: string[] = [];
  const reconnects = { count: 0 };
  const sent: CorrelationHarness["sent"] = [];
  const timers: Array<() => void> = [];
  const timerDelays: number[] = [];
  let uuid = 0;
  const correlation = new NativeRequestCorrelation({
    randomUUID: () => `request${uuid++}`,
    setTimeout: (fn, ms) => {
      timers.push(fn);
      timerDelays.push(ms);
    },
    ensureConnected: async () => true,
    connectionFailure: () => ({
      kind: "transport",
      code: "connection_timeout",
      message: "not connected",
    }),
    supportsFeature: (feature) => {
      preparedFeatures.push(feature);
      return featureAvailable;
    },
    send: (type, payload, jobID) => {
      sent.push({ type, payload, jobID });
      return sendResults.shift() ?? true;
    },
    reconnect: () => {
      reconnects.count += 1;
    },
  });
  return {
    correlation,
    preparedFeatures,
    reconnects,
    sent,
    sendResults,
    timers,
    timerDelays,
  };
}

function inbound(
  type: BrowserMessageType,
  payload: Record<string, unknown>,
): BrowserMessage {
  return {
    protocol: "papio-browser/1",
    type,
    msg_id: `inbound_${type}`,
    seq: 0,
    payload,
  } as BrowserMessage;
}

async function afterPrepare(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

test("agent decisions require the negotiated feature before sending", async () => {
  const h = makeHarness([], false);
  await expect(h.correlation.request("agent_decide_request_v1", {})).resolves.toMatchObject({
    kind: "response", code: "feature_unavailable",
  });
  expect(h.preparedFeatures).toEqual(["agent_fallback_v1"]);
  expect(h.sent).toHaveLength(0);
  expect(h.timers).toHaveLength(0);
});

test("agent decisions correlate versioned replies and keep job scope", async () => {
  const h = makeHarness();
  const pending = h.correlation.request("agent_decide_request_v1", { ordinal: 0 }, {
    requestID: "request-agent-001", jobID: "job-agent-001",
  });
  await afterPrepare();
  expect(h.preparedFeatures).toEqual(["agent_fallback_v1"]);
  expect(h.sent).toEqual([{
    type: "agent_decide_request_v1",
    payload: { request_id: "request-agent-001", ordinal: 0 },
    jobID: "job-agent-001",
  }]);
  expect(h.timerDelays).toEqual([45_000]);
  expect(h.correlation.handleInbound(inbound("agent_decide_result_v1", {
    request_id: "request-agent-001", outcome: "decision", choice: "WAIT",
  }))).toBe("handled");
  await expect(pending).resolves.toMatchObject({ kind: "response", payload: { choice: "WAIT" } });
  h.timers[0]!(); // A completed request cannot later time out.
});

test("agent decisions use one attempt on transport failure and time out after 45 seconds", async () => {
  const failed = makeHarness([false, true]);
  await expect(failed.correlation.request("agent_decide_request_v1", {})).resolves.toMatchObject({
    kind: "transport", code: "connection_lost",
  });
  expect(failed.sent).toHaveLength(1);
  expect(failed.preparedFeatures).toEqual(["agent_fallback_v1"]);

  const h = makeHarness();
  const pending = h.correlation.request("agent_decide_request_v1", {});
  await afterPrepare();
  expect(h.timerDelays).toEqual([45_000]);
  h.timers[0]!();
  await expect(pending).resolves.toEqual({ kind: "timeout" });
  expect(h.sent).toHaveLength(1);

  const regular = h.correlation.request("stats_request", {});
  await afterPrepare();
  expect(h.timerDelays).toEqual([45_000, 15_000]);
  h.timers[1]!();
  await expect(regular).resolves.toEqual({ kind: "timeout" });
});

test("a duplicate supplied request ID fails before a second send", async () => {
  const h = makeHarness();
  const first = h.correlation.request(
    "triage_counts_request",
    {},
    { requestID: "same-request" },
  );
  await afterPrepare();

  const duplicate = await h.correlation.request(
    "triage_counts_request",
    {},
    { requestID: "same-request" },
  );
  expect(duplicate).toMatchObject({
    kind: "transport",
    code: "duplicate_request_id",
  });
  expect(h.sent).toHaveLength(1);

  expect(
    h.correlation.handleInbound(
      inbound("triage_counts_response", { request_id: "same-request" }),
    ),
  ).toBe("handled");
  await expect(first).resolves.toMatchObject({ kind: "response" });
});

test("mismatched and late replies do not settle another pending request", async () => {
  const h = makeHarness();
  let firstSettlements = 0;
  const first = h.correlation.request(
    "triage_counts_request",
    {},
    { requestID: "first-request" },
  );
  void first.then(() => {
    firstSettlements += 1;
  });
  await afterPrepare();

  expect(
    h.correlation.handleInbound(
      inbound("stats_response", { request_id: "first-request" }),
    ),
  ).toBe("handled");
  await afterPrepare();
  expect(firstSettlements).toBe(0);

  h.correlation.handleInbound(
    inbound("triage_counts_response", {
      request_id: "first-request",
      marker: "first",
    }),
  );
  await expect(first).resolves.toMatchObject({
    kind: "response",
    payload: { marker: "first" },
  });

  let secondSettlements = 0;
  const second = h.correlation.request(
    "triage_counts_request",
    {},
    { requestID: "second-request" },
  );
  void second.then(() => {
    secondSettlements += 1;
  });
  await afterPrepare();
  h.correlation.handleInbound(
    inbound("triage_counts_response", { request_id: "first-request" }),
  );
  await afterPrepare();
  expect(secondSettlements).toBe(0);

  h.correlation.handleInbound(
    inbound("triage_counts_response", { request_id: "second-request" }),
  );
  await expect(second).resolves.toMatchObject({ kind: "response" });
  expect(firstSettlements).toBe(1);
  expect(secondSettlements).toBe(1);
});

test("read policy retries one transport failure and mutation policy does not", async () => {
  const read = makeHarness([false, true]);
  const readResult = read.correlation.request("stats_request", {});
  await afterPrepare();
  await afterPrepare();
  expect(read.sent).toHaveLength(2);
  expect(read.preparedFeatures).toEqual(["browser_stats_v1", "browser_stats_v1"]);
  expect(read.reconnects.count).toBe(1);
  const readRequestID = read.sent[1]?.payload["request_id"];
  expect(typeof readRequestID).toBe("string");
  read.correlation.handleInbound(
    inbound("stats_response", { request_id: readRequestID }),
  );
  await expect(readResult).resolves.toMatchObject({ kind: "response" });

  const mutation = makeHarness([false, true]);
  await expect(
    mutation.correlation.request("triage_decide", { item_id: "item-1" }),
  ).resolves.toMatchObject({
    kind: "transport",
    code: "connection_lost",
  });
  expect(mutation.sent).toHaveLength(1);
  expect(mutation.preparedFeatures).toEqual(["triage_mutations_v1"]);
  expect(mutation.reconnects.count).toBe(1);
});

test("surface presence keeps its single-attempt read policy", async () => {
  const h = makeHarness([false, true]);
  await expect(
    h.correlation.request("surface_presence", { surfaces: [] }),
  ).resolves.toMatchObject({ kind: "transport" });
  expect(h.sent).toHaveLength(1);
  expect(h.preparedFeatures).toEqual(["surface_presence_v1"]);
});

test("failAll settles every pending request exactly once", async () => {
  const h = makeHarness();
  let firstSettlements = 0;
  let secondSettlements = 0;
  const first = h.correlation.request(
    "triage_decide",
    {},
    { requestID: "pending-first" },
  );
  const second = h.correlation.request(
    "triage_decide",
    {},
    { requestID: "pending-second" },
  );
  void first.then(() => {
    firstSettlements += 1;
  });
  void second.then(() => {
    secondSettlements += 1;
  });
  await afterPrepare();

  h.correlation.failAll("connection_lost", "disconnected");
  await expect(first).resolves.toEqual({
    kind: "transport",
    code: "connection_lost",
    message: "disconnected",
  });
  await expect(second).resolves.toEqual({
    kind: "transport",
    code: "connection_lost",
    message: "disconnected",
  });

  h.correlation.failAll("connection_lost", "again");
  h.correlation.handleInbound(
    inbound("triage_decide_result", { request_id: "pending-first" }),
  );
  h.correlation.handleInbound(
    inbound("triage_decide_result", { request_id: "pending-second" }),
  );
  await afterPrepare();
  expect(firstSettlements).toBe(1);
  expect(secondSettlements).toBe(1);
});

test("matched errors settle requests and unmatched errors return to Bridge", async () => {
  const h = makeHarness();
  const pending = h.correlation.request(
    "triage_decide",
    {},
    { requestID: "preview-request" },
  );
  await afterPrepare();

  expect(
    h.correlation.handleInbound(
      inbound("error", {
        request_id: "preview-request",
        code: "refused",
        message: "not available",
      }),
    ),
  ).toBe("handled");
  await expect(pending).resolves.toEqual({
    kind: "transport",
    code: "refused",
    message: "not available",
  });
  expect(
    h.correlation.handleInbound(
      inbound("error", {
        request_id: "hello-request",
        code: "session_busy",
      }),
    ),
  ).toBe("unmatched_error");
  expect(
    h.correlation.handleInbound(
      inbound("error", { code: "extension_outdated" }),
    ),
  ).toBe("unmatched_error");
});

test("invalid and mismatched supplied IDs fail without sending", async () => {
  const h = makeHarness();
  await expect(
    h.correlation.request("stats_request", {}, { requestID: "bad\nrequest" }),
  ).resolves.toMatchObject({ code: "invalid_request_id" });
  await expect(
    h.correlation.request(
      "stats_request",
      { request_id: "payload-request" },
      { requestID: "option-request" },
    ),
  ).resolves.toMatchObject({ code: "request_id_mismatch" });
  expect(h.sent).toHaveLength(0);
});

test("timeout removes pending state and a late reply stays inert", async () => {
  const h = makeHarness();
  const request = h.correlation.request(
    "activity_request",
    {},
    { requestID: "timed-request" },
  );
  await afterPrepare();
  expect(h.timers).toHaveLength(1);
  h.timers[0]!();
  await expect(request).resolves.toEqual({ kind: "timeout" });

  expect(
    h.correlation.handleInbound(
      inbound("activity_response", { request_id: "timed-request" }),
    ),
  ).toBe("handled");
  h.correlation.failAll("connection_lost", "disconnected");
});
