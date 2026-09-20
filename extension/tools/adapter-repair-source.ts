// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development tooling only: preserve comments and patch exactly the literal
// fields of the adapter that the production planner verified.
import { parseExpressionAt, type Expression, type ObjectExpression } from "acorn";
import { isDeepStrictEqual } from "node:util";
import type { AdapterSpec } from "../src/adapters/types";

function properties(node: ObjectExpression): Map<string, Expression> {
  const result = new Map<string, Expression>();
  for (const property of node.properties) {
    if (property.type !== "Property" || property.computed || property.method || property.shorthand || property.kind !== "init") {
      throw new Error("adapter source must contain plain data properties");
    }
    const key = property.key.type === "Identifier" ? property.key.name :
      property.key.type === "Literal" && typeof property.key.value === "string" ? property.key.value : null;
    if (key === null || result.has(key)) throw new Error("ambiguous adapter source property");
    result.set(key, property.value);
  }
  return result;
}

function valueOf(node: Expression): unknown {
  if (node.type === "Literal" && !("regex" in node) && typeof node.value !== "bigint") return node.value;
  if (node.type === "ObjectExpression") return Object.fromEntries([...properties(node)].map(([key, value]) => [key, valueOf(value)]));
  if (node.type === "ArrayExpression") return node.elements.map(element => {
    if (element === null || element.type === "SpreadElement") throw new Error("adapter arrays must contain literal data");
    return valueOf(element);
  });
  throw new Error("adapter source contains an unsupported expression");
}

export function patchedAdapterSource(source: string, before: AdapterSpec, after: AdapterSpec): string {
  const declaration = /export\s+const\s+adapters\s*:\s*AdapterSpec\[\]\s*=\s*/.exec(source);
  if (declaration === null) throw new Error("adapter declaration is unavailable");
  const array = parseExpressionAt(source, declaration.index + declaration[0].length, { ecmaVersion: "latest" });
  if (array.type !== "ArrayExpression") throw new Error("adapter declaration is not an array");
  const matches = array.elements.filter((node): node is ObjectExpression => {
    if (node?.type !== "ObjectExpression") return false;
    const id = properties(node).get("id");
    return id !== undefined && valueOf(id) === before.id;
  });
  if (matches.length !== 1) throw new Error("adapter source is missing or ambiguous");
  const adapter = matches[0]!;
  if (!isDeepStrictEqual(valueOf(adapter), before)) throw new Error("adapter source differs from the spec that was verified");

  const edits: { start: number; end: number; text: string }[] = [];
  function compare(node: Expression, oldValue: unknown, newValue: unknown): void {
    if (isDeepStrictEqual(oldValue, newValue)) return;
    if (node.type === "Literal" && typeof oldValue === "string" && typeof newValue === "string") {
      edits.push({ start: node.start, end: node.end, text: JSON.stringify(newValue) });
      return;
    }
    if (node.type === "ArrayExpression" && Array.isArray(oldValue) && Array.isArray(newValue) && oldValue.length === newValue.length) {
      node.elements.forEach((element, index) => {
        if (element === null || element.type === "SpreadElement") throw new Error("unsupported adapter array change");
        compare(element, oldValue[index], newValue[index]);
      });
      return;
    }
    if (node.type === "ObjectExpression" && oldValue !== null && newValue !== null && typeof oldValue === "object" && typeof newValue === "object") {
      const oldObject = oldValue as Record<string, unknown>, newObject = newValue as Record<string, unknown>;
      if (!isDeepStrictEqual(Object.keys(oldObject).sort(), Object.keys(newObject).sort())) throw new Error("repair cannot add or remove adapter fields");
      for (const [key, child] of properties(node)) compare(child, oldObject[key], newObject[key]);
      return;
    }
    throw new Error("repair may change only existing string literals");
  }
  compare(adapter, before, after);
  for (const edit of edits.sort((a, b) => b.start - a.start)) source = source.slice(0, edit.start) + edit.text + source.slice(edit.end);
  return source;
}
