// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only native acquisition fixture. No daemon or extension changes.
// bun tools/native-spike-fixture.ts --run-dir /absolute/private/directory
import { createHash, randomUUID } from "node:crypto";
import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { parseArgs } from "node:util";

const { values } = parseArgs({ args: Bun.argv.slice(2), options: { "run-dir": { type: "string" } }, strict: true });
if (!values["run-dir"]) throw new Error("Required: --run-dir (new private directory)");
const dir = resolve(values["run-dir"]);
mkdirSync(dir, { mode: 0o700 }); // Refuse to reuse evidence from an earlier run.
const token = randomUUID(), prefix = `/papio-native-${token}`;
const filename = `papio-native-${token}.pdf`;
// Existing synthetic PDF corpus; no private reading data and no generated oracle.
const pdf = await Bun.file(new URL("../../internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf", import.meta.url)).arrayBuffer();
const digest = createHash("sha256").update(Buffer.from(pdf)).digest("hex");
const events = `${dir}/requests.jsonl`;
writeFileSync(events, "", { mode: 0o600, flag: "wx" });
const page = (title: string, body: string) => new Response(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Papio native spike ${token} — ${title}</title><style>body{font:20px system-ui;max-width:760px;margin:64px auto;line-height:1.6}a,button{font:inherit;margin:20px 0;display:block}aside{border-top:1px solid #aaa;margin-top:60px}</style><main><h1>${title}</h1><p>This is a local automation fixture. No publisher or account actions occur.</p>${body}</main></html>`, { headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" } });
const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch(request) {
  const path = new URL(request.url).pathname;
  if (request.method !== "GET" || !path.startsWith(prefix + "/")) return new Response("Not found", { status: 404 });
  appendFileSync(events, JSON.stringify({ at: new Date().toISOString(), path: path.slice(prefix.length) }) + "\n");
  switch (path.slice(prefix.length)) {
    case "/start": return page("Native acquisition experiment", `<p>Find the main article and save its PDF.</p><a href="${prefix}/article">Read the main article</a><aside><h2>Related material</h2><a href="${prefix}/unrelated">Unrelated reference PDF</a></aside>`);
    case "/article": return page("The main article", `<section aria-label="Article access"><p>Full text is available. Open the article PDF in the browser viewer.</p><a href="${prefix}/${filename}">View article PDF</a></section>`);
    case `/${filename}`: return new Response(pdf, { headers: { "Content-Type": "application/pdf", "Content-Disposition": `inline; filename="${filename}"`, "Cache-Control": "no-store" } });
    case "/unrelated": return page("Wrong reference", "<p>This is not the requested main article.</p>");
    default: return new Response("Not found", { status: 404 });
  }
}});
const receipt = { url: `http://127.0.0.1:${server.port}${prefix}/start`, prefix, filename, sha256: digest, bytes: pdf.byteLength, serverPID: process.pid };
writeFileSync(`${dir}/fixture.json`, JSON.stringify(receipt, null, 2) + "\n", { mode: 0o600, flag: "wx" });
console.log(JSON.stringify(receipt));
for (const signal of ["SIGINT", "SIGTERM"] as const) process.on(signal, () => { server.stop(true); process.exit(0); });
