import { expect, test, type BrowserContext, type Page } from "@playwright/test";

// CHE-3329: a DM of long agent replies (tables + mermaid) must scroll to a
// stable end that shows the newest message. Production shared UI with
// synthetic HTTP/WS boundaries; no backend or account.
const wsId = "11111111-1111-4111-8111-111111111111";
const userId = "22222222-2222-4222-8222-222222222222";
const agentId = "33333333-3333-4333-8333-333333333333";
const sessionId = "44444444-4444-4444-8444-444444444444";
const timestamp = "2026-10-20T00:00:00Z";
const session = {
  id: sessionId, workspace_id: wsId, agent_id: agentId, creator_id: userId,
  title: "Long rich DM", status: "active", has_unread: false, unread_count: 0,
  last_message: null, pinned: false, created_at: timestamp, updated_at: timestamp,
};
const agent = {
  id: agentId, workspace_id: wsId, runtime_id: "55555555-5555-4555-8555-555555555555",
  name: "Fixture Agent", description: "", instructions: "", avatar_url: null,
  runtime_mode: "local", runtime_config: {}, custom_args: [], visibility: "workspace",
  permission_mode: "public_to", invocation_targets: [{ target_type: "workspace", target_id: null }],
  status: "idle", max_concurrent_tasks: 1, model: "", owner_id: userId, skills: [],
  runtime_availability: "online", created_at: timestamp, updated_at: timestamp,
  archived_at: null, archived_by: null,
};

const table = (n: number) => [
  "| Gate | Owner | Status | Notes |", "| --- | --- | --- | --- |",
  ...Array.from({ length: 12 }, (_, i) => `| gate-${n}-${i} | owner ${i} | ${i % 2 ? "green" : "red"} | ${"note ".repeat(8)} |`),
].join("\n");
const GIANT = process.env.GIANT === "1";
const sequence = (n: number) =>
  "```mermaid\nsequenceDiagram\n" +
  Array.from({ length: 30 }, (_, i) => `  P${n}->>Q${n}: call ${i} with a longer label to widen the lane`).join("\n") + "\n```";
const mermaid = (n: number) => GIANT ? sequence(n) :
  "```mermaid\nflowchart TD\n" +
  Array.from({ length: Number(process.env.STEPS ?? 14) }, (_, i) => `  A${n}_${i}[Step ${i}] --> A${n}_${i + 1}[Step ${i + 1}]`).join("\n") +
  "\n```";
const reply = (n: number) => [
  `## Reply ${n}`, "Paragraph. ".repeat(40), table(n), "More prose. ".repeat(30), mermaid(n),
  table(n + 100), mermaid(n + 100), `Closing paragraph of reply ${n}.`,
].join("\n\n");
const LAST = "NEWEST-MESSAGE-SENTINEL";
const giantReply = () => Array.from({ length: 10 }, (_, k) => `### Part ${k}\n\n${table(k)}\n\n${mermaid(k + 500)}\n\nProse ${k}. ${"text ".repeat(60)}`).join("\n\n");
const TOTAL = 120;
const taskId = "77777777-7777-4777-8777-777777777777";
// A compact turn persists two assistant messages under one task_id: the
// compaction summary (i % 20 === 7) and the reply right after it (i % 20 === 8).
const compactTask = (i: number) => (i % 20 === 7 || i % 20 === 8 ? `compact-task-${i - (i % 20) + 7}` : null);
const isAssistant = (i: number) => i % 2 === 1 || i % 20 === 8;
const messages = Array.from({ length: TOTAL }, (_, i) => ({
  id: `message-${i}`, chat_session_id: sessionId,
  role: isAssistant(i) ? "assistant" : "user", task_id: compactTask(i),
  message_kind: i % 20 === 7 ? "compaction_summary" : "message",
  content: i === TOTAL - 1 ? `${GIANT ? giantReply() : reply(i)}\n\n${LAST}` : isAssistant(i) ? reply(i) : `question ${i}`,
  created_at: timestamp,
}));

test.setTimeout(900_000);
test.use({ viewport: { width: 1440, height: 900 }, locale: "en-US", serviceWorkers: "block" });

async function scenario(
  { page, context, baseURL }: { page: Page; context: BrowserContext; baseURL: string | undefined },
  { paged, streaming, refetch = false }: { paged: boolean; streaming: boolean; refetch?: boolean },
) {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  await context.addCookies([
    { name: "multica_logged_in", value: "1", url: origin },
    { name: "last_workspace_slug", value: "perf", url: origin },
  ]);
  await context.addInitScript(({ sessionId, agentId }) => {
    localStorage.setItem("multica:chat:isOpen", "true");
    localStorage.setItem("multica:chat:floatingChatEnabled", "true");
    localStorage.setItem("multica:chat:activeSessionId:perf", sessionId);
    localStorage.setItem("multica:chat:selectedAgentId:perf", agentId);
  }, { sessionId, agentId });
  // Newest `limit` messages, or the `limit` before the cursor. Unpaged: everything.
  let pageRequests = 0;
  const pageFor = (url: URL) => {
    const all = paged ? messages : messages.slice(-24);
    const beforeId = url.searchParams.get("before_id");
    const end = beforeId ? all.findIndex(m => m.id === beforeId) : all.length;
    const limit = Number(url.searchParams.get("limit") ?? 50);
    const slice = all.slice(Math.max(0, end - limit), end).map(m =>
      streaming ? { ...m, content: m.content.replace(LAST, "") } : m);
    const hasMore = end - limit > 0;
    return { messages: slice, limit, has_more: hasMore,
      next_cursor: hasMore ? { created_at: slice[0]!.created_at, id: slice[0]!.id } : null };
  };
  let currentUrl = new URL(origin);
  const taskMessages: { task_id: string; seq: number; type: string; content: string }[] = [];
  const responseFor = (path: string): unknown => {
    switch (path) {
      case "/api/config": return { feature_flags: {}, allow_signup: true };
      case "/api/me": return {
        id: userId, name: "Fixture User", email: "fixture@example.test", avatar_url: null,
        language: "en", onboarded_at: timestamp, onboarding_questionnaire: { source: "other" },
        created_at: timestamp, updated_at: timestamp,
      };
      case "/api/workspaces": return [{
        id: wsId, name: "Perf", slug: "perf", description: null, context: null,
        settings: { github_enabled: false }, repos: [], issue_prefix: "PERF", avatar_url: null,
        created_at: timestamp, updated_at: timestamp,
      }];
      case "/api/agents": return [agent];
      case "/api/chat/sessions": return [session];
      case `/api/chat/sessions/${sessionId}`: return session;
      case `/api/chat/sessions/${sessionId}/messages/page`: pageRequests++; return pageFor(currentUrl);
      case `/api/chat/sessions/${sessionId}/pending-task`: return streaming ? {
        task_id: taskId, status: "running", created_at: timestamp, supports_queue: true, queued_tasks: [],
      } : {};
      case `/api/tasks/${taskId}/messages`: return taskMessages;
      case `/api/chat/sessions/${sessionId}/read`: return {};
      case `/api/chat/sessions/${sessionId}/draft-restores`: return [];
      case "/api/chat/pending-tasks/has-any": return { has_pending: false };
      case "/api/projects": return { projects: [], total: 0 };
      case "/api/issues": return { issues: [], total: 0 };
      case "/api/issue-statuses": return { statuses: [] };
      case "/api/properties": return { properties: [], total: 0 };
      case "/api/quick-actions": return { quick_actions: [] };
      case "/api/client-usage": return {};
      case "/api/issue-view-preferences": return { scope_type: "workspace", scope_id: null,
        ordered_view_ids: [], hidden_view_ids: [], default_view_id: null };
      case "/api/issue-workflows": return { workflows: [] };
      case "/api/search-index/manifest": return {
        cursor: "0", issue_count: 0, comment_count: 0, project_count: 0, text_bytes: 0,
      };
      case "/api/search-index/snapshot": return {
        issues: [], comments: [], projects: [], next_after_number: 0, done: true,
      };
      case "/api/search-index/changes": return {
        issues: [], comments: [], projects: [], deleted: { issues: [], comments: [], projects: [] },
        cursor: "0", has_more: false,
      };
      case `/api/workspaces/${wsId}/members`: return [{
        id: "member-1", user_id: userId, workspace_id: wsId, role: "owner",
        name: "Fixture User", email: "fixture@example.test", avatar_url: null,
      }];
      case "/api/runtimes": case "/api/squads": case "/api/agent-task-snapshot":
      case "/api/invitations": case "/api/inbox": case "/api/inbox/unread-summary":
      case "/api/pins": case "/api/chat/pending-tasks": case "/api/assignee-frequency":
      case "/api/issue-views": case "/api/issue-wakeup-summaries":
      case "/api/agent-run-counts": case "/api/agent-activity-30d":
        return [];
      default: return undefined;
    }
  };
  await context.route("**/*", async route => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) return route.abort();
    if (!url.pathname.startsWith("/api/")) return route.continue();
    currentUrl = url;
    const body = responseFor(url.pathname);
    await route.fulfill({ status: body === undefined ? 404 : 200,
      contentType: "application/json", body: JSON.stringify(body ?? {}) });
  });
  let streamChunk = (_content: string) => {};
  let pushEvent = (_type: string, _payload: unknown) => {};
  await context.routeWebSocket(url => url.pathname === "/ws", socket => {
    socket.onMessage(() => {});
    pushEvent = (type, payload) => socket.send(JSON.stringify({ type, payload, actor_type: "system", actor_id: "" }));
    streamChunk = content => {
      const payload = { task_id: taskId, seq: taskMessages.length, type: "text", content };
      taskMessages.push(payload);
      socket.send(JSON.stringify({ type: "task:message", payload, actor_type: "system", actor_id: "" }));
    };
  });

  await page.goto(`${origin}/perf/chat?session=${sessionId}`, { waitUntil: "domcontentloaded" });
  const list = page.locator("[data-tab-scroll-root]");
  await expect(page.locator("[data-message-id]").first()).toBeVisible({ timeout: 30_000 });
  const metrics = () => list.evaluate(el => ({
    top: el.scrollTop, height: el.scrollHeight, client: el.clientHeight,
    sentinelVisible: (() => {
      const w = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
      for (let n = w.nextNode(); n; n = w.nextNode()) {
        if (!n.nodeValue?.includes("NEWEST-MESSAGE-SENTINEL")) continue;
        const range = document.createRange();
        range.selectNodeContents(n);
        const r = range.getBoundingClientRect(), v = el.getBoundingClientRect();
        return r.bottom <= v.bottom + 1 && r.top >= v.top;
      }
      return false;
    })(),
  }));

  // Read up toward the top (loading older pages when paged), then wheel down to the end.
  await list.hover();
  for (let i = 0; i < (paged ? 160 : GIANT ? 12 : 40); i++) {
    await page.mouse.wheel(0, -1500);
    await page.waitForTimeout(60);
  }
  for (let i = 0; i < 200; i++) {
    await page.mouse.wheel(0, 2000);
    if (streaming) {
      streamChunk(i === 199 ? `\n\n${LAST}` : i % 50 === 0 ? `\n\n${table(i)}\n\n${mermaid(i)}\n\n` : "streamed token ".repeat(30));
    }
    await page.waitForTimeout(80);
  }
  if (refetch) {
    // Same list, new array: every event refetches the message pages while the reader sits at the end.
    const tops: number[] = [];
    const before = pageRequests;
    for (let i = 0; i < 150; i++) {
      pushEvent("decision_answer:updated", { issue_id: "refetch-issue" });
      await page.waitForTimeout(100);
      tops.push((await metrics()).top);
    }
    expect(pageRequests - before).toBeGreaterThan(50);
    const stable = tops.slice(20);
    expect(Math.max(...stable) - Math.min(...stable)).toBeLessThanOrEqual(2);
  }
  await page.waitForTimeout(1500);
  const a = await metrics();
  await page.waitForTimeout(1500);
  const b = await metrics();
  // The extent stops moving at the end, and the end is the newest message.
  expect(b.height).toBe(a.height);
  expect(b.sentinelVisible).toBe(true);
}

test("long rich DM, single page", async ({ page, context, baseURL }) => scenario({ page, context, baseURL }, { paged: false, streaming: false }));
test("long rich DM, three pages of history", async ({ page, context, baseURL }) => scenario({ page, context, baseURL }, { paged: true, streaming: false }));
test("long rich DM, newest reply still growing", async ({ page, context, baseURL }) => scenario({ page, context, baseURL }, { paged: false, streaming: true }));
test("long rich DM, repeated refetches at the end", async ({ page, context, baseURL }) => scenario({ page, context, baseURL }, { paged: false, streaming: false, refetch: true }));
