#!/usr/bin/env node
/**
 * Validates design/patterns.yml (CHE-5420 S1, plan sections 1 and 2).
 *
 * Offline checks: component and docs paths exist, copy keys exist in the
 * English locale, frame.state rules, and the allowed frame.state moves against
 * the base branch's ledger.
 *
 * Run: node scripts/design/validate-patterns.mjs [--base <git-ref>]
 */
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { parse } from "yaml";

const LEDGER = "design/patterns.yml";
const ID = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const FRAME_URL = /^https:\/\/[^/\s?#]+\/c\/[\w-]+\?frame=[\w-]+$/;
const KEY = /^([a-z0-9_-]+):(\S+)$/i;
const FIELDS = new Set(["id", "intent", "origin", "status", "components", "docs", "copy", "surfaces", "states", "frame", "build", "open"]);
const MOVES = new Set(["pending>patterns", "locked>patterns"]);

export function parsePatterns(text) {
  const entries = parse(text);
  if (!Array.isArray(entries)) throw new Error(`${LEDGER} must be a YAML list of entries`);
  return entries;
}

// Rows of the "Approved revisions" table in DESIGN.md: [topic, revision cell].
function approvedRows(designMd) {
  const section = designMd.split(/^## Approved revisions\s*$/m)[1]?.split(/^## /m)[0] ?? "";
  return section
    .split("\n")
    .filter((line) => line.startsWith("|"))
    .map((line) => line.split("|").slice(1, -1).map((c) => c.trim()))
    .filter((cells) => cells.length >= 2);
}

const hasToken = (cell, token) => cell.split(/[^\w-]+/).includes(token);
const isList = (v) => Array.isArray(v) && v.every((x) => typeof x === "string" && x);

function checkFrame(id, frame, rows, errors, warnings, status) {
  if (!frame || typeof frame !== "object") return errors.push(`${id}: frame is required`);
  const extra = (allowed) => Object.keys(frame).filter((k) => !allowed.includes(k));
  switch (frame.state) {
    case "pending":
      if (status === "active") errors.push(`${id}: frame.state pending is not allowed on an active entry (capture its frame)`);
      else warnings.push(`${id}: frame.state pending (allowed on deprecated entries)`);
      for (const k of extra(["state"])) errors.push(`${id}: frame.state pending allows no other field (found ${k})`);
      break;
    case "locked": {
      for (const k of extra(["state", "topic", "doop_version", "frames"])) errors.push(`${id}: frame.state locked does not allow ${k}`);
      if (!frame.topic) errors.push(`${id}: frame.topic is required when locked`);
      if (!frame.doop_version) errors.push(`${id}: frame.doop_version is required when locked`);
      if (!isList(frame.frames) || frame.frames.length === 0) errors.push(`${id}: frame.frames must list at least one frame id when locked`);
      if (frame.topic && frame.doop_version && !rows.some(([topic, rev]) => topic === frame.topic && hasToken(rev, String(frame.doop_version)))) {
        errors.push(`${id}: no DESIGN.md approved-revisions row for topic ${frame.topic} with doop version ${frame.doop_version}`);
      }
      break;
    }
    case "patterns":
      for (const k of extra(["state", "url"])) errors.push(`${id}: frame.state patterns does not allow ${k}`);
      if (typeof frame.url !== "string" || !FRAME_URL.test(frame.url)) {
        errors.push(`${id}: frame.url must be https://<host>/c/<file>?frame=<frame> when patterns`);
      }
      break;
    default:
      errors.push(`${id}: frame.state must be pending, locked or patterns (got ${frame.state})`);
  }
}

function checkEntry(e, ctx, errors, warnings) {
  const id = typeof e?.id === "string" && e.id ? e.id : "<no id>";
  if (!e || typeof e !== "object") return errors.push(`entry is not a mapping`);
  if (!ID.test(id)) errors.push(`${id}: id must be lower-case kebab-case`);
  for (const k of Object.keys(e)) if (!FIELDS.has(k)) errors.push(`${id}: unknown field ${k}`);
  if (typeof e.intent !== "string" || !e.intent.trim()) errors.push(`${id}: intent is required`);
  if (!["upstream", "fork"].includes(e.origin)) errors.push(`${id}: origin must be upstream or fork`);
  if (!["active", "deprecated"].includes(e.status)) errors.push(`${id}: status must be active or deprecated`);
  if (!isList(e.components)) errors.push(`${id}: components must be a list of paths`);
  if (e.docs !== undefined && !isList(e.docs)) errors.push(`${id}: docs must be a list of paths`);
  for (const [field, paths] of [["components", e.components], ["docs", e.docs]]) {
    for (const p of isList(paths) ? paths : []) if (!ctx.pathExists(p)) errors.push(`${id}: ${field} path does not exist: ${p}`);
  }
  for (const [name, key] of Object.entries(e.copy ?? {})) {
    const m = typeof key === "string" && KEY.exec(key);
    if (!m) errors.push(`${id}: copy.${name} must be <namespace>:<key>, got ${key}`);
    else if (!ctx.i18nHas(m[1], m[2])) errors.push(`${id}: copy.${name} i18n key not found in en locale: ${key}`);
  }
  checkFrame(id, e.frame, ctx.rows, errors, warnings, e.status);
}

export function validatePatterns({ entries, base = null, designMd, pathExists, i18nHas }) {
  const errors = [];
  const warnings = [];
  const ctx = { rows: approvedRows(designMd), pathExists, i18nHas };
  const seen = new Set();
  for (const e of entries) {
    if (e?.id && seen.has(e.id)) errors.push(`duplicate id ${e.id}`);
    seen.add(e?.id);
    checkEntry(e, ctx, errors, warnings);
  }
  if (base) {
    const before = new Map(base.map((e) => [e.id, e.frame?.state]));
    for (const e of entries) {
      const to = e?.frame?.state;
      if (!before.has(e?.id)) {
        if (to === "pending") errors.push(`${e.id}: a new entry may not use frame.state pending`);
        continue;
      }
      const from = before.get(e.id);
      if (from !== to && !MOVES.has(`${from}>${to}`)) errors.push(`${e.id}: frame.state ${from} → ${to} is not allowed`);
    }
  }
  return { errors, warnings };
}

function loadBase(root, ref) {
  const git = (...args) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8" });
  if (!git("ls-tree", "-r", "--name-only", ref, "--", LEDGER).trim()) return null; // seed: no ledger on base yet
  return parsePatterns(git("show", `${ref}:${LEDGER}`));
}

export function validateRepo(root, { baseRef } = {}) {
  const locales = new Map();
  const i18nHas = (ns, key) => {
    if (!locales.has(ns)) {
      const file = join(root, "packages/views/locales/en", `${ns}.json`);
      locales.set(ns, existsSync(file) ? JSON.parse(readFileSync(file, "utf8")) : null);
    }
    let node = locales.get(ns);
    for (const part of key.split(".")) node = node && typeof node === "object" ? node[part] : undefined;
    return typeof node === "string";
  };
  const entries = parsePatterns(readFileSync(join(root, LEDGER), "utf8"));
  const result = validatePatterns({
    entries,
    base: baseRef ? loadBase(root, baseRef) : null,
    designMd: readFileSync(join(root, "DESIGN.md"), "utf8"),
    pathExists: (p) => existsSync(join(root, p)),
    i18nHas,
  });
  return { ...result, count: entries.length };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const i = process.argv.indexOf("--base");
  const baseRef = i > -1 ? process.argv[i + 1] : undefined;
  const { errors, warnings, count } = validateRepo(resolve(import.meta.dirname, "../.."), { baseRef });
  // One plain line, not a `warning:` line per entry: the CI warning budget fails on any new warning signature.
  const pending = warnings.map((w) => w.split(":")[0]);
  if (pending.length) console.log(`pending frames (deprecated entries only): ${pending.join(", ")}`);
  for (const e of errors) console.error(`error: ${e}`);
  console.log(`${LEDGER}: ${count} entries, ${errors.length} errors, ${warnings.length} warnings`);
  process.exit(errors.length ? 1 : 0);
}
