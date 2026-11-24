#!/usr/bin/env node
/**
 * UI ownership map check (CHE-5793 step 7, contract C3 rules 1 to 4).
 *
 * `design/ui-map.yml` says which screen owns which UI files and which lock folder each screen is drawn from:
 *
 *   legacy: { seed_sha: <40 hex> }   # the commit the map was seeded on; code that exists there is "legacy"
 *   entries:
 *     - screen: <id>
 *       paths: [<glob>]              # files the screen owns ([] while the screen is unmapped)
 *       routes: [<path>]             # optional: desktop routes.tsx `path` values the screen owns
 *       lock: <topic>                # design/<topic>/approved/
 *       lock_export_sha256: <hex>    # copied from that LOCK.json (see lockPin)
 *       blocks_map: <path>           # the blocks map (needed for a new entry)
 *       state: building | shipped | unmapped | upstream
 *     An `upstream` entry owns screens that came from the upstream project and have no design lock: it has no
 *     lock, lock_export_sha256 or blocks_map, and needs `paths` or `routes`. Rules 1 and 2 apply to it as to any entry.
 *
 * Rules (each failure has a named reason):
 *   1 ambiguous-owner     a changed UI file matches two named entries (a named entry wins over legacy)
 *   2 unowned-ui          a legacy-owned UI file sits in a directory that does not exist at seed_sha
 *     unowned-screen      a new apps/web page.tsx or a new desktop route has no named entry
 *   3 lock-without-owner  a lock folder design/*\/approved/ has no entry, or more than one
 *     lock-pin-mismatch   an entry's lock_export_sha256 differs from its LOCK.json
 *   4 new-unmapped        `unmapped` only for a lock that exists at seed_sha with the same pin; a new or re-pinned
 *                         entry needs state `building` and an existing blocks map
 * Also: bad-map (schema or missing file), seed-unreachable (seed_sha is not in the clone).
 * Rules 0, 5 and 6 (trigger, ratchet, crash) belong to the ui-fidelity job (steps 8 and 8b).
 *
 * Run: node scripts/design/check-ui-map.mjs [--base <ref> [--head <ref>]]
 *   no --base  rules 3 and 4 only (map-wide), on the working tree
 *   --base     also rules 1 and 2 for the files changed between <ref> and --head (default HEAD)
 */
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";
import { parse } from "yaml";
import { isUiPath } from "./check-ui-pattern-ref.mjs";

export const MAP_PATH = "design/ui-map.yml";
export const ROUTES_PATH = "apps/desktop/src/renderer/src/routes.tsx";
const STATES = new Set(["building", "shipped", "unmapped", "upstream"]);
const NEW_SCREEN = /^apps\/web\/app\/(?:.+\/)?page\.tsx$/;
const LOCK_FOLDER = /^design\/([^/]+)\/approved\//;

export function globToRegExp(glob) {
  let re = "";
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i];
    if (c === "*" && glob[i + 1] === "*") {
      if (glob[i + 2] === "/") {
        re += "(?:.*/)?";
        i += 2;
      } else {
        re += ".*";
        i += 1;
      }
    } else if (c === "*") re += "[^/]*";
    else if (c === "?") re += "[^/]";
    else re += c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
  }
  return new RegExp(`^${re}$`);
}

// The pin of a LOCK.json: its `export_sha256`, or, when that is a per-file object (older locks), the sha256 of the
// sha256sum lines of its files sorted by name (the method the newer locks state).
export function lockPin(lock) {
  const v = lock?.export_sha256;
  if (typeof v === "string") return v;
  if (v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length) {
    const lines = Object.keys(v).sort().map((f) => `${v[f]}  ${f}\n`).join("");
    return createHash("sha256").update(lines).digest("hex");
  }
  return null;
}

const git = (cwd, args) => execFileSync("git", ["-c", "core.quotepath=false", ...args], { cwd, encoding: "utf8", maxBuffer: 1 << 28, stdio: ["ignore", "pipe", "pipe"] });

// A tree is `{ list(), read(path), at(sha) }`. `at` gives the same repo at another commit (the seed).
function makeTree({ listAll, read, at }) {
  let files;
  let dirs;
  const list = () => (files ??= listAll());
  return {
    list,
    read,
    at,
    hasDir: (dir) => {
      dirs ??= new Set(list().flatMap((p) => p.split("/").slice(0, -1).map((_, i, a) => a.slice(0, i + 1).join("/"))));
      return dirs.has(dir);
    },
  };
}

export function gitTree(cwd, ref) {
  return makeTree({
    listAll: () => git(cwd, ["ls-tree", "-r", "--name-only", ref]).split("\n").filter(Boolean),
    read: (path) => {
      try {
        return git(cwd, ["show", `${ref}:${path}`]);
      } catch {
        return null;
      }
    },
    at: (sha) => gitTree(cwd, sha),
  });
}

export function fsTree(cwd) {
  return makeTree({
    listAll: () => git(cwd, ["ls-files", "-co", "--exclude-standard"]).split("\n").filter(Boolean),
    read: (path) => (existsSync(join(cwd, path)) ? readFileSync(join(cwd, path), "utf8") : null),
    at: (sha) => gitTree(cwd, sha),
  });
}

// Files changed between base and head, and the desktop route paths the PR adds.
export function readChanges(cwd, base, head = "HEAD") {
  const files = git(cwd, ["diff", "-M", "--name-status", base, head])
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      const [s, a, b] = line.split("\t");
      return { status: s[0], oldPath: a, path: b ?? a };
    });
  const pathsIn = (sign) => {
    const out = git(cwd, ["diff", "-U0", base, head, "--", ROUTES_PATH]);
    const found = new Set();
    for (const l of out.split("\n")) if (l.startsWith(sign) && !l.startsWith(sign.repeat(3))) for (const m of l.matchAll(/\bpath:\s*"([^"]+)"/g)) found.add(m[1]);
    return found;
  };
  const removed = pathsIn("-");
  return { files, routes: [...pathsIn("+")].filter((p) => !removed.has(p)) };
}

const err = (reason, message) => ({ reason, message });
const isList = (v) => Array.isArray(v) && v.every((s) => typeof s === "string");

function validateMap(doc) {
  const errors = [];
  if (!doc || typeof doc !== "object") return [err("bad-map", `${MAP_PATH} is not a mapping`)];
  if (typeof doc.legacy?.seed_sha !== "string" || !/^[0-9a-f]{40}$/.test(doc.legacy.seed_sha)) errors.push(err("bad-map", "legacy.seed_sha must be a 40-hex commit"));
  if (!Array.isArray(doc.entries)) return [...errors, err("bad-map", "entries must be a list")];
  const screens = new Set();
  for (const [i, e] of doc.entries.entries()) {
    const at = `entries[${e?.screen ?? i}]`;
    if (typeof e?.screen !== "string" || !e.screen) errors.push(err("bad-map", `${at}: screen is required`));
    else if (screens.has(e.screen)) errors.push(err("bad-map", `${at}: duplicate screen id`));
    else screens.add(e.screen);
    if (!isList(e?.paths)) errors.push(err("bad-map", `${at}: paths must be a list of globs`));
    if (e?.routes !== undefined && !isList(e.routes)) errors.push(err("bad-map", `${at}: routes must be a list of route paths`));
    if (e?.state === "upstream") {
      // An upstream screen comes from the upstream project and has no design lock here: it needs an owned path or route, and no lock fields.
      if (e.lock !== undefined || e.lock_export_sha256 !== undefined) errors.push(err("bad-map", `${at}: an upstream entry has no lock or lock_export_sha256`));
      if (!e.paths?.length && !e.routes?.length) errors.push(err("bad-map", `${at}: an upstream entry needs paths or routes`));
    } else {
      if (typeof e?.lock !== "string" || !e.lock) errors.push(err("bad-map", `${at}: lock (the design/<topic> name) is required`));
      if (typeof e?.lock_export_sha256 !== "string" || !/^[0-9a-f]{64}$/.test(e.lock_export_sha256)) errors.push(err("bad-map", `${at}: lock_export_sha256 must be a 64-hex sha256`));
    }
    if (!STATES.has(e?.state)) errors.push(err("bad-map", `${at}: state must be one of ${[...STATES].join(", ")}`));
    if (e?.blocks_map !== undefined && typeof e.blocks_map !== "string") errors.push(err("bad-map", `${at}: blocks_map must be a path`));
  }
  return errors;
}

// Entries of the map in a tree; none when the map is absent or unreadable (everything is then new).
function mapEntries(tree) {
  try {
    const doc = parse(tree.read(MAP_PATH));
    return Array.isArray(doc?.entries) ? doc.entries : [];
  } catch {
    return [];
  }
}

/**
 * @param head    tree of the PR head (see gitTree, fsTree)
 * @param base    tree of the PR base, or null for a map-wide run
 * @param changes readChanges(...) result, or null to check rules 3 and 4 only
 */
export function checkUiMap({ head, base = null, changes }) {
  const text = head.read(MAP_PATH);
  if (text == null) return { errors: [err("bad-map", `${MAP_PATH} is missing`)] };
  let doc;
  try {
    doc = parse(text);
  } catch (e) {
    return { errors: [err("bad-map", `${MAP_PATH} is not valid YAML: ${e.message}`)] };
  }
  const bad = validateMap(doc);
  if (bad.length) return { errors: bad };

  const seed = head.at(doc.legacy.seed_sha);
  try {
    seed.list();
  } catch {
    return { errors: [err("seed-unreachable", `legacy.seed_sha ${doc.legacy.seed_sha} is not in this clone. Fix: fetch it (git fetch origin ${doc.legacy.seed_sha}) or correct the map.`)] };
  }

  const errors = [];
  const entries = doc.entries.map((e) => ({ ...e, globs: e.paths.map(globToRegExp) }));
  const lockOf = (tree, topic) => {
    try {
      return JSON.parse(tree.read(`design/${topic}/approved/LOCK.json`));
    } catch {
      return null;
    }
  };

  // Rule 3
  const topics = [...new Set(head.list().map((p) => LOCK_FOLDER.exec(p)?.[1]).filter(Boolean))].sort();
  for (const topic of topics) {
    const owners = entries.filter((e) => e.lock === topic);
    if (owners.length !== 1) {
      errors.push(err("lock-without-owner", `design/${topic}/approved/ has ${owners.length} entries in ${MAP_PATH}. Fix: add exactly one entry with \`lock: ${topic}\`.`));
      continue;
    }
    const pin = lockPin(lockOf(head, topic));
    if (owners[0].lock_export_sha256 !== pin) {
      errors.push(err("lock-pin-mismatch", `${owners[0].screen}: lock_export_sha256 ${owners[0].lock_export_sha256} differs from design/${topic}/approved/LOCK.json (${pin ?? "no export_sha256"}). Fix: copy the lock's export_sha256 into the entry.`));
    }
  }
  for (const e of entries.filter((x) => x.state !== "upstream")) {
    if (!topics.includes(e.lock)) errors.push(err("bad-map", `${e.screen}: design/${e.lock}/approved/ does not exist`));
  }

  // Rule 4. An `unmapped` entry is legacy: it must still match the lock as at seed_sha.
  // Any other entry is judged against the PR base, so a re-pin that landed as `building`
  // may become `shipped` in a later PR. Without a base (map-wide run) only `unmapped` is judged.
  const baseEntries = base ? mapEntries(base) : null;
  for (const e of entries.filter((x) => topics.includes(x.lock))) {
    if (lockPin(lockOf(seed, e.lock)) === e.lock_export_sha256) continue; // same lock as at seed_sha
    if (e.state !== "unmapped") {
      if (!baseEntries) continue;
      if (baseEntries.some((b) => b.lock === e.lock && b.lock_export_sha256 === e.lock_export_sha256)) continue; // pin unchanged in this PR
    }
    if (e.state !== "building" || !e.blocks_map || head.read(e.blocks_map) == null) {
      errors.push(err("new-unmapped", `${e.screen}: a new entry or a changed lock_export_sha256 needs state \`building\` and an existing blocks_map (state ${e.state}, blocks_map ${e.blocks_map ?? "none"}).`));
    }
  }

  // Rules 1 and 2
  if (changes) {
    const named = (path) => entries.filter((e) => e.globs.some((g) => g.test(path)));
    for (const f of changes.files) {
      if (f.status === "D" || !isUiPath(f.path)) continue;
      const owners = named(f.path);
      const newScreen = (f.status === "A" || f.status === "R") && NEW_SCREEN.test(f.path);
      if (owners.length > 1) errors.push(err("ambiguous-owner", `${f.path} matches ${owners.map((o) => o.screen).join(" and ")}. Fix: narrow the paths so one entry owns it.`));
      else if (owners.length === 0 && newScreen) errors.push(err("unowned-screen", `${f.path} is a new screen with no named entry. Fix: add an entry in ${MAP_PATH} whose paths match it.`));
      else if (owners.length === 0 && !seed.hasDir(dirname(f.path))) errors.push(err("unowned-ui", `${f.path} is in a directory that does not exist at seed_sha. Fix: add a named entry in ${MAP_PATH} that owns it.`));
    }
    for (const route of changes.routes) {
      if (!entries.some((e) => e.routes?.includes(route))) errors.push(err("unowned-screen", `${ROUTES_PATH}: new route "${route}" has no named entry. Fix: list it under \`routes\` of an entry in ${MAP_PATH}.`));
    }
  }
  return { errors };
}

function main(argv) {
  const arg = (name) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : undefined);
  const base = arg("--base");
  const headRef = arg("--head");
  const cwd = process.cwd();
  const r = checkUiMap({
    head: headRef ? gitTree(cwd, headRef) : fsTree(cwd),
    base: base ? gitTree(cwd, base) : null,
    changes: base ? readChanges(cwd, base, headRef ?? "HEAD") : null,
  });
  for (const e of r.errors) console.error(`::error::ui-map ${e.reason}: ${e.message}`);
  console.log(`ui-map: ${base ? "rules 1 to 4" : "rules 3 and 4"}, ${r.errors.length} error(s)`);
  return r.errors.length ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) process.exit(main(process.argv.slice(2)));
