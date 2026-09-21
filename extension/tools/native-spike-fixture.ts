// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only native acquisition fixture. No daemon or extension changes.
// bun tools/native-spike-fixture.ts --run-dir /absolute/private/directory
import { createHash, randomUUID } from "node:crypto";
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { parseArgs } from "node:util";

export interface NativeFixtureReceipt {
  url: string; prefix: string; filename: string; sha256: string; bytes: number; serverPID: number;
  pdfURL: string; revokeURL: string; statusURL: string;
}
export interface FixtureRequest {
  sequence: number; at: string; method: string; path: string; query: string; range: string | null;
  userAgent: string; responseKind: "pdf" | "html" | "json" | "text";
  status: number; bytes: number; contentType: string; revoked: boolean; revocationSequence: number | null;
}

export function startNativeFixture(runDir: string) {
  const dir = resolve(runDir);
  mkdirSync(dir, { mode: 0o700 }); // Refuse to reuse evidence from an earlier run.
  const token = randomUUID(), prefix = `/papio-native-${token}`;
  const filename = `papio-native-${token}.pdf`, pdfPath = `${prefix}/${filename}`;
  // Existing synthetic corpus: the oracle never fabricates replacement PDF bytes.
  const pdf = readFileSync(new URL("../../internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf", import.meta.url));
  const digest = createHash("sha256").update(pdf).digest("hex");
  const requestsPath = `${dir}/requests.jsonl`;
  writeFileSync(requestsPath, "", { mode: 0o600, flag: "wx" });
  let sequence = 0, revocationSequence: number | null = null;
  const page = (title: string, body: string) => `<!doctype html><html lang="en"><meta charset="utf-8"><title>Papio native spike ${token} — ${title}</title><style>body{font:20px system-ui;max-width:760px;margin:64px auto;line-height:1.6}a,button{font:inherit;margin:20px 0;display:block}aside{border-top:1px solid #aaa;margin-top:60px}</style><main><h1>${title}</h1><p>This is a local automation fixture. No publisher or account actions occur.</p>${body}</main></html>`;
  const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch(request): Response {
    const url = new URL(request.url), path = url.pathname;
    const current = ++sequence;
    // Synchronous response selection + logging gives one total order, including
    // the revocation boundary. Byte counts describe response bodies, not TCP ACKs.
    const reply = (body: string | Buffer, responseKind: FixtureRequest["responseKind"], status = 200) => {
      const contentType = { pdf: "application/pdf", html: "text/html; charset=utf-8", json: "application/json", text: "text/plain; charset=utf-8" }[responseKind];
      const event: FixtureRequest = { sequence: current, at: new Date().toISOString(), method: request.method,
        path: path.startsWith(prefix + "/") ? path.slice(prefix.length) : path, query: url.search,
        range: request.headers.get("range"), userAgent: request.headers.get("user-agent") ?? "",
        responseKind, status, bytes: request.method === "HEAD" ? 0 : Buffer.byteLength(body), contentType,
        revoked: revocationSequence !== null, revocationSequence };
      appendFileSync(requestsPath, JSON.stringify(event) + "\n");
      return new Response(request.method === "HEAD" ? null : typeof body === "string" ? body : new Uint8Array(body), { status, headers: {
        "Content-Type": contentType, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
        "X-Papio-Fixture-Sequence": String(current),
        ...(responseKind === "pdf" ? { "Content-Disposition": `inline; filename="${filename}"` } : {}),
      } });
    };
    const state = () => ({ pdfURL: `http://127.0.0.1:${server.port}${pdfPath}`, revoked: revocationSequence !== null, revocationSequence });
    if (path === `${prefix}/control/revoke` || path === `${prefix}/control/status`) {
      if (url.search) return reply("Not found", "text", 404);
      const revoke = path.endsWith("/revoke");
      if (request.method !== (revoke ? "POST" : "GET")) return reply("Method not allowed", "text", 405);
      const alreadyRevoked = revocationSequence !== null;
      if (revoke && !alreadyRevoked) revocationSequence = current;
      return reply(JSON.stringify({ ...state(), ...(revoke ? { alreadyRevoked } : {}) }), "json");
    }
    // Check the irreversible latch before ANY PDF response, regardless of query,
    // Range, method or repetition. There is deliberately no reset/control setter.
    if (path === pdfPath && revocationSequence !== null) return reply(page("PDF access revoked", "<p>The original PDF is no longer served.</p>"), "html", 410);
    if (request.method !== "GET" || !path.startsWith(prefix + "/")) return reply("Not found", "text", 404);
    switch (path.slice(prefix.length)) {
      case "/start": return reply(page("Native acquisition experiment", `<p>Find the main article and save its PDF.</p><a href="${prefix}/article">Read the main article</a><aside><h2>Related material</h2><a href="${prefix}/unrelated">Unrelated reference PDF</a></aside>`), "html");
      case "/article": return reply(page("The main article", `<section aria-label="Article access"><p>Full text is available. Open the article PDF in the browser viewer.</p><a href="${pdfPath}">View article PDF</a></section>`), "html");
      // As in the baseline fixture, a pre-revocation Range may receive the full
      // representation (200). No streaming work can outlive the latch check.
      case `/${filename}`: return reply(pdf, "pdf");
      case "/unrelated": return reply(page("Wrong reference", "<p>This is not the requested main article.</p>"), "html");
      default: return reply("Not found", "text", 404);
    }
  } });
  const origin = `http://127.0.0.1:${server.port}`;
  const receipt: NativeFixtureReceipt = { url: `${origin}${prefix}/start`, prefix, filename, sha256: digest, bytes: pdf.byteLength, serverPID: process.pid,
    pdfURL: `${origin}${pdfPath}`, revokeURL: `${origin}${prefix}/control/revoke`, statusURL: `${origin}${prefix}/control/status` };
  writeFileSync(`${dir}/fixture.json`, JSON.stringify(receipt, null, 2) + "\n", { mode: 0o600, flag: "wx" });
  return { receipt, requestsPath, stop: () => server.stop(true) };
}

if (import.meta.main) {
  const { values } = parseArgs({ args: Bun.argv.slice(2), options: { "run-dir": { type: "string" } }, strict: true });
  if (!values["run-dir"]) throw new Error("Required: --run-dir (new private directory)");
  const fixture = startNativeFixture(values["run-dir"]);
  console.log(JSON.stringify(fixture.receipt));
  for (const signal of ["SIGINT", "SIGTERM"] as const) process.on(signal, () => { fixture.stop(); process.exit(0); });
}
