/**
 * DM list scroll cost (CHE-5773): 300 DMs, wheel from the top to the bottom of
 * the list at 4x CPU throttle. Reports long tasks over 50 ms, dropped frames,
 * API requests sent while scrolling and the most rows mounted at once.
 *
 * Report-only unless DM_LIST_ASSERT=1 (the acceptance criteria): zero requests,
 * zero long tasks and at most 40 mounted rows. PERF_REPORT_PATH also writes the JSON.
 */
import { writeFileSync } from "node:fs";
import { expect, test } from "@playwright/test";
import * as fx from "./fixtures/scroll-stability";
import { boot } from "./lib/dm-harness";

const DM_COUNT = 300;
const MAX_MOUNTED_ROWS = 40;
const DAY_MS = 86_400_000;

test.use({ viewport: { width: 1280, height: 800 }, locale: "en-US", serviceWorkers: "block" });

const uuid = (n: number) => `5555${n.toString(16).padStart(4, "0")}-4444-4444-8444-444444444444`;

test("DM list scroll: 300 DMs, top to bottom, 4x CPU", async ({ page, baseURL }) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const app = await boot(page, baseURL);
  const now = Date.now();
  // Two a day: today, yesterday, this week and older groups all appear.
  app.sessions = Array.from({ length: DM_COUNT }, (_, i) => {
    const at = new Date(now - i * (DAY_MS / 2)).toISOString();
    return {
      ...fx.session, id: uuid(i), title: `DM ${i}`, created_at: at, updated_at: at,
      has_unread: i % 7 === 0, unread_count: i % 7 === 0 ? 2 : 0, pinned: i < 3,
      last_message: {
        id: uuid(i + 1000), role: "agent", content: `Last reply in DM ${i}`, created_at: at,
        message_kind: "text", failure_reason: null,
      },
    };
  });

  await page.addInitScript(() => {
    const w = window as unknown as { __perf: { long: number[]; frames: number[] } };
    w.__perf = { long: [], frames: [] };
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__perf.long.push(e.duration);
    }).observe({ type: "longtask", buffered: true });
  });
  if (process.env.DM_LIST_NO_SHIELD) {
    // Diagnostic: drop the scroll pointer shield to see what it costs.
    await page.addInitScript(() => {
      Object.defineProperty(CSSStyleDeclaration.prototype, "pointerEvents", { set() {}, get: () => "" });
    });
  }
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/chat`, { waitUntil: "domcontentloaded" });
  const firstRow = page.getByTestId("chat-thread-row").first();
  await expect(firstRow).toBeVisible({ timeout: 60_000 });
  await page.waitForTimeout(1500);

  // One in-page read per step (mounted rows, at bottom): locator calls inject
  // Playwright's own scripts and would add their cost to the page's CPU.
  const readList = () =>
    page.evaluate(() => {
      const rows = document.querySelectorAll("[data-testid='chat-thread-row']");
      let atBottom = false;
      for (let p = rows[0]?.parentElement ?? null; p; p = p.parentElement) {
        const o = getComputedStyle(p).overflowY;
        if (o === "auto" || o === "scroll") { atBottom = p.scrollTop + p.clientHeight >= p.scrollHeight - 2; break; }
      }
      return { rows: rows.length, atBottom };
    });
  const box = (await firstRow.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  // Start from a quiet page: app-shell prefetch is not scroll cost.
  let quietFor = 0;
  let lastRequest = Date.now();
  const touch = () => { lastRequest = Date.now(); };
  page.on("request", touch);
  while (quietFor < 2000) { await page.waitForTimeout(250); quietFor = Date.now() - lastRequest; }
  page.off("request", touch);

  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: Number(process.env.DM_LIST_CPU ?? 4) });
  await page.evaluate(() => {
    const w = window as unknown as { __perf: { long: number[]; frames: number[] } };
    w.__perf.long.length = 0;
    let last = performance.now();
    const loop = () => {
      const t = performance.now();
      w.__perf.frames.push(t - last);
      last = t;
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  });

  const requestUrls: string[] = [];
  let agentListPolls = 0;
  const onRequest = (r: { url(): string }) => {
    const u = new URL(r.url());
    if (!u.pathname.startsWith("/api/")) return;
    // agentListOptions polls every 30 s while an agent is online; the 4x pass
    // outlasts that. A timer, not scrolling, so it is counted apart.
    if (u.pathname === "/api/agents") agentListPolls += 1;
    else requestUrls.push(u.pathname);
  };
  const metrics = async () =>
    Object.fromEntries((await cdp.send("Performance.getMetrics")).metrics.map((m) => [m.name, m.value]));
  await cdp.send("Performance.enable");
  const m0 = await metrics();
  const profile = process.env.DM_LIST_PROFILE === "1";
  if (profile) { await cdp.send("Profiler.enable"); await cdp.send("Profiler.start"); }
  page.on("request", onRequest);
  let maxRows = 0;
  let steps = 0;
  for (; steps < 3000; steps += 1) {
    await page.mouse.wheel(0, 120);
    await page.waitForTimeout(16);
    const state = await readList();
    maxRows = Math.max(maxRows, state.rows);
    if (state.atBottom) break;
  }
  await page.waitForTimeout(600);
  page.off("request", onRequest);
  let hot: string[] = [];
  if (profile) {
    const { profile: prof } = await cdp.send("Profiler.stop");
    const dt = new Map<number, number>();
    prof.samples!.forEach((id, i) => dt.set(id, (dt.get(id) ?? 0) + (prof.timeDeltas![i] ?? 0)));
    const self = new Map<string, number>();
    for (const n of prof.nodes) {
      const f = n.callFrame;
      const k = `${f.functionName || "(anon)"} ${f.url.split("/").slice(-2).join("/")}:${f.lineNumber}`;
      self.set(k, (self.get(k) ?? 0) + (dt.get(n.id) ?? 0));
    }
    hot = [...self.entries()].sort((a, b) => b[1] - a[1]).slice(0, 25).map(([k, v]) => `${Math.round(v / 1000)}ms ${k}`);
  }
  const m1 = await metrics();
  const secs = (k: string) => Math.round(((m1[k] ?? 0) - (m0[k] ?? 0)) * 1000);
  const perf = await page.evaluate(() => (window as unknown as { __perf: { long: number[]; frames: number[] } }).__perf);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });

  const longTasks = perf.long.filter((d) => d > 50);
  const report = {
    dms: DM_COUNT, wheel_steps: steps, requests_during_scroll: requestUrls.length, agent_list_polls: agentListPolls,
    long_tasks_over_50ms: longTasks.length, longest_task_ms: Math.round(Math.max(0, ...perf.long)),
    // A frame longer than 2 vsync intervals (33 ms) is dropped.
    dropped_frames: perf.frames.filter((d) => d > 33.4).length, total_frames: perf.frames.length,
    max_frame_ms: Math.round(Math.max(0, ...perf.frames)), max_mounted_rows: maxRows,
    final_rows_mounted: (await readList()).rows,
    // Main-thread time by kind while scrolling, ms of throttled CPU.
    script_ms: secs("ScriptDuration"), layout_ms: secs("LayoutDuration"),
    style_ms: secs("RecalcStyleDuration"), task_ms: secs("TaskDuration"),
    page_errors: app.pageErrors.length,
  };
  if (requestUrls.length) console.log(`DM_LIST_REQUESTS ${JSON.stringify(requestUrls)}`);
  if (hot.length) console.log(`DM_LIST_HOT\n${hot.join("\n")}`);
  console.log(`DM_LIST_REPORT ${JSON.stringify(report)}`);
  if (process.env.PERF_REPORT_PATH) writeFileSync(process.env.PERF_REPORT_PATH, JSON.stringify(report, null, 2));

  expect(app.pageErrors).toEqual([]);
  if (process.env.DM_LIST_ASSERT === "1") {
    expect(report.requests_during_scroll, "requests while scrolling").toBe(0);
    expect(report.long_tasks_over_50ms, "long tasks over 50 ms").toBe(0);
    expect(report.max_mounted_rows, "mounted rows").toBeLessThanOrEqual(MAX_MOUNTED_ROWS);
  }
});
