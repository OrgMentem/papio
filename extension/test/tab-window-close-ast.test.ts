import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { parse } from "acorn";

type AstNode = {
  type?: string;
  start?: number;
  callee?: AstNode;
  object?: AstNode;
  property?: AstNode;
  key?: AstNode;
  id?: AstNode;
  init?: AstNode;
  value?: unknown;
  declarations?: AstNode[];
  computed?: boolean;
  name?: string;
  [key: string]: unknown;
};

type Transpiler = new (options: { loader: "ts" }) => { transformSync(source: string): string };
type Aliases = { receiver: Set<string>; remove: Set<string> };

function sourceFiles(root: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name);
    if (entry.isDirectory()) files.push(...sourceFiles(path));
    else if (entry.isFile() && path.endsWith(".ts")) files.push(path);
  }
  return files;
}

function childNode(value: unknown): AstNode | undefined {
  return typeof value === "object" && value !== null ? (value as AstNode) : undefined;
}

function segmentName(expression: AstNode | undefined): string | undefined {
  if (expression?.type !== "MemberExpression") return undefined;
  if (!expression.computed && expression.property?.type === "Identifier") return expression.property.name;
  if (expression.computed && expression.property?.type === "Literal" && expression.property.value === "remove") return "remove";
  if (expression.computed && expression.property?.type === "Literal" && typeof expression.property.value === "string") {
    return expression.property.value;
  }
  return undefined;
}

function receiverKind(expression: AstNode | undefined, aliases: Aliases): "tabs" | "windows" | undefined {
  const segment = segmentName(expression);
  if (segment === "tabs" || segment === "windows") return segment;
  if (expression?.type === "Identifier" && expression.name !== undefined && aliases.receiver.has(expression.name)) {
    return "tabs";
  }
  return undefined;
}

function isRemovalCall(node: AstNode, aliases: Aliases): boolean {
  if (node.type !== "CallExpression") return false;
  const callee = node.callee;
  if (callee?.type === "Identifier" && callee.name !== undefined && aliases.remove.has(callee.name)) return true;
  if (segmentName(callee) !== "remove") return false;
  const receiver = callee?.object;
  return receiverKind(receiver, aliases) !== undefined || receiver?.type === "CallExpression";
}
function isRemovalReference(node: AstNode, aliases: Aliases, parent: AstNode | undefined): boolean {
  if (node.type !== "MemberExpression" || segmentName(node) !== "remove") return false;
  if (parent?.type === "CallExpression" && parent.callee === node) return false;
  const receiver = node.object;
  return receiverKind(receiver, aliases) !== undefined || receiver?.type === "CallExpression";
}

function visitChildren(node: AstNode, visit: (child: AstNode) => void): void {
  for (const value of Object.values(node)) {
    if (value === null || typeof value !== "object") continue;
    if (Array.isArray(value)) {
      for (const child of value) if (child !== null && typeof child === "object") visit(child as AstNode);
    } else {
      visit(value as AstNode);
    }
  }
}

function collectAliases(tree: AstNode): Aliases {
  const aliases: Aliases = { receiver: new Set(), remove: new Set() };
  const visit = (node: AstNode): void => {
    if (node.type === "VariableDeclarator" && node.init !== undefined) {
      const kind = receiverKind(node.init, aliases);
      if (kind !== undefined && node.id?.type === "Identifier" && node.id.name !== undefined) {
        aliases.receiver.add(node.id.name);
      }
      if (kind !== undefined && node.id?.type === "ObjectPattern") {
        for (const property of (node.id.properties as AstNode[] | undefined) ?? []) {
          if (property.type !== "Property" || property.key?.name !== "remove") continue;
          const value = childNode(property.value);
          if (value?.type === "Identifier" && value.name !== undefined) {
            aliases.remove.add(value.name);
          }
        }
      }
    }
    visitChildren(node, visit);
  };
  visit(tree);
  return aliases;
}

function forbiddenCalls(): Map<string, number> {
  const root = join(import.meta.dir, "../src");
  const transpiler = new (Bun as unknown as { Transpiler: Transpiler }).Transpiler({ loader: "ts" });
  const found = new Map<string, number>();
  for (const file of sourceFiles(root)) {
    const source = readFileSync(file, "utf8");
    const javascript = transpiler.transformSync(source);
    const tree = parse(javascript, { ecmaVersion: "latest", sourceType: "module" }) as unknown as AstNode;
    const aliases = collectAliases(tree);
    const visit = (node: AstNode, enclosingSymbol: string | undefined, parent?: AstNode): void => {
      let symbol = enclosingSymbol;
      if (node.type === "FunctionDeclaration" && node.id?.name !== undefined) symbol = node.id.name;
      if (node.type === "MethodDefinition" && node.key?.name !== undefined) symbol = node.key.name;
      if (isRemovalCall(node, aliases) || isRemovalReference(node, aliases, parent)) {
        const site = `${relative(root, file)}:${symbol ?? "<module>"}`;
        found.set(site, (found.get(site) ?? 0) + 1);
      }
      visitChildren(node, (child) => visit(child, symbol, node));
    };
    visit(tree, undefined);
  }
  return found;
}

const KEEPALIVE_ALLOWLIST = new Map<string, { count: number; why: string }>([
  [
    "background.ts:closeOwnedTab",
    { count: 1, why: "The sole lifecycle close primitive; its synchronous four-predicate gate is the invariant boundary." },
  ],
  [
    "background.ts:realDeps",
    {
      count: 2,
      why: "The adapter is the API boundary that wires the capability; the primitives are the only places that decide to use it. Two: tabs.remove for the handoff surface, windows.remove for the toast window.",
    },
  ],
  [
    "background.ts:closeToastWindow",
    {
      count: 1,
      why: "ADR-0023's seventh surface is papio's OWN transient window, not a work surface, so closeOwnedTab's lifecycle gate does not apply to it. Its own gate is ownership: the window must still hold exactly one tab showing the toast page, because a researcher who closes the toast manually reports nothing and leaves this id naming a window the browser may have recycled.",
    },
  ],
  [
    "keepalive.ts:removeStaleTab",
    { count: 1, why: "Pinned tabs are deliberately skipped by the tab governor, so this retires the manager's superseded session tab." },
  ],
  [
    "keepalive.ts:closeTab",
    { count: 1, why: "Pinned tabs are deliberately skipped by the tab governor, so this releases the manager's no-longer-demanded session tab." },
  ],
  [
    "keepalive.ts:chromeKeepaliveAPI",
    { count: 1, why: "Pinned tabs are deliberately skipped by the tab governor, so this is the keepalive-only Chrome lifecycle seam." },
  ],
]);
// The adapter wires the remove capability, while lifecycle policy code may not
// call it directly: closeOwnedTab is the only decision boundary. The guard
// catches direct calls and value references and does not attempt deliberate
// indirection across function boundaries, imports/re-exports, or dynamic keys.
test("papio tab/window removals equal the named keepalive allowlist", () => {
  const expected = new Map<string, number>([...KEEPALIVE_ALLOWLIST].map(([site, entry]) => [site, entry.count]));
  const actual = forbiddenCalls();
  expect(actual).toEqual(expected);
});
