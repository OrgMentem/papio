// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { NativeRequestCorrelation } from "../src/correlation";
import type { BrowserMessage, BrowserMessageType } from "../src/protocol";

for (const [request, response, timeout, feature] of [
  ["native_viewer_save_request_v1", "native_viewer_save_result_v1", 60_000, "native_viewer_save_v1"],
] as const) {
  test(`${request} requires negotiation and sends once`, async () => {
    for (const available of [false, true]) {
      const sends: BrowserMessageType[] = [];
      const features: string[] = [];
      let reconnects = 0;
      const c = new NativeRequestCorrelation({
        randomUUID: () => "native-request-1", setTimeout: () => {},
        ensureConnected: async () => true,
        connectionFailure: () => ({ kind: "transport", code: "connection_timeout", message: "timeout" }),
        supportsFeature: feature => { features.push(feature); return available; },
        send: type => { sends.push(type); return false; }, reconnect: () => { reconnects++; },
      });
      const result = await c.request(request, {}, { jobID: "job_native" });
      expect(features).toEqual([feature]);
      expect(result).toMatchObject({ code: available ? "connection_lost" : "feature_unavailable" });
      expect(sends).toEqual(available ? [request] : []);
      expect(reconnects).toBe(available ? 1 : 0);
    }
  });
  test(`${request} correlates only its response and expires without retry`, async () => {
    let timer: (() => void) | undefined;
    let sends = 0;
    const c = new NativeRequestCorrelation({
      randomUUID: () => "native-request-1", setTimeout: (fn, ms) => { expect(ms).toBe(timeout); timer = fn; },
      ensureConnected: async () => true,
      connectionFailure: () => ({ kind: "transport", code: "connection_timeout", message: "timeout" }),
      supportsFeature: () => true,
      send: (type, payload, jobID) => {
        sends++; expect(type).toBe(request); expect(jobID).toBe("job_native");
        expect(payload.request_id).toBe("native-request-1"); return true;
      }, reconnect: () => { throw new Error("mutation retried"); },
    });
    const pending = c.request(request, {}, { jobID: "job_native", requestID: "native-request-1" });
    await Promise.resolve(); await Promise.resolve();
    const frame = (type: BrowserMessageType, id: string): BrowserMessage => ({ protocol: "papio-browser/1", type, msg_id: "native-msg-1", seq: 0, job_id: "job_native", payload: { request_id: id } } as BrowserMessage);
    expect(c.handleInbound(frame(response, "wrong-request"))).toBe("handled");
    // A registered but different reply must not settle this mutation.
    expect(c.handleInbound(frame("native_download_import_result_v1", "native-request-1"))).toBe("handled");
    timer!();
    await expect(pending).resolves.toEqual({ kind: "timeout" });
    expect(sends).toBe(1);
    expect(c.handleInbound(frame(response, "native-request-1"))).toBe("handled");
    const next = c.request(request, {}, { jobID: "job_native", requestID: "native-request-1" });
    await Promise.resolve(); await Promise.resolve();
    expect(c.handleInbound(frame(response, "native-request-1"))).toBe("handled");
    await expect(next).resolves.toMatchObject({ kind: "response", payload: { request_id: "native-request-1" } });
    expect(sends).toBe(2);
  });
}
