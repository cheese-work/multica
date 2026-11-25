import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";

// Keeps Multica workspace data out of this public fork (CHE-9778). Three checks on one change:
//   1. paths: no workspace artifact path (plans, evidence, policy files) in the head tree;
//   2. names: no agent, product-org or person name in an ADDED line, a commit message or the PR title/body;
//   3. the report names the rule, file and line only. It never prints the matched text.
// Existing lines are not scanned: the name rule applies to what a change adds. The rules file is read from the
// trusted checkout, never from the PR head. Reads git objects as data; runs nothing from the change.

const DEFAULT_RULES = fileURLToPath(new URL("./public-fork-guard/rules.json", import.meta.url));

const matcher = (rule) => new RegExp(rule.pattern, rule.ignoreCase ? "i" : "");

export function loadRules(path) {
  const rules = JSON.parse(readFileSync(path, "utf8"));
  const ok = rules && typeof rules.fix === "string" && Array.isArray(rules.exempt) && Array.isArray(rules.paths) && Array.isArray(rules.names);
  if (!ok) throw new Error(`${path}: expected {fix, exempt[], paths[], names[]}`);
  const seen = new Set();
  for (const rule of [...rules.paths, ...rules.names]) {
    if (typeof rule.id !== "string" || typeof rule.pattern !== "string" || seen.has(rule.id)) throw new Error(`${path}: each rule needs a unique id and a pattern`);
    seen.add(rule.id);
    matcher(rule);
  }
  return rules;
}

const exempt = (file, rules) => rules.exempt.some((e) => file === e || (e.endsWith("/") && file.startsWith(e)));

// One hit per offending path: the first matching rule.
export function checkPaths(paths, rules) {
  const hits = [];
  for (const file of paths) {
    if (exempt(file, rules)) continue;
    const rule = rules.paths.find((r) => matcher(r).test(file));
    if (rule) hits.push({ file, line: 0, id: rule.id });
  }
  return hits;
}

// One hit per (line, rule).
export function checkText(label, text, rules) {
  const hits = [];
  text.split("\n").forEach((line, index) => {
    for (const rule of rules.names) if (matcher(rule).test(line)) hits.push({ file: label, line: index + 1, id: rule.id });
  });
  return hits;
}

// Added lines of a `git diff -U0` text.
export function checkAddedLines(diff, rules) {
  const hits = [];
  let file = null;
  let line = 0;
  for (const row of diff.split("\n")) {
    if (row.startsWith("+++ ")) {
      const target = row.slice(4).split("\t")[0];
      file = target === "/dev/null" ? null : target.replace(/^b\//, "");
    } else if (row.startsWith("@@")) {
      const m = /\+(\d+)/.exec(row);
      line = m ? Number(m[1]) : 0;
    } else if (file && row.startsWith("+") && !row.startsWith("+++")) {
      if (!exempt(file, rules)) {
        for (const rule of rules.names) if (matcher(rule).test(row.slice(1))) hits.push({ file, line, id: rule.id });
      }
      line += 1;
    } else if (file && row.startsWith(" ")) {
      line += 1;
    }
  }
  return hits;
}

const git = (args) => {
  const r = spawnSync("git", ["-c", "core.quotepath=off", ...args], { encoding: "utf8", maxBuffer: 1024 * 1024 * 1024 });
  if (r.error) throw r.error;
  return r;
};
const must = (args) => {
  const r = git(args);
  if (r.status !== 0) throw new Error(`git ${args[0]} failed: ${r.stderr.trim()}`);
  return r.stdout;
};

export function run({ base, head, rulesPath = DEFAULT_RULES, title = "", body = "" }) {
  const rules = loadRules(rulesPath);
  const hits = [];
  hits.push(...checkPaths(must(["ls-tree", "-r", "--name-only", head]).split("\n").filter(Boolean), rules));
  let diff = git(["diff", "-U0", "--no-color", "--no-ext-diff", "--diff-filter=AM", "--no-renames", `${base}...${head}`]);
  if (diff.status !== 0) diff = git(["diff", "-U0", "--no-color", "--no-ext-diff", "--diff-filter=AM", "--no-renames", base, head]);
  if (diff.status !== 0) throw new Error(`git diff failed: ${diff.stderr.trim()}`);
  hits.push(...checkAddedLines(diff.stdout, rules));
  must(["log", "--format=%H%x01%B%x00", `${base}..${head}`]).split("\0").filter((c) => c.trim()).forEach((entry) => {
    const [sha, message] = entry.trim().split("\u0001");
    hits.push(...checkText(`commit ${String(sha).slice(0, 12)}`, message ?? "", rules));
  });
  if (title) hits.push(...checkText("PR title", title, rules));
  if (body) hits.push(...checkText("PR body", body, rules));
  return { hits, fix: rules.fix };
}

function main(argv) {
  const opt = {};
  for (let i = 0; i < argv.length; i += 2) opt[argv[i].replace(/^--/, "")] = argv[i + 1];
  if (!opt.base || !opt.head) {
    console.error("usage: public-fork-guard.mjs --base <sha> --head <sha> [--rules <file>]  (PR_TITLE and PR_BODY from the environment)");
    return 2;
  }
  const { hits, fix } = run({ base: opt.base, head: opt.head, rulesPath: opt.rules, title: process.env.PR_TITLE ?? "", body: process.env.PR_BODY ?? "" });
  for (const h of hits) console.log(`::error file=${h.file}${h.line ? `,line=${h.line}` : ""}::public-fork-guard ${h.id}: ${fix}`);
  console.log(hits.length ? `public-fork-guard: ${hits.length} violation(s)` : "public-fork-guard: clean");
  return hits.length ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) process.exit(main(process.argv.slice(2)));
