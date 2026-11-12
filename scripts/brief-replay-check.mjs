#!/usr/bin/env node
// Replay check for brief-shrink PRs (CHE-4574). Static: compares the rules in an
// old and a new generated brief against a preserved-rule checklist. It proves
// text survived, not that a model still behaves the same.
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const squash = (s) => s.replace(/\s+/g, " ").trim();
// One unit per non-blank line, without list bullets or numbers (renumbering is not a change).
const units = (text) =>
  new Set(text.split("\n").map((l) => squash(l.replace(/^\s*(?:[-*]|\d+\.)\s+/, ""))).filter(Boolean));

export const parseChecklist = (text) =>
  text.split("\n").map((l) => squash(l.replace(/^\s*[-*]\s+/, ""))).filter((l) => l && !l.startsWith("#"));

// An empty checklist checks no rule, so it never passes.
export const compareBriefs = (oldText, newText, checklist) => {
  const r = diff(oldText, newText, checklist);
  return checklist.length ? r : { ...r, ok: false, emptyChecklist: true };
};

function diff(oldText, newText, checklist) {
  const [o, n] = [units(oldText), units(newText)];
  const [oldFlat, newFlat] = [squash(oldText), squash(newText)];
  const missing = checklist.filter((p) => !newFlat.includes(p));
  const notInOld = checklist.filter((p) => !oldFlat.includes(p));
  return {
    ok: !missing.length && !notInOld.length,
    missing,
    notInOld, // a checklist phrase the old brief never had cannot prove "preserved"
    removed: [...o].filter((u) => !n.has(u)),
    added: [...n].filter((u) => !o.has(u)),
  };
}

// Fixture dirs: every file in oldDir is compared with the same-named file in newDir.
// A rule is checked only where the old brief had it (a chat brief has no workflow).
export function compareDirs(oldDir, newDir, checklist) {
  const olds = readdirSync(oldDir).sort().map((name) => [name, readFileSync(join(oldDir, name), "utf8")]);
  const flat = olds.map(([, t]) => squash(t));
  const fixtures = olds.map(([name, oldText], i) => {
    if (!existsSync(join(newDir, name))) return { name, ok: false, vanished: true, missing: [], removed: [], added: [] };
    const here = checklist.filter((p) => flat[i].includes(p));
    return { name, ...diff(oldText, readFileSync(join(newDir, name), "utf8"), here) };
  });
  const notInOld = checklist.filter((p) => !flat.some((f) => f.includes(p)));
  const emptyChecklist = !checklist.length;
  return { ok: !emptyChecklist && !notInOld.length && fixtures.every((f) => f.ok), emptyChecklist, notInOld, fixtures };
}

// Real-brief mode. The platform keeps only each run's finished brief, not its inputs, so
// this checks the old side against real runs: every checklist phrase must appear in at
// least one recent real brief. A phrase no real brief has is a bad checklist.
// Layout: <root>/<workspace>/<task>/workdir/{CLAUDE,AGENTS}.md
export function findRecentBriefs(root, n) {
  const dirs = (d) => readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => join(d, e.name));
  const found = [];
  for (const ws of dirs(root)) for (const task of dirs(ws))
    for (const f of ["CLAUDE.md", "AGENTS.md"]) {
      const p = join(task, "workdir", f);
      if (existsSync(p)) found.push([p, statSync(p).mtimeMs]);
    }
  return found.sort((x, y) => y[1] - x[1]).slice(0, n).map(([p]) => p);
}

export function checkRealBriefs(files, checklist) {
  const flats = files.map((f) => squash(readFileSync(f, "utf8")));
  const counts = checklist.map((p) => [p, flats.filter((t) => t.includes(p)).length]);
  const neverSeen = counts.filter(([, c]) => !c).map(([p]) => p);
  const [noBriefs, emptyChecklist] = [!files.length, !checklist.length];
  return { ok: !noBriefs && !emptyChecklist && !neverSeen.length, noBriefs, emptyChecklist, briefs: files.length, counts, neverSeen };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  if (process.argv[2] === "--real") {
    const [listFile, root, n = "20"] = process.argv.slice(3);
    if (!root) {
      console.error("usage: brief-replay-check.mjs --real <checklist> <workspaces-root> [recent-count]");
      process.exit(2);
    }
    const r = checkRealBriefs(findRecentBriefs(root, Number(n)), parseChecklist(readFileSync(listFile, "utf8")));
    if (r.noBriefs) console.log("NO-BRIEFS: none found");
    if (r.emptyChecklist) console.log("EMPTY-CHECKLIST: no rule to check");
    for (const [p, c] of r.counts) console.log(`${c ? "seen" : "NEVER-SEEN"}: ${p}${c ? ` (${c}/${r.briefs} briefs)` : ""}`);
    console.log(r.ok ? "PASS" : "FAIL");
    process.exit(r.ok ? 0 : 1);
  }
  const [oldFile, newFile, listFile] = process.argv.slice(2);
  if (!listFile) {
    console.error("usage: brief-replay-check.mjs <old-brief> <new-brief> <checklist> | --real <checklist> <root> [n]");
    process.exit(2);
  }
  const read = (f) => readFileSync(f, "utf8");
  const list = parseChecklist(read(listFile));
  const dirs = statSync(oldFile).isDirectory();
  const r = dirs ? compareDirs(oldFile, newFile, list) : { ...compareBriefs(read(oldFile), read(newFile), list), fixtures: [] };
  const show = (pre, x) => {
    if (x.vanished) console.log(`${pre}VANISHED fixture`);
    for (const [tag, items] of [["MISSING", x.missing], ["NOT-IN-OLD", x.notInOld], ["REMOVED", x.removed], ["ADDED", x.added]])
      for (const item of items ?? []) console.log(`${pre}${tag}: ${item}`);
  };
  if (r.emptyChecklist) console.log("EMPTY-CHECKLIST: no rule to check");
  show("", r);
  for (const f of r.fixtures) { console.log(`== ${f.name}: ${f.ok ? "ok" : "FAIL"}`); show("  ", f); }
  console.log(r.ok ? "PASS" : "FAIL");
  process.exit(r.ok ? 0 : 1);
}
