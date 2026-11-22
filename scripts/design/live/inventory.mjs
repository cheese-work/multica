#!/usr/bin/env node
/**
 * Live UI inventory scanner (CHE-6753 A1, slice L1).
 *
 * Run: node scripts/design/live/inventory.mjs --check   (compare design/live-inventory.json with a fresh scan)
 *      node scripts/design/live/inventory.mjs --write   (regenerate design/live-inventory.json)
 * Rule: inventory-stale. Not wired into any CI job yet.
 *
 * Lists, from the TypeScript AST of packages/views and apps/web:
 *   dialogs           JSX elements Dialog, AlertDialog, Sheet, Drawer
 *   toasts            calls of `toast` or `toast.<method>` imported from "sonner"
 *   decisionVariants  string literals of the decision unions and the batch, covered and action card shapes
 * Ids are `<path>#<enclosing symbol>#<ordinal>`, so a line shift keeps the id. `line` is informational.
 */
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import ts from "typescript";

export const INVENTORY_FILE = "design/live-inventory.json";
const SCAN_ROOTS = ["packages/views", "apps/web"];
const SKIP_DIRS = new Set(["node_modules", ".next", "dist", ".turbo", "__tests__", "e2e"]);
const SKIP_FILE = /\.(?:test|spec|stories)\.[^/]+$|\.d\.ts$/;
const SOURCE_FILE = /\.(?:ts|tsx)$/;
const DIALOG_ELEMENTS = new Set(["Dialog", "AlertDialog", "Sheet", "Drawer"]);
const TOAST_MODULE = "sonner";

export const DECISION_FILE = "packages/core/types/decision.ts";
const DECISION_UNIONS = [
  { symbol: "DecisionRequestCard.state", interface: "DecisionRequestCard", property: "state" },
  { symbol: "DecisionState", alias: "DecisionState" },
  { symbol: "DecisionCommentKind", alias: "DecisionCommentKind" },
];
const DECISION_SHAPES = ["DecisionBatchItem", "DecisionCoveredCard", "DecisionRequestAction"];

const scriptKind = (path) => (path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
const parse = (path, text) => ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, scriptKind(path));
const lineOf = (sf, node) => sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1;

const unwrap = (node) => {
  let n = node;
  while (n && (ts.isParenthesizedExpression(n) || ts.isAsExpression(n) || ts.isNonNullExpression(n) || ts.isSatisfiesExpression(n))) n = n.expression;
  return n;
};
const isFunctionLike = (n) => !!n && (ts.isArrowFunction(n) || ts.isFunctionExpression(n));
// `const X = memo(forwardRef(() => ...))` names X; `const x = value` does not name a scope.
const holdsFunction = (init) => {
  const n = unwrap(init);
  if (isFunctionLike(n)) return true;
  return !!n && ts.isCallExpression(n) && n.arguments.some(holdsFunction);
};

/** Name that a node adds to the enclosing-symbol chain, or null. */
function scopeName(node) {
  if ((ts.isFunctionDeclaration(node) || ts.isClassDeclaration(node)) && node.name) return node.name.text;
  if (ts.isFunctionDeclaration(node) || ts.isClassDeclaration(node)) return "default";
  if (ts.isMethodDeclaration(node) && ts.isIdentifier(node.name)) return node.name.text;
  if ((ts.isPropertyDeclaration(node) || ts.isPropertyAssignment(node)) && ts.isIdentifier(node.name) && node.initializer && holdsFunction(node.initializer)) return node.name.text;
  if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer && holdsFunction(node.initializer)) return node.name.text;
  return null;
}
function symbolOf(node) {
  const chain = [];
  for (let n = node.parent; n; n = n.parent) {
    const name = scopeName(n);
    if (name) chain.unshift(name);
  }
  return chain.length ? chain.join(".") : "<module>";
}

/** Local names that `import { toast } from "sonner"` binds, aliases included. */
function toastBindings(sf) {
  const names = new Set();
  for (const st of sf.statements) {
    if (!ts.isImportDeclaration(st) || !ts.isStringLiteral(st.moduleSpecifier) || st.moduleSpecifier.text !== TOAST_MODULE) continue;
    const named = st.importClause?.namedBindings;
    if (!named || !ts.isNamedImports(named)) continue;
    for (const el of named.elements) if ((el.propertyName ?? el.name).text === "toast") names.add(el.name.text);
  }
  return names;
}

const sortById = (items) => items.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

/** Dialog elements and toast calls of one source file. Pure: no file access. */
export function scanSource(path, text) {
  const sf = parse(path, text);
  const toastNames = toastBindings(sf);
  const dialogs = [];
  const toasts = [];
  const ordinals = new Map();
  // One counter per enclosing symbol, shared by dialogs and toasts: an id names one call site, never two.
  const nextId = (node) => {
    const symbol = symbolOf(node);
    const n = (ordinals.get(symbol) ?? 0) + 1;
    ordinals.set(symbol, n);
    return `${path}#${symbol}#${n}`;
  };
  const visit = (node) => {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      const tag = node.tagName.getText(sf);
      if (DIALOG_ELEMENTS.has(tag)) dialogs.push({ id: nextId(node), element: tag, line: lineOf(sf, node) });
    } else if (ts.isCallExpression(node) && toastNames.size) {
      const callee = node.expression;
      let method = null;
      if (ts.isIdentifier(callee) && toastNames.has(callee.text)) method = "toast";
      else if (ts.isPropertyAccessExpression(callee) && ts.isIdentifier(callee.expression) && toastNames.has(callee.expression.text)) method = callee.name.text;
      if (method) toasts.push({ id: nextId(node), method, line: lineOf(sf, node) });
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return { dialogs, toasts };
}

const literalsOf = (typeNode) => {
  const out = [];
  const walk = (t) => {
    if (ts.isUnionTypeNode(t)) t.types.forEach(walk);
    else if (ts.isParenthesizedTypeNode(t)) walk(t.type);
    else if (ts.isLiteralTypeNode(t) && ts.isStringLiteral(t.literal)) out.push(t.literal.text);
  };
  walk(typeNode);
  return out;
};

/** Decision union literals and card shapes of the decision types file. Pure. */
export function scanDecisionTypes(path, text) {
  const sf = parse(path, text);
  const aliases = new Map();
  const interfaces = new Map();
  for (const st of sf.statements) {
    if (ts.isTypeAliasDeclaration(st)) aliases.set(st.name.text, st);
    else if (ts.isInterfaceDeclaration(st)) interfaces.set(st.name.text, st);
  }
  const variants = [];
  for (const u of DECISION_UNIONS) {
    let type;
    if (u.alias) type = aliases.get(u.alias)?.type;
    else {
      const member = interfaces.get(u.interface)?.members.find((m) => ts.isPropertySignature(m) && m.name.getText(sf) === u.property);
      type = member?.type;
    }
    const literals = type ? literalsOf(type) : [];
    if (!literals.length) throw new Error(`decision-union-missing: ${u.symbol} not found in ${path}`);
    for (const literal of literals) variants.push({ id: `${path}#${u.symbol}#${literal}`, kind: "literal", line: lineOf(sf, type) });
  }
  for (const name of DECISION_SHAPES) {
    const decl = interfaces.get(name);
    if (!decl) throw new Error(`decision-union-missing: interface ${name} not found in ${path}`);
    variants.push({ id: `${path}#${name}#shape`, kind: "shape", line: lineOf(sf, decl) });
  }
  return variants;
}

function* walkSources(root, rel) {
  let entries;
  try {
    entries = readdirSync(join(root, rel), { withFileTypes: true });
  } catch {
    return;
  }
  for (const e of entries) {
    const next = `${rel}/${e.name}`;
    if (e.isDirectory()) {
      if (!SKIP_DIRS.has(e.name)) yield* walkSources(root, next);
    } else if (SOURCE_FILE.test(e.name) && !SKIP_FILE.test(e.name)) yield next;
  }
}

/** Fresh inventory of the tree at `root`. */
export function buildInventory(root) {
  const dialogs = [];
  const toasts = [];
  for (const base of SCAN_ROOTS) {
    for (const file of walkSources(root, base)) {
      const found = scanSource(file, readFileSync(join(root, file), "utf8"));
      dialogs.push(...found.dialogs);
      toasts.push(...found.toasts);
    }
  }
  const decisionVariants = scanDecisionTypes(DECISION_FILE, readFileSync(join(root, DECISION_FILE), "utf8"));
  sortById(dialogs);
  sortById(toasts);
  sortById(decisionVariants);
  return { version: 1, counts: { dialogs: dialogs.length, toasts: toasts.length, decisionVariants: decisionVariants.length }, dialogs, toasts, decisionVariants };
}

export const serializeInventory = (inv) => `${JSON.stringify(inv, null, 2)}\n`;

const allIds = (inv) => new Set(["dialogs", "toasts", "decisionVariants"].flatMap((k) => (inv?.[k] ?? []).map((i) => i.id)));

/** Errors of the committed file against a fresh scan: `inventory-stale` or none. */
export function checkInventory(root) {
  const fresh = buildInventory(root);
  let committedText = null;
  try {
    committedText = readFileSync(join(root, INVENTORY_FILE), "utf8");
  } catch {}
  if (committedText === serializeInventory(fresh)) return [];
  let committed = null;
  try {
    committed = JSON.parse(committedText);
  } catch {}
  const had = allIds(committed);
  const has = allIds(fresh);
  const added = [...has].filter((id) => !had.has(id));
  const removed = [...had].filter((id) => !has.has(id));
  const parts = [`${INVENTORY_FILE} differs from a fresh scan; run: node scripts/design/live/inventory.mjs --write`];
  if (committedText === null) parts.push("file is missing");
  if (added.length) parts.push(`added: ${added.join(", ")}`);
  if (removed.length) parts.push(`removed: ${removed.join(", ")}`);
  if (!added.length && !removed.length && committedText !== null) parts.push("same ids, other fields changed (line, method or counts)");
  return [{ code: "inventory-stale", detail: parts.join("; ") }];
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const args = process.argv.slice(2);
  const rootAt = args.indexOf("--root");
  const root = rootAt >= 0 ? args[rootAt + 1] : fileURLToPath(new URL("../../../", import.meta.url));
  const summary = (c) => `dialogs: ${c.dialogs}, toasts: ${c.toasts}, decision variants: ${c.decisionVariants}`;
  if (args.includes("--write")) {
    const inv = buildInventory(root);
    mkdirSync(dirname(join(root, INVENTORY_FILE)), { recursive: true });
    writeFileSync(join(root, INVENTORY_FILE), serializeInventory(inv));
    console.log(`live-inventory written: ${summary(inv.counts)}`);
  } else if (args.includes("--check")) {
    const errors = checkInventory(root);
    for (const x of errors) console.error(`${x.code}: ${x.detail}`);
    if (errors.length) process.exit(1);
    console.log(`live-inventory: ${summary(buildInventory(root).counts)}`);
  } else {
    console.error("usage: node scripts/design/live/inventory.mjs --check | --write [--root <dir>]");
    process.exit(2);
  }
}
