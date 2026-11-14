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
  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/chat`, { waitUntil: "domcontentloaded" });
  const firstRow = page.getByTestId("chat-thread-row").first();
  await expect(firstRow).toBeVisible({ timeout: 60_000 });
  await page.waitForTimeout(1500);

  // The list's own scroll container: nearest vertically scrolling ancestor of a
  // row. Resolved per call; the page may re-create it after first paint.
  const atBottom = () =>
    page.getByTestId("chat-thread-row").first().evaluate((el) => {
      for (let p = el.parentElement; p; p = p.parentElement) {
        const o = getComputedStyle(p).overflowY;
        if (o === "auto" || o === "scroll") return p.scrollTop + p.clientHeight >= p.scrollHeight - 2;
      }
      throw new Error("no scroll container");
    });
  const box = (await page.getByTestId("chat-thread-row").first().boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);

  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 });
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

  let requests = 0;
  const onRequest = (r: { url(): string }) => { if (new URL(r.url()).pathname.startsWith("/api/")) requests += 1; };
  page.on("request", onRequest);
  let maxRows = 0;
  let steps = 0;
  for (; steps < 3000; steps += 1) {
    await page.mouse.wheel(0, 120);
    await page.waitForTimeout(16);
    maxRows = Math.max(maxRows, await page.getByTestId("chat-thread-row").count());
    if (await atBottom()) break;
  }
  await page.waitForTimeout(600);
  page.off("request", onRequest);
  const perf = await page.evaluate(() => (window as unknown as { __perf: { long: number[]; frames: number[] } }).__perf);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });

  const longTasks = perf.long.filter((d) => d > 50);
  const report = {
    dms: DM_COUNT, wheel_steps: steps, requests_during_scroll: requests,
    long_tasks_over_50ms: longTasks.length, longest_task_ms: Math.round(Math.max(0, ...perf.long)),
    // A frame longer than 2 vsync intervals (33 ms) is dropped.
    dropped_frames: perf.frames.filter((d) => d > 33.4).length, total_frames: perf.frames.length,
    max_frame_ms: Math.round(Math.max(0, ...perf.frames)), max_mounted_rows: maxRows,
    final_rows_mounted: await page.getByTestId("chat-thread-row").count(),
    page_errors: app.pageErrors.length,
  };
  console.log(`DM_LIST_REPORT ${JSON.stringify(report)}`);
  if (process.env.PERF_REPORT_PATH) writeFileSync(process.env.PERF_REPORT_PATH, JSON.stringify(report, null, 2));

  expect(app.pageErrors).toEqual([]);
  if (process.env.DM_LIST_ASSERT === "1") {
    expect(report.requests_during_scroll, "requests while scrolling").toBe(0);
    expect(report.long_tasks_over_50ms, "long tasks over 50 ms").toBe(0);
    expect(report.max_mounted_rows, "mounted rows").toBeLessThanOrEqual(MAX_MOUNTED_ROWS);
  }
});
