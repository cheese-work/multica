/**
 * Scroll-anchor stability in the chat list and the issue comment timeline
 * (CHE-3231, A0; plan CHE-3230).
 *
 * Cheese reports 2-3 scroll jumps a second. Each scenario holds a reader still
 * (or scrolling) on the real production page and measures how far the row they
 * are looking at moves unasked:
 *
 *  - C1 re-page:  one new chat message makes the list refetch and the page
 *                 boundary move.
 *  - C2 run burst: ten run-start events for unrelated issues in two seconds,
 *                 no new message. The DM's only decision card is for another issue.
 *  - C3 combined: C1 and C2 together.
 *  - D1 timeline: scroll up three screens through a long comment thread while
 *                 rows mount.
 *
 * Report-only by default: the numbers are printed and attached, and nothing
 * fails on the defect. A fix slice sets SCROLL_STABILITY_ASSERT=1 to make each
 * scenario assert jump count 0, max delta <= 1 px and, for C2, zero requests
 * for the DM. Only HTTP and the WebSocket are synthetic; no backend is needed.
 */
import { expect, test, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import * as fx from "./fixtures/scroll-stability";
import { addChatMessage, boot, chatMeter, nextFrames, openChatMidList, type App } from "./lib/dm-harness";
import { assertStable, createAnchorMeter, type AnchorSummary } from "./lib/scroll-anchor";

const ASSERT = process.env.SCROLL_STABILITY_ASSERT === "1";
/** Run-start events for C2 and C3. */
const BURST_EVENTS = 10;
const BURST_MS = 2000;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

async function runStartBurst(app: App) {
  for (let n = 1; n <= BURST_EVENTS; n += 1) {
    app.send("task:running", {
      task_id: `eeee${String(n).padStart(4, "0")}-4444-4444-8444-444444444444`,
      agent_id: fx.AGENT_ID,
      issue_id: `dddd${String(n).padStart(4, "0")}-4444-4444-8444-444444444444`,
      status: "running",
    });
    await new Promise((done) => setTimeout(done, BURST_MS / BURST_EVENTS));
  }
}

interface Report extends AnchorSummary {
  scenario: string;
  chat_requests: number | null;
  unmocked_requests: string[];
  page_errors: string[];
  assert_mode: boolean;
}

async function emit(report: Report) {
  const line = `SCROLL_STABILITY_REPORT ${JSON.stringify(report)}`;
  // eslint-disable-next-line no-console
  console.log(line);
  test.info().annotations.push({ type: "scroll-stability", description: JSON.stringify(report) });
  await test.info().attach(`scroll-stability-${report.scenario}.json`, {
    body: JSON.stringify(report, null, 2), contentType: "application/json",
  });
  const dir = process.env.PERF_REPORT_DIR;
  if (dir) {
    mkdirSync(resolve(dir), { recursive: true });
    writeFileSync(join(resolve(dir), `scroll-stability-${report.scenario}.json`), JSON.stringify(report, null, 2));
  }
}

async function finish(report: Report, options: { zeroRequests?: boolean } = {}) {
  await emit(report);
  expect(report.page_errors, "page errors").toEqual([]);
  if (!ASSERT) return;
  assertStable(report);
  if (options.zeroRequests) {
    expect(report.chat_requests, "chat message-list requests for the DM").toBe(0);
  }
}

async function scenario(
  page: Page, baseURL: string | undefined, name: string,
  act: (app: App) => Promise<void>,
  options: { zeroRequests?: boolean; delivered?: Record<string, number> } = {},
) {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  await openChatMidList(page, app, origin);
  const meter = chatMeter(page);
  app.resetChatListRequests();
  await expect.poll(() => app.socket.connected, { message: "mock WebSocket connected" }).toBe(true);
  await meter.start();
  await act(app);
  await page.waitForTimeout(1000);
  await nextFrames(page, 5);
  const summary = await meter.stop();
  // A stimulus the page never received would read as a stable list.
  for (const [type, count] of Object.entries(options.delivered ?? {})) {
    await expect.poll(() => app.socket.delivered(type), { message: `"${type}" frames received` })
      .toBeGreaterThanOrEqual(count);
  }
  await finish({
    scenario: name, ...summary, chat_requests: app.chatListRequests(),
    unmocked_requests: app.unmocked, page_errors: app.pageErrors, assert_mode: ASSERT,
  }, options);
}

test("C1 re-page: one new message moves the page boundary", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C1", addChatMessage, { delivered: { "chat:message": 1 } });
});

test("C2 run-start burst: unrelated run starts refetch the DM", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C2", runStartBurst, {
    zeroRequests: true, delivered: { "task:running": BURST_EVENTS },
  });
});

test("C3 combined: new message during a run-start burst", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C3", async (app) => {
    const burst = runStartBurst(app);
    await new Promise((done) => setTimeout(done, BURST_MS / 2));
    await addChatMessage(app);
    await burst;
  }, { delivered: { "chat:message": 1, "task:running": BURST_EVENTS } });
});

test("D1 issue detail: scroll up through a long comment thread", async ({ page, baseURL }) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/issues/${fx.ISSUE_IDENTIFIER}`, {
    waitUntil: "domcontentloaded",
  });
  const scroller = page.locator("[data-tab-scroll-root]").first();
  const rows = '[id^="comment-"]:not([id^="comment-body-"])';
  await expect(page.locator(rows).first()).toBeAttached({ timeout: 60_000 });
  // Bottom first: heights estimated on the way down settle as rows are measured.
  for (let i = 0; i < 6; i += 1) {
    await scroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
    await page.waitForTimeout(400);
  }
  await nextFrames(page, 10);
  const viewport = await scroller.evaluate((el) => el.clientHeight);
  const meter = createAnchorMeter(page, { scroller: "[data-tab-scroll-root]", rows, idAttr: "id" });
  const scrollTopBefore = await scroller.evaluate((el) => el.scrollTop);
  await meter.start();
  await meter.scrollBy(-3 * viewport, 90);
  await page.waitForTimeout(500);
  await nextFrames(page, 5);
  const summary = await meter.stop();
  // A zero reads as "stable" only if the reader really moved three screens.
  const travelled = scrollTopBefore - (await scroller.evaluate((el) => el.scrollTop));
  expect(travelled, "px scrolled up").toBeGreaterThan(2.5 * viewport);
  await finish({
    scenario: "D1", ...summary, chat_requests: null,
    unmocked_requests: app.unmocked, page_errors: app.pageErrors, assert_mode: ASSERT,
  });
});
