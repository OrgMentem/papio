// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";

import { adapters } from "../src/adapters/types";
import { PROVIDERS } from "../src/capture";

test("capture providers cover every actual adapter id", () => {
  const providers = new Set<string>(PROVIDERS);
  expect(adapters.map((adapter) => adapter.id).filter((id) => !providers.has(id))).toEqual([]);
});

test("capture providers retain the historical elsevier alias alongside sciencedirect", () => {
  expect(PROVIDERS).toContain("elsevier");
  expect(PROVIDERS).toContain("sciencedirect");
  expect(new Set(PROVIDERS).size).toBe(PROVIDERS.length);
});
