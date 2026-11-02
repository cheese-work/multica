/**
 * Page-load baseline for five routes (CHE-2976).
 *
 * Cold load of the issue list, issue detail, chat, inbox and projects pages on
 * the real production build, one fresh browser context per sample. Only HTTP
 * and the WebSocket are synthetic, as in the typing scenario. Runs once per
 * profile project in `playwright.perf.config.ts` (desktop, phone).
 *
 * Both profiles throttle the CPU 4x. The phone profile also emulates 150 ms RTT
 * and 1.6 Mbps down. Synthetic API responses never reach the network stack, so
 * on the phone profile they wait the same RTT plus their transfer time here.
 * The document goes through the network stack: Chromium throttles its body at
 * the emulated bandwidth but adds no latency, so the spec adds one RTT. Each
 * phone sample records the document transfer time and fails if the document
 * arrived faster than the link allows.
 *
 * A sample whose page never showed its data is invalid, not fast: an error page
 * loads quickly.
 */
import { expect, test, type CDPSession, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import * as fx from "./fixtures/typing-under-live-runs";
import * as pl from "./fixtures/page-load-baseline";
import { documentFasterThanLink, responseDelayMs } from "./phone-network.mjs";

const SAMPLES = Number(process.env.PERF_SAMPLES ?? 5);
const CPU_THROTTLE = 4;
const PHONE_NETWORK = { rttMs: 150, downBytesPerSec: (1.6 * 1_000_000) / 8 };
/** Quiet time after the page shows its data, so LCP and late chunks are counted. */
const SETTLE_MS = 2_500;
/** One extra load per route under the CPU profiler. It never joins the medians. */
const PROFILE = Boolean(process.env.PERF_PROFILE);
test.setTimeout(1_800_000);

const ROUTES = {
  issues: { path: `/${fx.WORKSPACE_SLUG}/issues`, ready: () => pl.issueTitle(2) },
  "issue-detail": {
    path: `/${fx.WORKSPACE_SLUG}/issues/${fx.ISSUE_IDENTIFIER}`,
    ready: () => "Paragraph 1 of comment 0",
  },
  chat: { path: `/${fx.WORKSPACE_SLUG}/chat`, ready: () => pl.chatTitle(0) },
  inbox: { path: `/${fx.WORKSPACE_SLUG}/inbox`, ready: () => pl.inboxTitle(pl.INBOX_COUNT - 1) },
  projects: { path: `/${fx.WORKSPACE_SLUG}/projects`, ready: () => pl.projectTitle(0) },
} as const;

const workspace = {
  id: fx.WORKSPACE_ID, name: "Perf", slug: fx.WORKSPACE_SLUG, description: null,
  context: null, settings: { github_enabled: false }, repos: [], issue_prefix: "PERF",
  avatar_url: null, created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
};
const me = {
  id: fx.USER_ID, name: "Perf User", email: "perf@example.test", avatar_url: null,
  language: null, onboarded_at: "2026-09-01T00:00:00Z",
  onboarding_questionnaire: { source: "other" },
  created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
};
const detailIssue = {
  ...pl.buildIssues()[0]!, id: fx.ISSUE_ID, number: 1, identifier: fx.ISSUE_IDENTIFIER,
  title: "Page-load baseline detail", project_id: null,
};

/** Every request the pages are allowed to make; anything else is reported. */
function responseFor(pathname: string, postData: string | null): unknown | undefined {
  if (pathname === "/api/issues/table/rows" || pathname === "/api/issues/table/facets") {
    const issues = pl.buildIssues();
    const body = JSON.parse(postData ?? "{}") as { group_key?: string | null };
    if (pathname.endsWith("facets")) {
      const counts = new Map<string, number>();
      for (const i of issues) counts.set(i.status, (counts.get(i.status) ?? 0) + 1);
      return {
        query_fingerprint: "perf", total: issues.length,
        facets: [{ kind: "status", values: [...counts].map(([key, count]) => ({ key, count })) }],
      };
    }
    const inGroup = issues.filter((i) => `status:${i.status}` === body.group_key);
    return {
      query_fingerprint: "perf", group_key: body.group_key ?? null, parent_id: null,
      total: inGroup.length, branch_total: inGroup.length, next_cursor: null,
      rows: inGroup.slice(0, 50).map((issue) => ({ issue, direct_child_count: 0 })),
    };
  }
  const messages = /^\/api\/chat\/sessions\/[0-9a-f-]+\/messages(\/page)?$/.exec(pathname);
  if (messages) {
    const rows = pl.buildChatMessages(pl.chatIds[0]!);
    return messages[1] ? { messages: rows, has_more: false } : rows;
  }
  switch (pathname) {
    case "/api/config": return { feature_flags: {}, allow_signup: true };
    case "/api/me": return me;
    case "/api/workspaces": return [workspace];
    case "/api/client-usage": return {};
    case "/api/agents": return [pl.agent];
    case `/api/issues/${fx.ISSUE_ID}/system-wakeups`: case "/api/decisions/open":
      return [];
    case `/api/issues/${fx.ISSUE_ID}/duplicates`: return { duplicate_of: null, duplicates: [] };
    case `/api/issues/${fx.ISSUE_ID}/relations`:
      return { nodes: [], edges: [], omitted_count: 0, depth: 1 };
    case "/api/inbox/archived/page": return { items: [], next_cursor: null, has_more: false };
    case "/api/chat/pending-tasks": return { tasks: [] };
    case "/api/issue-views": case "/api/issue-wakeup-summaries": case "/api/skills":
    case "/api/autopilots": case "/api/issue-wakeup-paused": case "/api/agent-activity-30d": case "/api/issue-workflows":
      return [];
    case "/api/agent-run-counts": return {};
    case "/api/labels": return { labels: [] };
    case "/api/issue-view-preferences": return { preferences: [] };
    case "/api/search-index/manifest": return { version: "perf", shards: [] };
    case "/api/runtimes": case "/api/squads": case "/api/agent-task-snapshot":
    case "/api/invitations": case "/api/pins": case "/api/inbox/unread-summary":
      return [];
    case "/api/inbox": return pl.buildInbox();
    case "/api/chat/sessions": return pl.buildChatSessions();
    case "/api/chat/pending-tasks/has-any": return { has_pending: false };
    case `/api/workspaces/${fx.WORKSPACE_ID}/members`: return [];
    case "/api/projects": return { projects: pl.buildProjects(), total: pl.PROJECT_COUNT };
    case "/api/issues": return { issues: pl.buildIssues(), total: pl.ISSUE_COUNT };
    case "/api/issue-statuses": return { statuses: [] };
    case "/api/properties": return { properties: [], total: 0 };
    case "/api/quick-actions": return { quick_actions: [] };
    case "/api/assignee-frequency": case "/api/issues/child-progress":
      return pathname.endsWith("progress") ? { progress: [] } : [];
    case `/api/issues/${fx.ISSUE_IDENTIFIER}`:
    case `/api/issues/${fx.ISSUE_ID}`: return detailIssue;
    case `/api/issues/${fx.ISSUE_ID}/timeline`: return fx.buildTimelineEntries();
    case `/api/issues/${fx.ISSUE_ID}/task-runs`: return [];
    case `/api/issues/${fx.ISSUE_ID}/subscribers`:
    case `/api/issues/${fx.ISSUE_ID}/wakeups`:
    case `/api/issues/${fx.ISSUE_ID}/attachments`: return [];
    case `/api/issues/${fx.ISSUE_ID}/labels`: return { labels: [] };
    case `/api/issues/${fx.ISSUE_ID}/children`: return { issues: [] };
    case `/api/issues/${fx.ISSUE_ID}/pull-requests`: return { pull_requests: [] };
    default: return undefined;
  }
}

const sleep = (ms: number) => new Promise((done) => setTimeout(done, ms));
const median = (xs: number[]) => {
  const s = [...xs].sort((a, b) => a - b);
  return s[s.length >> 1] ?? null;
};

type Sample = {
  ttfb_ms: number; lcp_ms: number | null; long_task_total_ms: number;
  js_transferred_bytes: number; js_decoded_bytes: number; script_count: number;
  top_scripts: { name: string; transfer_bytes: number }[];
  long_tasks: { start_ms: number; duration_ms: number }[];
  document_bytes: number; document_transfer_ms: number;
  api_requests: number; unmocked: string[]; page_errors: string[];
};

async function readSample(page: Page): Promise<Omit<Sample, "api_requests" | "unmocked" | "page_errors">> {
  await page.evaluate(() => (window as unknown as { __perfDrain: () => void }).__perfDrain());
  return page.evaluate(() => {
    const w = window as unknown as {
      __perfLongTasks: { start: number; duration: number }[]; __perfLcp: number | null;
    };
    const nav = performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming;
    const scripts = (performance.getEntriesByType("resource") as PerformanceResourceTiming[])
      .filter((r) => r.initiatorType === "script" || /\.js(\?|$)/.test(r.name));
    const bytes = (r: PerformanceResourceTiming) => r.transferSize || r.encodedBodySize;
    const top = [...scripts].sort((a, b) => bytes(b) - bytes(a)).slice(0, 5);
    return {
      ttfb_ms: Math.round(nav.responseStart),
      document_bytes: nav.transferSize,
      document_transfer_ms: Math.round(nav.responseEnd - nav.responseStart),
      lcp_ms: w.__perfLcp === null ? null : Math.round(w.__perfLcp),
      long_task_total_ms: Math.round(w.__perfLongTasks.reduce((s, t) => s + t.duration, 0)),
      js_transferred_bytes: scripts.reduce((s, r) => s + bytes(r), 0),
      js_decoded_bytes: scripts.reduce((s, r) => s + r.decodedBodySize, 0),
      script_count: scripts.length,
      top_scripts: top.map((r) => ({
        name: new URL(r.name).pathname.split("/").pop() ?? r.name, transfer_bytes: bytes(r),
      })),
      long_tasks: w.__perfLongTasks
        .map((t) => ({ start_ms: Math.round(t.start), duration_ms: Math.round(t.duration) }))
        .sort((a, b) => b.duration_ms - a.duration_ms).slice(0, 5),
    };
  });
}

/** Main-thread time by script file and by function, from the sampling profiler. */
async function profileEvidence(cdp: CDPSession) {
  const { profile } = await cdp.send("Profiler.stop");
  const { metrics } = await cdp.send("Performance.getMetrics");
  const byNode = new Map(profile.nodes.map((n) => [n.id, n.callFrame]));
  const byFile = new Map<string, number>();
  const byFunction = new Map<string, number>();
  profile.samples?.forEach((id, i) => {
    const ms = (profile.timeDeltas?.[i] ?? 0) / 1000;
    const frame = byNode.get(id);
    if (!frame || frame.functionName === "(idle)" || frame.functionName === "(program)") return;
    const file = frame.url ? (new URL(frame.url).pathname.split("/").pop() ?? frame.url) : "(native)";
    byFile.set(file, (byFile.get(file) ?? 0) + ms);
    const fn = `${frame.functionName || "(anonymous)"} @ ${file}:${frame.lineNumber}:${frame.columnNumber}`;
    byFunction.set(fn, (byFunction.get(fn) ?? 0) + ms);
  });
  const top = (m: Map<string, number>, n: number) =>
    [...m].sort((a, b) => b[1] - a[1]).slice(0, n).map(([name, ms]) => ({ name, ms: Math.round(ms) }));
  const metric = (name: string) => Math.round((metrics.find((m) => m.name === name)?.value ?? 0) * 1000);
  return {
    script_ms: metric("ScriptDuration"), layout_ms: metric("LayoutDuration"),
    recalc_style_ms: metric("RecalcStyleDuration"), task_ms: metric("TaskDuration"),
    top_files: top(byFile, 8), top_functions: top(byFunction, 8),
  };
}

test("page load baseline", async ({ browser, baseURL }, testInfo) => {
  const profile = testInfo.project.name;
  const use = testInfo.project.use;
  const phone = profile === "phone";
  const origin = new URL(baseURL ?? "http://localhost:3000").origin;
  const routes: Record<string, Sample[]> = {};
  const evidence: Record<string, unknown> = {};

  for (const [name, route] of Object.entries(ROUTES)) {
    routes[name] = [];
    for (let n = 0; n < SAMPLES + (PROFILE ? 1 : 0); n += 1) {
      const profiling = n === SAMPLES;
      const context = await browser.newContext({
        viewport: use.viewport ?? null, userAgent: use.userAgent,
        deviceScaleFactor: use.deviceScaleFactor, isMobile: use.isMobile,
        hasTouch: use.hasTouch, locale: "en-US", colorScheme: "light", serviceWorkers: "block",
      });
      try {
        const unmocked: string[] = [];
        const pageErrors: string[] = [];
        let apiRequests = 0;
        await context.addCookies([
          { name: "multica_logged_in", value: "1", url: origin },
          { name: "last_workspace_slug", value: fx.WORKSPACE_SLUG, url: origin },
        ]);
        await context.addInitScript(() => {
          window.localStorage.setItem("multica:chat:floatingChatEnabled", "false");
          const w = window as unknown as Record<string, unknown>;
          const tasks: { start: number; duration: number }[] = [];
          w.__perfLongTasks = tasks;
          w.__perfLcp = null;
          const longTasks = new PerformanceObserver((list) => {
            for (const e of list.getEntries()) tasks.push({ start: e.startTime, duration: e.duration });
          });
          longTasks.observe({ type: "longtask", buffered: true });
          const lcp = new PerformanceObserver((list) => {
            const last = list.getEntries().at(-1);
            if (last) w.__perfLcp = last.startTime;
          });
          lcp.observe({ type: "largest-contentful-paint", buffered: true });
          w.__perfDrain = () => {
            for (const e of longTasks.takeRecords()) tasks.push({ start: e.startTime, duration: e.duration });
            const last = lcp.takeRecords().at(-1);
            if (last) w.__perfLcp = last.startTime;
          };
        });
        await context.route("**/*", async (r) => {
          const url = new URL(r.request().url());
          if (url.origin !== origin) return r.abort();
          if (!url.pathname.startsWith("/api/")) {
            // Chromium applies emulated latency to in-page requests but not to
            // the navigation itself, so the document pays the RTT here.
            if (phone && r.request().isNavigationRequest()) await sleep(PHONE_NETWORK.rttMs);
            return r.continue();
          }
          apiRequests += 1;
          const body = responseFor(url.pathname, r.request().postData());
          if (body === undefined) {
            unmocked.push(`${r.request().method()} ${url.pathname}`);
            return r.fulfill({ status: 404, contentType: "application/json", body: "{}" });
          }
          const text = JSON.stringify(body);
          if (phone) {
            await sleep(responseDelayMs(PHONE_NETWORK, Buffer.byteLength(text)));
          }
          return r.fulfill({ status: 200, contentType: "application/json", body: text });
        });
        await context.routeWebSocket((url) => url.pathname === "/ws", (ws) => ws.onMessage(() => {}));

        const page = await context.newPage();
        page.on("pageerror", (e) => pageErrors.push(e.message));
        const cdp = await context.newCDPSession(page);
        await cdp.send("Emulation.setCPUThrottlingRate", { rate: CPU_THROTTLE });
        if (phone) {
          await cdp.send("Network.enable");
          await cdp.send("Network.emulateNetworkConditions", {
            offline: false, latency: PHONE_NETWORK.rttMs,
            downloadThroughput: PHONE_NETWORK.downBytesPerSec, uploadThroughput: -1,
          });
        }
        if (profiling) {
          await cdp.send("Profiler.enable");
          await cdp.send("Profiler.setSamplingInterval", { interval: 1000 });
          await cdp.send("Profiler.start");
          await cdp.send("Performance.enable");
        }
        await page.goto(`${origin}${route.path}`, { waitUntil: "load", timeout: 120_000 });
        try {
          await expect(page.getByText(route.ready()).first()).toBeVisible({ timeout: 30_000 });
        } catch (error) {
          const shown = (await page.locator("body").innerText().catch(() => "")).slice(0, 300);
          throw new Error(
            `${name}: "${route.ready()}" never showed. unmocked=${unmocked.join(", ")} ` +
              `errors=${pageErrors.join(" | ")} page=${JSON.stringify(shown)}`,
            { cause: error },
          );
        }
        await sleep(SETTLE_MS);
        if (profiling) {
          evidence[name] = await profileEvidence(cdp);
          continue;
        }
        const sample: Sample = {
          ...(await readSample(page)), api_requests: apiRequests, unmocked, page_errors: pageErrors,
        };
        if (phone && documentFasterThanLink(PHONE_NETWORK, sample.document_bytes, sample.document_transfer_ms)) {
          throw new Error(
            `${name}: document (${sample.document_bytes} B) took ${sample.document_transfer_ms} ms, ` +
            "less than the phone bandwidth allows; the network throttle did not apply to it",
          );
        }
        routes[name]!.push(sample);
      } finally {
        await context.close();
      }
    }
  }

  const out = resolve(process.env.PERF_REPORT_DIR ?? "perf-report");
  mkdirSync(out, { recursive: true });
  writeFileSync(
    join(out, `page-load-${profile}.json`),
    JSON.stringify({
      profile, samples: SAMPLES, cpu_throttle: CPU_THROTTLE,
      network: phone ? PHONE_NETWORK : null,
      browser_version: browser.version(), node_version: process.version,
      profiles: { [profile]: routes },
      profile_evidence: evidence,
    }, null, 2),
  );
  const bad = Object.values(routes).flat().filter((s) => s.unmocked.length || s.page_errors.length);
  console.log(`PAGE_LOAD ${profile} ` + JSON.stringify(
    Object.fromEntries(Object.entries(routes).map(([k, v]) => [k, {
      lcp: median(v.map((s) => s.lcp_ms ?? 0)), unmocked: [...new Set(v.flatMap((s) => s.unmocked))],
    }]))));
  expect(bad.map((s) => [...s.unmocked, ...s.page_errors]), "unmocked requests or page errors").toEqual([]);
});
