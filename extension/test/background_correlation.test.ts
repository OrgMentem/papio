import { expect, test } from "bun:test";

import { Bridge, type BridgeDeps } from "../src/background";
import type { BrowserMessageType } from "../src/protocol";

type PrivateBridge = {
  requestNative(
    type: BrowserMessageType,
    payload: Record<string, unknown>,
    expectedType: BrowserMessageType,
    feature: string,
    mutation: boolean,
  ): Promise<unknown>;
};

test("requestNative rejects a reply type that inbound routing cannot settle", async () => {
  // Execute the runtime guard instead of scanning source syntax: a variable or
  // helper wrapper that evaded a regex must still fail before it can time out.
  const bridge = new Bridge({ randomUUID: () => "worker-epoch-test" } as BridgeDeps);
  const expectedType: BrowserMessageType = "hello_ack";
  const requestNative = (bridge as unknown as PrivateBridge).requestNative.bind(bridge);

  await expect(
    requestNative("triage_counts_request", {}, expectedType, "test-only feature", false),
  ).rejects.toThrow("papio: correlated request expects unrouted reply type hello_ack");
});
