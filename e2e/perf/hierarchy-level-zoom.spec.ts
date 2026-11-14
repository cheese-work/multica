/**
 * Hierarchy level-zoom gate (CHE-4312 R1, success criterion 3).
 *
 * Opens the hierarchy dialog on one issue of a real workspace (read-only: the spec only reads and presses keys),
 * then steps in (`+`) to the deepest card along the first-child path and out (`-`) to the root, repeatedly.
 * Interval per level change: the `level-start` performance mark (set on the zoom input) to the `level-ready`
 * mark (set once the new level's `data-level-ready` attribute is on the canvas).
 *
 * One warm-up pass (its first change is the cold value: recorded, not gated), then 3 passes x 20 level changes
 * = 60 warm samples. Gate: warm p95 < 500 ms.
 *
 * Needs a running production build of the candidate and a token for the workspace:
 *   PLAYWRIGHT_BASE_URL=http://localhost:3000 PERF_TOKEN=<pat> pnpm exec playwright test \
 *     --config=playwright.perf.config.ts hierarchy-level-zoom
 * Optional: PERF_WORKSPACE_SLUG (default cheese-work), PERF_ISSUE (default CHE-2797), PERF_REPORT_PATH.
 */
import { expect, test, type Page } from "@playwright/test";
import { writeFileSync } from "node:fs";

const SLUG = process.env.PERF_WORKSPACE_SLUG ?? "cheese-work";
const ISSUE = process.env.PERF_ISSUE ?? "CHE-2797";
const PASSES = 3;
const CHANGES_PER_PASS = 20;
const GATE_P95_MS = 500;

/** Nearest-rank percentile. */
function percentile(values: number[], p: number): number {
  if (values.length === 0) throw new Error("no samples");
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.max(0, Math.ceil((p / 100) * sorted.length) - 1)]!;
}

/** The first level change after load: the cold value. Recorded, not gated. */
let firstChangeMs: number | null = null;

/** One level change: press the key, wait for the new depth to be ready, return the mark interval in ms. */
async function change(page: Page, key: "+" | "-", expectedDepth: number): Promise<number> {
  await page.evaluate(() => { performance.clearMarks("level-start"); performance.clearMarks("level-ready"); });
  await page.keyboard.press(key);
  await page.waitForFunction(
    (d) => document.querySelector('[data-testid="hierarchy-canvas"]')?.getAttribute("data-level-ready") === String(d),
    expectedDepth,
    { timeout: 30_000 },
  );
  const ms = await page.evaluate(() => {
    const start = performance.getEntriesByName("level-start").at(-1);
    const ready = performance.getEntriesByName("level-ready").at(-1);
    return start && ready ? ready.startTime - start.startTime : null;
  });
  firstChangeMs ??= ms;
  if (ms === null || ms < 0) throw new Error(`level-start/level-ready marks missing or inverted after "${key}"`);
  return ms;
}

/** Ids of the focus-level cards that have children (a dashed border marks a leaf). */
async function branchIds(page: Page): Promise<string[]> {
  return page.locator("a[data-card-id]:not(.border-dashed)").evaluateAll((els) => els.map((e) => e.getAttribute("data-card-id")!));
}

async function stepIn(page: Page, id: string, depth: number): Promise<number> {
  await page.locator(`a[data-card-id="${id}"]`).focus();
  return change(page, "+", depth + 1);
}

/** Depth-first over every branch card by keyboard; returns the card-id path to the deepest level. */
async function findDeepestPath(page: Page): Promise<string[]> {
  let best: string[] = [];
  const visit = async (depth: number, path: string[]) => {
    if (path.length > best.length) best = path;
    const ids = depth === 0 ? [] : await branchIds(page);
    // Depth 0 shows only the root card, which is focused already.
    for (const id of depth === 0 ? [""] : ids) {
      if (depth === 0) await page.getByTestId("hierarchy-canvas").focus();
      else if (await page.locator(`a[data-card-id="${id}"]`).count() === 0) continue;
      const canIn = depth === 0 ? !(await page.getByRole("button", { name: "Step in" }).isDisabled()) : true;
      if (!canIn) continue;
      if (depth === 0) await change(page, "+", 1); else await stepIn(page, id, depth);
      await visit(depth + 1, [...path, id]);
      await change(page, "-", depth);
    }
  };
  await visit(0, []);
  return best;
}

/** In along `path` to its deepest level, then out to the root, repeated until `count` changes are recorded. */
async function walk(page: Page, path: string[], count: number): Promise<number[]> {
  const samples: number[] = [];
  let depth = 0;
  let direction: "in" | "out" = "in";
  const down = async () => {
    if (depth === 0) { await page.getByTestId("hierarchy-canvas").focus(); samples.push(await change(page, "+", 1)); }
    else samples.push(await stepIn(page, path[depth]!, depth));
    depth += 1;
  };
  const up = async () => { samples.push(await change(page, "-", depth - 1)); depth -= 1; };
  while (samples.length < count) {
    if (depth === path.length) direction = "out";
    if (depth === 0) direction = "in";
    await (direction === "in" ? down() : up());
  }
  const measured = samples.slice(0, count);
  while (depth > 0) await up();
  return measured;
}

test("hierarchy level zoom: warm p95 under 500 ms", async ({ page }) => {
  const token = process.env.PERF_TOKEN;
  if (!token) throw new Error("PERF_TOKEN is required: the gate never skips");
  await page.addInitScript((t) => {
    localStorage.setItem("multica_token", t);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, token);

  await page.goto(`/${SLUG}/issues/${ISSUE}`, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Expand hierarchy" }).click();
  await expect(page.getByTestId("hierarchy-canvas")).toHaveAttribute("data-level-ready", "0", { timeout: 60_000 });
  await page.getByTestId("hierarchy-canvas").focus();

  const path = await findDeepestPath(page);
  const deepest = path.length;
  expect(deepest, "the tree must have at least one level below the root").toBeGreaterThanOrEqual(1);
  await page.getByTestId("hierarchy-canvas").focus();

  const cold = firstChangeMs!;
  // Warm-up pass, not counted.
  await walk(page, path, CHANGES_PER_PASS);
  const warm: number[] = [];
  for (let pass = 0; pass < PASSES; pass++) warm.push(...(await walk(page, path, CHANGES_PER_PASS)));

  const report = {
    issue: ISSUE,
    deepest_depth: deepest,
    cold_ms: Math.round(cold),
    warm_samples: warm.length,
    warm_p50_ms: Math.round(percentile(warm, 50)),
    warm_p95_ms: Math.round(percentile(warm, 95)),
    warm_max_ms: Math.round(Math.max(...warm)),
    gate_p95_ms: GATE_P95_MS,
  };
  console.log(`HIERARCHY_LEVEL_ZOOM ${JSON.stringify(report)}`);
  if (process.env.PERF_REPORT_PATH) writeFileSync(process.env.PERF_REPORT_PATH, JSON.stringify(report, null, 2));

  expect(warm).toHaveLength(PASSES * CHANGES_PER_PASS);
  expect(report.warm_p95_ms).toBeLessThan(GATE_P95_MS);
});
