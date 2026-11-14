import { expect, test } from "@playwright/test";
import {
  AGENT_ID, USER_ID, WORKSPACE_ID, WORKSPACE_SLUG, agent, installHarness, issueIdForKey,
} from "./fixtures/chip-stability";
import { MockSocket } from "./lib/mock-socket";

// CHE-4808: one long DM open while other DMs run. Meters long tasks (>50 ms),
// INP-style event latency and React commits per second, under synthetic
// HTTP/WS boundaries. Report-only unless CHAT_LAG_ASSERT=1.
const T = "2026-11-09T00:00:00Z";
const LONG = "44444444-4444-4444-8444-000000000000";
const OTHERS = [1, 2, 3].map((n) => `44444444-4444-4444-8444-00000000000${n}`);
const taskOf = (sid: string) => `77777777-7777-4777-8777-${sid.slice(-12)}`;
const TOTAL = Number(process.env.CHAT_LAG_MESSAGES ?? 220);
const SECONDS = Number(process.env.CHAT_LAG_SECONDS ?? 20);
const ISSUE_RUNS_PER_SEC = Number(process.env.CHAT_LAG_RUNS_PER_SEC ?? 5);
const DM_EVENTS_PER_SEC = Number(process.env.CHAT_LAG_DM_EVENTS_PER_SEC ?? 3);

const reply = (i: number) => [
  `## Reply ${i}`,
  `Status of PERF-${i % 40 + 1} and PERF-${(i + 7) % 40 + 1}: \`useChatController\`, \`staleTime\`, \`WhileSubscribed\`, \`PERF-${(i + 13) % 40 + 1}\`. ${"Sentence with prose. ".repeat(12)}`,
  "```ts\n" + Array.from({ length: 10 }, (_, k) => `const v${k} = compute(${i}, ${k}); // step ${k}`).join("\n") + "\n```",
  `- item PERF-${(i + 3) % 40 + 1}\n- item \`code ${i}\`\n- item **bold** and *italic*`,
  `| Gate | Owner |\n| --- | --- |\n${Array.from({ length: 5 }, (_, k) => `| g${k} | o${k} |`).join("\n")}`,
].join("\n\n");
const messages = Array.from({ length: TOTAL }, (_, i) => ({
  id: `message-${i}`, chat_session_id: LONG, role: i % 2 ? "assistant" : "user", task_id: null,
  message_kind: "message", content: i % 2 ? reply(i) : `question ${i}`, created_at: T,
}));
const session = (id: string, title: string) => ({
  id, workspace_id: WORKSPACE_ID, agent_id: AGENT_ID, creator_id: USER_ID, title, status: "active",
  has_unread: false, unread_count: 0, last_message: null, pinned: false, created_at: T, updated_at: T,
});
const sessions = [session(LONG, "Long DM"), ...OTHERS.map((id, n) => session(id, `Busy DM ${n + 1}`))];

test.setTimeout(Number(process.env.CHAT_LAG_TIMEOUT ?? 300_000));
test.use({ viewport: { width: 1440, height: 900 }, locale: "en-US", serviceWorkers: "block" });

test("long DM open while three other DMs run", async ({ page, context, baseURL }) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const socket = new MockSocket();
  const requests: Record<string, number> = {};
  let currentUrl = new URL(origin);
  const h = await installHarness(page, context, origin, (path, url) => {
    currentUrl = url;
    requests[path] = (requests[path] ?? 0) + 1;
    switch (path) {
      case "/api/chat/sessions": return sessions;
      case "/api/chat/pending-tasks": return { tasks: OTHERS.map((id) => ({ task_id: taskOf(id), status: "running", chat_session_id: id })) };
      case "/api/chat/pending-tasks/has-any": return { has_pending: true };
      case `/api/chat/sessions/${LONG}`: return sessions[0];
      case `/api/chat/sessions/${LONG}/messages/page`: {
        const beforeId = currentUrl.searchParams.get("before_id");
        const end = beforeId ? messages.findIndex((m) => m.id === beforeId) : TOTAL;
        const slice = messages.slice(Math.max(0, end - 50), end);
        const more = end - 50 > 0;
        return { messages: slice, limit: 50, has_more: more, next_cursor: more ? { created_at: T, id: slice[0]!.id } : null };
      }
      case `/api/chat/sessions/${LONG}/pending-task`: return {};
      case `/api/chat/sessions/${LONG}/read`: case `/api/chat/sessions/${LONG}/draft-restores`: return [];
      default: {
        const other = OTHERS.find((id) => path.startsWith(`/api/chat/sessions/${id}`));
        if (!other) return undefined;
        if (path.endsWith("/messages/page")) return { messages: [], limit: 50, has_more: false, next_cursor: null };
        if (path.endsWith("/pending-task")) return { task_id: taskOf(other), status: "running", created_at: T, supports_queue: true, queued_tasks: [] };
        return path.endsWith(other) ? sessions.find((s) => s.id === other) : [];
      }
    }
  });
  h.gate.release();
  await context.routeWebSocket((u) => u.pathname === "/ws", socket.attach);
  await socket.watch(page);
  await context.addInitScript(({ sid }) => {
    localStorage.setItem("multica:chat:activeSessionId:perf", sid);
    const w = window as unknown as Record<string, unknown>;
    // Commit counter: React calls this hook on every root commit, production build included.
    let commits = 0;
    w.__commits = () => commits;
    w.__REACT_DEVTOOLS_GLOBAL_HOOK__ = {
      supportsFiber: true, renderers: new Map(), inject: () => 1, checkDCE() {},
      onCommitFiberRoot() { commits++; }, onCommitFiberUnmount() {}, onPostCommitFiberRoot() {},
    };
    const longTasks: number[] = [];
    const events: number[] = [];
    const slow: string[] = [];
    w.__lag = { longTasks, events, slow };
    new PerformanceObserver((l) => l.getEntries().forEach((e) => longTasks.push(e.duration))).observe({ type: "longtask", buffered: true });
    new PerformanceObserver((l) => l.getEntries().forEach((e) => { events.push(e.duration); if (e.duration >= 200) slow.push(`${e.name}:${Math.round(e.duration)}`); }))
      .observe({ type: "event", durationThreshold: 16, buffered: true } as PerformanceObserverInit);
  }, { sid: LONG });

  await page.goto(`${origin}/${WORKSPACE_SLUG}/chat?session=${LONG}`, { waitUntil: "domcontentloaded" });
  await expect(page.locator("[data-message-id]").first()).toBeVisible({ timeout: 60_000 });
  await expect.poll(() => socket.connected).toBe(true);
  const list = page.locator("[data-tab-scroll-root]");
  await list.hover();
  await page.waitForTimeout(1500);

  const read = () => page.evaluate(() => {
    const w = window as unknown as { __commits: () => number; __lag: { longTasks: number[]; events: number[] } };
    return { commits: w.__commits(), longTasks: [...w.__lag.longTasks], events: [...w.__lag.events] };
  });
  // Browser time split (layout / style / script / task), cumulative CDP metrics.
  const metrics = await context.newCDPSession(page);
  await metrics.send("Performance.enable");
  const pick = async () => Object.fromEntries(((await metrics.send("Performance.getMetrics")) as { metrics: { name: string; value: number }[] }).metrics
    .filter((m) => ["LayoutDuration", "RecalcStyleDuration", "ScriptDuration", "TaskDuration", "LayoutCount", "RecalcStyleCount"].includes(m.name)).map((m) => [m.name, m.value]));
  const metricsBefore = await pick();
  const cdp = process.env.CHAT_LAG_PROFILE ? await context.newCDPSession(page) : null;
  if (cdp) { await cdp.send("Profiler.enable"); await cdp.send("Profiler.setSamplingInterval", { interval: 500 }); await cdp.send("Profiler.start"); }
  const before = await read();
  const t0 = Date.now();
  const sent = { dm: 0, runs: 0 };
  let n = 0;
  // Event driver: DM chatter on the three busy sessions plus issue-run starts for chip issues.
  const driver = (async () => {
    while (Date.now() - t0 < SECONDS * 1000) {
      for (let i = 0; i < DM_EVENTS_PER_SEC; i++) {
        const sid = OTHERS[n % 3]!;
        socket.send("chat:message", { id: `live-${n}`, chat_session_id: sid, role: "assistant", content: "tick", message_kind: "message", task_id: taskOf(sid), created_at: T });
        socket.send("task:message", { task_id: taskOf(sid), seq: n, type: "text", content: "streamed token ".repeat(20) });
        n++; sent.dm++;
      }
      for (let i = 0; i < ISSUE_RUNS_PER_SEC; i++) {
        socket.send("task:running", { task_id: `run-${n}`, agent_id: AGENT_ID, issue_id: issueIdForKey(String(n % 40 + 1)), status: "running" });
        n++; sent.runs++;
      }
      await page.waitForTimeout(1000);
    }
  })();
  // Interaction driver: scrolling up and down, then typing, interleaved.
  const composer = page.locator(".ProseMirror, [role=textbox], [contenteditable=true]").last();
  while (Date.now() - t0 < SECONDS * 1000) {
    if (process.env.CHAT_LAG_NOINPUT === "1") { await page.waitForTimeout(500); continue; }
    if (process.env.CHAT_LAG_ONLY !== "type") for (let i = 0; i < 6; i++) { await page.mouse.wheel(0, i < 3 ? -900 : 900); await page.waitForTimeout(120); }
    if (process.env.CHAT_LAG_ONLY === "scroll") continue;
    await composer.click({ timeout: 10_000 });
    await page.keyboard.type("typing while other chats run", { delay: 30 });
    await page.keyboard.press("Control+A");
    await page.keyboard.press("Backspace");
    await list.hover();
  }
  await driver;
  const after = await read();
  const metricsAfter = await pick();
  console.log(`CHAT_LAG_SLOW_EVENTS ${JSON.stringify(await page.evaluate(() => (window as unknown as { __lag: { slow: string[] } }).__lag.slow))}`);
  console.log(`CHAT_LAG_BROWSER_TIME ${JSON.stringify(Object.fromEntries(Object.entries(metricsAfter).map(([k, v]) => [k, Math.round((v - (metricsBefore[k] ?? 0)) * (k.endsWith("Duration") ? 1000 : 1))])))}`);
  if (cdp) {
    const { profile } = await cdp.send("Profiler.stop") as { profile: { nodes: { id: number; children?: number[]; callFrame: { functionName: string; url: string; lineNumber: number; columnNumber: number } }[]; samples: number[]; timeDeltas: number[] } };
    const byId = new Map(profile.nodes.map((nd) => [nd.id, nd.callFrame]));
    const self = new Map<string, number>();
    profile.samples.forEach((id, i) => {
      const f = byId.get(id)!;
      const k = `${f.functionName || "(anon)"} ${f.url.split("/").slice(-2).join("/")}:${f.lineNumber + 1}:${f.columnNumber}`;
      self.set(k, (self.get(k) ?? 0) + (profile.timeDeltas[i] ?? 0) / 1000);
    });
    const top = [...self].sort((a, b) => b[1] - a[1]).slice(0, 25).map(([k, ms]) => `${Math.round(ms)}ms ${k}`);
    console.log(`CHAT_LAG_PROFILE\n${top.join("\n")}`);
  }
  const longTasks = after.longTasks.slice(before.longTasks.length);
  const events = after.events.slice(before.events.length);
  const seconds = (Date.now() - t0) / 1000;
  const report = {
    messages: TOTAL, seconds: Math.round(seconds), sent, cpu_throttle: 4,
    long_tasks_over_50ms: longTasks.length, long_task_max_ms: Math.round(Math.max(0, ...longTasks)),
    long_task_total_ms: Math.round(longTasks.reduce((a, b) => a + b, 0)),
    inp_ms: Math.round(events.sort((a, b) => b - a)[Math.max(0, Math.floor(events.length * 0.02))] ?? 0),
    event_max_ms: Math.round(Math.max(0, ...events)), event_count: events.length,
    commits_per_sec: Math.round(((after.commits - before.commits) / seconds) * 10) / 10,
    requests: Object.fromEntries(Object.entries(requests).filter(([k]) => k.startsWith("/api/chat")).sort()),
    page_errors: h.pageErrors, unmocked: h.unmocked,
  };
  console.log(`CHAT_LAG_REPORT ${JSON.stringify(report)}`);
  if (process.env.CHAT_LAG_ASSERT === "1") {
    expect(report.long_task_max_ms).toBeLessThanOrEqual(200);
    expect(report.event_max_ms).toBeLessThanOrEqual(200);
  }
});
