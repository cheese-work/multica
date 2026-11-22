#!/usr/bin/env node
/**
 * Lock-vs-code fidelity check, core (CHE-5793 step 2, contract C2).
 *
 * For each case of a blocks map (tests/fidelity/<topic>.blocks.yml) it renders the lock state and the real
 * component with the same fixture, reads the mapped blocks from both and compares them:
 *   unmapped-lock-content (rule 1)  a lock item (visible text, control, image) inside the state root is outside every mapped block
 *   unmapped-code-content (rule 2)  the same for an item in the rendered component root
 *   missing-block  (rule 3)  a mapped block is absent in the lock or in the code render
 *   duplicate-block (rule 4) a block id without `repeat` matches more than one element on either side
 *   order          (rule 5)  the order of the blocks differs
 *   text           (rule 6)  the text of a block differs (carries the sha256 of the code text)
 *   missing-render (rule 9)  a render fails or times out
 * Lock inventory (step 3b). Before it reads the map, the check builds the inventory from the lock alone: every HTML file in
 * the lock folder's LOCK.json `export_files`, rendered at 1440 px; its items are the visible text nodes, controls and images.
 *   uncovered-state (rule 7)  a data-state root in the lock has no case in the blocks map
 *   profile-mismatch (rule 8) the case's profile is not in design/devices.yml, differs from the root's data-profile, or the root
 *                             renders more than 1 px away from the profile width
 *   unclaimed-lock-content (rule 10)  an inventory item sits outside every data-state root and every data-annotation element
 *   unclaimed-lock-file (rule 11)     an export file has no data-state root and is not marked data-annotation as a whole
 *   bad-marker (rule 12)      a data-annotation element contains a control, or contains or sits inside a state root; a state
 *                             root has no data-profile; two roots share one state id x profile
 * Size and colour are not compared (D1). Rules 7, 10, 11 and 12 give one instance per root, item, file or marked element.
 * Rules 1 and 2 give one instance per item (rule, case, item kind, normalized text / accessible name / image hash), so two
 * equal items are two instances.
 *
 * A case may list `steps` (click / type) that bring the real component to the state the lock draws; result.json
 * `renders` records the steps and a hash of the code render per case. Every failure is one instance: rule x block x case. Writes <out>/result.json and, per case,
 * <out>/side-by-side-<state>-<profile>.png. Exit 1 on any instance.
 *
 * Run on X99 (headless Chromium, vite build):
 *   node scripts/design/check-fidelity.mjs [--blocks <file>] [--out <dir>] [--case <state>]... [--harness <page.html>]
 */
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parse } from "yaml";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const DEFAULT_BLOCKS = "tests/fidelity/decision-card.blocks.yml";
const DEVICES = "design/devices.yml";
const RENDER_TIMEOUT_MS = 20_000;
const LOCK_FILE_WIDTH = 1440; // the page width a lock file is rendered at for the inventory

export const normalizeText = (s) => String(s).replace(/\s+/g, " ").trim();
const STEP_TIMEOUT_MS = 3_000;
const sha256 = (s) => createHash("sha256").update(s).digest("hex");

const instance = (c, rule, block, side, reason, extra = {}) => ({
  rule, state: c.state, profile: c.profile, fixture: c.fixture, ...(block ? { block } : {}), side, reason, ...extra,
});

const fileInstance = (rule, file, reason, extra = {}) => ({ rule, file, side: "lock", reason, ...extra });
const rootInstance = (rule, file, r, reason) => fileInstance(rule, file, reason, { state: r.state, ...(r.profile ? { profile: r.profile } : {}) });

/**
 * Rules 10, 11 and 12 for one lock file, from what the lock page reports (readLockInventory):
 * { states: [{ state, profile }], wholeFileAnnotation, unclaimed: [{ kind, key }], badAnnotations: [{ why, label }] }.
 * Rule 10 gives one instance per item (kind and text / name / image hash), so equal items repeat; rule 11 one per file;
 * rule 12 one per marked element or root.
 */
export function lockFileViolations(file, inv) {
  const out = [];
  if (inv.states.length === 0 && !inv.wholeFileAnnotation) {
    out.push(fileInstance("unclaimed-lock-file", file, `${file} has no data-state root and is not marked data-annotation as a whole`));
  }
  for (const it of inv.unclaimed) {
    out.push(fileInstance("unclaimed-lock-content", file, `the lock has a ${it.kind} outside every data-state root and data-annotation element: "${it.key}"`, { item_kind: it.kind, item: it.key }));
  }
  for (const a of inv.badAnnotations) {
    out.push(fileInstance("bad-marker", file, `a data-annotation element (${a.label}) ${a.why}`, { element: a.label }));
  }
  const count = new Map();
  for (const r of inv.states) count.set(`${r.state}|${r.profile}`, (count.get(`${r.state}|${r.profile}`) ?? 0) + 1);
  for (const r of inv.states) {
    if (!r.profile) out.push(rootInstance("bad-marker", file, r, `state root ${r.state} has no data-profile`));
    else if (count.get(`${r.state}|${r.profile}`) > 1) out.push(rootInstance("bad-marker", file, r, `two state roots share state ${r.state} and profile ${r.profile}`));
  }
  return out;
}

/**
 * Rule 7: the roots of one lock file that no case covers. Coverage is per root: a root is covered only by a case with its
 * state id and its profile. A case with the right state id and another profile covers no root (it is a profile-mismatch, rule 8).
 */
export function uncoveredStates(file, roots, cases) {
  const mine = cases.filter((c) => c.lock_file === file);
  return roots
    .filter((r) => !mine.some((c) => c.state === r.state && c.profile === r.profile))
    .map((r) => rootInstance("uncovered-state", file, r, `the lock draws state ${r.state}${r.profile ? ` at ${r.profile}` : ""}, and the blocks map has no case for it`));
}

/**
 * Rule 8 for one case: one instance, with every reason. rootMark is what the lock render reports for the case's state root:
 * { marked, profile, width }. An unmarked root (the numbered-label fallback) is judged by rule 12, not here.
 */
export function profileViolations(c, devices, rootMark) {
  const profile = devices.get(c.profile);
  const reasons = [];
  if (!profile) reasons.push(`profile ${c.profile} is not in design/devices.yml`);
  if (rootMark?.marked) {
    if (rootMark.profile !== c.profile) reasons.push(`the case profile ${c.profile} differs from data-profile ${rootMark.profile ?? "(none)"} on the state root`);
    if (profile && typeof rootMark.width === "number" && Math.abs(rootMark.width - profile.width) > 1) {
      reasons.push(`the state root renders ${rootMark.width} px wide; profile ${c.profile} is ${profile.width} px`);
    }
  }
  return reasons.length === 0 ? [] : [instance(c, "profile-mismatch", undefined, "lock", reasons.join("; "))];
}

/** One violation for a render that failed or timed out (rule 9). */
export function renderFailure(c, side, err) {
  return [instance(c, "missing-render", undefined, side, `${side} render failed: ${err?.message ?? err}`)];
}

/**
 * Compare what the lock and the code render for one case.
 * A side is { order: [block id in document order], blocks: { id: { text } }, unmapped?: [{ kind, key }], dups?: { id: count } }.
 * `unmapped` lists the items outside every mapped block, `dups` the non-repeat blocks that matched more than one element.
 */
export function compareBlocks(c, lock, code, mapped = [...new Set([...Object.keys(lock.blocks), ...Object.keys(code.blocks)])]) {
  const out = [];
  for (const side of ["lock", "code"]) {
    const s = side === "lock" ? lock : code;
    for (const [id, count] of Object.entries(s.dups ?? {})) {
      out.push(instance(c, "duplicate-block", id, side, `${id} is not a repeat block, but ${count} elements in the ${side} match it`, { count }));
    }
    for (const it of s.unmapped ?? []) {
      out.push(instance(c, `unmapped-${side}-content`, undefined, side, `the ${side} has a ${it.kind} outside every mapped block: "${it.key}"`, { item_kind: it.kind, item: it.key }));
    }
  }
  const common = mapped.filter((id) => id in lock.blocks && id in code.blocks);
  // Every mapped id is judged, so a block absent on both sides still fails (rule 3).
  for (const id of mapped) {
    const inLock = id in lock.blocks;
    const inCode = id in code.blocks;
    if (inLock && inCode) continue;
    if (!inLock && !inCode) out.push(instance(c, "missing-block", id, "both", `${id} is mapped, but the lock state draws no block that matches its selector and the code render has no element with data-testid="${id}"`));
    else if (!inCode) out.push(instance(c, "missing-block", id, "code", `the lock draws ${id}; the code render has no element with data-testid="${id}"`));
    else out.push(instance(c, "missing-block", id, "lock", `the code renders ${id}; the lock state draws no block that matches its selector`));
  }
  // Order is compared over the blocks both sides have, so one missing block does not shift the others.
  const lockSeq = lock.order.filter((id) => common.includes(id));
  const codeSeq = code.order.filter((id) => common.includes(id));
  for (const id of common) {
    if (lockSeq.indexOf(id) !== codeSeq.indexOf(id)) {
      out.push(instance(c, "order", id, "code", `${id} is at position ${codeSeq.indexOf(id) + 1} of ${codeSeq.length} in the code, ${lockSeq.indexOf(id) + 1} in the lock`));
    }
  }
  for (const id of common) {
    const want = normalizeText(lock.blocks[id].text);
    const got = normalizeText(code.blocks[id].text);
    if (want !== got) {
      out.push(instance(c, "text", id, "code", `${id} text differs: lock "${want}", code "${got}"`, { code_text_sha256: sha256(got) }));
    }
  }
  return out;
}

// Runs in the page. `mode` "lock": block selectors from the map, root from data-state or the numbered label.
// Mode "code": data-testid selectors, root is [data-fidelity-root]. A block's text leaves out the text of
// mapped blocks nested inside it, so one gap is one instance and not one per ancestor.
function readSide({ mode, state, profile, blocks }) {
  let root;
  if (mode === "lock") {
    const n = state.replace(/^s/, "");
    root = document.querySelector(`[data-state="${state}"][data-profile="${profile}"]`)
      ?? document.querySelector(`[data-state="${state}"]`)
      ?? [...document.querySelectorAll(".lbl")].find((l) => l.textContent.trim().startsWith(`${n} ·`))?.parentElement;
  } else {
    root = document.querySelector("[data-fidelity-root]");
  }
  if (!root) return { error: mode === "lock" ? `the lock has no state ${state}` : "the harness page has no [data-fidelity-root]" };
  // The state root itself can be a block (the lock puts data-state on .card / .cc).
  const all = (sel) => [...(root.matches(sel) ? [root] : []), ...root.querySelectorAll(sel)];
  const found = blocks.map((b) => ({
    id: b.id,
    els: all(mode === "lock" ? b.selector : `[data-testid="${b.id}"]`),
    repeat: b.repeat === true,
  })).filter((b) => b.els.length > 0);
  const mapped = new Set(found.flatMap((b) => b.els));
  const textOf = (el) => {
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    let text = "";
    for (let t = walker.nextNode(); t; t = walker.nextNode()) {
      let a = t.parentElement;
      while (a && !mapped.has(a)) a = a.parentElement;
      if (a === el) text += t.nodeValue;
    }
    return text;
  };
  const out = {};
  for (const b of found) {
    const els = b.repeat ? b.els : [b.els[0]];
    out[b.id] = { text: els.map(textOf).join("\n"), first: b.els[0] };
  }
  const dups = Object.fromEntries(found.filter((b) => !b.repeat && b.els.length > 1).map((b) => [b.id, b.els.length]));
  // Items outside every mapped block (rules 1 and 2): visible text nodes, controls and images, one per node or element.
  const norm = (t) => String(t).replace(/\s+/g, " ").trim();
  const ROLES = new Set(["button", "link", "checkbox", "radio", "switch", "tab", "menuitem", "menuitemcheckbox", "menuitemradio", "option", "textbox", "searchbox", "combobox", "slider", "spinbutton", "treeitem"]);
  const isControl = (el) => el.matches("button, a, input:not([type=hidden]), select, textarea") || ROLES.has(el.getAttribute("role") ?? "");
  const labelledBy = (el) => norm((el.getAttribute("aria-labelledby") ?? "").split(/\s+/).map((i) => document.getElementById(i)?.textContent ?? "").join(" "));
  const nameOf = (el) => norm(el.getAttribute("aria-label") ?? "") || labelledBy(el) || norm(el.textContent)
    || norm(el.getAttribute("alt") ?? el.getAttribute("title") ?? el.getAttribute("placeholder") ?? el.value ?? "");
  const unmapped = [];
  const walk = (el) => {
    if (mapped.has(el) || !el.checkVisibility({ visibilityProperty: true })) return;
    if (el.matches("script, style, noscript, template")) return;
    if (el.matches("img, svg")) { unmapped.push({ kind: "image", key: el.matches("img") ? `img|${el.getAttribute("src") ?? ""}|${el.getAttribute("alt") ?? ""}` : norm(el.outerHTML) }); return; }
    if (isControl(el)) { unmapped.push({ kind: "control", key: nameOf(el) }); return; }
    for (const n of el.childNodes) {
      if (n.nodeType === Node.ELEMENT_NODE) walk(n);
      else if (n.nodeType === Node.TEXT_NODE && norm(n.nodeValue) !== "") unmapped.push({ kind: "text", key: norm(n.nodeValue) });
    }
  };
  walk(root);
  const order = found.sort((a, b) => (a.els[0].compareDocumentPosition(b.els[0]) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1)).map((b) => b.id);
  const rootMark = mode === "lock" ? { marked: root.hasAttribute("data-state"), profile: root.getAttribute("data-profile"), width: root.getBoundingClientRect().width } : undefined;
  return { order, dups, unmapped, rootMark, blocks: Object.fromEntries(Object.entries(out).map(([id, v]) => [id, { text: v.text }])) };
}

// Runs in the page, on a whole lock file: the lock inventory (rules 10, 11 and 12). Items are the visible text nodes, controls
// and images (as readSide); an item is claimed when it sits inside a data-state root or a data-annotation element.
function readLockInventory() {
  const norm = (t) => String(t).replace(/\s+/g, " ").trim();
  const ROLES = new Set(["button", "link", "checkbox", "radio", "switch", "tab", "menuitem", "menuitemcheckbox", "menuitemradio", "option", "textbox", "searchbox", "combobox", "slider", "spinbutton", "treeitem"]);
  const isControl = (el) => el.matches("button, a, input:not([type=hidden]), select, textarea") || ROLES.has(el.getAttribute("role") ?? "");
  const labelledBy = (el) => norm((el.getAttribute("aria-labelledby") ?? "").split(/\s+/).map((i) => document.getElementById(i)?.textContent ?? "").join(" "));
  const nameOf = (el) => norm(el.getAttribute("aria-label") ?? "") || labelledBy(el) || norm(el.textContent)
    || norm(el.getAttribute("alt") ?? el.getAttribute("title") ?? el.getAttribute("placeholder") ?? el.value ?? "");
  const claimed = (el) => el.closest("[data-state], [data-annotation]") !== null;
  const unclaimed = [];
  const walk = (el) => {
    if (!el.checkVisibility({ visibilityProperty: true })) return;
    if (el.matches("script, style, noscript, template")) return;
    if (el.matches("img, svg")) { if (!claimed(el)) unclaimed.push({ kind: "image", key: el.matches("img") ? `img|${el.getAttribute("src") ?? ""}|${el.getAttribute("alt") ?? ""}` : norm(el.outerHTML) }); return; }
    if (isControl(el)) { if (!claimed(el)) unclaimed.push({ kind: "control", key: nameOf(el) }); return; }
    for (const n of el.childNodes) {
      if (n.nodeType === Node.ELEMENT_NODE) walk(n);
      else if (n.nodeType === Node.TEXT_NODE && norm(n.nodeValue) !== "" && !claimed(el)) unclaimed.push({ kind: "text", key: norm(n.nodeValue) });
    }
  };
  walk(document.body);
  const states = [...document.querySelectorAll("[data-state]")].map((e) => ({ state: e.getAttribute("data-state"), profile: e.getAttribute("data-profile") || null }));
  const badAnnotations = [...document.querySelectorAll("[data-annotation]")].flatMap((el) => {
    const label = `<${el.tagName.toLowerCase()}> "${norm(el.textContent).slice(0, 40)}"`;
    const why = [];
    if (el.matches("[data-state]") || el.querySelector("[data-state]")) why.push("contains a state root");
    if (el.parentElement?.closest("[data-state]")) why.push("sits inside a state root");
    if (isControl(el) || [...el.querySelectorAll("*")].some(isControl)) why.push("contains a control");
    return why.map((w) => ({ why: w, label }));
  });
  const wholeFileAnnotation = document.documentElement.hasAttribute("data-annotation") || document.body.hasAttribute("data-annotation");
  return { states, wholeFileAnnotation, unclaimed, badAnnotations };
}

/** A case's interaction state: steps run on the code render, in order. { click: sel } or { type: sel, text }. */
const stepLabel = (st) => (st.click ? `click ${st.click}` : `type ${st.type}`);
async function runSteps(page, steps = []) {
  for (const st of steps) {
    try {
      const target = page.locator(st.click ?? st.type).first();
      await target.click({ timeout: STEP_TIMEOUT_MS });
      if (st.type) await page.keyboard.type(st.text);
    } catch (err) {
      throw new Error(`step ${stepLabel(st)} failed: ${String(err?.message ?? err).split("\n")[0]}`);
    }
  }
  if (steps.length > 0) await page.waitForTimeout(200);
}

/**
 * The files the lock inventory reads: every HTML file in `export_files` of the LOCK.json of each lock folder. The folders do
 * not come from the cases alone (a map that dropped every case of a lock would hide it): they are the folders of the cases,
 * the topic's own folder design/<topic>/approved when it exists, and every --lock-dir (which must exist).
 */
function lockInventoryFiles(caseLockFiles, topic, extraDirs, shown) {
  const dirs = new Set(caseLockFiles.map((f) => dirname(resolve(ROOT, f))));
  const topicDir = resolve(ROOT, "design", String(topic ?? ""), "approved");
  if (topic && existsSync(topicDir)) dirs.add(topicDir);
  for (const d of extraDirs) {
    const abs = resolve(ROOT, d);
    if (!existsSync(abs)) throw new Error(`--lock-dir ${d} does not exist`);
    dirs.add(abs);
  }
  return [...dirs].flatMap((dir) => {
    let lock;
    try {
      lock = JSON.parse(readFileSync(resolve(dir, "LOCK.json"), "utf8"));
    } catch (err) {
      throw new Error(`${shown(resolve(dir, "LOCK.json"))} cannot be read (${err?.code ?? err?.message}); the lock inventory takes its files from LOCK.json export_files`);
    }
    return Object.keys(lock.export_files ?? {}).filter((n) => /\.html?$/i.test(n)).map((n) => shown(resolve(dir, n)));
  });
}

function buildHarness(out) {
  const build = resolve(out, "harness-build");
  const r = spawnSync(resolve(ROOT, "node_modules/.bin/vite"), ["build", "--config", "scripts/design/pattern-harness/fidelity.vite.config.mjs"], {
    cwd: ROOT, env: { ...process.env, HARNESS_OUT: build }, encoding: "utf8", maxBuffer: 1 << 28,
  });
  if (r.status !== 0) throw new Error(`harness build failed:\n${r.stdout}\n${r.stderr}`);
  const read = (ext) => readdirSync(`${build}/assets`).filter((f) => f.endsWith(ext)).map((f) => readFileSync(`${build}/assets/${f}`, "utf8")).join("\n");
  const safe = (s, tag) => s.replaceAll(`</${tag}`, `<\\/${tag}`);
  const page = resolve(build, "page.html");
  writeFileSync(page, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>${safe(read(".css"), "style")}</style></head><body><div id="root"></div><script>${safe(read(".js"), "script")}</script></body></html>`);
  return page;
}

async function sideBySide(browser, lockPng, codePng, file) {
  const page = await browser.newPage();
  const img = (b) => `<img src="data:image/png;base64,${b.toString("base64")}" style="vertical-align:top;margin:8px;border:1px solid #888">`;
  await page.setContent(`<body style="margin:0;background:#fff;font:12px sans-serif"><div style="display:flex;align-items:flex-start"><div>lock${lockPng ? img(lockPng) : "<p>no render</p>"}</div><div>code${codePng ? img(codePng) : "<p>no render</p>"}</div></div></body>`);
  await page.screenshot({ path: file, fullPage: true });
  await page.close();
}

export async function checkFidelity({ blocksPath = DEFAULT_BLOCKS, out, only = [], harness, lockDirs = [] } = {}) {
  const { chromium } = await import("@playwright/test");
  const map = parse(readFileSync(resolve(ROOT, blocksPath), "utf8"));
  const devices = new Map(parse(readFileSync(resolve(ROOT, DEVICES), "utf8")).profiles.map((p) => [p.id, p]));
  const blocks = Object.entries(map.blocks).map(([id, b]) => ({ id, selector: b.selector, repeat: b.repeat }));
  const cases = map.cases.filter((c) => only.length === 0 || only.includes(c.state));
  // The inventory and rule 7 read every case of the map, whatever --case selects to render.
  const shown = (p) => { const a = resolve(ROOT, p); const r = relative(ROOT, a); return r.startsWith("..") ? a : r; };
  const allCases = map.cases.map((c) => ({ ...c, lock_file: shown(c.lock_file) }));
  const lockFiles = lockInventoryFiles(map.cases.map((c) => c.lock_file), map.topic, lockDirs, shown);
  mkdirSync(out, { recursive: true });
  const harnessPage = harness ?? buildHarness(out);
  const browser = await chromium.launch();
  const violations = [];
  const renders = [];
  try {
    for (const file of lockFiles) {
      const page = await browser.newPage({ viewport: { width: LOCK_FILE_WIDTH, height: 900 } });
      try {
        await page.goto(pathToFileURL(resolve(ROOT, file)).href, { timeout: RENDER_TIMEOUT_MS });
        const inv = await page.evaluate(readLockInventory);
        for (const it of inv.unclaimed) if (it.kind === "image") it.key = sha256(it.key);
        violations.push(...lockFileViolations(file, inv), ...uncoveredStates(file, inv.states, allCases));
      } catch (err) {
        violations.push(fileInstance("missing-render", file, `lock inventory render failed: ${err?.message ?? err}`));
      } finally {
        await page.close();
      }
    }
    for (const c of cases) {
      const profile = devices.get(c.profile);
      if (!profile) { violations.push(...profileViolations(c, devices)); continue; }
      const open = () => browser.newPage({ viewport: { width: profile.width, height: profile.height }, deviceScaleFactor: profile.dpr });
      const shots = { lock: null, code: null };
      const sides = {};
      let rootHash;
      for (const side of ["lock", "code"]) {
        const page = await open();
        try {
          const url = side === "lock"
            ? pathToFileURL(resolve(ROOT, c.lock_file)).href
            : `${pathToFileURL(harnessPage).href}?fixture=${encodeURIComponent(c.fixture)}`;
          await page.goto(url, { timeout: RENDER_TIMEOUT_MS });
          if (side === "code") {
            await page.waitForSelector("[data-fidelity-root], [data-fidelity-error]", { timeout: RENDER_TIMEOUT_MS });
            if (await page.$("[data-fidelity-error]")) throw new Error(`fixture not found: ${c.fixture}`);
            await page.waitForTimeout(300); // lazy content (markdown, editor) settles
          }
          if (side === "code") {
            await runSteps(page, c.steps);
            rootHash = sha256(await page.evaluate(() => document.querySelector("[data-fidelity-root]").outerHTML));
          }
          const read = await page.evaluate(readSide, { mode: side, state: c.state, profile: c.profile, blocks });
          if (read.error) throw new Error(read.error);
          if (side === "lock") violations.push(...profileViolations(c, devices, read.rootMark));
          for (const it of read.unmapped) if (it.kind === "image") it.key = sha256(it.key);
          sides[side] = read;
          const rootSel = side === "code" ? "[data-fidelity-root]" : `[data-state="${c.state}"]`;
          const rootEl = (await page.$(rootSel)) ?? (await page.evaluateHandle((n) => [...document.querySelectorAll(".lbl")].find((l) => l.textContent.trim().startsWith(`${n} ·`))?.parentElement, c.state.replace(/^s/, "")));
          shots[side] = await rootEl.asElement()?.screenshot();
        } catch (err) {
          violations.push(...renderFailure(c, side, err));
        } finally {
          await page.close();
        }
      }
      renders.push({ state: c.state, profile: c.profile, fixture: c.fixture, steps: (c.steps ?? []).map(stepLabel), code_root_sha256: rootHash });
      if (sides.lock && sides.code) violations.push(...compareBlocks(c, sides.lock, sides.code, blocks.map((b) => b.id)));
      await sideBySide(browser, shots.lock, shots.code, resolve(out, `side-by-side-${c.state}-${c.profile}.png`));
    }
  } finally {
    await browser.close();
  }
  const result = { topic: map.topic, cases: cases.map((c) => `${c.state}@${c.profile}`), lock_files: lockFiles, ok: violations.length === 0, renders, violations };
  writeFileSync(resolve(out, "result.json"), `${JSON.stringify(result, null, 2)}\n`);
  return result;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const args = process.argv.slice(2);
  const opt = (name) => args.flatMap((a, i) => (a === name ? [args[i + 1]] : []));
  const out = resolve(opt("--out")[0] ?? ".fidelity-out/run");
  const result = await checkFidelity({ blocksPath: opt("--blocks")[0], out, only: opt("--case"), lockDirs: opt("--lock-dir"), harness: opt("--harness")[0] && resolve(opt("--harness")[0]) });
  for (const v of result.violations) console.error(`${v.rule} ${v.state ? `${v.state}@${v.profile}` : v.file} ${v.block ?? v.item_kind ?? ""}: ${v.reason}`);
  console.log(`${result.violations.length} violation instance(s) in ${result.cases.length} case(s); result: ${resolve(out, "result.json")}`);
  process.exit(result.ok ? 0 : 1);
}
