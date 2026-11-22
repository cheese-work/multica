#!/usr/bin/env node
// Fork-only UI scan (CHE-5420 S0a). Re-run: `node scripts/design/scan-fork-ui.mjs`
//
// Lists .tsx files (no tests) that exist at --ref but not at --base, counts raw
// <button|input|select|textarea> tags and hex colour literals per file, and groups
// the result by surface. Read-only: it only runs `git diff` and `git cat-file`.
//
//   --ref   git ref to scan            (default HEAD)
//   --base  upstream ref to compare    (default upstream/main; run `git fetch upstream main` first)
//   --scope path prefix for the scan   (default packages/views)
//   --json  print JSON instead of Markdown
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const RAW_ELEMENT = /<(?:button|input|select|textarea)(?=[\s>/])/g;
// Hex literal right after a quote or backtick, length 3/4/6/8. Skips "(#7113)" in comments and "#section" hrefs.
const HEX_COLOR = /(?<=["'`])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})(?![0-9a-zA-Z_-])/g;

const SURFACES = ["issues", "chat/DM", "settings", "wakeups", "hierarchy", "other"];

export const countRawElements = (src) => (src.match(RAW_ELEMENT) ?? []).length;
export const countHexColors = (src) => (src.match(HEX_COLOR) ?? []).length;

// Name beats directory: wakeup-* and hierarchy-* files form their own surfaces.
export function classifySurface(path) {
  const name = path.split("/").pop();
  if (/wakeup/i.test(name)) return "wakeups";
  if (/hierarchy/i.test(path)) return "hierarchy";
  if (/\/chat\//.test(path)) return "chat/DM";
  if (/\/issues\//.test(path)) return "issues";
  if (/\/settings\//.test(path)) return "settings";
  return "other";
}

const isUiFile = (p) => p.endsWith(".tsx") && !/\.(test|spec)\.tsx$/.test(p);
const git = (cwd, args, opts = {}) =>
  execFileSync("git", args, { cwd, encoding: "utf8", maxBuffer: 1 << 28, ...opts });

function readBlobs(cwd, ref, paths) {
  if (paths.length === 0) return new Map();
  const out = execFileSync("git", ["cat-file", "--batch"], {
    cwd,
    input: paths.map((p) => `${ref}:${p}\n`).join(""),
    maxBuffer: 1 << 30,
  });
  const blobs = new Map();
  let pos = 0;
  for (const p of paths) {
    const eol = out.indexOf(10, pos);
    const [, type, size] = out.toString("utf8", pos, eol).split(" ");
    if (type !== "blob") throw new Error(`${ref}:${p} is not a blob`);
    const start = eol + 1;
    blobs.set(p, out.toString("utf8", start, start + Number(size)));
    pos = start + Number(size) + 1;
  }
  return blobs;
}

export function scan({ cwd = process.cwd(), ref = "HEAD", base = "upstream/main", scope = "packages/views" } = {}) {
  const lines = (s) => s.split("\n").filter(Boolean);
  const added = lines(git(cwd, ["diff", "--no-renames", "--diff-filter=A", "--name-only", base, ref, "--", scope]));
  const all = lines(git(cwd, ["ls-tree", "-r", "--name-only", ref, "--", scope]));
  const forkFiles = added.filter(isUiFile).sort();
  const scopeFiles = all.filter(isUiFile);
  const blobs = readBlobs(cwd, ref, scopeFiles);
  const rawIn = (p) => countRawElements(blobs.get(p));
  // Git's rename detection pairs a fork file with a deleted upstream file. These stay in the list
  // (they are fork surfaces) but are flagged: without them the count is the plan's 114.
  const renames = new Map(
    lines(git(cwd, ["diff", "--diff-filter=R", "--name-status", base, ref, "--", scope]))
      .map((l) => l.split("\t"))
      .map(([, from, to]) => [to, from]),
  );
  const files = forkFiles.map((path) => ({
    path,
    ...(renames.has(path) && { renamedFrom: renames.get(path) }),
    surface: classifySurface(path),
    raw: rawIn(path),
    hex: countHexColors(blobs.get(path)),
  }));
  return {
    ref: git(cwd, ["rev-parse", ref]).trim(),
    base: git(cwd, ["rev-parse", base]).trim(),
    scope,
    forkOnlyFiles: files.length,
    renamedFromUpstream: files.filter((f) => f.renamedFrom).length,
    withRawElements: files.filter((f) => f.raw > 0).length,
    withHex: files.filter((f) => f.hex > 0).length,
    scopeFiles: scopeFiles.length,
    scopeWithRawElements: scopeFiles.filter((p) => rawIn(p) > 0).length,
    files,
  };
}

export function toMarkdown(r) {
  const pct = (a, b) => `${((100 * a) / b).toFixed(0)}%`;
  const out = [
    `# Fork-only UI scan`,
    ``,
    `- ref: \`${r.ref}\``,
    `- base: \`${r.base}\` (upstream)`,
    `- scope: \`${r.scope}\`, \`.tsx\` files, tests excluded`,
    `- fork-only UI files: **${r.forkOnlyFiles}** (${r.forkOnlyFiles - r.renamedFromUpstream} new + ${r.renamedFromUpstream} git-detected renames of upstream files, marked "renamed from")`,
    `- with raw \`<button|input|select|textarea>\`: **${r.withRawElements}** (${pct(r.withRawElements, r.forkOnlyFiles)})`,
    `- with a hex colour literal: **${r.withHex}**`,
    `- all of \`${r.scope}\`: ${r.scopeWithRawElements} of ${r.scopeFiles} files with raw elements (${pct(r.scopeWithRawElements, r.scopeFiles)})`,
    ``,
    `| Surface | Files | With raw | Raw tags | Hex |`,
    `| --- | ---: | ---: | ---: | ---: |`,
  ];
  for (const s of SURFACES) {
    const f = r.files.filter((x) => x.surface === s);
    const sum = (k) => f.reduce((n, x) => n + x[k], 0);
    out.push(`| ${s} | ${f.length} | ${f.filter((x) => x.raw > 0).length} | ${sum("raw")} | ${sum("hex")} |`);
  }
  for (const s of SURFACES) {
    const f = r.files.filter((x) => x.surface === s);
    if (f.length === 0) continue;
    out.push(``, `## ${s} (${f.length})`, ``, `| File | Raw | Hex |`, `| --- | ---: | ---: |`);
    for (const x of f) out.push(`| \`${x.path}\`${x.renamedFrom ? ` (renamed from \`${x.renamedFrom}\`)` : ""} | ${x.raw || ""} | ${x.hex || ""} |`);
  }
  return out.join("\n") + "\n";
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const opt = (name, dflt) => {
    const i = args.indexOf(`--${name}`);
    return i >= 0 ? args[i + 1] : dflt;
  };
  const r = scan({ ref: opt("ref", "HEAD"), base: opt("base", "upstream/main"), scope: opt("scope", "packages/views") });
  process.stdout.write(args.includes("--json") ? JSON.stringify(r, null, 2) + "\n" : toMarkdown(r));
}
