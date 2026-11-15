/**
 * Content retention of issue chips and the description disclosure (CHE-2970 / A1c).
 *
 * R3 (CHE-1830, H3): a chip that showed its issue's title keeps the title and
 *    its box when a list update drops that issue from the list's first page.
 * R4 (CHE-1830, H5): the description's "Show more" label stays identical, frame
 *    by frame, for 500 ms after a child mutation in the description.
 *
 * Runs once per profile project in playwright.perf.config.ts (desktop, phone),
 * both under 4x CPU throttling, against the production build with synthetic
 * HTTP and WebSocket. Assertions are soft so one run reports every requirement.
 *
 * Both requirements gate on text and geometry, which are the same on both
 * profiles. No layout-shift limit applies; the raw sum is reported only.
 */
import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";
import * as fx from "./fixtures/chip-stability";

test.use({ locale: "en-US", serviceWorkers: "block" });

/** One pixel: sub-pixel rounding, not a visible jump. */
const GEOMETRY_LIMIT_PX = 1;
/** How long R4 samples the label after the mutation. */
const LABEL_WINDOW_MS = 500;
const commentId = (i: number) => `cccc${String(i).padStart(4, "0")}-4444-4444-8444-444444444444`;
const CHIP_KEY = "PERF-101";
/** One chip per comment. */
const CHIP_COUNT = 3;
const CHIP_ISSUE = fx.issueFor(CHIP_KEY);

/** One line per assertion input, so the PR body can quote observed values. */
const report = (name: string, values: Record<string, unknown>) =>
  // eslint-disable-next-line no-console
  console.log(`PERF_REPORT ${JSON.stringify({ test: name, ...values })}`);

function expectClean(h: fx.Harness) {
  expect(h.unmocked, "unmocked requests").toEqual([]);
  expect(h.external, "external requests").toEqual([]);
  expect(h.pageErrors, "page errors").toEqual([]);
}

const nextFrame = (page: Page) =>
  page.evaluate(() => new Promise<void>((done) => {
    requestAnimationFrame(() => requestAnimationFrame(() => done()));
  }));

const timeline = (content: (i: number) => string, count: number) =>
  Array.from({ length: count }, (_, i) => ({
    type: "comment" as const, id: commentId(i), actor_type: "user" as const, actor_id: fx.USER_ID,
    created_at: new Date(Date.UTC(2026, 8, 1) + i * 1000).toISOString(),
    updated_at: new Date(Date.UTC(2026, 8, 1) + i * 1000).toISOString(),
    issue_id: fx.THREAD_ISSUE_ID, parent_id: null, content: content(i),
    resolved_at: null, reactions: [], attachments: [], source_task_id: null,
  }));

// ---- R3 ---------------------------------------------------------------------

interface ChipSample { text: string; w: number; h: number }

test("R3: a chip keeps its title when its issue leaves the list's first page", async ({
  page, context, baseURL,
}, testInfo) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const profile = fx.profileOf(testInfo.project.name);
  const threadIssue = fx.issueFor(fx.THREAD_ISSUE_IDENTIFIER, fx.THREAD_ISSUE_ID);
  const comments = timeline((i) => `Comment ${i} tracks ${CHIP_KEY} in the thread.`, CHIP_COUNT);
  // The chip's issue is on page 1 until the synthetic list update removes it.
  const list = { holdsChipIssue: true, requests: 0 };
  const h = await fx.installHarness(page, context, origin, (path) => {
    if (path === "/api/issues") {
      list.requests += 1;
      return { issues: list.holdsChipIssue ? [CHIP_ISSUE] : [], total: list.holdsChipIssue ? 1 : 0 };
    }
    return fx.issuePageRoute(path, threadIssue, comments);
  });
  h.gate.release();
  await context.addInitScript(() => {
    localStorage.setItem("multica:chat:floatingChatEnabled", "false");
    // Every frame, every chip: its text and box. Started by the test once the chips show titles.
    const w = window as unknown as {
      __samples: ChipSample[][]; __sampling: boolean;
    };
    w.__samples = [];
    w.__sampling = false;
    const sample = () => {
      if (w.__sampling) {
        w.__samples.push([...document.querySelectorAll("span.issue-mention")].map((chip) => {
          const r = chip.getBoundingClientRect();
          return { text: chip.textContent ?? "", w: r.width, h: r.height };
        }));
      }
      requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
  });
  let socket!: WebSocketRoute;
  let onSocket!: () => void;
  const socketReady = new Promise<void>((done) => { onSocket = done; });
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => {
    socket = ws;
    ws.onMessage(() => {});
    onSocket();
  });

  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/issues/${fx.THREAD_ISSUE_IDENTIFIER}`, {
    waitUntil: "domcontentloaded",
  });
  await socketReady;
  const chips = page.locator("span.issue-mention");
  await expect(chips).toHaveCount(CHIP_COUNT, { timeout: 30_000 });
  // Every chip shows the title, served from the list.
  for (const chip of await chips.all()) {
    await expect(chip).toContainText(CHIP_ISSUE.title, { timeout: 15_000 });
  }
  await nextFrame(page);
  await fx.resetShift(page);
  const titleText = (await chips.first().textContent()) ?? "";
  await page.evaluate(() => { (window as unknown as { __sampling: boolean }).__sampling = true; });

  // The list update: page 1 no longer holds the issue. The detail lookup that
  // replaces it answers after LOOKUP_DELAY_MS; sample past that.
  list.holdsChipIssue = false;
  socket.send(JSON.stringify({ type: "label:updated", payload: {}, actor_type: "system", actor_id: "" }));
  await expect.poll(() => list.requests, { timeout: 10_000 }).toBeGreaterThanOrEqual(2);
  await page.waitForTimeout(fx.LOOKUP_DELAY_MS * 3);
  await nextFrame(page);
  const samples = await page.evaluate(() => {
    const w = window as unknown as { __samples: ChipSample[][]; __sampling: boolean };
    w.__sampling = false;
    return w.__samples;
  });

  // Every frame counts, including one with no chip: a chip that disappears is the defect.
  const frames = samples;
  const chipCounts = [...new Set(frames.map((s) => s.length))];
  const present = frames.filter((s) => s.length > 0);
  const texts = new Set(present.flat().map((c) => c.text));
  const widthDelta = Math.max(0, ...present.flat().map((c) => Math.abs(c.w - present[0]![0]!.w)));
  const heightDelta = Math.max(0, ...present.flat().map((c) => Math.abs(c.h - present[0]![0]!.h)));
  const shift = await fx.readShift(page);

  expect.soft(frames.length, "frames sampled after the list update").toBeGreaterThan(10);
  expect.soft(chipCounts, "chips present in every frame").toEqual([CHIP_COUNT]);
  expect.soft([...texts], "distinct chip texts after the list update").toEqual([titleText]);
  expect.soft(widthDelta, "chip width delta (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);
  expect.soft(heightDelta, "chip height delta (px)").toBeLessThanOrEqual(GEOMETRY_LIMIT_PX);

  report("R3", {
    profile, frames: frames.length, chipCounts, distinctTexts: [...texts], widthDelta, heightDelta,
    cls: shift.cls, rawShift: shift.raw,
  });
  expectClean(h);
});

// ---- R4 ---------------------------------------------------------------------

const DESCRIPTION_PARAGRAPHS = 40;
const description = Array.from(
  { length: DESCRIPTION_PARAGRAPHS },
  (_, p) => `Paragraph ${p + 1}. The author describes the change, what was measured and why it matters.`,
).join("\n\n");
const SHOW_MORE = /^Show more \d+ more lines?$/;

test("R4: the disclosure label is stable after a child mutation in the description", async ({
  page, context, baseURL,
}, testInfo) => {
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const profile = fx.profileOf(testInfo.project.name);
  const issue = { ...fx.issueFor(fx.THREAD_ISSUE_IDENTIFIER, fx.THREAD_ISSUE_ID), description };
  const h = await fx.installHarness(page, context, origin, (path) => fx.issuePageRoute(path, issue, []));
  h.gate.release();
  await context.addInitScript(() => localStorage.setItem("multica:chat:floatingChatEnabled", "false"));
  await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => { ws.onMessage(() => {}); });

  await page.goto(`${origin}/${fx.WORKSPACE_SLUG}/issues/${fx.THREAD_ISSUE_IDENTIFIER}`, {
    waitUntil: "domcontentloaded",
  });
  const disclosure = page.locator("[data-description-disclosure]");
  const button = disclosure.locator("button[aria-controls]");
  await expect(button).toHaveText(SHOW_MORE, { timeout: 30_000 });
  // Let the settled measurement land, then take the label the reader sees.
  await page.waitForTimeout(fx.LOOKUP_DELAY_MS);
  await nextFrame(page);
  const settled = (await button.textContent()) ?? "";
  await fx.resetShift(page);

  // A child mutation inside the description: an empty node, so the line count is unchanged.
  // It lands right after a frame, so the render it triggers has the rest of the frame to commit in.
  // The window starts when the node is appended, and `mutated` proves it was.
  const { labels, mutated } = await page.evaluate(({ windowMs }) => new Promise<{ labels: string[]; mutated: boolean }>((done) => {
    const label = () =>
      document.querySelector("[data-description-disclosure] button[aria-controls]")?.textContent ?? "";
    const host = document.querySelector("[data-description-editor] [data-comment-content]")!;
    const seen: string[] = [];
    let mutatedAt = 0;
    requestAnimationFrame(() => setTimeout(() => {
      host.appendChild(document.createElement("span"));
      mutatedAt = performance.now();
    }, 0));
    const tick = () => {
      if (mutatedAt) seen.push(label());
      if (!mutatedAt || performance.now() - mutatedAt < windowMs) requestAnimationFrame(tick);
      else done({ labels: seen, mutated: true });
    };
    tick();
  }), { windowMs: LABEL_WINDOW_MS });

  const distinct = [...new Set(labels)];
  const shift = await fx.readShift(page);
  expect.soft(mutated, "the mutation ran before the window").toBe(true);
  expect.soft(labels.length, "requestAnimationFrame samples after the mutation").toBeGreaterThan(5);
  expect.soft(distinct, "distinct disclosure labels over 500 ms").toEqual([settled]);

  report("R4", { profile, settled, samples: labels.length, distinctLabels: distinct, cls: shift.cls, rawShift: shift.raw });
  expectClean(h);
});
