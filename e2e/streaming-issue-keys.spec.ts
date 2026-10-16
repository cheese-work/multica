import "./env";

import fs from "node:fs";
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
// Opt-in only, against an explicitly selected isolated environment:
//   MULTICA_E2E_STREAMING_ISSUE_KEYS=1 pnpm exec playwright test e2e/streaming-issue-keys.spec.ts
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

for (const viewport of VIEWPORTS) {
  test(`streamed bare issue keys never chip a partial key (${viewport.name})`, async ({ page }, testInfo) => {
    test.skip(!ENABLED, "Set MULTICA_E2E_STREAMING_ISSUE_KEYS=1 against an isolated environment");
    const databaseUrl = process.env.DATABASE_URL;
    if (!databaseUrl || new URL(databaseUrl).pathname === "/multica") {
      throw new Error("Streaming issue-key E2E requires an isolated, non-default database");
    }

    const api = await createTestApi();
    const db = new pg.Client(databaseUrl);
    await db.connect();
    const lookups: string[] = [];
    let runtimeId: string | undefined;
    try {
      const workspace = (await api.getWorkspaces())[0];
      if (!workspace) throw new Error("E2E workspace missing");
      api.setWorkspaceId(workspace.id);
      api.setWorkspaceSlug(workspace.slug);
      const token = api.getToken();
      if (!token) throw new Error("E2E token missing");
      const userId = (
        await db.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()])
      ).rows[0]!.id;

      // The workspace belongs to this run only, so its prefix and counter can
      // be set to make CHE-1 … CHE-1824 real issues.
      await db.query(`UPDATE workspace SET issue_prefix = 'CHE', issue_counter = GREATEST(issue_counter, 1824) WHERE id = $1`, [workspace.id]);
      const titles: Record<number, string> = { 1: SHORT_TITLE, 18: "Partial key eighteen", 182: "Partial key one eight two", 1824: LONG_TITLE };
      for (const [number, title] of Object.entries(titles)) {
        await db.query(
          `INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number)
           VALUES ($1, $2, 'todo', 'none', 'member', $3, $4, $4)`,
          [workspace.id, title, userId, Number(number)],
        );
      }

      runtimeId = (
        await db.query<{ id: string }>(
          `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
           VALUES ($1, 'Streaming keys E2E', 'cloud', 'e2e_streaming_keys', 'online', 'Streaming keys E2E', '{}'::jsonb, now()) RETURNING id::text`,
          [workspace.id],
        )
      ).rows[0]!.id;
      const agentId = (
        await db.query<{ id: string }>(
          `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
           VALUES ($1, 'Stream Fixture', '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
          [workspace.id, runtimeId, userId],
        )
      ).rows[0]!.id;

      async function seedRunningTurn(title: string, prompt: string, firstChunk: string) {
        const sessionId = (
          await db.query<{ id: string }>(
            `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
             VALUES ($1, $2, $3, $4, 'active', now()) RETURNING id::text`,
            [workspace!.id, agentId, userId, title],
          )
        ).rows[0]!.id;
        const taskId = (
          await db.query<{ id: string }>(
            `INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, dispatched_at, started_at)
             VALUES ($1, $2, $3, 'running', now(), now()) RETURNING id::text`,
            [agentId, runtimeId, sessionId],
          )
        ).rows[0]!.id;
        await db.query(
          `INSERT INTO chat_message (chat_session_id, role, content, task_id) VALUES ($1, 'user', $2, $3)`,
          [sessionId, prompt, taskId],
        );
        await db.query(`INSERT INTO task_message (task_id, seq, type, content) VALUES ($1, 1, 'text', $2)`, [taskId, firstChunk]);
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

      async function waitForText(text: string) {
        await page.waitForFunction((expected) => document.body.innerText.replace(/\s+/g, " ").includes(expected), text, {
          timeout: 20_000,
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

      // 1. Streaming, then a boundary: the full key links only once it is
      //    followed by a boundary character.
      const boundary = await seedRunningTurn("Streaming keys boundary", "Which change fixed the login?", "Fixed in ");
      await page.goto(`/${workspace.slug}/chat?session=${boundary.sessionId}`, { waitUntil: "domcontentloaded" });
      await waitForText("Fixed in");
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

      // 2. Streaming that ends on the key with no delimiter, then settling: no
      //    chip while it streams, the full chip once the task completes.
      const settle = await seedRunningTurn("Streaming keys settle", "Where is it tracked?", "Tracked as ");
      await page.goto(`/${workspace.slug}/chat?session=${settle.sessionId}`, { waitUntil: "domcontentloaded" });
      await waitForText("Tracked as");
      streamed = "Tracked as ";
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

      // 3. Settled description-size text: an issue comment rendered through
      //    the same RichContent, with a short and a long title.
      const chatLookups = [...lookups];
      const host = await api.createIssue("Streaming keys description host", {
        description: `Related: CHE-1, CHE-18, CHE-182 and ${FULL_KEY}.`,
      });
      await db.query(
        `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'member', $3, $4)`,
        [workspace.id, host.id, userId, `Short CHE-1 and long ${FULL_KEY} in a sentence that wraps on narrow screens.`],
      );
      await page.goto(`/${workspace.slug}/issues/${host.id}`, { waitUntil: "domcontentloaded" });
      await expect(page.locator("a.issue-mention", { hasText: FULL_KEY }).first()).toBeVisible({ timeout: 20_000 });
      await page.waitForTimeout(600 + LOOKUP_DELAY_MS);
      await page.screenshot({ path: testInfo.outputPath("description.png"), fullPage: true });
      const descriptionMeasure = await measureChips(page, "body");

      const trace = {
        viewport,
        lookupDelayMs: LOOKUP_DELAY_MS,
        boundary: { frames: boundaryFrames, chips: boundaryMeasure },
        settle: { streamingFrames: streamingOnlyFrames, frames: settleFrames, chips: settleMeasure },
        description: { chips: descriptionMeasure },
        chatLookups,
        lookups,
      };
      await saveArtifact(testInfo, "trace.json", JSON.stringify(trace, null, 2));
      for (const shot of fs.readdirSync(testInfo.outputDir).filter((name) => name.endsWith(".png"))) {
        await saveArtifact(testInfo, shot, fs.readFileSync(testInfo.outputPath(shot)));
      }

      // The chat page never showed a chip for a partial key, and never looked
      // one up. Lookups after the chat steps belong to the settled comment.
      const chatSeen = chipKeysSeen([...boundaryFrames, ...settleFrames]);
      for (const key of PARTIAL_KEYS) {
        expect(chatSeen.has(key), `chip for ${key} while streaming`).toBe(false);
        expect(chatLookups, `lookup of ${key} while streaming`).not.toContain(key);
      }
      expect(chipKeysSeen(streamingOnlyFrames).size, "chip while the reply still ended on the key").toBe(0);
      expect(chatSeen.has(FULL_KEY)).toBe(true);
    } finally {
      await db.end();
      await api.cleanup();
    }
  });
}
