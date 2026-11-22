/**
 * Landing on a decision's comment from the layout chip (CHE-5376, review round 1).
 *
 * The chip links to `issue#comment-<id>`. The issue page lands on that comment,
 * then must hold still: the target may move only while the page lands it. The
 * landing ends when the target reaches its requested spot (16 px under the top of
 * the scroller, as the page asks). From there, any frame where it moves more than
 * 8 px in 3 s is an unrequested jump.
 */
import { expect, test } from "@playwright/test";
import * as fx from "./fixtures/scroll-stability";
import { boot } from "./lib/dm-harness";

const MAX_JUMP_PX = 8;
const SETTLE_MS = 3000;
const TARGET_INDEX = 20;
const LANDING_TOP_PX = 16;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

test("L1 chip click lands on the decision comment and holds still", async ({ page, baseURL }) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  app.openDecisions = [{
    id: "aaaa0001-4444-4444-8444-444444444444", issue_id: fx.ISSUE_ID, issue_identifier: fx.ISSUE_IDENTIFIER,
    asking_agent_id: fx.AGENT_ID, asking_agent_name: "Perf agent", ask: "**Decision**\nPick?",
    escalation_class: "reversible", created_at: fx.at(1), chat_session_id: "", message_id: null,
    source_comment_id: fx.commentId(TARGET_INDEX),
  }];
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/inbox`, { waitUntil: "domcontentloaded" });
  const chip = page.getByRole("link", { name: /decision/i }).first();
  await expect(chip).toBeVisible({ timeout: 60_000 });

  await page.evaluate((targetId) => {
    const w = window as unknown as { __landing: { t: number; top: number | null; scroll: number | null }[]; __stop: boolean };
    w.__landing = []; w.__stop = false;
    const t0 = performance.now();
    const tick = () => {
      if (w.__stop) return;
      const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]");
      const el = document.getElementById(`comment-${targetId}`);
      w.__landing.push({
        t: performance.now() - t0,
        top: root && el ? el.getBoundingClientRect().top - root.getBoundingClientRect().top : null,
        scroll: root ? root.scrollTop : null,
      });
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, fx.commentId(TARGET_INDEX));
  await chip.click();
  await page.waitForTimeout(SETTLE_MS + 3000);
  const series = await page.evaluate(() => {
    const w = window as unknown as { __landing: { t: number; top: number | null }[]; __stop: boolean };
    w.__stop = true;
    return w.__landing;
  });

  // The landing is over once the target sits at its requested spot; hold from there.
  const first = series.findIndex((s) => s.top !== null && Math.abs(s.top - LANDING_TOP_PX) <= MAX_JUMP_PX);
  expect(first, "the page never landed on the decision comment").toBeGreaterThanOrEqual(0);
  const hold = series.slice(first).filter((s) => s.t - series[first]!.t <= SETTLE_MS);
  let jumps = 0; let maxDelta = 0;
  for (let i = 1; i < hold.length; i += 1) {
    const a = hold[i - 1]!.top; const b = hold[i]!.top;
    if (a === null || b === null) { jumps += 1; maxDelta = Math.max(maxDelta, 800); continue; }
    const d = Math.abs(b - a);
    maxDelta = Math.max(maxDelta, d);
    if (d > MAX_JUMP_PX) jumps += 1;
  }
  test.info().annotations.push({ type: "landing", description: JSON.stringify({ frames: hold.length, jumps, maxDelta }) });
  // eslint-disable-next-line no-console
  console.log("LANDING_REPORT", JSON.stringify({ frames: hold.length, jumps, maxDelta, trace: hold.filter((_, i) => i % 6 === 0).slice(0, 40) }));
  expect(jumps, `jumps over ${MAX_JUMP_PX}px, max ${maxDelta}px`).toBe(0);
});
