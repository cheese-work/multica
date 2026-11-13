#!/usr/bin/env node
/**
 * CI `ui-pattern-ref` (CHE-5420 plan section 3, slice S2).
 *
 * 1. A PR that touches a UI path needs one pattern line in its description:
 *      pattern: <id>
 *      new-pattern: <id> — <reason>
 *      deviation: <id> — <reason>
 *    `pattern` and `deviation` ids must exist in design/patterns.yml. CI cannot prove the line is right; the
 *    reviewer's Sibling row does.
 * 2. A new raw <button|input|select|textarea> in packages/views fails when a @multica/ui primitive exists.
 *    JSX is parsed with the TypeScript compiler API, so a multiline element counts once. Only added lines count
 *    (a renamed file is compared with its old path). Exempt an element with `// ui-pattern-ref: allow <reason>`
 *    on or right above it, or with a `deviation:` line that names its file. A file may keep as many raw elements as
 *    the baseline lists for it; the baseline may only shrink.
 *
 * Run: node scripts/design/check-ui-pattern-ref.mjs [--base <ref>] [--head <ref>] [--body-file <path>] [--write-baseline]
 *   --base   default `git merge-base origin/main HEAD`
 *   --body   PR description: --body-file, else the PR_BODY environment variable
 *   --write-baseline  print the baseline of the head tree (JSON) and exit
 */
import { execFileSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import ts from "typescript";
import { parse } from "yaml";

const LEDGER = "design/patterns.yml";
const BASELINE = "scripts/design/ui-raw-baseline.json";

// UI paths: web and desktop app code, shared UI and views (the same set CHE-1485 Layer A calls "web app pages and
// components"). Tests, stories and e2e are not UI changes.
const UI_PATH = /^(?:apps\/(?:web|desktop)|packages\/(?:ui|views))\/.+\.(?:tsx|jsx|css)$/;
const NOT_UI = /(?:\.(?:test|spec|stories)\.[^/]+$|\/__tests__\/|\/e2e\/)/;
export const isUiPath = (p) => UI_PATH.test(p) && !NOT_UI.test(p);
const isViewsJsx = (p) => /^packages\/views\/.+\.(?:tsx|jsx)$/.test(p) && !NOT_UI.test(p);

// Only tags with a @multica/ui primitive are checked.
export const PRIMITIVES = {
  button: "Button from @multica/ui/components/ui/button",
  input: "Input from @multica/ui/components/ui/input",
  select: "Select from @multica/ui/components/ui/select",
  textarea: "Textarea from @multica/ui/components/ui/textarea",
};

const PATTERN_LINE = /^\s*(?:[-*]\s+)?(new-pattern|pattern|deviation):[ \t]*([a-z0-9]+(?:-[a-z0-9]+)*)\b(.*)$/i;
const ALLOW = /ui-pattern-ref:\s*allow\s+(\S.*?)\s*(?:\*\/\}?)?\s*$/;

const git = (cwd, args) => execFileSync("git", ["-c", "core.quotepath=false", ...args], { cwd, encoding: "utf8", maxBuffer: 1 << 28 });
const show = (cwd, ref, path) => {
  try {
    return execFileSync("git", ["show", `${ref}:${path}`], { cwd, encoding: "utf8", maxBuffer: 1 << 28, stdio: ["ignore", "pipe", "ignore"] });
  } catch {
    return null;
  }
};

export function parsePatternLines(body) {
  const visible = String(body ?? "").replace(/<!--[\s\S]*?-->/g, "").replace(/^ {0,3}(`{3,}|~{3,})[\s\S]*?^ {0,3}\1[`~]*[ \t]*$/gm, "");
  const lines = [];
  for (const text of visible.split("\n")) {
    const m = PATTERN_LINE.exec(text);
    if (!m) continue;
    const reason = m[3].replace(/^\s*(?:—|–|--|-|:)\s*/, "").trim();
    lines.push({ kind: m[1].toLowerCase(), id: m[2], reason, text: text.trim() });
  }
  return lines;
}

// One `git diff -M -U0`: per new path, its status, old path and added line numbers.
function readDiff(cwd, base, head) {
  const out = git(cwd, ["diff", "-M", "-U0", "--no-color", "--no-ext-diff", base, head]);
  const files = new Map();
  let cur = null;
  let line = 0;
  for (const text of out.split("\n")) {
    if (text.startsWith("diff --git ")) {
      const s = text.slice("diff --git ".length);
      const path = s.slice(2, (s.length - 1) / 2 + 1 - 1);
      cur = { path, oldPath: path, status: "M", added: new Set() };
      files.set(path, cur);
    } else if (!cur) continue;
    else if (text.startsWith("new file mode")) cur.status = "A";
    else if (text.startsWith("deleted file mode")) cur.status = "D";
    else if (text.startsWith("rename from ")) {
      cur.status = "R";
      cur.oldPath = text.slice("rename from ".length);
    } else if (text.startsWith("rename to ")) {
      files.delete(cur.path);
      cur.path = text.slice("rename to ".length);
      files.set(cur.path, cur);
    } else if (text.startsWith("@@")) {
      line = Number(/\+(\d+)/.exec(text)[1]);
    } else if (text.startsWith("+") && !text.startsWith("+++")) cur.added.add(line++);
  }
  return files;
}

export function rawElements(source, fileName = "x.tsx") {
  const sf = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const found = [];
  const visit = (node) => {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      const tag = node.tagName.getText(sf);
      if (Object.hasOwn(PRIMITIVES, tag)) {
        const pos = (p) => sf.getLineAndCharacterOfPosition(p).line + 1;
        found.push({ tag, line: pos(node.getStart(sf)), endLine: pos(node.getEnd()) });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return found;
}

const allowReason = (lines, el) => {
  for (let n = Math.max(1, el.line - 1); n <= el.endLine; n++) {
    const m = ALLOW.exec(lines[n - 1] ?? "");
    if (m) return m[1];
  }
  return null;
};

const readJson = (text) => {
  try {
    return text == null ? null : JSON.parse(text);
  } catch {
    return null;
  }
};

export function checkUiPatternRef({ cwd = process.cwd(), base, head = "HEAD", body = "" }) {
  const errors = [];
  const allowed = [];
  const files = readDiff(cwd, base ?? git(cwd, ["merge-base", "origin/main", head]).trim(), head);
  const uiFiles = [...files.values()]
    .filter((f) => isUiPath(f.path) || isUiPath(f.oldPath))
    .map((f) => f.path)
    .sort();
  const lines = parsePatternLines(body);

  // 1. pattern line
  const ledgerText = show(cwd, head, LEDGER);
  const ids = new Set((ledgerText ? parse(ledgerText) ?? [] : []).map((e) => e?.id));
  const uiChange = uiFiles.length > 0 || /^\s*ui-change:\s*yes\b/im.test(String(body));
  if (uiChange && lines.length === 0) {
    const what = uiFiles.length ? `${uiFiles.length} UI file(s) changed (first: ${uiFiles[0]})` : "ui-change: yes";
    errors.push(
      `ui-pattern-ref: ${what} but the PR description has no pattern line. Fix: add \`pattern: <id>\` ` +
        `(or \`new-pattern: <id> — <reason>\` / \`deviation: <id> — <reason>\`) as its own line; ids are in ${LEDGER}.`,
    );
  }
  for (const l of lines) {
    if (l.kind !== "pattern" && !l.reason) {
      errors.push(`ui-pattern-ref: \`${l.text}\` needs a reason. Fix: write \`${l.kind}: ${l.id} — <reason>\`.`);
    }
    if (l.kind !== "new-pattern" && !ids.has(l.id)) {
      errors.push(`ui-pattern-ref: unknown pattern id \`${l.id}\`. Fix: use an id from ${LEDGER}, or \`new-pattern: ${l.id} — <reason>\` if none fits.`);
    }
  }

  // 2. raw elements
  const baseline = readJson(show(cwd, head, BASELINE)) ?? {};
  for (const f of [...files.values()].sort((a, b) => a.path.localeCompare(b.path))) {
    if (f.status === "D" || f.added.size === 0 || !isViewsJsx(f.path)) continue;
    const source = show(cwd, head, f.path);
    if (source == null) continue;
    const els = rawElements(source, f.path);
    const keep = baseline[f.path] ?? baseline[f.oldPath] ?? 0;
    if (els.length <= keep) continue;
    const srcLines = source.split("\n");
    const deviation = lines.find((l) => l.kind === "deviation" && l.reason.includes(f.path));
    for (const el of els.filter((e) => f.added.has(e.line))) {
      const where = `${f.path}:${el.line}`;
      const reason = allowReason(srcLines, el);
      if (reason) allowed.push(`${where} <${el.tag}> allowed inline: ${reason}`);
      else if (deviation) allowed.push(`${where} <${el.tag}> allowed by deviation line: ${deviation.text}`);
      else {
        errors.push(
          `${where} raw <${el.tag}>. Fix: use ${PRIMITIVES[el.tag]}, or add \`// ui-pattern-ref: allow <reason>\` on the element, ` +
            `or a \`deviation:\` line that names ${f.path}.`,
        );
      }
    }
  }

  // 3. baseline only shrinks
  const before = readJson(show(cwd, base ?? git(cwd, ["merge-base", "origin/main", head]).trim(), BASELINE));
  if (before) {
    const moved = new Map([...files.values()].filter((f) => f.status === "R").map((f) => [f.path, f.oldPath]));
    for (const [path, count] of Object.entries(baseline)) {
      const was = before[path] ?? before[moved.get(path)];
      if (was === undefined || count > was) {
        errors.push(
          `${BASELINE} may only shrink: ${path} lists ${count}, base lists ${was ?? "nothing"}. ` +
            `Fix: lower or remove the entry and replace the raw element with a @multica/ui primitive.`,
        );
      }
    }
  }
  return { errors, allowed, uiFiles };
}

export function writeBaseline({ cwd = process.cwd(), head = "HEAD" } = {}) {
  const paths = git(cwd, ["ls-tree", "-r", "--name-only", head, "--", "packages/views"]).split("\n").filter(isViewsJsx);
  const out = {};
  for (const p of paths) {
    const n = rawElements(show(cwd, head, p), p).length;
    if (n) out[p] = n;
  }
  return out;
}

function main(argv) {
  const arg = (name) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : undefined);
  const head = arg("--head") ?? "HEAD";
  if (argv.includes("--write-baseline")) {
    process.stdout.write(`${JSON.stringify(writeBaseline({ head }), null, 2)}\n`);
    return 0;
  }
  const bodyFile = arg("--body-file");
  const body = bodyFile ? readFileSync(bodyFile, "utf8") : (process.env.PR_BODY ?? "");
  const r = checkUiPatternRef({ base: arg("--base"), head, body });
  const summary = [
    `ui-pattern-ref: ${r.uiFiles.length} UI file(s), ${r.allowed.length} allowed raw element(s), ${r.errors.length} error(s)`,
    ...r.allowed.map((a) => `  allowed: ${a}`),
  ];
  console.log(summary.join("\n"));
  if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${summary.join("\n")}\n`);
  for (const e of r.errors) console.error(`::error::${e}`);
  return r.errors.length ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) process.exit(main(process.argv.slice(2)));
