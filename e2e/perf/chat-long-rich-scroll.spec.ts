import { expect, test } from "@playwright/test";

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
const mermaid = (n: number) =>
  "```mermaid\nflowchart TD\n" +
  Array.from({ length: Number(process.env.STEPS ?? 14) }, (_, i) => `  A${n}_${i}[Step ${i}] --> A${n}_${i + 1}[Step ${i + 1}]`).join("\n") +
  "\n```";
const reply = (n: number) => [
  `## Reply ${n}`, "Paragraph. ".repeat(40), table(n), "More prose. ".repeat(30), mermaid(n),
  table(n + 100), mermaid(n + 100), `Closing paragraph of reply ${n}.`,
].join("\n\n");
const LAST = "NEWEST-MESSAGE-SENTINEL";
const messages = Array.from({ length: 24 }, (_, i) => ({
  id: `message-${i}`, chat_session_id: sessionId,
  role: i % 2 ? "assistant" : "user", task_id: null,
  content: i === 23 ? `${reply(i)}\n\n${LAST}` : i % 2 ? reply(i) : `question ${i}`,
  created_at: timestamp,
}));

test.use({ viewport: { width: 1440, height: 900 }, locale: "en-US", serviceWorkers: "block" });

test("long rich DM scrolls to a stable end showing the newest message", async ({ page, context, baseURL }) => {
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
      case `/api/chat/sessions/${sessionId}/messages/page`: return { messages, limit: 50, has_more: false };
      case `/api/chat/sessions/${sessionId}/pending-task`: return {};
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
    const body = responseFor(url.pathname);
    await route.fulfill({ status: body === undefined ? 404 : 200,
      contentType: "application/json", body: JSON.stringify(body ?? {}) });
  });
  await context.routeWebSocket(url => url.pathname === "/ws", socket => socket.onMessage(() => {}));

  await page.goto(`${origin}/perf/chat?session=${sessionId}`, { waitUntil: "domcontentloaded" });
  const list = page.locator("[data-tab-scroll-root]").filter({ has: page.locator("[data-message-id]") });
  await expect(list).toBeVisible({ timeout: 30_000 });
  const metrics = () => list.evaluate(el => ({
    top: el.scrollTop, height: el.scrollHeight, client: el.clientHeight,
    sentinelVisible: (() => {
      const s = [...el.querySelectorAll("*")].find(n => n.children.length === 0 && n.textContent?.includes("NEWEST-MESSAGE-SENTINEL"));
      if (!s) return false;
      const r = s.getBoundingClientRect(), v = el.getBoundingClientRect();
      return r.bottom <= v.bottom + 1 && r.top >= v.top;
    })(),
  }));

  // Scroll up a page at a time to the top, then back down to the bottom, as a reader would.
  await list.hover();
  const up: unknown[] = [];
  for (let i = 0; i < 40; i++) { await page.mouse.wheel(0, -1500); await page.waitForTimeout(60); up.push(await metrics()); }
  const svgs = await list.evaluate(el => el.querySelectorAll("[data-rich-block-shell] svg").length);
  const trace: unknown[] = [];
  for (let i = 0; i < 80; i++) {
    await page.mouse.wheel(0, 1000);
    await page.waitForTimeout(80);
    trace.push(await metrics());
  }
  await page.waitForTimeout(1500);
  const a = await metrics();
  await page.waitForTimeout(1500);
  const b = await metrics();
  console.log("TRACE", JSON.stringify({ svgs, upHeights: up.map((t: any) => t.height), heights: trace.map((t: any) => t.height), a, b }));
  expect(b.height).toBe(a.height);
  expect(b.sentinelVisible).toBe(true);
});
