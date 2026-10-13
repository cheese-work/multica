import "./env";

import { expect, test } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

test("a fresh continuation shows its source on open and the archived DM links forward", async ({ page }, testInfo) => {
  const databaseUrl = process.env.DATABASE_URL;
  if (!databaseUrl || !new URL(databaseUrl).pathname.endsWith("_test")) {
    throw new Error("Continuation E2E requires an isolated _test database");
  }
  const api = await createTestApi();
  const database = new pg.Client(databaseUrl);
  await database.connect();
  const sessionIds: string[] = [];
  let agentId: string | undefined;
  let runtimeId: string | undefined;
  try {
    const workspace = (await api.getWorkspaces())[0];
    if (!workspace) throw new Error("E2E workspace missing");
    api.setWorkspaceId(workspace.id);
    api.setWorkspaceSlug(workspace.slug);
    const user = await database.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()]);
    const runtime = await database.query<{ id: string }>(
      `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
       VALUES ($1, 'Continuation E2E', 'cloud', 'e2e_continuation', 'online', 'Continuation E2E', '{}'::jsonb, now()) RETURNING id::text`,
      [workspace.id],
    );
    runtimeId = runtime.rows[0]!.id;
    const agent = await database.query<{ id: string }>(
      `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
       VALUES ($1, 'Context Copilot', '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
      [workspace.id, runtimeId, user.rows[0]!.id],
    );
    agentId = agent.rows[0]!.id;
    const source = await database.query<{ id: string }>(
      `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
       VALUES ($1, $2, $3, 'Continue a focused launch', 'active', now()) RETURNING id::text`,
      [workspace.id, agentId, user.rows[0]!.id],
    );
    const sourceId = source.rows[0]!.id;
    sessionIds.push(sourceId);
    await database.query(
      `INSERT INTO chat_message (chat_session_id, role, content, created_at)
       SELECT $1, 'assistant', 'Original launch evidence ' || step, now() - (40 - step) * interval '1 second'
       FROM generate_series(1, 40) AS step`, [sourceId],
    );
    const token = api.getToken();
    if (!token) throw new Error("E2E token missing");
    const continued = await page.request.post(`${process.env.NEXT_PUBLIC_API_URL}/api/chat/sessions/${sourceId}/continue`, {
      headers: { Authorization: `Bearer ${token}`, "X-Workspace-ID": workspace.id },
      data: { summary: "Keep the launch goal, decisions, and evidence.\n\n".repeat(200) },
    });
    expect(continued.status()).toBe(201);
    const result = await continued.json();
    const nextId = result.session.id as string;
    sessionIds.push(nextId);
    expect(result.session.continued_from_session_id).toBe(sourceId);
    await page.addInitScript((authToken) => {
      localStorage.setItem("multica_token", authToken);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, token);
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.goto(`/${workspace.slug}/chat?session=${nextId}`, { waitUntil: "domcontentloaded" });
    const previousLink = page.getByRole("link", { name: "previous DM", exact: true });
    await expect(previousLink).toBeVisible({ timeout: 30_000 });
    await expect(previousLink).toBeInViewport();
    await expect(previousLink).toHaveAttribute("href", `/${workspace.slug}/chat?session=${sourceId}`);
    await expect(page.getByRole("button", { name: "Conversation summary" })).toHaveAttribute("aria-expanded", "false");
    const freshScreenshot = testInfo.outputPath("fresh-continued-dm.png");
    await page.screenshot({ path: freshScreenshot, fullPage: true });
    await testInfo.attach("Fresh continued DM on first open", { path: freshScreenshot, contentType: "image/png" });

    await previousLink.click();
    const nextLink = page.getByRole("link", { name: "Continued in →", exact: true });
    await expect(nextLink).toBeVisible({ timeout: 30_000 });
    await expect(nextLink).toBeInViewport();
    await expect(nextLink).toHaveAttribute("href", `/${workspace.slug}/chat?session=${nextId}`);
    await expect(page.getByText("Original launch evidence 40", { exact: true })).toBeInViewport();
    const archivedScreenshot = testInfo.outputPath("archived-source-dm.png");
    await page.screenshot({ path: archivedScreenshot, fullPage: true });
    await testInfo.attach("Archived source DM", { path: archivedScreenshot, contentType: "image/png" });
    await nextLink.click();
    await expect(previousLink).toBeInViewport();
    await page.getByRole("button", { name: "Conversation summary" }).click();
    await expect(page.getByText("Previous DM", { exact: true })).toBeVisible();
  } finally {
    for (const sessionId of sessionIds.reverse()) {
      await database.query(`DELETE FROM chat_message WHERE chat_session_id = $1`, [sessionId]);
      await database.query(`DELETE FROM chat_session WHERE id = $1`, [sessionId]);
    }
    if (agentId) await database.query(`DELETE FROM agent WHERE id = $1`, [agentId]);
    if (runtimeId) await database.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]);
    await database.end();
    await api.cleanup();
  }
});
