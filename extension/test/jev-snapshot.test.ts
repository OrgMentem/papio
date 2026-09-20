// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { captureSnapshot } from "../tools/jev-snapshot";

test("snapshot discovers native and JS controls without provider selectors or form contents", () => {
  const snapshot = captureSnapshot(`<main><form action='/download'><input value='private-value'>
    <div class='button'>Download PDF</div></form><a href='/supplement'>Supplement</a>
    <button disabled>Purchase</button></main>`, "Obtain the requested article PDF", "https://example.org/article?token=secret");
  expect(snapshot.controls.map(control => control.label)).toEqual(["Download PDF", "Supplement", "Purchase"]);
  expect(snapshot.controls[2]?.disabled).toBe(true);
  expect(JSON.stringify(snapshot)).not.toContain("private-value");
  expect(JSON.stringify(snapshot)).not.toContain("token=secret");
  expect(snapshot.provenance.visibility_verified).toBe(false);
});

test("snapshot excludes hidden controls and scripts and strips contact addresses and URL queries", () => {
  const snapshot = captureSnapshot(`<main><script>throw new Error('script-ran')</script>
    <p>Contact person@example.org https://example.org/path?secret=private</p>
    <nav><a>Account</a></nav><div hidden><button>Hidden PDF</button></div>
    <button style='display:none'>Hidden download</button><a>Article PDF</a></main>`, "Find the PDF", "https://example.org/article");
  expect(snapshot.controls.map(control => control.label)).toEqual(["Article PDF"]);
  expect(snapshot.page.text).not.toContain("script-ran");
  expect(snapshot.page.text).not.toContain("person@example.org");
  expect(snapshot.page.text).not.toContain("secret=private");
});
