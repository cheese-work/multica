/**
 * Layout stability of issue-key chips (CHE-2970 / A1a).
 *
 * R1 (CHE-1828, H1): a streaming chat message that ends in a partial key must
 *    not turn the partial into a chip, and must not shift the list while the
 *    key is completed.
 * R2 (CHE-1829, H2): bare keys in a long thread resolve to chips once, with a
 *    stable box, without moving what the reader is looking at.
 *
 * Runs once per profile project in playwright.perf.config.ts (desktop, phone),
 * both under 4x CPU throttling, against the production build with synthetic
 * HTTP and WebSocket. Assertions are soft so one run reports every requirement.
 *
 * Geometry and count limits are the same on both profiles. Layout-shift limits
 * are keyed by profile (SHIFT_LIMIT).
 */
import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";
import * as fx from "./fixtures/chip-stability";

test.use({ locale: "en-US", serviceWorkers: "block" });

/**
 * Raw layout-shift sums per profile, frozen at half of the lowest value observed
 * red on `main` over 6 runs (R1 desktop 0.0012-0.0027, R1 phone 0.0270-0.0408,
 * R2 desktop 0.0048-0.0110, R2 phone 0.3587-0.3666). A2 turns the scenarios
 * green without touching them; a threshold change needs a new plan revision.
 */
const SHIFT_LIMIT: Record<fx.Profile, { r1: number; r2: number }> = {
  desktop: { r1: 0.0006, r2: 0.0024 },
  phone: { r1: 0.0135, r2: 0.18 },
};
/** One pixel: sub-pixel rounding, not a visible jump. */
const GEOMETRY_LIMIT_PX = 1;

const nextFrame = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => {
    requestAnimationFrame(() => requestAnimationFrame(() => done()));
  }));

/** One line per assertion input, so the PR body can quote observed values. */
const report = (name: string, values: Record<string, unknown>) =>
  // eslint-disable-next-line no-console
  console.log(`PERF_REPORT ${JSON.stringify({ test: name, ...values })}`);

function expectClean(h: fx.Harness) {
  expect(h.unmocked, "unmocked requests").toEqual([]);
  expect(h.external, "external requests").toEqual([]);
  expect(h.pageErrors, "page errors").toEqual([]);
}

// ---- R1 ---------------------------------------------------------------------

const SETTLED_ANSWER = "Settled answer, see PERF-77";
const STREAM_HEAD = "Checking now, see PERF-18";
const STREAM_TAIL = "24 more";

test("R1: a streamed partial key stays text until its boundary arrives", async ({
  page, context, baseURL,
}, testInfo) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const profile = fx.profileOf(testInfo.project.name);
  const taskMessages: unknown[] = [];
  const session = {
    id: fx.SESSION_ID, workspace_id: fx.WORKSPACE_ID, agent_id: fx.AGENT_ID,
    creator_id: fx.USER_ID, title: "Chip stability", status: "active", has_unread: false,
    unread_count: 0, last_message: null, pinned: false,
    created_at: "2026-10-20T00:00:00Z", updated_at: "2026-10-20T00:00:00Z",
  };
  const h = await fx.installHarness(page, context, origin, (path) => {
    switch (path) {
      case "/api/chat/sessions": return [session];
      case `/api/chat/sessions/${fx.SESSION_ID}`: return session;
      case `/api/chat/sessions/${fx.SESSION_ID}/messages/page`: return {
        messages: [{
          id: "settled-1", chat_session_id: fx.SESSION_ID, role: "assistant", task_id: null,
          content: SETTLED_ANSWER, created_at: "2026-10-20T00:00:00Z",
        }],
        limit: 50, has_more: false,
      };
      case `/api/chat/sessions/${fx.SESSION_ID}/pending-task`: return {
        task_id: fx.TASK_ID, status: "running", created_at: "2026-10-20T00:00:01Z",
        supports_queue: true, queued_tasks: [],
      };
      case `/api/tasks/${fx.TASK_ID}/messages`: return taskMessages;
      case `/api/chat/sessions/${fx.SESSION_ID}/read`: return {};
      case `/api/chat/sessions/${fx.SESSION_ID}/draft-restores`: return [];
      case "/api/chat/pending-tasks/has-any": return { has_pending: true };
      default: return undefined;
    }
  });
  h.gate.release();
  await context.addInitScript(({ sessionId, agentId }) => {
    localStorage.setItem("multica:chat:floatingChatEnabled", "false");
    localStorage.setItem("multica:chat:activeSessionId:perf", sessionId);
    localStorage.setItem("multica:chat:selectedAgentId:perf", agentId);
  }, { sessionId: fx.SESSION_ID, agentId: fx.AGENT_ID });

  let socket!: WebSocketRoute;
  let onSocket!: () => void;
  const socketReady = new Promise<void>((done) => { onSocket = done; });
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => {
    socket = ws;
    ws.onMessage(() => {});
    onSocket();
  });
  const stream = (content: string) => {
    const payload = { task_id: fx.TASK_ID, seq: taskMessages.length, type: "text", content };
    taskMessages.push(payload);
    socket.send(JSON.stringify({ type: "task:message", payload, actor_type: "system", actor_id: "" }));
  };

  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/chat`, { waitUntil: "domcontentloaded" });
  await socketReady;
  const list = page.locator("[data-tab-scroll-root]").filter({ hasText: "Settled answer" });
  await expect(list).toBeVisible();
  const chips = (phase: "streaming" | "settled") =>
    list.locator(`[data-rich-content][data-phase="${phase}"] span.issue-mention`);

  // A message that settled with a key at its very end links it.
  await expect.soft(chips("settled")).toHaveCount(1, { timeout: 5_000 });

  await nextFrame(page);
  await fx.resetShift(page);

  // The key touches the end of the streamed text: it may still grow.
  stream(STREAM_HEAD);
  const streaming = list.locator('[data-rich-content][data-phase="streaming"]');
  await expect(streaming).toContainText("PERF-18");
  await page.waitForTimeout(fx.LOOKUP_DELAY_MS * 2);
  const chipsWhileOpen = await chips("streaming").count();
  expect.soft(chipsWhileOpen, "chips while the key touches the end of the text").toBe(0);

  // The boundary arrives: the key is complete.
  stream(STREAM_TAIL);
  await expect(streaming).toContainText("more");
  await page.waitForTimeout(fx.LOOKUP_DELAY_MS * 2);
  const chipsAfterBoundary = await chips("streaming").count();
  expect.soft(chipsAfterBoundary, "chips after the boundary arrives").toBe(1);

  await nextFrame(page);
  const shift = await fx.readShift(page);
  expect.soft(shift.raw, "raw layout-shift sum while streaming").toBeLessThanOrEqual(SHIFT_LIMIT[profile].r1);

  report("R1", {
    profile, chipsWhileOpen, chipsAfterBoundary, settledChips: await chips("settled").count(),
    cls: shift.cls, rawShift: shift.raw, partialKeyLookups: h.keyLookups.get("PERF-18") ?? 0,
  });
  expectClean(h);
});

// ---- R2 ---------------------------------------------------------------------

const COMMENT_COUNT = 30;
/** Distinct keys; they repeat across comments, so "requests per key" is a real ratio. */
const KEY_COUNT = 10;
/** The timeline is virtualized: the reader scrolls here, with keyed rows above and below. */
const ANCHOR_INDEX = 12;
const commentId = (i: number) => `cccc${String(i).padStart(4, "0")}-4444-4444-8444-444444444444`;
const keyOf = (i: number) => `PERF-${101 + (i % KEY_COUNT)}`;

function commentBody(i: number): string {
  const lead = `Tracked in ${keyOf(i)}, see the thread.`;
  const paragraphs = Array.from(
    { length: 3 },
    (_, p) => `Paragraph ${p + 1} of comment ${i}. The reviewer walks through the change and what was measured.`,
  );
  return [lead, ...paragraphs].join("\n\n");
}

const timeline = () => Array.from({ length: COMMENT_COUNT }, (_, i) => ({
  type: "comment" as const, id: commentId(i), actor_type: "user" as const, actor_id: fx.USER_ID,
  created_at: new Date(Date.UTC(2026, 8, 1) + i * 1000).toISOString(),
  updated_at: new Date(Date.UTC(2026, 8, 1) + i * 1000).toISOString(),
  issue_id: fx.THREAD_ISSUE_ID, parent_id: null, content: commentBody(i),
  resolved_at: null, reactions: [], attachments: [], source_task_id: null,
}));

test("R2: delayed key resolution keeps chips and the reader's place stable", async ({
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
      // Side panels that do not touch chips; an empty object lets their parsers fall back.
      case `/api/issues/${fx.THREAD_ISSUE_ID}/relations`:
      case "/api/inbox/archived/page": return {};
      case "/api/issues/child-progress": return { progress: [] };
      default: return undefined;
    }
  });
  await context.addInitScript(() => {
    localStorage.setItem("multica:chat:floatingChatEnabled", "false");
    // Sampled every frame: the chip's box from its first appearance to the end.
    const w = window as unknown as { __chipBoxes: { w: number; h: number }[] };
    w.__chipBoxes = [];
    const sample = () => {
      const chip = document.querySelector("span.issue-mention");
      if (chip) {
        const r = chip.getBoundingClientRect();
        w.__chipBoxes.push({ w: r.width, h: r.height });
      }
      requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
  });
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => { ws.onMessage(() => {}); });

  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/issues/${fx.THREAD_ISSUE_IDENTIFIER}`, {
    waitUntil: "domcontentloaded",
  });
  const anchor = page.locator(`[id="comment-${commentId(ANCHOR_INDEX)}"]`);
  const firstRow = page.locator('[id^="comment-cccc"]').first();
  await expect(firstRow).toBeVisible({ timeout: 30_000 });
  // Scroll the thread's own container until the virtualized row mounts, then centre it.
  for (let step = 0; step < 60 && (await anchor.count()) === 0; step += 1) {
    await firstRow.evaluate((row) => {
      for (let n = row.parentElement; n; n = n.parentElement) {
        const overflow = getComputedStyle(n).overflowY;
        if ((overflow === "auto" || overflow === "scroll") && n.scrollHeight > n.clientHeight) {
          n.scrollTop += 600;
          return;
        }
      }
    });
    await page.waitForTimeout(100);
  }
  await expect(anchor).toBeVisible();
  // Keys are still plain text: lookups are held. The reader sits mid-thread.
  await expect(page.locator("span.issue-mention")).toHaveCount(0);
  await anchor.evaluate((el) => el.scrollIntoView({ block: "center" }));
  await nextFrame(page);
  const topBefore = (await anchor.boundingBox())!.y;
  await fx.resetShift(page);

  h.gate.release();
  // Every mounted row holds exactly one key, so one chip per mounted row once all resolve.
  const rows = page.locator('[id^="comment-cccc"]').filter({ hasText: /PERF-1\d\d/ });
  const chips = page.locator("span.issue-mention");
  await expect.poll(async () => (await chips.count()) > 0 && (await chips.count()) === (await rows.count()),
    { timeout: 15_000 }).toBe(true);
  await page.waitForTimeout(fx.LOOKUP_DELAY_MS * 2);
  await nextFrame(page);

  const scrollDelta = Math.abs((await anchor.boundingBox())!.y - topBefore);
  const boxes = await page.evaluate(
    () => (window as unknown as { __chipBoxes: { w: number; h: number }[] }).__chipBoxes,
  );
  const first = boxes[0]!;
  const last = boxes[boxes.length - 1]!;
  const widthDelta = Math.abs(last.w - first.w);
  const heightDelta = Math.abs(last.h - first.h);
  const shift = await fx.readShift(page);

  const requestsPerKey = [...h.keyLookups.values()];
  expect(h.keyLookups.size, "distinct keys looked up").toBeGreaterThan(0);
  expect.soft(Math.max(...requestsPerKey), "getIssue requests per key").toBe(1);
  expect.soft(widthDelta, "chip width delta, placeholder to resolved (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(heightDelta, "chip height delta, placeholder to resolved (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(scrollDelta, "anchored comment scroll delta (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(shift.raw, "raw layout-shift sum").toBeLessThanOrEqual(SHIFT_LIMIT[profile].r2);

  report("R2", {
    profile, maxRequestsPerKey: Math.max(...requestsPerKey), widthDelta, heightDelta,
    scrollDelta, cls: shift.cls, rawShift: shift.raw, samples: boxes.length,
  });
  expectClean(h);
});
