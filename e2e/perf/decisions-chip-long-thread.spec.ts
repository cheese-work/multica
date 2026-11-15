/**
 * The chip on a long real thread (CHE-5376, live proof 2.4): the click must end with
 * the decision comment in view. L1 uses a 45-comment thread of equal rows; CHE-4091 is
 * long, with rows of very different heights, so the heights Virtuoso estimates for the
 * rows above the target are wrong. The failure seen live is the page at the top of the
 * thread with the target not rendered. This asserts where the page ends up (it lands within 40 s and
 * still shows 3 s later), not how fast it gets there.
 */
import { expect, test } from "@playwright/test";
import * as fx from "./fixtures/scroll-stability";
import { boot } from "./lib/dm-harness";

const THREAD_LENGTH = 400;
const TARGET_INDEX = 320;
const LAND_LIMIT_MS = 40_000;
const HOLD_MS = 3000;
const CPU_SLOWDOWN = 4;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

/** Short one-liners, mid prose, and tall code or list comments, in a fixed pseudo-random mix. */
function body(index: number): string {
  const kind = (index * 7919) % 5;
  if (kind === 0) return `Ack ${index}.`;
  if (kind === 1) return fx.timelineEntries()[index % fx.COMMENT_COUNT]!.content;
  if (kind === 2) return Array.from({ length: 40 }, (_, i) => `- item ${i} of comment ${index}`).join("\n");
  if (kind === 3) return `Comment ${index}\n\n${"```ts\n" + Array.from({ length: 60 }, (_, i) => `const v${i} = ${i};`).join("\n") + "\n```"}`;
  return `Comment ${index}: ${"a longer sentence about the change. ".repeat(12)}`;
}

const entries = () =>
  Array.from({ length: THREAD_LENGTH }, (_, index) => ({
    type: "comment" as const, id: fx.commentId(index), actor_type: "user" as const,
    actor_id: fx.USER_ID, created_at: fx.at(index), updated_at: fx.at(index), issue_id: fx.ISSUE_ID,
    parent_id: null, content: body(index), resolved_at: null, reactions: [],
    attachments: [], source_task_id: null,
  }));

test("L2 chip click on a long varied thread ends with the decision comment in view", async ({ page, baseURL }) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  await page.route(new RegExp(`/api/issues/${fx.ISSUE_ID}/timeline`), (route) =>
    route.fulfill({ json: entries() }));
  app.openDecisions = [{
    id: "aaaa0002-4444-4444-8444-444444444444", issue_id: fx.ISSUE_ID, issue_identifier: fx.ISSUE_IDENTIFIER,
    asking_agent_id: fx.AGENT_ID, asking_agent_name: "Perf agent", ask: "**Decision**\nPick?",
    escalation_class: "reversible", created_at: fx.at(1), chat_session_id: "", message_id: null,
    source_comment_id: fx.commentId(TARGET_INDEX),
  }];
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/inbox`, { waitUntil: "domcontentloaded" });
  const chip = page.getByRole("link", { name: /decision/i }).first();
  await expect(chip).toBeVisible({ timeout: 60_000 });
  // A reader's laptop is slower than this host: the list mounts and renders its rows later.
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: CPU_SLOWDOWN });
  await chip.click();
  const view = () => page.evaluate((targetId) => {
    const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]");
    const el = document.getElementById(`comment-${targetId}`);
    if (!root) return { visiblePx: 0, reason: "no scroller" };
    if (!el) return { visiblePx: 0, reason: "target not rendered", scrollTop: Math.round(root.scrollTop), scrollHeight: root.scrollHeight };
    const r = root.getBoundingClientRect(); const e = el.getBoundingClientRect();
    return {
      visiblePx: Math.min(e.bottom, r.bottom) - Math.max(e.top, r.top), top: Math.round(e.top - r.top),
      scrollTop: Math.round(root.scrollTop), scrollHeight: root.scrollHeight,
    };
  }, fx.commentId(TARGET_INDEX));
  // The page lands on the comment, in any time up to the limit ...
  const deadline = Date.now() + LAND_LIMIT_MS;
  let landed = await view();
  while (landed.visiblePx <= 0 && Date.now() < deadline) {
    await page.waitForTimeout(250);
    landed = await view();
  }
  expect(landed.visiblePx, `never landed: ${JSON.stringify(landed)}`).toBeGreaterThan(0);
  // ... and still shows it after the hold: a landing that snaps back to the top fails here.
  await page.waitForTimeout(HOLD_MS);
  const held = await view();
  expect(held.visiblePx, JSON.stringify(held)).toBeGreaterThan(0);
});
