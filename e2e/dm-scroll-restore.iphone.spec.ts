import "./env";

import { expect, test, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

// iPhone: scroll up in a DM, open an issue linked from it, go Back. The DM must
// come back at the same place, and must not move while the reader is still.
test.describe("DM scroll position on iPhone (CHE-4638)", () => {
  test("Back from a linked issue restores the DM offset and the offset holds", async ({ page }, testInfo) => {
    const databaseUrl = process.env.DATABASE_URL;
    if (!databaseUrl) throw new Error("DATABASE_URL missing");
    const api = await createTestApi();
    const database = new pg.Client(databaseUrl);
    await database.connect();
    let sessionId: string | undefined;
    let agentId: string | undefined;
    let runtimeId: string | undefined;
    try {
      const workspace = (await api.getWorkspaces())[0]!;
      api.setWorkspaceId(workspace.id);
      api.setWorkspaceSlug(workspace.slug);
      const linked = await api.createIssue(`DM scroll linked ${Date.now()}`);
      const user = await database.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()]);
      const runtime = await database.query<{ id: string }>(
        `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
         VALUES ($1, 'Scroll E2E', 'cloud', 'e2e_scroll', 'online', 'Scroll E2E', '{}'::jsonb, now()) RETURNING id::text`,
        [workspace.id],
      );
      runtimeId = runtime.rows[0]!.id;
      const agent = await database.query<{ id: string }>(
        `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
         VALUES ($1, 'Scroll Copilot', '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
        [workspace.id, runtimeId, user.rows[0]!.id],
      );
      agentId = agent.rows[0]!.id;
      const session = await database.query<{ id: string }>(
        `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
         VALUES ($1, $2, $3, 'Scroll restore', 'active', now()) RETURNING id::text`,
        [workspace.id, agentId, user.rows[0]!.id],
      );
      sessionId = session.rows[0]!.id;
      const link = `[${linked.identifier}](mention://issue/${linked.id})`;
      await database.query(
        `INSERT INTO chat_message (chat_session_id, role, content, created_at)
         SELECT $1, 'assistant',
           CASE WHEN step = 40 THEN 'Target message 40: see ' || $2 || E'\n\n' || repeat('filler text ', 30)
                ELSE 'Message ' || step || ': ' || repeat('filler text ', 30) END,
           now() - (80 - step) * interval '1 second'
         FROM generate_series(1, 80) AS step`,
        [sessionId, link],
      );

      await page.addInitScript((authToken) => {
        localStorage.setItem("multica_token", authToken);
        localStorage.setItem("multica:chat:isOpen", "false");
      }, api.getToken()!);
      await page.goto(`/${workspace.slug}/chat?session=${sessionId}`, { waitUntil: "domcontentloaded" });
      await expect(page.getByText("Message 80:")).toBeVisible({ timeout: 30_000 });

      // Scroll up until the linked message is on screen.
      const target = page.getByText("Target message 40");
      for (let i = 0; i < 120 && !(await onScreen(page)); i++) {
        await dragUp(page, 300);
        await page.waitForTimeout(80);
      }
      expect(await onScreen(page)).toBe(true);
      await settle(page);
      const before = await targetTop(page);
      await testInfo.attach("before", { body: await page.screenshot(), contentType: "image/png" });

      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      await page.goBack();
      await expect.poll(() => onScreen(page), { timeout: 15_000 }).toBe(true);
      await settle(page);
      const after = await targetTop(page);
      await testInfo.attach("after", { body: await page.screenshot(), contentType: "image/png" });
      expect(Math.abs(after - before)).toBeLessThanOrEqual(2);

      // Still reader: the offset must not drift on its own.
      await page.waitForTimeout(3000);
      expect(Math.abs((await targetTop(page)) - after)).toBeLessThanOrEqual(2);
    } finally {
      if (sessionId) {
        await database.query(`DELETE FROM chat_message WHERE chat_session_id = $1`, [sessionId]);
        await database.query(`DELETE FROM chat_session WHERE id = $1`, [sessionId]);
      }
      if (agentId) await database.query(`DELETE FROM agent WHERE id = $1`, [agentId]);
      if (runtimeId) await database.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]);
      await database.end();
      await api.cleanup();
    }
  });
});

async function settle(page: Page) {
  await page.waitForTimeout(1500);
}

async function targetTop(page: Page) {
  return page.getByText("Target message 40").evaluate((el) => el.getBoundingClientRect().top);
}

// Mobile WebKit has no mouse.wheel and no touch-swipe API. Stage the same input
// a finger drag does (a wheel delta the follow latch reads), then move the list
// in the same task, so a scroll counts as the reader's and is not pinned back.
async function dragUp(page: Page, px: number) {
  await page.evaluate((delta) => {
    const el = document.querySelector("[data-tab-scroll-root]")!;
    el.dispatchEvent(new WheelEvent("wheel", { deltaY: -delta, bubbles: true }));
    el.scrollTop -= delta;
  }, px);
}

// Virtuoso renders rows above and below the fold, so "visible" is not "on screen".
async function onScreen(page: Page) {
  return page.evaluate(() => {
    const row = [...document.querySelectorAll("[data-message-id]")].find((n) => n.textContent?.includes("Target message 40"));
    if (!row) return false;
    const { top } = row.getBoundingClientRect();
    return top > 80 && top < 400;
  });
}
