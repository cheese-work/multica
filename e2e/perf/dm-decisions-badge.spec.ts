/**
 * DM scroll stability around the "decisions need you" badge (CHE-5376).
 *
 * CHE-3231's scenarios are report-only unless SCROLL_STABILITY_ASSERT=1 and none
 * clicked the badge, so a jump never failed CI. These always assert: while the
 * reader sits mid-list, an unrequested move of their row above 8 px fails.
 *
 *  - B1 badge click: opens the list from the DM header, measured for 3 s; the DM
 *       URL stays put and the badge sits inside the header row.
 *  - B2 new message: one message arrives while the reader is scrolled up.
 */
import { expect, test } from "@playwright/test";
import * as fx from "./fixtures/scroll-stability";
import { addChatMessage, boot, chatMeter, nextFrames, openChatMidList } from "./lib/dm-harness";

const MAX_JUMP_PX = 8;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

const decision = (n: number) => ({
  id: `aaaa000${n}-4444-4444-8444-444444444444`, issue_id: `dddd000${n}-4444-4444-8444-444444444444`,
  issue_identifier: `PERF-${n}`, asking_agent_id: fx.AGENT_ID, asking_agent_name: "Perf agent",
  ask: `**Decision**\nPick option ${n}?`, escalation_class: "reversible", created_at: fx.at(n),
  chat_session_id: fx.SESSION_ID, message_id: fx.messageId(fx.CARD_MESSAGE_INDEX), source_comment_id: null,
});

async function parkedReader(page: import("@playwright/test").Page, baseURL: string | undefined) {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  app.openDecisions = [1, 2, 3, 4].map(decision);
  await openChatMidList(page, app, origin);
  await expect.poll(() => app.socket.connected, { message: "mock WebSocket connected" }).toBe(true);
  const meter = chatMeter(page);
  await meter.start();
  return { app, meter, origin };
}

function expectStable(summary: { jump_count: number; max_delta_px: number; lost_frames: number }) {
  expect(summary.lost_frames, JSON.stringify(summary)).toBe(0);
  expect(summary.max_delta_px, JSON.stringify(summary)).toBeLessThanOrEqual(MAX_JUMP_PX);
}

test("B1 badge click opens the list without moving the message list", async ({ page, baseURL }) => {
  const { app, meter } = await parkedReader(page, baseURL);
  const badge = page.getByTestId("open-decisions-badge");
  await expect(badge).toHaveText(/4 decisions need you/);
  // In the header row, next to the panel content: not floating over the panel border.
  const header = page.locator("header").filter({ has: badge });
  await expect(header).toHaveCount(1);
  const [h, b] = [await header.boundingBox(), await badge.boundingBox()];
  expect(b!.y).toBeGreaterThanOrEqual(h!.y);
  expect(b!.y + b!.height).toBeLessThanOrEqual(h!.y + h!.height);

  const url = page.url();
  await badge.click();
  await expect(page.getByRole("link", { name: /Pick option 1\?/ }).first()).toBeVisible();
  await page.waitForTimeout(3000);
  await nextFrames(page, 5);
  expect(page.url()).toBe(url);
  expectStable(await meter.stop());
  expect(app.pageErrors).toEqual([]);
});

test("B2 a new message while scrolled up does not move the reader", async ({ page, baseURL }) => {
  const { app, meter } = await parkedReader(page, baseURL);
  await addChatMessage(app);
  await expect.poll(() => app.socket.delivered("chat:message")).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(1500);
  await nextFrames(page, 5);
  expectStable(await meter.stop());
  expect(app.pageErrors).toEqual([]);
});
