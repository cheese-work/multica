// Player traversal for the Live track (CHE-6753 A2, slice L6): drives the real doop
// player in Playwright, from the index frame, through every edge and Home, and
// asserts the frame id reached at each step. doop drops an interaction without an
// action and `get_prototype` does not report it, so this is the check of record.
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { chromium } from "playwright-core";

/** The Playwright-pinned Chromium, else the newest one in the local browser cache. */
export async function launchBrowser() {
  try {
    return await chromium.launch();
  } catch (first) {
    const root = process.env.PLAYWRIGHT_BROWSERS_PATH || path.join(homedir(), ".cache", "ms-playwright");
    const dirs = existsSync(root) ? readdirSync(root).filter((d) => /^chromium-\d+$/.test(d)).sort() : [];
    for (const dir of dirs.reverse()) {
      for (const bin of ["chrome-linux64/chrome", "chrome-linux/chrome"]) {
        const executablePath = path.join(root, dir, bin);
        if (existsSync(executablePath)) return chromium.launch({ executablePath });
      }
    }
    throw first;
  }
}

const fail = (code, message) => Object.assign(new Error(message), { code });

/**
 * Edge triggers: `click` (needs `selector`), `key:<Key>`, `delay:<ms>`.
 * Returns edges with `kind` and its payload (`key`, `ms`).
 */
export function normalizeEdges(edges) {
  return edges.map((e) => {
    const base = { ...e, id: e.id ?? `${e.from}:${e.on}` };
    if (e.on === "click") {
      if (!e.selector) throw fail("invalid-edge", `edge ${base.id}: click needs a selector`);
      return { ...base, kind: "click" };
    }
    const key = /^key:(.+)$/.exec(e.on ?? "");
    if (key) return { ...base, kind: "key", key: key[1] };
    const delay = /^delay:(\d{1,7})$/.exec(e.on ?? "");
    if (delay) return { ...base, kind: "delay", ms: Number(delay[1]) };
    throw fail("invalid-edge", `edge ${base.id}: unknown trigger ${JSON.stringify(e.on)}`);
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** The frame id on top in the player, or null while a transition has no incoming screen. */
async function current(page) {
  return page.evaluate(() => {
    const screens = [...document.querySelectorAll("[data-proto-screen]")].filter(
      (el) => el.getAttribute("data-proto-role") !== "from",
    );
    return screens.at(-1)?.getAttribute("data-proto-screen") ?? null;
  });
}

async function waitFor(page, frameId, ms) {
  const end = Date.now() + ms;
  let now = await current(page);
  while (now !== frameId && Date.now() < end) {
    await sleep(50);
    now = await current(page);
  }
  return now;
}

/** Press a key with the focus on the player, not in a frame: the player listens on the top window. */
async function press(page, key) {
  await page.evaluate(() => document.activeElement?.blur());
  await page.keyboard.press(key);
}

/**
 * Press `key` until the player shows `frameId`: a screen's triggers go live shortly after its HTML lands.
 * It stops pressing the moment the player leaves `from` for another frame, so a delay that fires first
 * is never answered with a key press on the next frame.
 */
async function pressUntil(page, key, frameId, from, ms) {
  const end = Date.now() + ms;
  for (;;) {
    await press(page, key);
    const now = await waitFor(page, frameId, 250);
    if (now === frameId || now !== from || Date.now() > end) return now;
  }
}

/** Fire one edge's trigger in the player. A click retries until the screen's runtime is live. */
async function fire(page, edge, settleMs) {
  if (edge.kind === "delay") return waitFor(page, edge.to, edge.ms + settleMs);
  if (edge.kind === "key") {
    return pressUntil(page, edge.key, edge.to, edge.from, settleMs);
  }
  const target = page.frameLocator(`iframe[data-doop-proto-frame="${edge.from}"]`).locator(edge.selector);
  const end = Date.now() + settleMs;
  for (;;) {
    try {
      await target.first().click({ timeout: 500 });
    } catch {
      /* the screen is not rendered yet; try again until the deadline */
    }
    const now = await waitFor(page, edge.to, 250);
    if (now === edge.to || Date.now() > end) return now;
  }
}

/**
 * Walk every edge from the index frame, and check Home on every frame the walk reaches.
 *
 * @param {object} o
 * @param {import("playwright-core").Page} o.page   a page whose context holds the doop session
 * @param {string} o.base          doop origin
 * @param {string} o.canvasId
 * @param {string} o.indexFrameId  the flow start
 * @param {object[]} o.edges       `{ id?, from, on, selector?, to }`, frame ids
 * @param {number} [o.settleMs]    how long a step may take to land (default 2000)
 * @returns {Promise<{ ok: boolean, problems: object[], checked: object[] }>}
 */
export async function traverse({ page, base, canvasId, indexFrameId, edges, settleMs = 2000 }) {
  const list = normalizeEdges(edges);
  const problems = [];
  const checked = [];
  const seen = new Set();
  const report = (p) => {
    const k = JSON.stringify([p.code, p.edge, p.frame, p.on]);
    if (!seen.has(k)) {
      seen.add(k);
      problems.push(p);
    }
  };
  const open = async () => {
    await page.goto(`${base}/c/${canvasId}/proto?frame=${encodeURIComponent(indexFrameId)}`);
    const now = await waitFor(page, indexFrameId, settleMs * 3);
    if (now !== indexFrameId) throw fail("index-not-shown", `the player opened on ${now}, not the index ${indexFrameId}`);
    await page.frameLocator(`iframe[data-doop-proto-frame="${indexFrameId}"]`).locator("body").waitFor({ timeout: settleMs * 3 });
  };

  // shortest edge path from the index to each frame (breadth first)
  const path = new Map([[indexFrameId, []]]);
  for (const frame of path.keys()) {
    for (const e of list.filter((x) => x.from === frame && !path.has(x.to))) path.set(e.to, [...path.get(frame), e]);
  }

  /** Open the player on the index and follow `steps`; returns the failing step, or null. */
  async function walk(steps) {
    await open();
    for (const step of steps) {
      const reached = await fire(page, step, settleMs);
      checked.push({ edge: step.id, frame: step.from, on: step.on, reached, expected: step.to });
      if (reached !== step.to) return { step, actual: reached };
    }
    return null;
  }

  for (const edge of list) {
    const steps = path.get(edge.from);
    if (!steps) {
      report({ code: "edge-unreachable", edge: edge.id, frame: edge.from, on: edge.on, expected: edge.to, actual: null, message: "the from frame is not reachable from the index" });
      continue;
    }
    const broken = await walk([...steps, edge]);
    if (broken) {
      report({ code: "edge-unreachable", edge: broken.step.id, frame: broken.step.from, on: broken.step.on, expected: broken.step.to, actual: broken.actual });
    }
  }

  // Home on every frame the walk reaches except the index: pressed at once, it reaches the index
  // and the index is still shown after twice the longest delay of that frame.
  for (const [frame, steps] of path) {
    if (frame === indexFrameId) continue;
    const delays = list.filter((e) => e.from === frame && e.kind === "delay").map((e) => e.ms);
    const hold = delays.length ? 2 * Math.max(...delays) : 0;
    const arrived = await walk(steps);
    if (arrived) continue; // the broken step is already reported
    let reached = await pressUntil(page, "Home", indexFrameId, frame, settleMs);
    if (reached === indexFrameId && hold) {
      await sleep(hold);
      reached = await current(page);
    }
    checked.push({ edge: `home:${frame}`, frame, on: "key:Home", reached, expected: indexFrameId, heldMs: hold });
    if (reached !== indexFrameId) {
      report({ code: "edge-unreachable", edge: `home:${frame}`, frame, on: "key:Home", expected: indexFrameId, actual: reached });
    }
  }

  return { ok: problems.length === 0, problems, checked };
}
