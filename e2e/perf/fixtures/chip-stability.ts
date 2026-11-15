/**
 * Synthetic boundary shared by the chip-stability specs (CHE-2970 / A1).
 *
 * Everything the page may request is answered here; anything else is recorded
 * as unmocked so a business call can never be quietly absorbed by a catch-all.
 * Nothing in this file is a captured transcript or real workspace data.
 */
import type { BrowserContext, CDPSession, Page } from "@playwright/test";

export const WORKSPACE_SLUG = "perf";
export const WORKSPACE_ID = "11111111-1111-4111-8111-111111111111";
export const USER_ID = "22222222-2222-4222-8222-222222222222";
export const AGENT_ID = "33333333-3333-4333-8333-333333333333";
export const SESSION_ID = "44444444-4444-4444-8444-444444444444";
export const TASK_ID = "77777777-7777-4777-8777-777777777777";
export const THREAD_ISSUE_ID = "55555555-5555-4555-8555-555555555555";
export const THREAD_ISSUE_IDENTIFIER = "PERF-1";
const TIMESTAMP = "2026-10-20T00:00:00Z";

/** 4x, per the measurement profiles in the plan. */
export const CPU_THROTTLE_RATE = 4;

/** Issue-key lookups (`GET /api/issues/PERF-n`) and chip details answer this late. */
export const LOOKUP_DELAY_MS = 400;

export const workspace = {
  id: WORKSPACE_ID, name: "Perf", slug: WORKSPACE_SLUG, description: null, context: null,
  settings: { github_enabled: false }, repos: [], issue_prefix: "PERF", avatar_url: null,
  created_at: TIMESTAMP, updated_at: TIMESTAMP,
};

const me = {
  id: USER_ID, name: "Perf User", email: "perf@example.test", avatar_url: null,
  language: "en", onboarded_at: TIMESTAMP, onboarding_questionnaire: { source: "other" },
  created_at: TIMESTAMP, updated_at: TIMESTAMP,
};

export const agent = {
  id: AGENT_ID, workspace_id: WORKSPACE_ID, runtime_id: "66666666-6666-4666-8666-666666666666",
  name: "Fixture Agent", description: "", instructions: "", avatar_url: null,
  runtime_mode: "local", runtime_config: {}, custom_args: [], visibility: "workspace",
  permission_mode: "public_to", invocation_targets: [{ target_type: "workspace", target_id: null }],
  status: "idle", max_concurrent_tasks: 1, model: "", owner_id: USER_ID, skills: [],
  runtime_availability: "online", created_at: TIMESTAMP, updated_at: TIMESTAMP,
  archived_at: null, archived_by: null,
};

/** A key such as PERF-101 maps to a stable issue id; other shapes are not keys. */
export const keyNumber = (key: string) => /^PERF-(\d+)$/.exec(key)?.[1];
export const issueIdForKey = (n: string) => `99999999-9999-4999-8999-${n.padStart(12, "0")}`;
const keyForIssueId = (id: string) => {
  const m = /^99999999-9999-4999-8999-0*(\d+)$/.exec(id);
  return m ? `PERF-${m[1]}` : undefined;
};

export function issueFor(key: string, id = issueIdForKey(keyNumber(key) ?? "0")) {
  return {
    id, workspace_id: WORKSPACE_ID, number: Number(keyNumber(key)), identifier: key,
    title: `Fixture issue ${key} with a title long enough to fill a chip`,
    description: null, status: "in_progress", status_category: "in_progress",
    status_name: "In Progress", priority: "none", assignee_type: null, assignee_id: null,
    creator_type: "user", creator_id: USER_ID, parent_issue_id: null, project_id: null,
    position: 0, start_date: null, due_date: null, stage: null, metadata: {}, properties: {},
    labels: [], reactions: [], created_at: TIMESTAMP, updated_at: TIMESTAMP,
  };
}

/** Requests that are the same for every scenario. */
function commonResponse(pathname: string): unknown | undefined {
  switch (pathname) {
    case "/api/config": return { feature_flags: {}, allow_signup: true };
    case "/api/me": return me;
    case "/api/workspaces": return [workspace];
    case "/api/client-usage": return {};
    case "/api/agents": return [agent];
    case "/api/runtimes": case "/api/squads": case "/api/agent-task-snapshot":
    case "/api/invitations": case "/api/inbox": case "/api/inbox/unread-summary":
    case "/api/pins": case "/api/assignee-frequency": case "/api/issue-views":
    case "/api/issue-wakeup-summaries": case "/api/agent-run-counts":
    case "/api/agent-activity-30d": case "/api/chat/pending-tasks": case "/api/chat/sessions":
    case "/api/skills": case "/api/autopilots":
      return [];
    case "/api/labels": return { labels: [] };
    case "/api/decisions/open": return [];
    case "/api/chat/pending-tasks/has-any": return { has_pending: false };
    case `/api/workspaces/${WORKSPACE_ID}/members`: return [];
    case `/api/chat/sessions/${SESSION_ID}/wakeups`: return [];
    case `/api/agents/${AGENT_ID}/presets/completions`: return [];
    case "/api/projects": return { projects: [], total: 0 };
    case "/api/issues": return { issues: [], total: 0 };
    case "/api/issue-statuses": return { statuses: [] };
    case "/api/properties": return { properties: [], total: 0 };
    case "/api/quick-actions": return { quick_actions: [] };
    case "/api/issue-workflows": return { workflows: [] };
    case "/api/issue-view-preferences": return {
      scope_type: "workspace", scope_id: null, ordered_view_ids: [], hidden_view_ids: [],
      default_view_id: null,
    };
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
    default: return undefined;
  }
}

/** Requests of the issue-detail page for `issue`, with `timeline` as its comments. */
export function issuePageRoute(pathname: string, issue: { id: string; identifier: string }, timeline: unknown[]) {
  const base = `/api/issues/${issue.id}`;
  switch (pathname) {
    case `/api/issues/${issue.identifier}`:
    case base: return issue;
    case `${base}/timeline`: return timeline;
    case `${base}/task-runs`: case `${base}/subscribers`: case `${base}/wakeups`:
    case `${base}/attachments`: case `${base}/system-wakeups`: return [];
    case `${base}/comments/trigger-preview`: return { agents: [], blocked: [] };
    case `${base}/labels`: return { labels: [] };
    case `${base}/children`: return { issues: [] };
    case `${base}/pull-requests`: return { pull_requests: [] };
    case `${base}/duplicates`: return { duplicate_of: null, duplicates: [] };
    // Side panels that do not touch chips; an empty object lets their parsers fall back.
    case `${base}/relations`: case "/api/inbox/archived/page": return {};
    case "/api/issues/child-progress": return { progress: [] };
    default: return undefined;
  }
}

export interface Harness {
  /** `GET /api/issues/PERF-n` counts, keyed by the key. */
  keyLookups: Map<string, number>;
  unmocked: string[];
  external: string[];
  pageErrors: string[];
  /** Hold every key lookup and chip detail until released. */
  gate: { release: () => void };
}

/**
 * Installs routing, layout-shift and rAF observers, and CPU throttling.
 * `scenario` answers the paths specific to the page under test.
 */
export async function installHarness(
  page: Page,
  context: BrowserContext,
  origin: string,
  scenario: (pathname: string) => unknown | undefined,
): Promise<Harness & { cdp: CDPSession }> {
  const h: Harness = {
    keyLookups: new Map(), unmocked: [], external: [], pageErrors: [],
    gate: { release: () => {} },
  };
  const released = new Promise<void>((done) => { h.gate.release = done; });
  page.on("pageerror", (error) => h.pageErrors.push(error.message));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "no-preference" });
  await context.addCookies([
    { name: "multica_logged_in", value: "1", url: origin },
    { name: "last_workspace_slug", value: WORKSPACE_SLUG, url: origin },
  ]);
  // Every shift is kept with its time and input flag, so standard CLS (session
  // windows, input-driven shifts dropped) and the raw sum both come from one list.
  await context.addInitScript(() => {
    const w = window as unknown as {
      __shifts: ShiftEntry[];
      __resetShift: () => void;
    };
    w.__shifts = [];
    const observer = new PerformanceObserver((list) => {
      for (const e of list.getEntries() as unknown as {
        value: number; startTime: number; hadRecentInput: boolean;
      }[]) {
        w.__shifts.push({ value: e.value, startTime: e.startTime, hadRecentInput: e.hadRecentInput });
      }
    });
    observer.observe({ type: "layout-shift", buffered: true });
    w.__resetShift = () => {
      observer.takeRecords();
      w.__shifts = [];
    };
  });

  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) { h.external.push(url.href); return route.abort(); }
    if (!url.pathname.startsWith("/api/")) return route.continue();
    // The scenario's own routes win, so the page's issue is never taken for a chip key.
    const own = scenario(url.pathname);
    if (own !== undefined) return route.fulfill(json(own));
    const keyed = /^\/api\/issues\/(PERF-\d+)$/.exec(url.pathname);
    if (keyed) {
      h.keyLookups.set(keyed[1]!, (h.keyLookups.get(keyed[1]!) ?? 0) + 1);
      await released;
      await new Promise((done) => setTimeout(done, LOOKUP_DELAY_MS));
      return route.fulfill(json(issueFor(keyed[1]!)));
    }
    const detail = /^\/api\/issues\/([0-9a-f-]{36})$/.exec(url.pathname);
    const detailKey = detail && keyForIssueId(detail[1]!);
    if (detailKey) {
      await new Promise((done) => setTimeout(done, LOOKUP_DELAY_MS));
      return route.fulfill(json(issueFor(detailKey, detail![1]!)));
    }
    const body = commonResponse(url.pathname);
    if (body === undefined) {
      h.unmocked.push(`${route.request().method()} ${url.pathname}`);
      return route.fulfill({ status: 404, contentType: "application/json", body: "{}" });
    }
    return route.fulfill(json(body));
  });

  const cdp = await context.newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: CPU_THROTTLE_RATE });
  return { ...h, cdp };
}

const json = (body: unknown) => ({
  status: 200, contentType: "application/json", body: JSON.stringify(body),
});

export interface ShiftEntry { value: number; startTime: number; hadRecentInput: boolean }

/**
 * Standard CLS: the largest session-window sum, where a window ends after a 1 s
 * gap or 5 s of length, and shifts within 500 ms of input are not counted.
 */
export function sessionCls(entries: ShiftEntry[]): number {
  let best = 0;
  let sum = 0;
  let windowStart = 0;
  let last = 0;
  for (const e of entries) {
    if (e.hadRecentInput) continue;
    if (sum > 0 && (e.startTime - last >= 1000 || e.startTime - windowStart >= 5000)) sum = 0;
    if (sum === 0) windowStart = e.startTime;
    sum += e.value;
    last = e.startTime;
    best = Math.max(best, sum);
  }
  return best;
}

const round4 = (n: number) => Math.round(n * 10000) / 10000;

/** `cls` is standard CLS; `raw` is the sum of every layout-shift entry. */
export async function readShift(page: Page) {
  const entries = await page.evaluate(() => (window as unknown as { __shifts: ShiftEntry[] }).__shifts);
  return { cls: round4(sessionCls(entries)), raw: round4(entries.reduce((n, e) => n + e.value, 0)) };
}

export const resetShift = (page: Page) =>
  page.evaluate(() => (window as unknown as { __resetShift: () => void }).__resetShift());

/** Layout-shift thresholds frozen per profile; see the spec for how they were set. */
export type Profile = "desktop" | "phone";
export const profileOf = (name: string): Profile => (name === "phone" ? "phone" : "desktop");
