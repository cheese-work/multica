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
import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import * as fx from "./fixtures/scroll-stability";
import { assertStable, createAnchorMeter, type AnchorSummary } from "./lib/scroll-anchor";

const ASSERT = process.env.SCROLL_STABILITY_ASSERT === "1";
/** Run-start events for C2 and C3. */
const BURST_EVENTS = 10;
const BURST_MS = 2000;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

const nextFrames = (page: Page, n: number) =>
  page.evaluate((count) => new Promise<void>((done) => {
    const tick = (left: number) => (left === 0 ? done() : requestAnimationFrame(() => tick(left - 1)));
    tick(count);
  }), n);

interface App {
  send(type: string, payload: unknown): void;
  /** Chat message-list requests for the DM since the last reset. */
  chatListRequests: () => number;
  resetChatListRequests(): void;
  messages: fx.FixtureMessage[];
  unmocked: string[];
  pageErrors: string[];
}

async function boot(page: Page, baseURL: string | undefined): Promise<App> {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const context = page.context();
  const app: App = {
    send: () => {}, messages: fx.initialMessages(), unmocked: [], pageErrors: [],
    chatListRequests: () => chatRequests, resetChatListRequests: () => { chatRequests = 0; },
  };
  let chatRequests = 0;
  page.on("pageerror", (error) => app.pageErrors.push(error.message));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "no-preference" });
  await context.addCookies([
    { name: "multica_logged_in", value: "1", url: origin },
    { name: "last_workspace_slug", value: fx.WORKSPACE_SLUG, url: origin },
  ]);
  await context.addInitScript(() => {
    localStorage.setItem("multica:chat:floatingChatEnabled", "false");
  });

  const chatPrefix = `/api/chat/sessions/${fx.SESSION_ID}`;
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) return route.abort();
    if (!url.pathname.startsWith("/api/")) return route.continue();
    const json = (body: unknown) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
    if (url.pathname === `${chatPrefix}/messages/page`) {
      chatRequests += 1;
      return json(fx.messagesPage(app.messages, url.searchParams));
    }
    if (url.pathname === `${chatPrefix}/messages`) {
      chatRequests += 1;
      return json(app.messages);
    }
    switch (url.pathname) {
      case "/api/chat/sessions": return json([fx.session]);
      case chatPrefix: return json(fx.session);
      case `${chatPrefix}/pending-task`: return json({});
      case `${chatPrefix}/read`: return json({});
      case `${chatPrefix}/draft-restores`: return json([]);
      case `${chatPrefix}/wakeups`: return json([]);
      case "/api/labels": return json({ labels: [] });
      case "/api/inbox/archived/page": return json({ items: [], has_more: false });
      case `/api/issues/${fx.ISSUE_ID}/system-wakeups`:
      case `/api/issues/${fx.ISSUE_ID}/duplicates`:
      case `/api/issues/${fx.ISSUE_ID}/relations`: return json([]);
      case `/api/agents/${fx.AGENT_ID}/presets/completions`: return json({ names: [] });
      case "/api/decisions/bbbb0000-4444-4444-8444-444444444444/artifacts": return json({ artifacts: [] });
    }
    const body = fx.commonResponse(url.pathname) ?? fx.issueResponse(url.pathname);
    if (body === undefined) {
      app.unmocked.push(`${route.request().method()} ${url.pathname}`);
      return route.fulfill({ status: 404, contentType: "application/json", body: "{}" });
    }
    return json(body);
  });

  let socket: WebSocketRoute | null = null;
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => {
    socket = ws;
    ws.onMessage(() => {});
  });
  app.send = (type, payload) =>
    socket?.send(JSON.stringify({ type, payload, actor_id: "", actor_type: "system" }));
  return app;
}

/** Opens the DM, loads every page of history and parks the reader mid-list. */
async function openChatMidList(page: Page, app: App, origin: string) {
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/chat?session=${fx.SESSION_ID}`, {
    waitUntil: "domcontentloaded",
  });
  const scroller = page.locator("[data-tab-scroll-root]");
  await expect(page.locator(`[data-message-id="${fx.messageId(fx.CHAT_MESSAGE_COUNT - 1)}"]`))
    .toBeVisible({ timeout: 60_000 });
  const pagesNeeded = Math.ceil(fx.CHAT_MESSAGE_COUNT / fx.PAGE_SIZE);
  // Walk to the top until every page has been fetched.
  await expect.poll(async () => {
    const box = (await scroller.boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.wheel(0, -4000);
    await nextFrames(page, 10);
    await page.waitForTimeout(300);
    return app.chatListRequests();
  }, { timeout: 60_000 }).toBeGreaterThanOrEqual(pagesNeeded);
  await scroller.evaluate((el) => { el.scrollTop = (el.scrollHeight - el.clientHeight) / 2; });
  await page.waitForTimeout(800);
  await nextFrames(page, 10);
  expect(await page.locator("[data-message-id]").count()).toBeGreaterThan(0);
}

const chatMeter = (page: Page) =>
  createAnchorMeter(page, { scroller: "[data-tab-scroll-root]", rows: "[data-message-id]", idAttr: "data-message-id" });

/** Appends one message the way the server would and tells the client. */
async function addChatMessage(app: App) {
  const next = fx.chatMessage(app.messages.length);
  app.messages.push({ ...next, role: "user", content: "A brand new message." });
  app.send("chat:message", {
    chat_session_id: fx.SESSION_ID, message_id: next.id, role: "user",
    content: "A brand new message.", created_at: next.created_at,
  });
}

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

function emit(report: Report) {
  const line = `SCROLL_STABILITY_REPORT ${JSON.stringify(report)}`;
  // eslint-disable-next-line no-console
  console.log(line);
  test.info().annotations.push({ type: "scroll-stability", description: JSON.stringify(report) });
  const dir = process.env.PERF_REPORT_DIR;
  if (dir) {
    mkdirSync(resolve(dir), { recursive: true });
    writeFileSync(join(resolve(dir), `scroll-stability-${report.scenario}.json`), JSON.stringify(report, null, 2));
  }
}

function finish(report: Report, options: { zeroRequests?: boolean } = {}) {
  emit(report);
  expect(report.page_errors, "page errors").toEqual([]);
  if (!ASSERT) return;
  assertStable(report);
  if (options.zeroRequests) {
    expect(report.chat_requests, "chat message-list requests for the DM").toBe(0);
  }
}

async function scenario(
  page: Page, baseURL: string | undefined, name: string,
  act: (app: App) => Promise<void>, options: { zeroRequests?: boolean } = {},
) {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  await openChatMidList(page, app, origin);
  const meter = chatMeter(page);
  app.resetChatListRequests();
  await meter.start();
  await act(app);
  await page.waitForTimeout(1000);
  await nextFrames(page, 5);
  const summary = await meter.stop();
  finish({
    scenario: name, ...summary, chat_requests: app.chatListRequests(),
    unmocked_requests: app.unmocked, page_errors: app.pageErrors, assert_mode: ASSERT,
  }, options);
}

test("C1 re-page: one new message moves the page boundary", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C1", addChatMessage);
});

test("C2 run-start burst: unrelated run starts refetch the DM", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C2", runStartBurst, { zeroRequests: true });
});

test("C3 combined: new message during a run-start burst", async ({ page, baseURL }) => {
  await scenario(page, baseURL, "C3", async (app) => {
    const burst = runStartBurst(app);
    await new Promise((done) => setTimeout(done, BURST_MS / 2));
    await addChatMessage(app);
    await burst;
  });
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
  finish({
    scenario: "D1", ...summary, chat_requests: null,
    unmocked_requests: app.unmocked, page_errors: app.pageErrors, assert_mode: ASSERT,
  });
});
