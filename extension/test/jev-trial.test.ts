// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { afterEach, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildRequest, ENDPOINT, LIMITS, runTrial, sha256, validateResponse, type TrialIO } from "../tools/jev-trial";

const fixturePassword = crypto.randomUUID();
function credentialURL(path: string): string {
  const url = new URL(path, "https://example.test");
  url.username = "test-user";
  url.password = fixturePassword;
  return url.href;
}
const snapshot = () => ({ goal: "Read the article PDF", page: { url: credentialURL("/article?token=secret#private"), title: "Article", text: "Full text available" },
  controls: [{ id: "pdf", role: "link", label: "Download PDF" }, { id: "buy", role: "button", label: "Purchase", disabled: true }],
  provenance: { privatePath: "/private/paper.json", capturedAt: "2026-09-20T00:00:00Z" } });
const response = () => ({ model: "jev-1.13.0", answers: { action: { type: "choice", choice: "pdf", probabilities: { pdf: 0.8, BLOCKED: 0.15, WAIT: 0.05 }, confidence: 0.7 } },
  usage: { input_tokens: 318, output_tokens: 34 } });
const temporary: string[] = [];
afterEach(() => { for (const path of temporary.splice(0)) rmSync(path, { recursive: true, force: true }); });
function fixture() {
  const root = mkdtempSync(join(tmpdir(), "papio-jev-test-")); temporary.push(root);
  const path = join(root, "snapshot.json"), runDir = join(root, "run");
  writeFileSync(path, JSON.stringify(snapshot()));
  return { snapshot: path, runDir };
}
function offlineIO(body: unknown = response()) {
  let calls = 0, keys = 0;
  const io: TrialIO = { key: () => { keys++; return "test-key-only"; }, fetch: async (url, init) => {
    calls++;
    expect(url).toBe(ENDPOINT); expect(init.redirect).toBe("error"); expect(init.method).toBe("POST");
    expect(init.signal).toBeInstanceOf(AbortSignal);
    expect(JSON.parse(init.body as string)).toEqual(buildRequest(snapshot()).request);
    return Response.json(body);
  } };
  return { io, calls: () => calls, keys: () => keys };
}

// runTrial refuses a run directory unless POSIX mode bits and uid prove it is
// private. Windows reports neither (process.getuid is undefined), so the tool
// refuses every run directory there and these runs cannot start.
const posixRun = test.skipIf(process.platform === "win32");

test("request allowlists state and choices, removes URL secrets and binds all snapshot provenance locally", () => {
  const input = { ...snapshot(), cookies: "cookie-secret" };
  Object.assign(input.controls[0]!, { value: "password-secret", selector: "#secret", href: "https://example.test/?token=hidden" });
  input.page.text = `Open ${credentialURL("/full?session=private#secret")}`;
  const prepared = buildRequest(input);
  expect(prepared.request.state.page).toMatchObject({ url: "https://example.test/article", text: "Open https://example.test/full" });
  for (const secret of ["cookie-secret", "password-secret", "#secret", "privatePath", "/private/", "?token", "?session"]) expect(prepared.body).not.toContain(secret);
  expect(Object.keys(prepared.request.questions.action.criteria)).toEqual(["pdf", "BLOCKED", "WAIT"]);
  expect(prepared.snapshotHash).toBe(sha256(JSON.stringify(input)));
  input.provenance.capturedAt = "new capture";
  expect(buildRequest(input).snapshotHash).not.toBe(prepared.snapshotHash);
});

test("input bounds and hostile IDs fail closed", () => {
  for (const id of ["BLOCKED", "WAIT", "__proto__", "constructor", "pdf;echo secret", "#selector", ""]) {
    const input = snapshot(); input.controls[0]!.id = id;
    expect(() => buildRequest(input)).toThrow();
  }
  const duplicate = snapshot(); duplicate.controls[1]!.id = "pdf";
  expect(() => buildRequest(duplicate)).toThrow("duplicate");
  const many = snapshot(); many.controls = Array.from({ length: 81 }, (_, i) => ({ id: `c${i}`, role: "link", label: "PDF" }));
  expect(() => buildRequest(many)).toThrow("controls");
  const big = snapshot(); big.page.text = "x".repeat(12_001);
  expect(() => buildRequest(big)).toThrow("oversized");
  expect(() => buildRequest({ ...snapshot(), provenance: { excess: "x".repeat(LIMITS.fileBytes) } })).toThrow("input limit");
  const bad = snapshot(); bad.page.url = "javascript:alert(1)";
  expect(() => buildRequest(bad)).toThrow("URL");
});

test("validator accepts documented response and ties, rejects stale bindings", () => {
  const prepared = buildRequest(snapshot());
  expect(validateResponse(response(), prepared)).toMatchObject({ choice: "pdf", model: "jev-1.13.0", snapshotHash: prepared.snapshotHash, usage: { input_tokens: 318, output_tokens: 34 } });
  expect(() => validateResponse(response(), prepared, "changed")).toThrow("Stale");
  const tied = response(); tied.answers.action.probabilities = { pdf: 0.5, BLOCKED: 0.5, WAIT: 0 };
  expect(validateResponse(tied, prepared).choice).toBe("pdf");
});

test("validator rejects invented or disabled actions and malformed distributions/confidence/usage", () => {
  const prepared = buildRequest(snapshot());
  for (const choice of ["buy", "unknown", "__proto__", "document.querySelector('#pdf').click()", "WAIT"]) {
    const value = response(); value.answers.action.choice = choice;
    expect(() => validateResponse(value, prepared)).toThrow();
  }
  for (const invalid of [NaN, Infinity, -Infinity, -0.01, 1.01, "0.8", null]) {
    for (const field of ["confidence", "probability"] as const) {
      const value = response();
      if (field === "confidence") Object.assign(value.answers.action, { confidence: invalid });
      else Object.assign(value.answers.action.probabilities, { pdf: invalid });
      expect(() => validateResponse(value, prepared)).toThrow();
    }
  }
  for (const probabilities of [{ pdf: 0.8, BLOCKED: 0.2 }, { pdf: 0.8, BLOCKED: 0.1, WAIT: 0, unknown: 0.1 }, { pdf: 0.7, BLOCKED: 0.1, WAIT: 0.1 }]) {
    const value = response(); Object.assign(value.answers.action, { probabilities });
    expect(() => validateResponse(value, prepared)).toThrow();
  }
  for (const input_tokens of [-1, 0.5, Infinity, "318"]) {
    const value = response(); Object.assign(value.usage, { input_tokens });
    expect(() => validateResponse(value, prepared)).toThrow("usage");
  }
});

posixRun("run writes private hash receipts and exact usage, enforces persistent call and byte budgets before key access", async () => {
  const options = fixture(), mock = offlineIO();
  const result = await runTrial({ ...options, maxCalls: 1 }, mock.io);
  expect(result.responseHash).toBe(sha256(JSON.stringify(response())));
  expect(result.latencyMs).toBeGreaterThanOrEqual(0);
  expect(statSync(options.runDir).mode & 0o077).toBe(0);
  for (const name of readdirSync(options.runDir)) {
    const path = join(options.runDir, name);
    expect(statSync(path).mode & 0o077).toBe(0);
    const text = readFileSync(path, "utf8"); expect(text).not.toContain("test-key-only"); expect(text).not.toContain("/private/");
  }
  await expect(runTrial({ ...options, maxCalls: 1 }, mock.io)).rejects.toThrow("budget");
  await expect(runTrial({ ...options, maxInputBytes: buildRequest(snapshot()).inputBytes }, mock.io)).rejects.toThrow("budget");
  expect(mock.calls()).toBe(1); expect(mock.keys()).toBe(1);
  const receipt = JSON.parse(readFileSync(join(options.runDir, "request-01.json"), "utf8"));
  expect(receipt.counters).toEqual({ calls: 1, inputBytes: buildRequest(snapshot()).inputBytes });
});

posixRun("failed HTTP/validation/network attempts remain counted and never retry or expose response/error secrets", async () => {
  for (const fetch of [async () => new Response("remote-secret", { status: 401 }), async () => Response.json({ secret: "remote-secret" }),
    async () => { throw new Error("network-secret"); }]) {
    const options = fixture(); let calls = 0;
    const io: TrialIO = { key: () => "test-key-only", fetch: async () => { calls++; return fetch(); } };
    await expect(runTrial({ ...options, maxCalls: 1 }, io)).rejects.toThrow("attempt recorded, no retry");
    await expect(runTrial({ ...options, maxCalls: 1 }, io)).rejects.toThrow("budget");
    expect(calls).toBe(1);
    expect(readFileSync(join(options.runDir, "failure-01.json"), "utf8")).not.toContain("secret");
  }
});

posixRun("missing credential fails without a request; unsafe directories and symlink snapshots fail before key access", async () => {
  const options = fixture(), mock = offlineIO();
  await expect(runTrial(options, { ...mock.io, key: () => "" })).rejects.toThrow("credential unavailable");
  await expect(runTrial(options, { ...mock.io, key: () => { throw new Error("keychain-secret"); } })).rejects.toThrow("Keychain credential unavailable");
  expect(mock.calls()).toBe(0); expect(readdirSync(options.runDir)).toEqual([]);
  chmodSync(options.runDir, 0o755);
  await expect(runTrial(options, mock.io)).rejects.toThrow("private");
  expect(mock.keys()).toBe(0);
  const link = `${options.snapshot}.link`; symlinkSync(options.snapshot, link);
  await expect(runTrial({ ...options, snapshot: link }, mock.io)).rejects.toThrow();
  expect(mock.keys()).toBe(0);
});

posixRun("concurrent run cannot bill or overwrite an in-flight receipt", async () => {
  const options = fixture(), mock = offlineIO();
  let release!: (value: Response) => void;
  const pending = runTrial(options, { key: () => "test-key-only", fetch: () => new Promise(resolve => { release = resolve; }) });
  const original = readFileSync(join(options.runDir, "request-01.json"), "utf8");
  await expect(runTrial(options, mock.io)).rejects.toThrow();
  expect(mock.keys()).toBe(0); expect(mock.calls()).toBe(0);
  expect(readFileSync(join(options.runDir, "request-01.json"), "utf8")).toBe(original);
  release(Response.json(response())); await pending;
});

posixRun("oversized responses fail once and consume the attempt budget", async () => {
  const options = fixture(); let calls = 0;
  const io: TrialIO = { key: () => "test-key-only", fetch: async () => { calls++; return new Response("x".repeat(LIMITS.fileBytes + 1)); } };
  await expect(runTrial({ ...options, maxCalls: 1 }, io)).rejects.toThrow("attempt recorded");
  await expect(runTrial({ ...options, maxCalls: 1 }, io)).rejects.toThrow("budget");
  expect(calls).toBe(1);
});

posixRun("validation failures retain bounded private body evidence with key redacted and safe reason only", async () => {
  const options = fixture(), value = { ...response(), debug: "echo test-key-only twice test-key-only" };
  value.answers.action.probabilities.pdf = 0.79;
  const raw = JSON.stringify(value);
  await expect(runTrial(options, { key: () => "test-key-only", fetch: async () => new Response(raw, {
    headers: { "x-secret-header": "must-not-be-persisted" },
  }) })).rejects.toThrow("probability_sum");
  const path = join(options.runDir, "response-01.txt"), evidence = readFileSync(path, "utf8");
  expect(evidence).toBe(raw.replaceAll("test-key-only", "[REDACTED]"));
  expect(evidence).not.toContain("must-not-be-persisted");
  expect(statSync(path).mode & 0o077).toBe(0);
  const failure = JSON.parse(readFileSync(join(options.runDir, "failure-01.json"), "utf8"));
  expect(failure.reason).toBe("probability_sum");
  expect(failure.responseHash).toBe(sha256(raw));
  expect(JSON.stringify(failure)).not.toContain("echo");
});
