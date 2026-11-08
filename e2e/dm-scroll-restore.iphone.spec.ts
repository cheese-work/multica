import "./env";

import { expect, test, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

type Dm = { slug: string; sessionId: string; token: string; linked: { id: string; identifier: string; title: string } };

// Seeds a DM with 80 messages (message 40 links an issue), opens it and scrolls
// up until message 40 is on screen. The reader is then still.
async function withScrolledDm(page: Page, run: (dm: Dm) => Promise<void>) {
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

    for (let i = 0; i < 120 && !(await onScreen(page)); i++) {
      await dragUp(page, 300);
      await page.waitForTimeout(80);
    }
    expect(await onScreen(page)).toBe(true);
    await settle(page);
    await run({ slug: workspace.slug, sessionId, token: api.getToken()!, linked });
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
}

// iPhone: scroll up in a DM, open an issue linked from it, go Back. The DM must
// come back at the same place, and must not move while the reader is still.
test.describe("DM scroll position on iPhone (CHE-4638)", () => {
  test("Back from a linked issue restores the DM offset and the offset holds", async ({ page }, testInfo) => {
    await withScrolledDm(page, async ({ linked }) => {
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
    });
  });

  test("after Back, a row above that grows late does not move the restored message", async ({ page }) => {
    await withScrolledDm(page, async ({ linked }) => {
      const before = await targetTop(page);
      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      await page.goBack();
      await expect.poll(() => onScreen(page), { timeout: 15_000 }).toBe(true);
      await settle(page);

      // No wheel, touch or key input since Back: the reader is still.
      await growRow(page, -2);
      await page.waitForTimeout(1500);
      expect(Math.abs((await targetTop(page)) - before)).toBeLessThanOrEqual(2);
    });
  });

  test("right after Back, a row above that grows before the list settles does not move the restored message", async ({ page }) => {
    await withScrolledDm(page, async ({ linked }) => {
      const before = await targetTop(page);
      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      // Grow the row one frame after the restored list first renders it: after the
      // row was measured, inside the restore window, with no input from the reader.
      await page.evaluate(() => {
        const w = window as unknown as { __grownAt?: number };
        const watch = new MutationObserver(() => {
          const rows = [...document.querySelectorAll<HTMLElement>("[data-message-id]")];
          const i = rows.findIndex((n) => n.textContent?.includes("Target message 40"));
          if (i < 2) return;
          watch.disconnect();
          const row = rows[i - 2]!;
          // Second frame: the list has measured the row once, so this is late growth.
          requestAnimationFrame(() =>
            requestAnimationFrame(() => {
              row.style.minHeight = "260px";
              w.__grownAt = performance.now();
            }),
          );
        });
        watch.observe(document.body, { childList: true, subtree: true });
      });
      await page.goBack();
      await expect
        .poll(() => page.evaluate(() => (window as unknown as { __grownAt?: number }).__grownAt), { timeout: 15_000 })
        .toBeDefined();
      await settle(page);
      await page.waitForTimeout(1500);
      expect(Math.abs((await targetTop(page)) - before)).toBeLessThanOrEqual(2);
    });
  });

  test("the offset holds while a message arrives and rows above and below resize", async ({ page }, testInfo) => {
    await withScrolledDm(page, async ({ sessionId, token }) => {
      const before = await targetTop(page);

      // A new message lands at the live end while the reader is scrolled up.
      const api = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
      const slug = new URL(page.url()).pathname.split("/")[1]!;
      const sent = await fetch(`${api}/api/chat/sessions/${sessionId}/messages`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}`, "X-Workspace-Slug": slug },
        body: JSON.stringify({ content: "Late arrival from the reader" }),
      });
      expect(sent.ok).toBe(true);

      const drift = async () => Math.abs((await targetTop(page)) - before);
      await page.waitForTimeout(2000);
      const afterMessage = await drift();

      // Late height changes, as an image or a code block finishing layout does.
      await growRow(page, 2);
      await page.waitForTimeout(1500);
      const afterBelow = await drift();
      await growRow(page, -2);
      await page.waitForTimeout(1500);
      const afterAbove = await drift();

      await testInfo.attach("after-late-content", { body: await page.screenshot(), contentType: "image/png" });
      expect(afterMessage).toBeLessThanOrEqual(2);
      expect(afterBelow).toBeLessThanOrEqual(2);
      expect(afterAbove).toBeLessThanOrEqual(2);
    });
  });
});

// A late height change, as an image or a code block finishing layout does.
// `offset` is the row's distance from the target message (negative = above).
async function growRow(page: Page, offset: number) {
  await page.evaluate((delta) => {
    const rows = [...document.querySelectorAll<HTMLElement>("[data-message-id]")];
    const i = rows.findIndex((n) => n.textContent?.includes("Target message 40"));
    rows[i + delta]!.style.minHeight = "260px";
  }, offset);
}

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
