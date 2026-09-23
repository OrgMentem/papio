// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parseBrowserMessage, parseBrowserMessageBytes, ProtocolError } from "../src/protocol";

const root = join(import.meta.dir, "..", "..", "testdata", "protocol");
const cases: {
  name: string; base: string; path: string[]; value?: unknown;
  remove?: boolean; valid: boolean; prefix?: string; repeat?: number;
}[] = JSON.parse(readFileSync(join(root, "native-viewer-save-cases.json"), "utf8"));
for (const entry of cases) test(`native viewer contract: ${entry.name}`, () => {
  const frame = JSON.parse(readFileSync(join(root, "valid", `browser-native-viewer-save-${entry.base}.json`), "utf8"));
  let target = frame;
  for (const key of entry.path.slice(0, -1)) target = target[key];
  const key = entry.path.at(-1)!;
  if (entry.remove) delete target[key];
  else Object.defineProperty(target, key, {
    value: entry.repeat ? (entry.prefix ?? "") + (entry.value as string).repeat(entry.repeat) : entry.value,
    enumerable: true, configurable: true, writable: true,
  });
  if (entry.valid) {
    expect(parseBrowserMessage(frame)).toEqual(frame);
    expect(parseBrowserMessageBytes(JSON.stringify(frame))).toEqual(frame);
  } else {
    expect(() => parseBrowserMessage(frame)).toThrow(ProtocolError);
    expect(() => parseBrowserMessageBytes(JSON.stringify(frame))).toThrow(ProtocolError);
  }
});

const rawCases: { name: string; raw: string }[] = JSON.parse(readFileSync(join(root, "native-viewer-save-raw-cases.json"), "utf8"));
for (const entry of rawCases) test(`native viewer wire: ${entry.name}`, () => {
  expect(() => parseBrowserMessageBytes(entry.raw)).toThrow("duplicate");
});

test("native viewer source URL errors do not disclose source material", () => {
  const frame = JSON.parse(readFileSync(join(root, "valid", "browser-native-viewer-save-prepare.json"), "utf8"));
  for (const source of ["https://synthetic:secret@example.invalid/p", "https://example.invalid/%invalid-synthetic"]) { // gitleaks:allow -- synthetic userinfo rejection fixture
    frame.payload.source_url = source;
    try {
      parseBrowserMessage(frame);
      throw new Error("invalid source accepted");
    } catch (error) {
      expect(error).toBeInstanceOf(ProtocolError);
      expect(String(error)).not.toContain("synthetic");
      expect(String(error)).not.toContain("https://");
    }
  }
});
