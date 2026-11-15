#!/usr/bin/env node
/**
 * Lock marks check (CHE-5793 step 1b, contract C2 "Lock marks").
 * In a lock HTML file every state root carries data-state and data-profile (a profile id from
 * design/devices.yml); every note that is not UI carries data-annotation; no visible text sits outside both.
 *
 * Run: node scripts/design/check-lock-marks.mjs [lock.html ...]   (default: the decision-card lock)
 * Zero dependencies: a small tag scanner, enough for the self-contained lock exports.
 */
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

export const LOCK_PATH = "design/decision-card/approved/direction-b.html";
const VOID = new Set(["br", "img", "hr", "input", "meta", "link", "path", "circle", "rect", "line", "polyline", "polygon"]);
const RAW = new Set(["script", "style"]);
const CONTROL_TAGS = new Set(["button", "a", "input", "select", "textarea"]);
const CONTROL_ROLES = new Set(["button", "link", "radio", "checkbox", "switch", "tab", "menuitem", "option", "textbox", "combobox", "slider"]);
const TAG = /<!--[\s\S]*?-->|<![^>]*>|<(\/?)([a-zA-Z][\w:-]*)((?:[^>"']|"[^"]*"|'[^']*')*)>/g;
const attr = (s, name) => s.match(new RegExp(`(?:^|\\s)${name}(?:=("[^"]*"|'[^']*'|[^\\s>]*))?(?=\\s|/|$)`))?.[1]?.replace(/^["']|["']$/g, "") ?? (new RegExp(`(?:^|\\s)${name}(?=\\s|/|$)`).test(s) ? "" : null);

export function profileIdsOf(devicesYml) {
  return new Set([...devicesYml.matchAll(/^\s*-\s*id:\s*([a-z0-9-]+)\s*$/gm)].map((m) => m[1]));
}

export function checkLockMarks(html, profileIds) {
  const errors = [];
  const stack = [];
  const states = new Set();
  const labels = [];
  let last = 0;
  const text = (to) => {
    const t = html.slice(last, to);
    if (t.trim() && !stack.some((e) => e.mark)) errors.push(`unclaimed-lock-content: "${t.trim().slice(0, 50)}"`);
  };
  for (const m of html.matchAll(TAG)) {
    const [raw, close, name, attrs = ""] = m;
    if (!raw.startsWith("<!") && !RAW.has(stack.at(-1)?.name)) text(m.index);
    if (RAW.has(stack.at(-1)?.name) && !(close && name === stack.at(-1).name)) { last = m.index + raw.length; continue; }
    last = m.index + raw.length;
    if (raw.startsWith("<!")) continue;
    const tag = name.toLowerCase();
    if (close) { while (stack.length && stack.pop().name !== tag); continue; }
    const state = attr(attrs, "data-state");
    const note = attr(attrs, "data-annotation") !== null;
    const role = attr(attrs, "role");
    const isControl = CONTROL_TAGS.has(tag) || (role && CONTROL_ROLES.has(role));
    const where = `<${tag}${state ? ` data-state=${state}` : ""}>`;
    if (state !== null) {
      const profile = attr(attrs, "data-profile");
      if (!profile || !profileIds.has(profile)) errors.push(`bad-marker: ${where} needs data-profile from devices.yml, got ${profile}`);
      if (states.has(`${state}|${profile}`)) errors.push(`bad-marker: duplicate state ${state} x ${profile}`);
      states.add(`${state}|${profile}`);
      if (stack.some((e) => e.note)) errors.push(`bad-marker: ${where} sits inside an annotation`);
      if (stack.some((e) => e.state)) errors.push(`bad-marker: ${where} sits inside another state root`);
    }
    if (note && stack.some((e) => e.state)) errors.push(`bad-marker: annotation sits inside state root ${stack.find((e) => e.state).state}`);
    if (note && stack.some((e) => e.note)) errors.push("bad-marker: annotation inside annotation");
    if (note && state !== null) errors.push(`bad-marker: ${where} is both state and annotation`);
    if (isControl && stack.some((e) => e.note) ) errors.push(`bad-marker: annotation contains a control <${tag}>`);
    if (/\bclass="lbl\b/.test(attrs)) labels.push(html.slice(m.index + raw.length).match(/^\s*(\d+[a-z]?)\s*·/)?.[1]);
    if (!VOID.has(tag) && !raw.endsWith("/>")) stack.push({ name: tag, state: state !== null ? state : null, note, mark: state !== null || note });
  }
  const ids = new Set([...states].map((s) => s.split("|")[0]));
  // 22 is the collapse-animation loop: a motion demo, annotated as a whole; its stills are states 1 and 8.
  for (const n of labels) if (n && !ids.has(`s${n}`)) errors.push(`uncovered-state: label ${n} has no data-state="s${n}" root`);
  return { errors, states: [...states] };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const profiles = profileIdsOf(readFileSync("design/devices.yml", "utf8"));
  let bad = 0;
  for (const f of process.argv.length > 2 ? process.argv.slice(2) : [LOCK_PATH]) {
    const { errors, states } = checkLockMarks(readFileSync(f, "utf8"), profiles);
    for (const e of errors) console.error(`${f}: ${e}`);
    if (!errors.length) console.log(`${f}: ${states.length} state roots, marks ok`);
    bad += errors.length;
  }
  if (bad) process.exit(1);
}
