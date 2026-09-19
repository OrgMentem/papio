import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Window } from "happy-dom";
import { adapters } from "../src/adapters/types";
import { executePlannedPageEffect } from "../src/background";
import { planExecution, type Plan } from "../src/plan";

const href = "https://www.annualreviews.org/content/journals/10.1146/annurev.clinpsy.1.102803.143833";
const spec = adapters.find((item) => item.id === "annualreviews")!;
const expected = { title: "Motivational Interviewing", doi: "10.1146/annurev.clinpsy.1.102803.143833" };
const endpoint = "https://www.annualreviews.org/deliver/fulltext/cp/1/1/annurev.clinpsy.1.102803.143833.pdf";
function page() {
  const win = new Window({ url: href });
  win.document.write(readFileSync(new URL("../fixtures/annualreviews/marker-only.html", import.meta.url), "utf8"));
  return win.document as unknown as Document;
}
function plan(doc: Document) {
  const result = planExecution(doc, spec, expected, { access_mode: "delegated" });
  if ("assisted" in result) throw new Error(result.assisted);
  return JSON.parse(JSON.stringify(result, (_key, value) => value === null ? undefined : value)) as Plan;
}
async function execute(doc: Document, planned: Plan) {
  const previous = { document: globalThis.document, location: globalThis.location, HTMLElement: globalThis.HTMLElement };
  Object.assign(globalThis, { document: doc, location: new URL(href), HTMLElement: doc.defaultView!.HTMLElement });
  try { return await executePlannedPageEffect(planned, spec.download!); }
  finally { Object.assign(globalThis, previous); }
}

test("Annual Reviews plans and revalidates its captured empty PDF POST without clicking", async () => {
  const doc = page();
  let clicks = 0;
  doc.addEventListener("click", () => { clicks++; });
  const planned = plan(doc);
  expect(planned.method).toBe("post");
  expect(planned.url).toBe(endpoint);
  expect(await execute(doc, planned)).toEqual({ ok: true, url: endpoint });
  expect(clicks).toBe(0);
});

const unsafeChanges: [string, (doc: Document) => void][] = [
  ["GET form", doc => doc.querySelector("form")!.setAttribute("method", "GET")],
  ["missing action", doc => doc.querySelector("form")!.removeAttribute("action")],
  ["cross-origin action", doc => doc.querySelector("form")!.setAttribute("action", "https://other.example/paper.pdf")],
  ["non-PDF action", doc => doc.querySelector("form")!.setAttribute("action", "/account/delete")],
  ["credentials in action", doc => {
    const action = new URL(endpoint);
    action.username = "test-only-user";
    action.password = "test-only-password"; // betterleaks:allow -- synthetic negative-test input, never sent
    doc.querySelector("form")!.setAttribute("action", action.href);
  }],
  ["form field", doc => { doc.querySelector("form")!.innerHTML += '<input name="acceptTerms" value="yes">'; }],
  ["external form field", doc => { doc.querySelector("form")!.id = "download"; doc.body.innerHTML += '<input form="download" name="acceptTerms" value="yes">'; }],
  ["different encoding", doc => doc.querySelector("form")!.setAttribute("enctype", "multipart/form-data")],
];
for (const [name, change] of unsafeChanges) {
  test(`PDF POST refuses ${name} at planning and execution`, async () => {
    const doc = page();
    const planned = plan(doc);
    change(doc);
    expect("assisted" in planExecution(doc, spec, expected, {})).toBe(true);
    expect(await execute(doc, planned)).toMatchObject({ ok: false });
  });
}
test("PDF POST refuses a changed endpoint or requested work after planning", async () => {
  for (const change of [
    (doc: Document) => doc.querySelector("form")!.setAttribute("action", "/deliver/another.pdf"),
    (doc: Document) => doc.querySelector("meta[name=citation_title]")!.setAttribute("content", "Another review"),
  ]) {
    const doc = page();
    const planned = plan(doc);
    change(doc);
    expect(await execute(doc, planned)).toMatchObject({ ok: false });
  }
});
