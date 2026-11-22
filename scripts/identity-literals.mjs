import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";

// Refuses workspace identity literals (agent and member UUIDs, host tokens, people names) in fork
// runtime code. It reads a git tree object with `git grep` and `git show`: it never checks out,
// installs or runs the code under test. The denylist and the count baseline are base-owned files,
// read from the trusted checkout (default: next to this script), never from the tree.

const DEFAULT_DIR = fileURLToPath(new URL("./identity-literals/", import.meta.url));
const SCOPE = ["server", "packages", "apps"];
const EXCLUDE = [
  "apps/docs",
  "server/migrations",
  ["glob", "**/testdata/**"],
  ["glob", "**/*_test.go"],
  ["glob", "**/*.test.ts"],
  ["glob", "**/*.test.tsx"],
].map((entry) => (Array.isArray(entry) ? `:(exclude,${entry[0]})${entry[1]}` : `:(exclude)${entry}`));

// String literals of a source text, with `//` and `#` line comments removed first. A backtick
// string stays open across lines; a quoted string ends at its closing quote or at the line end.
export function literalsOf(source) {
  const found = [];
  let raw = false;
  source.split("\n").forEach((line, index) => {
    const lineNo = index + 1;
    const push = (text) => text && found.push({ line: lineNo, text });
    let i = 0;
    if (raw) {
      const close = line.indexOf("`");
      push(close === -1 ? line : line.slice(0, close));
      if (close === -1) return;
      raw = false;
      i = close + 1;
    }
    while (i < line.length) {
      const ch = line[i];
      if (ch === "#" || (ch === "/" && line[i + 1] === "/")) break;
      if (ch === "`") {
        const close = line.indexOf("`", i + 1);
        push(line.slice(i + 1, close === -1 ? line.length : close));
        if (close === -1) raw = true;
        i = close === -1 ? line.length : close + 1;
      } else if (ch === '"' || ch === "'") {
        let j = i + 1;
        while (j < line.length && line[j] !== ch) j += line[j] === "\\" ? 2 : 1;
        push(line.slice(i + 1, Math.min(j, line.length)));
        i = j + 1;
      } else {
        i += 1;
      }
    }
  });
  return found;
}

const WORD = "A-Za-z0-9_";
const matcher = (pattern) => new RegExp(
  pattern.wholeWord ? `(?<![${WORD}])(?:${pattern.pattern})(?![${WORD}])` : pattern.pattern,
  pattern.ignoreCase ? "gi" : "g",
);

export function loadDenylist(path) {
  const list = JSON.parse(readFileSync(path, "utf8"));
  if (!list || !Array.isArray(list.patterns) || typeof list.fix !== "string") throw new Error(`${path}: expected {fix, patterns[]}`);
  const ids = new Set();
  for (const pattern of list.patterns) {
    if (typeof pattern.id !== "string" || typeof pattern.pattern !== "string" || ids.has(pattern.id)) throw new Error(`${path}: each pattern needs a unique id and a pattern`);
    ids.add(pattern.id);
    matcher(pattern);
  }
  return list;
}

export function loadBaseline(path) {
  const baseline = JSON.parse(readFileSync(path, "utf8"));
  const valid = baseline && typeof baseline === "object" && !Array.isArray(baseline)
    && Object.values(baseline).every((counts) => counts && typeof counts === "object" && !Array.isArray(counts)
      && Object.values(counts).every((count) => Number.isInteger(count) && count >= 0));
  if (!valid) throw new Error(`${path}: expected {file: {patternId: count}}`);
  return baseline;
}

const git = (cwd, args, options = {}) => {
  const result = spawnSync("git", args, { cwd, encoding: "utf8", maxBuffer: 256 * 1024 * 1024, ...options });
  if (result.error) throw result.error;
  return result;
};

function resolveTree(tree, cwd) {
  if (tree.startsWith("-")) throw new Error(`Invalid tree ${tree}`);
  const result = git(cwd, ["rev-parse", "--verify", "--quiet", `${tree}^{tree}`]);
  if (result.status !== 0) throw new Error(`Cannot resolve ${tree} to a tree object`);
  return result.stdout.trim();
}

// One `git cat-file --batch` process for all files: [[path, text]].
function readBlobs(cwd, sha, paths) {
  if (!paths.length) return [];
  const result = git(cwd, ["cat-file", "--batch"], { input: Buffer.from(paths.map((path) => `${sha}:${path}\n`).join("")), encoding: "buffer" });
  if (result.status !== 0) throw new Error(`git cat-file failed: ${result.stderr.toString().trim()}`);
  const out = result.stdout;
  const blobs = [];
  let offset = 0;
  for (const path of paths) {
    const end = out.indexOf(10, offset);
    const [, type, size] = out.subarray(offset, end).toString().split(" ");
    if (type !== "blob") throw new Error(`Cannot read ${path} from the tree`);
    const start = end + 1;
    blobs.push([path, out.subarray(start, start + Number(size)).toString("utf8")]);
    offset = start + Number(size) + 1;
  }
  return blobs;
}

// Every denylisted literal in the scoped files of a tree: [{file, line, id}].
export function scanTree({ tree, denylist, cwd = process.cwd() }) {
  const sha = resolveTree(tree, cwd);
  const files = new Set();
  for (const pattern of denylist.patterns) {
    const flags = [pattern.wholeWord && "-w", pattern.ignoreCase && "-i"].filter(Boolean);
    const result = git(cwd, ["grep", "-I", "-l", "-z", "-E", ...flags, "-e", pattern.pattern, sha, "--", ...SCOPE, ...EXCLUDE]);
    if (result.status > 1) throw new Error(`git grep failed: ${result.stderr.trim()}`);
    for (const name of result.stdout.split("\0")) if (name) files.add(name.slice(sha.length + 1));
  }
  const hits = [];
  const sorted = [...files].sort();
  for (const [file, source] of readBlobs(cwd, sha, sorted)) {
    const literals = literalsOf(source);
    for (const pattern of denylist.patterns) {
      const regex = matcher(pattern);
      for (const literal of literals) {
        for (const _ of literal.text.matchAll(regex)) hits.push({ file, line: literal.line, id: pattern.id });
      }
    }
  }
  return hits;
}

export function countHits(hits) {
  const counts = {};
  for (const { file, id } of hits) {
    counts[file] ??= {};
    counts[file][id] = (counts[file][id] ?? 0) + 1;
  }
  return counts;
}

// Failures: a (file, pattern) count above the baseline, or any hit in a file the baseline lacks.
// Stale: a baseline count that is now lower, so its line can be lowered or removed.
export function compare(hits, baseline, fix) {
  const counts = countHits(hits);
  const failures = [];
  for (const [file, byId] of Object.entries(counts)) {
    for (const [id, found] of Object.entries(byId)) {
      const allowed = baseline[file]?.[id] ?? 0;
      if (found <= allowed) continue;
      for (const hit of hits.filter((h) => h.file === file && h.id === id)) {
        failures.push(`${file}:${hit.line}  ${id}  (baseline allows ${allowed}, found ${found})  ${fix}`);
      }
    }
  }
  const stale = [];
  for (const [file, byId] of Object.entries(baseline)) {
    for (const [id, allowed] of Object.entries(byId)) {
      const found = counts[file]?.[id] ?? 0;
      if (found < allowed) stale.push(`${file} ${id} ${allowed} -> ${found}`);
    }
  }
  return { failures, stale };
}

function parseArgs(argv) {
  const options = { tree: "FETCH_HEAD", denylist: `${DEFAULT_DIR}denylist.json`, baseline: `${DEFAULT_DIR}baseline.json`, printBaseline: false };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    if (flag === "--print-baseline") options.printBaseline = true;
    else if (["--tree", "--denylist", "--baseline"].includes(flag) && argv[i + 1] !== undefined) options[flag.slice(2)] = argv[++i];
    else throw new Error(`Unknown argument ${flag}`);
  }
  return options;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const options = parseArgs(process.argv.slice(2));
    const denylist = loadDenylist(options.denylist);
    const hits = scanTree({ tree: options.tree, denylist });
    if (options.printBaseline) {
      console.log(JSON.stringify(countHits(hits), null, 2));
    } else {
      const { failures, stale } = compare(hits, loadBaseline(options.baseline), denylist.fix);
      if (stale.length) console.log(`Counts below the baseline; lower or remove these baseline lines:\n${stale.map((line) => `  ${line}`).join("\n")}`);
      if (failures.length) {
        console.error(`Identity literals FAILED:\n${failures.join("\n")}`);
        process.exitCode = 1;
      } else {
        console.log("Identity literals PASSED: no hit above the baseline.");
      }
    }
  } catch (error) {
    console.error(`Identity literals check could not run: ${error.message}`);
    process.exitCode = 2;
  }
}
