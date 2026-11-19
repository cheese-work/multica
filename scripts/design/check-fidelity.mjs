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
 * Size and colour are not compared (D1). Rules 7 to 12 are later steps.
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
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parse } from "yaml";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const DEFAULT_BLOCKS = "tests/fidelity/decision-card.blocks.yml";
const DEVICES = "design/devices.yml";
const RENDER_TIMEOUT_MS = 20_000;

export const normalizeText = (s) => String(s).replace(/\s+/g, " ").trim();
const STEP_TIMEOUT_MS = 3_000;
const sha256 = (s) => createHash("sha256").update(s).digest("hex");

const instance = (c, rule, block, side, reason, extra = {}) => ({
  rule, state: c.state, profile: c.profile, fixture: c.fixture, ...(block ? { block } : {}), side, reason, ...extra,
});

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
function readSide({ mode, state, blocks }) {
  let root;
  if (mode === "lock") {
    const n = state.replace(/^s/, "");
    root = document.querySelector(`[data-state="${state}"]`)
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
  return { order, dups, unmapped, blocks: Object.fromEntries(Object.entries(out).map(([id, v]) => [id, { text: v.text }])) };
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

export async function checkFidelity({ blocksPath = DEFAULT_BLOCKS, out, only = [], harness } = {}) {
  const { chromium } = await import("@playwright/test");
  const map = parse(readFileSync(resolve(ROOT, blocksPath), "utf8"));
  const devices = new Map(parse(readFileSync(resolve(ROOT, DEVICES), "utf8")).profiles.map((p) => [p.id, p]));
  const blocks = Object.entries(map.blocks).map(([id, b]) => ({ id, selector: b.selector, repeat: b.repeat }));
  const cases = map.cases.filter((c) => only.length === 0 || only.includes(c.state));
  mkdirSync(out, { recursive: true });
  const harnessPage = harness ?? buildHarness(out);
  const browser = await chromium.launch();
  const violations = [];
  const renders = [];
  try {
    for (const c of cases) {
      const profile = devices.get(c.profile);
      if (!profile) { violations.push(...renderFailure(c, "lock", new Error(`profile ${c.profile} is not in ${DEVICES}`))); continue; }
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
          const read = await page.evaluate(readSide, { mode: side, state: c.state, blocks });
          if (read.error) throw new Error(read.error);
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
  const result = { topic: map.topic, cases: cases.map((c) => `${c.state}@${c.profile}`), ok: violations.length === 0, renders, violations };
  writeFileSync(resolve(out, "result.json"), `${JSON.stringify(result, null, 2)}\n`);
  return result;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const args = process.argv.slice(2);
  const opt = (name) => args.flatMap((a, i) => (a === name ? [args[i + 1]] : []));
  const out = resolve(opt("--out")[0] ?? ".fidelity-out/run");
  const result = await checkFidelity({ blocksPath: opt("--blocks")[0], out, only: opt("--case"), harness: opt("--harness")[0] && resolve(opt("--harness")[0]) });
  for (const v of result.violations) console.error(`${v.rule} ${v.state}@${v.profile} ${v.block ?? v.item_kind ?? ""}: ${v.reason}`);
  console.log(`${result.violations.length} violation instance(s) in ${result.cases.length} case(s); result: ${resolve(out, "result.json")}`);
  process.exit(result.ok ? 0 : 1);
}
