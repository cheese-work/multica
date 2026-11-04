/**
 * Layout stability of a compact thread's reply box (CHE-2970 / A1b, CHE-1935).
 *
 * R5: open a compact thread (more than three replies, oldest hidden behind
 *    "Show N more replies"), focus its reply box (click on desktop, tap on
 *    phone) and type the first character. The thread must stay folded and
 *    neither the reply box nor the viewport may move.
 *
 * Runs once per profile project in playwright.perf.config.ts (desktop, phone),
 * both under 4x CPU throttling, against the production build with synthetic
 * HTTP and WebSocket. Assertions are soft so one run reports every requirement.
 *
 * Layout shift is gated on the raw sum: standard CLS ignores shifts within
 * 500 ms of input, and this scenario is all input.
 */
import { expect, test, type Page } from "@playwright/test";
import * as fx from "./fixtures/chip-stability";

test.use({ locale: "en-US", serviceWorkers: "block" });

/**
 * Raw layout-shift sums per profile, frozen from runs on `main`. R5 is red on both profiles: the
 * limit is half of the lowest value seen on `main` over four runs (desktop 0.1453-0.1462, phone
 * 0.6394 every run). Standard CLS read 0-0.0009 on the same runs: it drops shifts within 500 ms
 * of the keystroke, which is why this gate uses the raw sum.
 *
 * A2-R5 turns the scenario green without touching these; a change needs a new plan revision.
 */
const SHIFT_LIMIT: Record<fx.Profile, number> = { desktop: 0.0726, phone: 0.3197 };
/** One pixel: sub-pixel rounding, not a visible jump. */
const GEOMETRY_LIMIT_PX = 1;

const REPLY_COUNT = 9;
/** Latest three replies show, so the rest are hidden behind the button. */
const VISIBLE_REPLIES = 3;
const FILLER_BEFORE = 3;
const FILLER_AFTER = 8;
const ROOT_INDEX = FILLER_BEFORE;
const commentId = (kind: string, i: number) => `${kind}${String(i).padStart(4, "0")}-4444-4444-8444-444444444444`;
const ROOT_ID = commentId("aaaa", 0);
const hiddenLabel = new RegExp(`^Show ${REPLY_COUNT - VISIBLE_REPLIES} more replies$`);

const at = (n: number) => new Date(Date.UTC(2026, 8, 1) + n * 1000).toISOString();
const entry = (id: string, n: number, parentId: string | null, content: string) => ({
  type: "comment" as const, id, actor_type: "user" as const, actor_id: fx.USER_ID,
  created_at: at(n), updated_at: at(n), issue_id: fx.THREAD_ISSUE_ID, parent_id: parentId, content,
  resolved_at: null, reactions: [], attachments: [], source_task_id: null,
});

/** Filler roots above and below the thread, so the page scrolls and the thread can sit mid-view. */
function timeline() {
  const rows = [];
  let n = 0;
  const filler = (count: number, label: string) => {
    for (let i = 0; i < count; i += 1) {
      rows.push(entry(commentId("ffff", n), n, null, `${label} comment ${i}. Context for the thread in the middle.`));
      n += 1;
    }
  };
  filler(FILLER_BEFORE, "Earlier");
  rows.push(entry(ROOT_ID, n, null, "The thread under test starts here."));
  n += 1;
  for (let i = 0; i < REPLY_COUNT; i += 1) {
    rows.push(entry(commentId("rrrr", i), n, ROOT_ID, `Reply ${i + 1} of ${REPLY_COUNT}. The reviewer walks through what changed.`));
    n += 1;
  }
  filler(FILLER_AFTER, "Later");
  return rows;
}

const nextFrame = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => {
    requestAnimationFrame(() => requestAnimationFrame(() => done()));
  }));

/** One line per assertion input, so the PR body can quote observed values. */
const report = (name: string, values: Record<string, unknown>) =>
  // eslint-disable-next-line no-console
  console.log(`PERF_REPORT ${JSON.stringify({ test: name, ...values })}`);

test("R5: the first typed character keeps a compact thread folded and the reply box in place", async ({
  page, context, baseURL,
}, testInfo) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const profile = fx.profileOf(testInfo.project.name);
  const threadIssue = fx.issueFor(fx.THREAD_ISSUE_IDENTIFIER, fx.THREAD_ISSUE_ID);
  const h = await fx.installHarness(page, context, origin, (path) => {
    switch (path) {
      case `/api/issues/${fx.THREAD_ISSUE_IDENTIFIER}`:
      case `/api/issues/${fx.THREAD_ISSUE_ID}`: return threadIssue;
      case `/api/issues/${fx.THREAD_ISSUE_ID}/timeline`: return timeline();
      case `/api/issues/${fx.THREAD_ISSUE_ID}/task-runs`:
      case `/api/issues/${fx.THREAD_ISSUE_ID}/subscribers`:
      case `/api/issues/${fx.THREAD_ISSUE_ID}/wakeups`:
      case `/api/issues/${fx.THREAD_ISSUE_ID}/attachments`: return [];
      case `/api/issues/${fx.THREAD_ISSUE_ID}/comments/trigger-preview`:
        return { agents: [], blocked: [] };
      case `/api/issues/${fx.THREAD_ISSUE_ID}/labels`: return { labels: [] };
      case `/api/issues/${fx.THREAD_ISSUE_ID}/children`: return { issues: [] };
      case `/api/issues/${fx.THREAD_ISSUE_ID}/pull-requests`: return { pull_requests: [] };
      case `/api/issues/${fx.THREAD_ISSUE_ID}/system-wakeups`: return [];
      case `/api/issues/${fx.THREAD_ISSUE_ID}/duplicates`: return { duplicate_of: null, duplicates: [] };
      // Side panels that do not touch the thread; an empty object lets their parsers fall back.
      case `/api/issues/${fx.THREAD_ISSUE_ID}/relations`:
      case "/api/inbox/archived/page": return {};
      case "/api/issues/child-progress": return { progress: [] };
      default: return undefined;
    }
  });
  h.gate.release();
  await context.addInitScript(() => localStorage.setItem("multica:chat:floatingChatEnabled", "false"));
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => { ws.onMessage(() => {}); });

  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/issues/${fx.THREAD_ISSUE_IDENTIFIER}`, {
    waitUntil: "domcontentloaded",
  });
  // The virtualized timeline renders the thread as several cards that share data-thread-root;
  // the filler comments have reply boxes too, so the box is looked up inside this thread only.
  const thread = page.locator(`[data-thread-root="${ROOT_ID}"]`);
  const showMore = thread.getByRole("button", { name: hiddenLabel });
  const replyRows = thread.locator('[id^="comment-rrrr"]');
  const shell = thread.getByTestId("reply-composer-shell");
  await expect(page.locator('[id^="comment-ffff"]').first()).toBeVisible({ timeout: 30_000 });
  // The timeline is virtualized: scroll the page's own container until the thread's box mounts.
  // Resolve that container once and keep its handle, so the scroll reading never depends on a row
  // that virtualization may unmount; no scrollable ancestor fails the test instead of reading 0.
  const scroller = await page.locator('[id^="comment-ffff"]').first().evaluateHandle((row) => {
    for (let n = row.parentElement; n; n = n.parentElement) {
      const overflow = getComputedStyle(n).overflowY;
      if ((overflow === "auto" || overflow === "scroll") && n.scrollHeight > n.clientHeight) return n;
    }
    throw new Error("no scrollable ancestor found for the issue timeline");
  });
  const scrollTop = () => scroller.evaluate((n) => n.scrollTop);
  for (let step = 0; step < 60 && (await shell.count()) === 0; step += 1) {
    await scroller.evaluate((n) => { n.scrollTop += 400; });
    await page.waitForTimeout(100);
  }
  await expect(thread.first()).toBeVisible();
  await expect(showMore, "thread starts compact").toBeVisible();
  await expect(replyRows, "visible replies when compact").toHaveCount(VISIBLE_REPLIES);

  // The reply box's wrapper survives the swap from static shell to live editor.
  const box = await shell.evaluateHandle((el) => el.parentElement!);
  await shell.evaluate((el) => el.scrollIntoView({ block: "center" }));
  await nextFrame(page);
  expect(await shell.evaluate((el, c) => c.contains(el), scroller), "reply box sits inside the measured scroll container").toBe(true);
  const boxTop = async () => (await box.asElement()!.boundingBox())!.y;
  const topBefore = await boxTop();
  const scrollBefore = await scrollTop();
  await fx.resetShift(page);

  // Focus the reply box the way the profile's user does, then type one character.
  if (profile === "phone") await shell.tap();
  else await shell.click();
  const editor = thread.locator(".ProseMirror");
  await expect(editor).toBeVisible();
  await expect(editor, "reply box has focus").toBeFocused();
  await page.keyboard.type("x");
  await expect(editor).toContainText("x");
  // Draft store write (debounced) and the expansion effect that follows it.
  await page.waitForTimeout(600);
  await nextFrame(page);

  const folded = await showMore.count();
  const repliesShown = await replyRows.count();
  const topDelta = Math.abs((await boxTop()) - topBefore);
  const scrollDelta = Math.abs((await scrollTop()) - scrollBefore);
  const shift = await fx.readShift(page);

  expect.soft(folded, "Show-more button still present (thread stays folded)").toBe(1);
  await expect.soft(showMore, "hidden count unchanged").toBeVisible();
  expect.soft(repliesShown, "visible replies").toBe(VISIBLE_REPLIES);
  expect.soft(topDelta, "reply box top delta (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(scrollDelta, "viewport scroll delta (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(shift.raw, "raw layout-shift sum").toBeLessThanOrEqual(SHIFT_LIMIT[profile]);

  report("R5", { profile, folded: folded === 1, repliesShown, topDelta, scrollDelta, cls: shift.cls, rawShift: shift.raw });
  expect(h.unmocked, "unmocked requests").toEqual([]);
  expect(h.external, "external requests").toEqual([]);
  expect(h.pageErrors, "page errors").toEqual([]);
});
