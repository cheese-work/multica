import "./env";

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

// Browser regression for bare issue keys in a streaming chat reply.
//
// While a reply streams, a key that ends the text may still grow: CHE-1 can
// become CHE-18, CHE-182, CHE-1824. Each prefix here is a real synthetic
// issue, so a renderer that autolinks a partial key shows a wrong chip instead
// of quietly missing. The fixture seeds a running chat task directly and
// appends chunks through the task-message endpoint with the user's own token,
// which runs the real task:message WebSocket path and RichContent's streaming
// phase without any agent. The settled case completes the task through the
// same endpoint the daemon uses.
//
// Opt-in only, against a task-owned environment of this checkout
// (bash scripts/dev-env.sh up --ephemeral):
//   MULTICA_E2E_STREAMING_ISSUE_KEYS=1 pnpm exec playwright test e2e/streaming-issue-keys.spec.ts
// The run refuses before any write unless the dev-env registry proves that
// ownership, and it removes every row it seeds. e2e/streaming-issue-keys.safety.mjs
// tests both.
// Optional:
//   STREAMING_ISSUE_KEYS_LOOKUP_DELAY_MS=<ms>  delay identifier lookups
//   STREAMING_ISSUE_KEYS_ARTIFACTS=<dir>       copy traces and screenshots there

const ENABLED = process.env.MULTICA_E2E_STREAMING_ISSUE_KEYS === "1";
const API_URL = process.env.NEXT_PUBLIC_API_URL ?? `http://localhost:${process.env.PORT ?? "8080"}`;
const LOOKUP_DELAY_MS = Number(process.env.STREAMING_ISSUE_KEYS_LOOKUP_DELAY_MS ?? "0");
const ARTIFACTS_DIR = process.env.STREAMING_ISSUE_KEYS_ARTIFACTS;

const PARTIAL_KEYS = ["CHE-1", "CHE-18", "CHE-182"];
const FULL_KEY = "CHE-1824";
const SHORT_TITLE = "Login fix";
const LONG_TITLE =
  "fix(db): resolve duplicate migration prefix collisions before the backend test gate runs on every pull request";

const VIEWPORTS = [
  { name: "desktop", width: 1280, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;

type ChipFrame = { at: number; chips: string[] };

declare global {
  interface Window {
    __chipFrames?: ChipFrame[];
  }
}

// Records every distinct set of issue chips the document shows, from the first
// mutation on. A chip that renders for one frame and disappears is still kept.
function installChipRecorder() {
  window.__chipFrames = [];
  let last = "";
  const record = () => {
    const chips = Array.from(document.querySelectorAll("a.issue-mention")).map(
      (el) => (el.textContent ?? "").trim(),
    );
    const key = JSON.stringify(chips);
    if (key === last) return;
    last = key;
    window.__chipFrames!.push({ at: performance.now(), chips });
  };
  new MutationObserver(record).observe(document, {
    subtree: true,
    childList: true,
    characterData: true,
  });
}

function chipKeysSeen(frames: ChipFrame[]): Set<string> {
  const seen = new Set<string>();
  for (const frame of frames) {
    for (const chip of frame.chips) {
      const match = /^[A-Z][A-Z0-9]*-\d+/.exec(chip.replace(/\s+/g, ""));
      if (match) seen.add(match[0]);
    }
  }
  return seen;
}

async function saveArtifact(testInfo: TestInfo, name: string, body: string | Buffer) {
  const file = testInfo.outputPath(name);
  fs.writeFileSync(file, body);
  if (ARTIFACTS_DIR) {
    fs.mkdirSync(ARTIFACTS_DIR, { recursive: true });
    fs.copyFileSync(file, path.join(ARTIFACTS_DIR, `${testInfo.title.replace(/\W+/g, "-")}-${name}`));
  }
}

async function measureChips(page: Page, scope: string) {
  return page.evaluate((selector) => {
    return Array.from(document.querySelectorAll(`${selector} a.issue-mention`)).map((el) => {
      const box = el.getBoundingClientRect();
      const paragraph = el.closest("p, li") ?? el.parentElement;
      return {
        text: (el.textContent ?? "").trim(),
        width: Math.round(box.width * 10) / 10,
        height: Math.round(box.height * 10) / 10,
        top: Math.round(box.top * 10) / 10,
        paragraphHeight: paragraph ? Math.round(paragraph.getBoundingClientRect().height * 10) / 10 : null,
      };
    });
  }, scope);
}

const LOCAL_HOSTS = new Set(["localhost", "127.0.0.1", "::1", "[::1]"]);

type DevEnvironment = {
  name: string;
  dir: string;
  owner: string;
  database: string;
  databaseUrl: string;
  backend_port: number;
  ttlHours: number;
  expiresAt: string;
};

// Reads the dev-env registry the way scripts/dev-env.sh loads a manifest
// (sourcing it), but without `dev-env.sh list`, which probes each registered
// API: the ownership check must not touch any service before it decides.
function readDevEnvironments(): DevEnvironment[] {
  const devHome = process.env.MULTICA_DEV_HOME || path.join(os.homedir(), ".multica", "dev");
  const envsDir = path.join(devHome, "envs");
  if (!fs.existsSync(envsDir)) return [];
  const environments: DevEnvironment[] = [];
  for (const name of fs.readdirSync(envsDir)) {
    const manifest = path.join(envsDir, name, "manifest.env");
    if (!fs.existsSync(manifest)) continue;
    const fields = execFileSync(
      "bash",
      [
        "-c",
        'set -e; . "$1"; printf "%s\\0" "$NAME" "$DIR" "$OWNER" "$DB_NAME" "$DATABASE_URL" "$BACKEND_PORT" "${TTL_HOURS:-0}" "${EXPIRES_AT:-}"',
        "_",
        manifest,
      ],
      { encoding: "utf8" },
    ).split("\0");
    const [envName, dir, owner, database, databaseUrl, backendPort, ttlHours, expiresAt] = fields;
    if (envName !== name) continue;
    environments.push({
      name: envName,
      dir: dir!,
      owner: owner!,
      database: database!,
      databaseUrl: databaseUrl!,
      backend_port: Number(backendPort),
      ttlHours: Number(ttlHours),
      expiresAt: expiresAt!,
    });
  }
  return environments;
}

// One spelling for the loopback address, so `localhost` and `127.0.0.1` name the
// same endpoint and any other host stays distinct.
function endpointHost(hostname: string) {
  return LOCAL_HOSTS.has(hostname) ? "loopback" : hostname;
}

// Query parameters pg may read from the URL without changing which server,
// user or database it connects to.
const NEUTRAL_QUERY_PARAMS = new Set(["sslmode"]);

// The identity of a PostgreSQL endpoint and database: host, port, user, name.
// It must be the identity pg actually connects with, so every part has to come
// from the URL authority: pg lets query parameters (host, port, user, ...)
// override the authority, and fills a missing port or user from PGPORT/PGUSER.
function databaseIdentity(raw: string) {
  const url = new URL(raw);
  for (const key of url.searchParams.keys()) {
    if (!NEUTRAL_QUERY_PARAMS.has(key)) throw new Error(`query parameter ${key} can change the connection`);
  }
  const user = decodeURIComponent(url.username);
  const database = decodeURIComponent(url.pathname.slice(1));
  if (!url.hostname || !url.port || !user || !database) {
    throw new Error("host, port, user and database must all be explicit");
  }
  return [endpointHost(url.hostname), url.port, user, database].join(" ");
}

// Proves, before any API client or database connection exists, that
// DATABASE_URL and the API are the services of a disposable environment of
// this checkout: an agent-owned, ephemeral (TTL, not expired) dev-env
// registration with the same database endpoint, user and name, and a local API
// on its backend port. A blacklist of database names cannot prove that; the
// dev-env registry can.
function requireTaskOwnedEnvironment(repoRoot: string): DevEnvironment {
  const refuse = (reason: string): never => {
    throw new Error(
      `Streaming issue-key E2E needs a task-owned environment of this checkout (bash scripts/dev-env.sh up --ephemeral): ${reason}`,
    );
  };
  const raw = process.env.DATABASE_URL;
  if (!raw) return refuse("DATABASE_URL is not set");
  let database: string;
  try {
    const url = new URL(raw);
    if (!LOCAL_HOSTS.has(url.hostname)) return refuse(`database host ${url.hostname} is not local`);
    database = databaseIdentity(raw);
  } catch (error) {
    return refuse(`DATABASE_URL cannot be verified: ${error instanceof Error ? error.message : String(error)}`);
  }
  let api: URL;
  try {
    api = new URL(API_URL);
  } catch {
    return refuse(`API URL ${API_URL} is not a URL`);
  }
  if (!LOCAL_HOSTS.has(api.hostname)) return refuse(`API host ${api.hostname} is not local`);
  const apiPort = api.port || (api.protocol === "https:" ? "443" : "80");

  let environments: DevEnvironment[];
  try {
    environments = readDevEnvironments();
  } catch (error) {
    return refuse(`the dev-env registry could not be read: ${String(error)}`);
  }
  const root = fs.realpathSync(repoRoot);
  const mine = environments.filter((env) => {
    try {
      return fs.realpathSync(env.dir) === root;
    } catch {
      return false;
    }
  });
  if (mine.length === 0) return refuse(`no environment is registered for ${root}`);
  const now = Date.now();
  const problems = (env: DevEnvironment) => {
    const found: string[] = [];
    if (env.owner !== "agent") found.push(`owner ${env.owner}`);
    if (!(env.ttlHours > 0) || !env.expiresAt) found.push("not ephemeral");
    else if (!(Date.parse(env.expiresAt) > now)) found.push(`expired ${env.expiresAt}`);
    let registered = "";
    try {
      registered = databaseIdentity(env.databaseUrl);
    } catch {
      found.push("registered DATABASE_URL unreadable");
    }
    if (registered && registered !== database) found.push(`database ${registered}`);
    if (String(env.backend_port) !== apiPort) found.push(`api :${env.backend_port}`);
    return found;
  };
  const match = mine.find((env) => problems(env).length === 0);
  if (!match) {
    const registered = mine.map((env) => `${env.name} (${problems(env).join(", ")})`);
    return refuse(`requested database ${database}, api :${apiPort}; registered ${registered.join("; ")}`);
  }
  return match;
}

// The column that points at a row of each table this fixture writes directly.
// Rows other code writes in reaction (task messages, the completed answer,
// comments, activity) are found through these columns.
const REFERENCE_COLUMN = {
  comment: "comment_id",
  agent_task_queue: "task_id",
  chat_session: "chat_session_id",
  issue: "issue_id",
  agent: "agent_id",
  agent_runtime: "runtime_id",
} as const;
type SeededTable = keyof typeof REFERENCE_COLUMN;
// Object order is the delete order: a row's dependents go before the row.
const DELETE_ORDER = Object.keys(REFERENCE_COLUMN) as SeededTable[];

// Records every row the fixture seeds and the workspace fields it changes, so
// a run can remove them and prove it did, after success or failure alike.
class SeedLedger {
  private readonly rows = new Map<SeededTable, string[]>();
  private workspace?: { id: string; issuePrefix: string; issueCounter: number };

  constructor(private readonly db: pg.Client) {}

  async rememberWorkspace(id: string) {
    const { rows } = await this.db.query<{ issue_prefix: string; issue_counter: string }>(
      `SELECT issue_prefix, issue_counter::text FROM workspace WHERE id = $1`,
      [id],
    );
    if (!rows[0]) throw new Error(`workspace ${id} not found`);
    this.workspace = { id, issuePrefix: rows[0].issue_prefix, issueCounter: Number(rows[0].issue_counter) };
  }

  track(table: SeededTable, id: string | undefined) {
    if (id) this.rows.set(table, [...(this.rows.get(table) ?? []), id]);
    return id;
  }

  private async references() {
    const { rows } = await this.db.query<{ table_name: string; column_name: string }>(
      `SELECT table_name, column_name FROM information_schema.columns
       WHERE table_schema = 'public' AND data_type = 'uuid' AND column_name = ANY($1::text[])`,
      [Object.values(REFERENCE_COLUMN)],
    );
    return rows;
  }

  private async count(sql: string, ids: string[]) {
    const { rows } = await this.db.query<{ count: string }>(sql, [ids]);
    return Number(rows[0]!.count);
  }

  async cleanup() {
    const references = await this.references();
    for (const table of DELETE_ORDER) {
      const ids = this.rows.get(table) ?? [];
      if (ids.length === 0) continue;
      for (const ref of references.filter((r) => r.column_name === REFERENCE_COLUMN[table])) {
        await this.db.query(`DELETE FROM "${ref.table_name}" WHERE "${ref.column_name}" = ANY($1::uuid[])`, [ids]);
      }
      await this.db.query(`DELETE FROM "${table}" WHERE id = ANY($1::uuid[])`, [ids]);
    }
    if (this.workspace) {
      await this.db.query(`UPDATE workspace SET issue_prefix = $2, issue_counter = $3 WHERE id = $1`, [
        this.workspace.id,
        this.workspace.issuePrefix,
        this.workspace.issueCounter,
      ]);
    }

    const left: string[] = [];
    for (const [table, ids] of this.rows) {
      const rows = await this.count(`SELECT count(*) FROM "${table}" WHERE id = ANY($1::uuid[])`, ids);
      if (rows > 0) left.push(`${table}: ${rows}`);
      for (const ref of references.filter((r) => r.column_name === REFERENCE_COLUMN[table])) {
        const dependents = await this.count(
          `SELECT count(*) FROM "${ref.table_name}" WHERE "${ref.column_name}" = ANY($1::uuid[])`,
          ids,
        );
        if (dependents > 0) left.push(`${ref.table_name}.${ref.column_name}: ${dependents}`);
      }
    }
    if (this.workspace) {
      const { rows } = await this.db.query<{ issue_prefix: string; issue_counter: string }>(
        `SELECT issue_prefix, issue_counter::text FROM workspace WHERE id = $1`,
        [this.workspace.id],
      );
      if (rows[0]?.issue_prefix !== this.workspace.issuePrefix || Number(rows[0]?.issue_counter) !== this.workspace.issueCounter) {
        left.push("workspace issue_prefix/issue_counter not restored");
      }
    }
    if (left.length > 0) throw new Error(`Streaming fixture cleanup left rows behind: ${left.join(", ")}`);
  }
}

test.describe("streaming bare issue keys", () => {
  test.skip(!ENABLED, "Set MULTICA_E2E_STREAMING_ISSUE_KEYS=1 against a task-owned environment of this checkout");

  let environment: DevEnvironment | undefined;
  test.beforeAll(() => {
    environment = requireTaskOwnedEnvironment(path.resolve(test.info().project.testDir, ".."));
  });

  // Each phase is its own test with its own setup and cleanup, so a run can
  // select the streaming phases alone with `--grep @streaming` and leave the
  // issue-page capture out. The issue-page phase renders an issue comment
  // through RichContent; it is not a saved (Tiptap) issue description.
  const PHASES = [
    { phase: "boundary", title: "streaming boundary links the key only after a boundary", tag: " @streaming" },
    { phase: "settle", title: "streaming settle links the key only once settled", tag: " @streaming" },
    { phase: "issue-page", title: "issue page comment shows resolved chips", tag: "" },
  ] as const;

  for (const viewport of VIEWPORTS) {
    for (const { phase, title, tag } of PHASES) {
      test(`${title} (${viewport.name})${tag}`, async ({ page }, testInfo) => {
        // A dev server compiles each route on first visit.
        test.setTimeout(240_000);
          if (!environment) throw new Error("ownership check did not run");

        // Test hook for e2e/streaming-issue-keys.safety.mjs: fail on purpose at
        // one stage of setup, with a marker the safety test looks for.
        const failAt = (point: string) => {
          if (process.env.STREAMING_ISSUE_KEYS_FAIL_AT === point) throw new Error(`injected failure at ${point}`);
        };

        // Everything acquired below is released on success, failure and partial
        // setup alike. The helper's E2E user and workspace are not removed here:
        // they live only in the task-owned database, which dev-env destroy drops.
        let api: Awaited<ReturnType<typeof createTestApi>> | undefined;
        let db: pg.Client | undefined;
        let connected = false;
        let ledger: SeedLedger | undefined;
        const lookups: string[] = [];
        let runtimeId: string | undefined;
        let failure: unknown;
        try {
          api = await createTestApi();
          failAt("after-api-client");
          db = new pg.Client(process.env.DATABASE_URL);
          await db.connect();
          connected = true;
          ledger = new SeedLedger(db);
          failAt("after-db-connect");
          const client = db;
          const seeds = ledger;
          const apiClient = api;
          const workspace = (await apiClient.getWorkspaces())[0];
          if (!workspace) throw new Error("E2E workspace missing");
          apiClient.setWorkspaceId(workspace.id);
          apiClient.setWorkspaceSlug(workspace.slug);
          const token = apiClient.getToken();
          if (!token) throw new Error("E2E token missing");
          const userId = (
            await client.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [apiClient.getEmail()])
          ).rows[0]!.id;

          // The workspace belongs to this run only (both viewports share it), so its
          // prefix and counter can be set to make CHE-1 … CHE-1824 real issues.
          await seeds.rememberWorkspace(workspace.id);
          await client.query(`UPDATE workspace SET issue_prefix = 'CHE', issue_counter = GREATEST(issue_counter, 1824) WHERE id = $1`, [workspace.id]);
          const titles: Record<number, string> = { 1: SHORT_TITLE, 18: "Partial key eighteen", 182: "Partial key one eight two", 1824: LONG_TITLE };
          for (const [number, title] of Object.entries(titles)) {
            const inserted = await client.query<{ id: string }>(
              `INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number)
               VALUES ($1, $2, 'todo', 'none', 'member', $3, $4::double precision, $5::integer)
               ON CONFLICT (workspace_id, number) DO NOTHING RETURNING id::text`,
              [workspace.id, title, userId, Number(number), Number(number)],
            );
            seeds.track("issue", inserted.rows[0]?.id);
          }
          failAt("mid-seed");

          runtimeId = seeds.track("agent_runtime", (
            await client.query<{ id: string }>(
              `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
               VALUES ($1, $2, 'cloud', 'e2e_streaming_keys', 'online', 'Streaming keys E2E', '{}'::jsonb, now()) RETURNING id::text`,
              [workspace.id, `Streaming keys E2E ${viewport.name} ${phase}`],
            )
          ).rows[0]!.id)!;
          const agentId = seeds.track("agent", (
            await client.query<{ id: string }>(
              `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
               VALUES ($1, $4, '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
              [workspace.id, runtimeId, userId, `Stream Fixture ${viewport.name} ${phase}`],
            )
          ).rows[0]!.id)!;

          async function seedRunningTurn(title: string, prompt: string, firstChunk: string) {
            const sessionId = seeds.track("chat_session", (
              await client.query<{ id: string }>(
                `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
                 VALUES ($1, $2, $3, $4, 'active', now()) RETURNING id::text`,
                [workspace!.id, agentId, userId, title],
              )
            ).rows[0]!.id)!;
            const taskId = seeds.track("agent_task_queue", (
              await client.query<{ id: string }>(
                `INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, dispatched_at, started_at)
                 VALUES ($1, $2, $3, 'running', now(), now()) RETURNING id::text`,
                [agentId, runtimeId, sessionId],
              )
            ).rows[0]!.id)!;
            await client.query(
              `INSERT INTO chat_message (chat_session_id, role, content, task_id) VALUES ($1, 'user', $2, $3)`,
              [sessionId, prompt, taskId],
            );
            await client.query(`INSERT INTO task_message (task_id, seq, type, content) VALUES ($1, 1, 'text', $2)`, [taskId, firstChunk]);
            return { sessionId, taskId, seq: 1 };
          }

          async function appendChunk(turn: { taskId: string; seq: number }, content: string) {
            turn.seq += 1;
            const response = await page.request.post(`${API_URL}/api/daemon/tasks/${turn.taskId}/messages`, {
              headers: { Authorization: `Bearer ${token}` },
              data: { messages: [{ seq: turn.seq, type: "text", content }] },
            });
            expect(response.status(), await response.text()).toBe(200);
          }

          async function waitForText(text: string, timeout = 20_000) {
            await page.waitForFunction((expected) => document.body.innerText.replace(/\s+/g, " ").includes(expected), text, {
              timeout,
            });
            // Give lookups and chip renders time to land before the next chunk.
            await page.waitForTimeout(600 + LOOKUP_DELAY_MS);
          }

          await page.setViewportSize({ width: viewport.width, height: viewport.height });
          await page.addInitScript((authToken) => {
            localStorage.setItem("multica_token", authToken);
          }, token);
          await page.addInitScript(installChipRecorder);
          page.on("request", (request) => {
            const match = /\/api\/issues\/([A-Z][A-Z0-9]*-\d+)(?:[/?]|$)/.exec(new URL(request.url()).pathname);
            if (match) lookups.push(match[1]!);
          });
          if (LOOKUP_DELAY_MS > 0) {
            await page.route(/\/api\/issues\/[A-Z][A-Z0-9]*-\d+$/, async (route) => {
              await new Promise((resolve) => setTimeout(resolve, LOOKUP_DELAY_MS));
              await route.continue();
            });
          }

          if (phase === "boundary") {
            // 1. Streaming, then a boundary: the full key links only once it is
            //    followed by a boundary character.
            const boundary = await seedRunningTurn("Streaming keys boundary", "Which change fixed the login?", "Fixed in ");
            failAt("after-seed");
            await page.goto(`/${workspace.slug}/chat?session=${boundary.sessionId}`, { waitUntil: "domcontentloaded" });
            await waitForText("Fixed in", 90_000);
            let streamed = "Fixed in ";
            for (const chunk of ["CHE-1", "8", "2", "4"]) {
              await appendChunk(boundary, chunk);
              streamed += chunk;
              await waitForText(streamed.trim());
              await page.screenshot({ path: testInfo.outputPath(`stream-${streamed.trim().split(" ").pop()}.png`) });
            }
            await appendChunk(boundary, " and its follow-up.");
            await expect(page.locator("a.issue-mention", { hasText: FULL_KEY })).toBeVisible({ timeout: 20_000 });
            await page.waitForTimeout(600 + LOOKUP_DELAY_MS);
            await page.screenshot({ path: testInfo.outputPath("stream-boundary.png") });
            const boundaryFrames = await page.evaluate(() => window.__chipFrames ?? []);
            const boundaryMeasure = await measureChips(page, "body");
            const chatSeen = chipKeysSeen(boundaryFrames);
            await saveArtifact(testInfo, "trace.json", JSON.stringify({ viewport, phase, lookupDelayMs: LOOKUP_DELAY_MS, frames: boundaryFrames, chips: boundaryMeasure, lookups }, null, 2));
            // The chat page never showed a chip for a partial key and never looked
            // one up; the full key links once a boundary follows it.
            for (const key of PARTIAL_KEYS) {
              expect(chatSeen.has(key), `chip for ${key} while streaming`).toBe(false);
              expect(lookups, `lookup of ${key} while streaming`).not.toContain(key);
            }
            expect(chatSeen.has(FULL_KEY)).toBe(true);
          } else if (phase === "settle") {
            // 2. Streaming that ends on the key with no delimiter, then settling: no
            //    chip while it streams, the full chip once the task completes.
            const settle = await seedRunningTurn("Streaming keys settle", "Where is it tracked?", "Tracked as ");
            await page.goto(`/${workspace.slug}/chat?session=${settle.sessionId}`, { waitUntil: "domcontentloaded" });
            await waitForText("Tracked as", 90_000);
            let streamed = "Tracked as ";
            for (const chunk of ["CHE-1", "8", "2", "4"]) {
              await appendChunk(settle, chunk);
              streamed += chunk;
              await waitForText(streamed.trim());
            }
            await page.screenshot({ path: testInfo.outputPath("settle-before.png") });
            const streamingOnlyFrames = await page.evaluate(() => window.__chipFrames ?? []);
            const completed = await page.request.post(`${API_URL}/api/daemon/tasks/${settle.taskId}/complete`, {
              headers: { Authorization: `Bearer ${token}` },
              data: { output: "Tracked as CHE-1824" },
            });
            expect(completed.status(), await completed.text()).toBe(200);
            await expect(page.locator("a.issue-mention", { hasText: FULL_KEY })).toBeVisible({ timeout: 20_000 });
            await page.waitForTimeout(600 + LOOKUP_DELAY_MS);
            await page.screenshot({ path: testInfo.outputPath("settle-after.png") });
            const settleFrames = await page.evaluate(() => window.__chipFrames ?? []);
            const settleMeasure = await measureChips(page, "body");
            const chatSeen = chipKeysSeen(settleFrames);
            await saveArtifact(testInfo, "trace.json", JSON.stringify({ viewport, phase, lookupDelayMs: LOOKUP_DELAY_MS, streamingFrames: streamingOnlyFrames, frames: settleFrames, chips: settleMeasure, lookups }, null, 2));
            // No chip while the reply still ended on the key, no partial-key chip or
            // lookup at any point, and the full key links once the task settles.
            expect(chipKeysSeen(streamingOnlyFrames).size, "chip while the reply still ended on the key").toBe(0);
            for (const key of PARTIAL_KEYS) {
              expect(chatSeen.has(key), `chip for ${key} while streaming`).toBe(false);
              expect(lookups, `lookup of ${key} while streaming`).not.toContain(key);
            }
            expect(chatSeen.has(FULL_KEY)).toBe(true);
          } else {
            // 3. Settled description-size text: an issue comment rendered through
            //    the same RichContent, with a short and a long title.
            const host = await apiClient.createIssue("Streaming keys description host", {
              description: `Related: CHE-1, CHE-18, CHE-182 and ${FULL_KEY}.`,
            });
            seeds.track("issue", host.id);
            const comment = await client.query<{ id: string }>(
              `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
               VALUES ($1, $2, 'member', $3, $4) RETURNING id::text`,
              [workspace.id, host.id, userId, `Short CHE-1 and long ${FULL_KEY} in a sentence that wraps on narrow screens.`],
            );
            seeds.track("comment", comment.rows[0]?.id);
            await page.goto(`/${workspace.slug}/issues/${host.id}`, { waitUntil: "domcontentloaded" });
            await expect(page.locator("a.issue-mention", { hasText: FULL_KEY }).first()).toBeVisible({ timeout: 20_000 });
            await page.waitForTimeout(600 + LOOKUP_DELAY_MS);
            await page.screenshot({ path: testInfo.outputPath("description.png"), fullPage: true });
            const descriptionMeasure = await measureChips(page, "body");
            await saveArtifact(testInfo, "trace.json", JSON.stringify({ viewport, phase, lookupDelayMs: LOOKUP_DELAY_MS, chips: descriptionMeasure, lookups }, null, 2));
          }
          for (const shot of fs.readdirSync(testInfo.outputDir).filter((name) => name.endsWith(".png"))) {
            await saveArtifact(testInfo, shot, fs.readFileSync(testInfo.outputPath(shot)));
          }
        } catch (error) {
          failure = error;
        }
        // Cleanup runs after a failure too, and its own failure is never hidden.
        const cleanupErrors: unknown[] = [];
        const release = async (step: () => Promise<unknown>) => {
          try {
            await step();
          } catch (error) {
            cleanupErrors.push(error);
          }
        };
        if (ledger) await release(() => ledger!.cleanup());
        if (db && connected) await release(() => db!.end());
        if (api) await release(() => api!.cleanup());
        if (failure !== undefined && cleanupErrors.length > 0) {
          throw new AggregateError([failure, ...cleanupErrors], "fixture and its cleanup both failed");
        }
        if (failure !== undefined) throw failure;
        if (cleanupErrors.length > 0) throw new AggregateError(cleanupErrors, "fixture cleanup failed");
      });
    }
  }
});
