#!/usr/bin/env node
/**
 * Schema check for the fidelity inputs (CHE-5793 step 1, contracts C1 and C2):
 *   design/devices.yml                         device profiles
 *   tests/fidelity/decision-card.blocks.yml    cases and block ids for the decision-card lock
 *
 * Run: node scripts/design/validate-fidelity-files.mjs
 */
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { parse } from "yaml";

export const DEVICES_PATH = "design/devices.yml";
export const BLOCKS_PATH = "tests/fidelity/decision-card.blocks.yml";

const PLATFORMS = new Set(["web", "desktop", "android", "ios"]);
const THEMES = new Set(["light", "dark"]);
const ID = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const STATE = /^s\d+[a-z]?$/;
const posInt = (v) => Number.isInteger(v) && v > 0;

export function validateDevices(doc) {
  const errors = [];
  const profiles = doc?.profiles;
  if (!Array.isArray(profiles) || profiles.length === 0) return { errors: ["devices: profiles needs at least one entry"] };
  const seen = new Set();
  for (const [i, p] of profiles.entries()) {
    const at = `devices[${p?.id ?? i}]`;
    if (typeof p?.id !== "string" || !ID.test(p.id)) errors.push(`${at}: id must be a kebab-case string`);
    else if (seen.has(p.id)) errors.push(`${at}: duplicate id`);
    else seen.add(p.id);
    if (!PLATFORMS.has(p?.platform)) errors.push(`${at}: platform must be one of ${[...PLATFORMS].join(", ")}`);
    if (!posInt(p?.width)) errors.push(`${at}: width must be a positive integer (px)`);
    if (!posInt(p?.height)) errors.push(`${at}: height must be a positive integer (px)`);
    if (typeof p?.dpr !== "number" || !(p.dpr > 0)) errors.push(`${at}: dpr must be a positive number`);
    if (!Array.isArray(p?.themes) || p.themes.length === 0 || !p.themes.every((t) => THEMES.has(t))) {
      errors.push(`${at}: themes must be a non-empty list of ${[...THEMES].join(", ")}`);
    }
    if (typeof p?.font_pack !== "string" || !p.font_pack) errors.push(`${at}: font_pack is required`);
    const extra = Object.keys(p ?? {}).filter((k) => !["id", "platform", "width", "height", "dpr", "themes", "font_pack"].includes(k));
    for (const k of extra) errors.push(`${at}: unknown field ${k}`);
  }
  return { errors };
}

export function validateBlocksMap(doc, { profileIds, lockLabels }) {
  const errors = [];
  for (const k of Object.keys(doc ?? {})) {
    if (/annotation/i.test(k)) errors.push(`blocks: ${k}: the blocks map has no annotation field (C2)`);
    else if (!["topic", "cases", "blocks"].includes(k)) errors.push(`blocks: unknown field ${k}`);
  }
  if (typeof doc?.topic !== "string" || !doc.topic) errors.push("blocks: topic is required");
  if (!Array.isArray(doc?.cases) || doc.cases.length === 0) errors.push("blocks: cases needs at least one entry");
  const seen = new Set();
  for (const [i, c] of (doc?.cases ?? []).entries()) {
    const at = `cases[${c?.state ?? i}]`;
    for (const k of ["lock_file", "state", "profile", "fixture"]) {
      if (typeof c?.[k] !== "string" || !c[k]) errors.push(`${at}: ${k} is required`);
    }
    for (const k of Object.keys(c ?? {})) {
      if (!["lock_file", "state", "profile", "fixture", "steps"].includes(k)) errors.push(`${at}: ${/annotation/i.test(k) ? "annotation field not allowed" : `unknown field ${k}`}`);
    }
    if (c?.steps !== undefined) {
      if (!Array.isArray(c.steps)) errors.push(`${at}: steps must be a list`);
      else c.steps.forEach((st, n) => {
        const sel = st?.click ?? st?.type;
        const shape = st && typeof sel === "string" && sel && (st.click !== undefined ? Object.keys(st).length === 1 : typeof st.text === "string" && Object.keys(st).length === 2);
        if (!shape) errors.push(`${at}: steps[${n}] must be { click: <selector> } or { type: <selector>, text: <string> }`);
      });
    }
    if (typeof c?.state === "string" && (!STATE.test(c.state) || !lockLabels.has(c.state.slice(1)))) {
      errors.push(`${at}: state ${c.state} is not a state the lock draws`);
    }
    if (typeof c?.profile === "string" && !profileIds.has(c.profile)) errors.push(`${at}: profile ${c.profile} is not in devices.yml`);
    const key = `${c?.state}|${c?.profile}`;
    if (seen.has(key)) errors.push(`${at}: duplicate state x profile ${key}`);
    seen.add(key);
  }
  const blocks = doc?.blocks;
  if (!blocks || typeof blocks !== "object" || Array.isArray(blocks) || Object.keys(blocks).length === 0) {
    errors.push("blocks: blocks needs at least one entry");
    return { errors };
  }
  for (const [id, b] of Object.entries(blocks)) {
    if (!ID.test(id)) errors.push(`blocks[${id}]: id must be a kebab-case string`);
    if (typeof b?.selector !== "string" || !b.selector) errors.push(`blocks[${id}]: selector is required`);
    if (b?.repeat !== undefined && typeof b.repeat !== "boolean") errors.push(`blocks[${id}]: repeat must be a boolean`);
    for (const k of Object.keys(b ?? {})) {
      if (/annotation/i.test(k)) errors.push(`blocks[${id}]: annotation field not allowed (C2)`);
      else if (!["selector", "repeat"].includes(k)) errors.push(`blocks[${id}]: unknown field ${k}`);
    }
  }
  return { errors };
}

// Label number -> width (px) of the lock column it sits in. A column is `<div class="col ...">`:
// 540 px by default, 358 px with class `ph`, or the inline width.
export function lockLabelWidths(html) {
  const widths = new Map();
  let width = 540;
  for (const line of html.split("\n")) {
    const col = line.match(/<div class="col( ph)?"(?: style="width:(\d+)px")?/);
    if (col) width = col[2] ? Number(col[2]) : col[1] ? 358 : 540;
    for (const m of line.matchAll(/class="lbl">(\d+[a-z]?) ·/g)) widths.set(m[1], width);
  }
  return widths;
}

export function validateRepoFiles({ read }) {
  const devices = parse(read(DEVICES_PATH));
  const map = parse(read(BLOCKS_PATH));
  const errors = [...validateDevices(devices).errors];
  const profiles = new Map((devices?.profiles ?? []).map((p) => [p.id, p]));
  const lockFile = map?.cases?.[0]?.lock_file;
  const widths = lockLabelWidths(read(lockFile));
  errors.push(...validateBlocksMap(map, { profileIds: new Set(profiles.keys()), lockLabels: new Set(widths.keys()) }).errors);
  const covered = new Set((map?.cases ?? []).map((c) => c.state));
  for (const [label, width] of widths) {
    if (!covered.has(`s${label}`)) errors.push(`lock draws state ${label} but no case covers it`);
    const profile = (map?.cases ?? []).filter((c) => c.state === `s${label}`).map((c) => profiles.get(c.profile));
    for (const p of profile) if (p && p.width !== width) errors.push(`state ${label}: profile ${p.id} is ${p.width} px, the lock draws ${width} px`);
  }
  return { errors };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const { errors } = validateRepoFiles({ read: (p) => readFileSync(p, "utf8") });
  for (const e of errors) console.error(`error: ${e}`);
  if (errors.length) process.exit(1);
  console.log("fidelity files ok");
}
