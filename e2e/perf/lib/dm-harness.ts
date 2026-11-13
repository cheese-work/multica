/** Shared DM harness for the scroll specs: synthetic HTTP and WebSocket against a production build. */
import { expect, type Page } from "@playwright/test";
import * as fx from "../fixtures/scroll-stability";
import { MockSocket } from "./mock-socket";
import { createAnchorMeter } from "./scroll-anchor";

export const nextFrames = (page: Page, n: number) =>
  page.evaluate((count) => new Promise<void>((done) => {
    const tick = (left: number) => (left === 0 ? done() : requestAnimationFrame(() => tick(left - 1)));
    tick(count);
  }), n);

export interface App {
  socket: MockSocket;
  send(type: string, payload: unknown): void;
  /** Chat message-list requests for the DM since the last reset. */
  chatListRequests: () => number;
  resetChatListRequests(): void;
  messages: fx.FixtureMessage[];
  /** Body of `/api/decisions/open`. */
  openDecisions: unknown[];
  /** Body of `GET .../pending-task`: a running reply makes the DM show a live row. */
  pendingTask: Record<string, unknown>;
  /** Body of `GET /api/tasks/:id/messages`. */
  taskMessages: unknown[];
  unmocked: string[];
  pageErrors: string[];
}

export async function boot(page: Page, baseURL: string | undefined): Promise<App> {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const context = page.context();
  const app: App = {
    socket: new MockSocket(), send: (type, payload) => app.socket.send(type, payload), messages: fx.initialMessages(), openDecisions: [], pendingTask: {}, taskMessages: [], unmocked: [], pageErrors: [],
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
      case `${chatPrefix}/pending-task`: return json(app.pendingTask);
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
    if (/^\/api\/tasks\/[^/]+\/messages$/.test(url.pathname)) return json(app.taskMessages);
    if (url.pathname === "/api/decisions/open") return json(app.openDecisions);
    const body = fx.commonResponse(url.pathname) ?? fx.issueResponse(url.pathname);
    if (body === undefined) {
      app.unmocked.push(`${route.request().method()} ${url.pathname}`);
      return route.fulfill({ status: 404, contentType: "application/json", body: "{}" });
    }
    return json(body);
  });

  await app.socket.watch(page);
  await context.routeWebSocket((url) => url.pathname === "/ws", app.socket.attach);
  return app;
}

/** Opens the DM, loads every page of history and parks the reader mid-list. */
export async function openChatMidList(page: Page, app: App, origin: string) {
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

export const chatMeter = (page: Page) =>
  createAnchorMeter(page, { scroller: "[data-tab-scroll-root]", rows: "[data-message-id]", idAttr: "data-message-id" });

/** Appends one message the way the server would and tells the client. */
export async function addChatMessage(app: App) {
  const next = fx.chatMessage(app.messages.length);
  app.messages.push({ ...next, role: "user", content: "A brand new message." });
  app.send("chat:message", {
    chat_session_id: fx.SESSION_ID, message_id: next.id, role: "user",
    content: "A brand new message.", created_at: next.created_at,
  });
}

