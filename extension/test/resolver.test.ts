// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

import { expect, test } from "bun:test";

import { routeResolverService } from "../src/resolver";
import { parseHTML } from "./harness";

function serviceLink(name: string, id: string): string {
  return `<a aria-label="${name} - (Opens in a new tab)" href="/view/action/uresolver.do?operation=resolveService&amp;package_service_id=${id}&amp;institutionId=2101">${name}</a>`;
}

test("resolver routing follows the institution-ranked first electronic service", async () => {
  const doc = parseHTML(
    `<a href="/discovery/blankIll">Get It from another library</a>` +
      serviceLink("JSTOR scholarly archive", "first") +
      serviceLink("ProQuest Central", "second"),
  );

  await expect(routeResolverService(doc)).resolves.toEqual({
    kind: "routed",
    service: "JSTOR scholarly archive - (Opens in a new tab)",
  });
});

test("resolver routing rejects request links and cross-origin lookalikes", async () => {
  const doc = parseHTML(
    `<a href="/discovery/blankIll">Get It from another library</a>` +
      `<a href="https://attacker.example/view/action/uresolver.do?operation=resolveService">Full text</a>`,
  );

  await expect(routeResolverService(doc)).resolves.toEqual({ kind: "no_service" });
});

test("resolver routing recognizes Alma's exact no-full-text marker", async () => {
  const doc = parseHTML(`<li>\n  No   full text available \n</li>`);

  await expect(routeResolverService(doc)).resolves.toEqual({ kind: "no_entitlement" });
});

test("resolver routing recognizes Primo's exact no-links marker", async () => {
  const doc = parseHTML(
    `<section data-qa="full_display_links_online_links">\nNo links are available for this record\n</section>`,
  );

  await expect(routeResolverService(doc)).resolves.toEqual({ kind: "no_entitlement" });
});

test("resolver routing treats near-match marker text and wrong elements as inconclusive", async () => {
  const nearMatch = parseHTML(`<li>No full text available for this record</li>`);
  const wrongElement = parseHTML(`<p>No links are available for this record</p>`);

  await expect(routeResolverService(nearMatch)).resolves.toEqual({ kind: "no_service" });
  await expect(routeResolverService(wrongElement)).resolves.toEqual({ kind: "no_service" });
});

test("resolver routing leaves empty and still-loading pages inconclusive", async () => {
  const empty = parseHTML("");
  const loading = parseHTML(`<main>Loading full text availability</main>`);

  await expect(routeResolverService(empty)).resolves.toEqual({ kind: "no_service" });
  await expect(routeResolverService(loading, 20)).resolves.toEqual({ kind: "no_service" });
});

test("resolver routing prioritizes a valid service over a no-entitlement marker", async () => {
  const doc = parseHTML(
    `<li>No full text available</li>` + serviceLink("JSTOR scholarly archive", "first"),
  );

  await expect(routeResolverService(doc)).resolves.toEqual({
    kind: "routed",
    service: "JSTOR scholarly archive - (Opens in a new tab)",
  });
});

test("resolver routing waits for Primo NDE's asynchronously rendered service list", async () => {
  const doc = parseHTML(`<main>Loading full text availability</main>`);
  const result = routeResolverService(doc, 200);
  doc.defaultView?.setTimeout(() => {
    doc.body.insertAdjacentHTML("beforeend", serviceLink("JSTOR scholarly archive", "late"));
  }, 10);

  await expect(result).resolves.toEqual({
    kind: "routed",
    service: "JSTOR scholarly archive - (Opens in a new tab)",
  });
});
