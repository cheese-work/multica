/**
 * Self-test of the scroll-anchor meter (CHE-3231, A0).
 *
 * The scroll-stability scenarios are only worth reading if the meter can tell
 * a list that moves under the reader from one that does not. This runs the
 * meter, in assert mode, against a synthetic virtualized list with a switch:
 * content inserted above the anchor row (unstable), none (stable), or the same
 * insert with the scroll offset corrected (stable). Browser scroll anchoring is
 * off in the fixture, as it is in Virtuoso. No app, API or server.
 */
import { expect, test, type Page } from "@playwright/test";
import { assertStable, createAnchorMeter } from "./lib/scroll-anchor";
import { MockSocket } from "./lib/mock-socket";

const PAGE = `<!doctype html><body style="margin:0">
<div id="scroller" style="height:400px;overflow-y:auto;overflow-anchor:none"><div id="list"></div></div>
<script>
  const list = document.getElementById("list");
  const scroller = document.getElementById("scroller");
  const row = (id) => {
    const el = document.createElement("div");
    el.dataset.rowId = id;
    el.style.cssText = "height:50px;box-sizing:border-box;border-bottom:1px solid #ccc";
    el.textContent = id;
    return el;
  };
  for (let i = 0; i < 40; i++) list.appendChild(row("row-" + i));
  scroller.scrollTop = 500;
  window.insertAbove = (compensate) => {
    list.insertBefore(row("inserted-" + Math.random()), list.firstChild);
    if (compensate) scroller.scrollTop += 50;
  };
  // Re-keys the row at the top of the viewport; every other row stays put.
  window.replaceTopRow = () => {
    const rows = [...list.children];
    const top = rows.find((el) => el.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    list.replaceChild(row("replaced-" + Math.random()), top);
  };
</script></body>`;

const options = { scroller: "#scroller", rows: "[data-row-id]", idAttr: "data-row-id" };

const nextFrames = (page: Page, n: number) =>
  page.evaluate((count) => new Promise<void>((done) => {
    const tick = (left: number) => (left === 0 ? done() : requestAnimationFrame(() => tick(left - 1)));
    tick(count);
  }), n);

async function measure(page: Page, action: "none" | "insert" | "insert-compensated" | "replace-top") {
  await page.setContent(PAGE);
  await nextFrames(page, 3);
  const meter = createAnchorMeter(page, options);
  await meter.start();
  await nextFrames(page, 3);
  if (action === "replace-top") {
    await page.evaluate(() => (window as unknown as { replaceTopRow: () => void }).replaceTopRow());
  } else if (action !== "none") {
    await page.evaluate((compensate) => (window as unknown as {
      insertAbove: (c: boolean) => void;
    }).insertAbove(compensate), action === "insert-compensated");
  }
  await nextFrames(page, 3);
  return meter.stop();
}

test("reports a jump when content is inserted above the anchor row", async ({ page }) => {
  const summary = await measure(page, "insert");
  expect(summary.jump_count, JSON.stringify(summary)).toBeGreaterThan(0);
  expect(summary.max_delta_px).toBeGreaterThan(1);
});

test("reports no jump when nothing moves", async ({ page }) => {
  const summary = await measure(page, "none");
  expect(summary.frames).toBeGreaterThan(0);
  expect(summary.jump_count).toBe(0);
  expect(() => assertStable(summary)).not.toThrow();
});

test("reports no jump when the offset is corrected for the insert", async ({ page }) => {
  const summary = await measure(page, "insert-compensated");
  expect(summary.jump_count).toBe(0);
  expect(() => assertStable(summary)).not.toThrow();
});

test("reports a jump when the row the reader is on disappears and the rest stays put", async ({ page }) => {
  const summary = await measure(page, "replace-top");
  expect(summary.jump_count, JSON.stringify(summary)).toBeGreaterThan(0);
});

test("a stimulus sent with no connected socket fails instead of vanishing", () => {
  expect(() => new MockSocket().send("task:running", {})).toThrow(/not connected/);
});
