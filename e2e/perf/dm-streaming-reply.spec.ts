/**
 * A streaming reply must not move a reader who scrolled up (CHE-5376, round 3).
 *
 * Live proof on the merged build showed the DM pane following the streaming
 * reply to the bottom after the reader had scrolled up (479 px, limit 8 px).
 * Here the reader scrolls up with the real mouse wheel, then a reply streams
 * for 3 s. The reader's row and the pane position must hold still.
 *
 *  - S1 wheel scroll-up, then a growing reply.
 *  - S2 the reader moves the pane with a scrollTop write (scrollbar, find, automation):
 *       also a reader move, so the reply must not pull the pane back.
 */
import { expect, test, type Page } from "@playwright/test";
import * as fx from "./fixtures/scroll-stability";
import { boot, chatMeter, nextFrames, openChatMidList, type App } from "./lib/dm-harness";

const MAX_JUMP_PX = 8;
const STREAM_MS = 3000;
const TASK_ID = "eeee0001-4444-4444-8444-444444444444";

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

const scrollTop = (page: Page) => page.locator("[data-tab-scroll-root]").evaluate((el) => el.scrollTop);

function taskMessage(seq: number, content: string) {
  return {
    task_id: TASK_ID, issue_id: "", chat_session_id: fx.SESSION_ID, seq, type: "text", content,
  };
}

/** Streams one text frame every 100 ms for STREAM_MS; the live row grows by a line each time. */
async function streamReply(app: App) {
  const frames = STREAM_MS / 100;
  for (let seq = 1; seq <= frames; seq += 1) {
    app.send("task:message", taskMessage(seq, `Streaming line ${seq}. `.repeat(8)));
    await new Promise((done) => setTimeout(done, 100));
  }
}

/**
 * `fresh`: the reader opens the DM and scrolls up from the live end, the way a reader does.
 * Otherwise the harness first walks the list to the top, which already releases the follow.
 */
async function readerAbove(
  page: Page, baseURL: string | undefined, scrollUp: (page: Page) => Promise<void>, fresh = false,
) {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  app.pendingTask = { task_id: TASK_ID, status: "running", created_at: fx.at(fx.CHAT_MESSAGE_COUNT) };
  if (fresh) {
    await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/chat?session=${fx.SESSION_ID}`, { waitUntil: "domcontentloaded" });
    await expect(page.locator(`[data-message-id="${fx.messageId(fx.CHAT_MESSAGE_COUNT - 1)}"]`))
      .toBeVisible({ timeout: 60_000 });
    await page.waitForTimeout(1000);
  } else {
    await openChatMidList(page, app, origin);
  }
  await expect.poll(() => app.socket.connected, { message: "mock WebSocket connected" }).toBe(true);
  await scrollUp(page);
  await page.waitForTimeout(500);
  await nextFrames(page, 10);
  return app;
}

async function expectHeld(page: Page, app: App, before: number) {
  const scroller = page.locator("[data-tab-scroll-root]");
  const heightBefore = await scroller.evaluate((el) => el.scrollHeight);
  const meter = chatMeter(page);
  await meter.start();
  await streamReply(app);
  await page.waitForTimeout(500);
  await nextFrames(page, 5);
  const summary = await meter.stop();
  await expect.poll(() => app.socket.delivered("task:message"), { message: "task frames received" })
    .toBeGreaterThanOrEqual(STREAM_MS / 100);
  const after = await scrollTop(page);
  // A stimulus the list never grew from would read as a held reader.
  const grew = (await scroller.evaluate((el) => el.scrollHeight)) - heightBefore;
  expect(grew, "px the live reply grew the list").toBeGreaterThan(100);
  expect(summary.max_delta_px, JSON.stringify(summary)).toBeLessThanOrEqual(MAX_JUMP_PX);
  expect(Math.abs(after - before), `scrollTop ${before} -> ${after}`).toBeLessThanOrEqual(MAX_JUMP_PX);
}

test("S1 wheel scroll-up, then a streaming reply grows the list", async ({ page, baseURL }) => {
  const app = await readerAbove(page, baseURL, async (p) => {
    const box = (await p.locator("[data-tab-scroll-root]").boundingBox())!;
    await p.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    for (let i = 0; i < 5; i += 1) {
      await p.mouse.wheel(0, -300);
      await p.waitForTimeout(60);
    }
  });
  await expectHeld(page, app, await scrollTop(page));
});

test("S2 scrollTop write far from the bottom, then a streaming reply", async ({ page, baseURL }) => {
  const app = await readerAbove(page, baseURL, async (p) => {
    await p.locator("[data-tab-scroll-root]").evaluate((el) => {
      el.scrollTop = Math.max(0, el.scrollHeight - el.clientHeight - 4000);
    });
  });
  await expectHeld(page, app, await scrollTop(page));
});

test("S3 fresh DM, wheel scroll-up from the live end, then a streaming reply", async ({ page, baseURL }) => {
  const app = await readerAbove(page, baseURL, async (p) => {
    const box = (await p.locator("[data-tab-scroll-root]").boundingBox())!;
    await p.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    for (let i = 0; i < 5; i += 1) {
      await p.mouse.wheel(0, -300);
      await p.waitForTimeout(60);
    }
  }, true);
  await expectHeld(page, app, await scrollTop(page));
});

test("S4 fresh DM, scrollTop write far from the live end, then a streaming reply", async ({ page, baseURL }) => {
  const app = await readerAbove(page, baseURL, async (p) => {
    await p.locator("[data-tab-scroll-root]").evaluate((el) => {
      el.scrollTop = Math.max(0, el.scrollHeight - el.clientHeight - 4000);
    });
  }, true);
  await expectHeld(page, app, await scrollTop(page));
});
