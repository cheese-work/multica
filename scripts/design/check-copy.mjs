#!/usr/bin/env node
/**
 * Copy check and locale key rule (CHE-5420 S3, plan section 3).
 *
 * Locale rule (blocking): a key removed or renamed in `en` must be removed in every other locale of
 * packages/views/locales.
 * Copy check (advisory, never fails): a new or changed `en` value that equals, after normalising case and
 * punctuation and a short synonym list, the `en` value of a key listed under a pattern's `copy` in
 * design/patterns.yml. Only `en` values are compared.
 *
 * Run: node scripts/design/check-copy.mjs --base <git-ref> [--advisory-out <md>] [--json-out <json>]
 */
import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { parsePatterns } from "./validate-patterns.mjs";

const LOCALES = "packages/views/locales";
export const MARKER = "<!-- copy-check -->";
// ponytail: exact match on a short synonym list; a fuzzy matcher only if the advisory log shows misses it would catch.
const SYNONYMS = [
  ["comment", ["comment", "reply", "annotation", "note"]],
  ["retry", ["retry", "try again"]],
];

export function* flatten(obj, prefix = "") {
  for (const [k, v] of Object.entries(obj ?? {})) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (typeof v === "string") yield [key, v];
    else if (v && typeof v === "object") yield* flatten(v, key);
  }
}

export function normalise(value, synonyms = true) {
  let s = ` ${value.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim()} `;
  if (synonyms) {
    for (const [canon, words] of SYNONYMS) for (const w of words) s = s.replaceAll(` ${w} `, ` ${canon} `);
  }
  return s.trim();
}

// en has `_one` + `_other`, zh only `_other`: compare plural forms as one key (same rule as locales/parity.test.ts).
const pluralKey = (key) => key.replace(/_(one|other)$/, "_count");
const keySet = (ns) => new Set([...flatten(ns)].map(([k]) => pluralKey(k)));

/** trees: { <lang>: { <ns>: <json> } } */
export function localeErrors(base, head) {
  const errors = [];
  for (const [ns, json] of Object.entries(base.en ?? {})) {
    const now = keySet(head.en?.[ns]);
    const removed = [...keySet(json)].filter((k) => !now.has(k));
    for (const lang of Object.keys(head).filter((l) => l !== "en")) {
      const kept = keySet(head[lang]?.[ns]);
      for (const k of removed) {
        if (kept.has(k)) errors.push(`${ns}:${k} was removed or renamed in en but is still in ${lang}; change it in every locale`);
      }
    }
  }
  return errors;
}

/** candidates: [{ns, key, value}]; refs: [{pattern, key: "ns:key", value}] */
export function copyHits(candidates, refs) {
  const hits = [];
  for (const c of candidates) {
    const norm = normalise(c.value);
    if (!norm) continue;
    for (const r of refs) {
      if (r.key === `${c.ns}:${c.key}` || normalise(r.value) !== norm) continue;
      hits.push({
        id: `${r.pattern}:${c.ns}:${c.key}`,
        pattern: r.pattern,
        key: `${c.ns}:${c.key}`,
        value: c.value,
        patternKey: r.key,
        patternValue: r.value,
        kind: normalise(c.value, false) === normalise(r.value, false) ? "exact" : "synonym",
      });
    }
  }
  return hits;
}

const git = (root, ...args) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8", maxBuffer: 1 << 26 });

// One `git cat-file --batch` process: a `git show` per file takes seconds each on a large repo.
function showAll(root, ref, files) {
  const out = execFileSync("git", ["-C", root, "cat-file", "--batch"], {
    input: files.map((f) => `${ref}:${f}\n`).join(""),
    maxBuffer: 1 << 28,
  });
  const texts = [];
  for (let i = 0; texts.length < files.length; ) {
    const nl = out.indexOf("\n", i);
    const size = Number(out.subarray(i, nl).toString().split(" ")[2]);
    if (!Number.isInteger(size)) throw new Error(`git cat-file: ${out.subarray(i, nl)}`);
    texts.push(out.subarray(nl + 1, nl + 1 + size).toString("utf8"));
    i = nl + 2 + size;
  }
  return texts;
}

function loadTrees(root, ref) {
  const files = ref
    ? git(root, "ls-tree", "-r", "--name-only", ref, "--", LOCALES).split("\n").filter((f) => f.endsWith(".json"))
    : readdirSync(join(root, LOCALES), { recursive: true }).filter((f) => f.endsWith(".json")).map((f) => `${LOCALES}/${f}`);
  const texts = ref ? showAll(root, ref, files) : files.map((f) => readFileSync(join(root, f), "utf8"));
  const trees = {};
  files.forEach((file, i) => {
    const [lang, name] = file.slice(LOCALES.length + 1).split("/");
    if (name) (trees[lang] ??= {})[name.replace(/\.json$/, "")] = JSON.parse(texts[i]);
  });
  return trees;
}

export function checkRepo(root, baseRef) {
  const base = loadTrees(root, baseRef);
  const head = loadTrees(root, null);
  const errors = localeErrors(base, head);
  const flat = (trees) => Object.fromEntries(Object.entries(trees.en ?? {}).map(([ns, json]) => [ns, new Map(flatten(json))]));
  const baseEn = flat(base);
  const headEn = flat(head);
  const refs = parsePatterns(readFileSync(join(root, "design/patterns.yml"), "utf8")).flatMap((p) =>
    Object.values(p.copy ?? {}).flatMap((key) => {
      const [ns, k] = key.split(/:(.*)/s);
      const value = headEn[ns]?.get(k);
      return value === undefined ? [] : [{ pattern: p.id, key, value }];
    }),
  );
  const candidates = Object.entries(headEn).flatMap(([ns, values]) =>
    [...values].filter(([k, v]) => baseEn[ns]?.get(k) !== v).map(([key, value]) => ({ ns, key, value })),
  );
  return { errors, hits: copyHits(candidates, refs) };
}

export function advisoryReport(hits) {
  const rows = hits.map((h) => `| \`${h.id}\` | \`${h.key}\` "${h.value}" | \`${h.patternKey}\` "${h.patternValue}" (\`${h.pattern}\`) | ${h.kind} | |`);
  return [
    MARKER,
    "### Copy check (advisory)",
    "A new or changed English label matches the label of an existing pattern. This does not fail the PR. Use the pattern's label, or say in the Sibling row why not. The Sibling-row reviewer labels each hit true or false by hit id.",
    "",
    "| Hit id | New copy | Pattern copy | Match | Label (true/false) |",
    "| --- | --- | --- | --- | --- |",
    ...rows,
  ].join("\n");
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const arg = (name) => process.argv[process.argv.indexOf(name) + 1];
  const has = (name) => process.argv.includes(name);
  if (!has("--base")) {
    console.error("usage: check-copy.mjs --base <git-ref> [--advisory-out <md>] [--json-out <json>]");
    process.exit(2);
  }
  const { errors, hits } = checkRepo(resolve(import.meta.dirname, "../.."), arg("--base"));
  if (hits.length && has("--advisory-out")) writeFileSync(arg("--advisory-out"), advisoryReport(hits) + "\n");
  if (has("--json-out")) writeFileSync(arg("--json-out"), JSON.stringify(hits, null, 2) + "\n");
  for (const h of hits) console.log(`advisory: ${h.id} (${h.kind}) matches ${h.patternKey}`);
  for (const e of errors) console.error(`error: ${e}`);
  console.log(`copy check: ${hits.length} advisory hits, ${errors.length} locale errors`);
  process.exit(errors.length ? 1 : 0);
}
