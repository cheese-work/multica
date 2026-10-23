import "./env";

import { expect, test, type Page, type TestInfo } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

type Surface = "page" | "window";

async function seedSourceDM(database: pg.Client, api: Awaited<ReturnType<typeof createTestApi>>) {
  const workspace = (await api.getWorkspaces())[0];
  if (!workspace) throw new Error("E2E workspace missing");
  api.setWorkspaceId(workspace.id);
  api.setWorkspaceSlug(workspace.slug);
  const user = await database.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()]);
  const runtime = await database.query<{ id: string }>(
    `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
     VALUES ($1, 'Fork E2E', 'cloud', 'e2e_fork', 'online', 'Fork E2E', '{}'::jsonb, now()) RETURNING id::text`,
    [workspace.id],
  );
  const agent = await database.query<{ id: string }>(
    `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
     VALUES ($1, 'Branch Copilot', '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
    [workspace.id, runtime.rows[0]!.id, user.rows[0]!.id],
  );
  const source = await database.query<{ id: string }>(
    `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at, created_at)
     VALUES ($1, $2, $3, 'Storage engine choice', 'active', now() - interval '1 day', now() - interval '1 day') RETURNING id::text`,
    [workspace.id, agent.rows[0]!.id, user.rows[0]!.id],
  );
  await database.query(
    `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES
     ($1, 'user', 'Compare Postgres and SQLite for the sync service.', now() - interval '1 day'),
     ($1, 'assistant', 'Postgres fits the current deployment; SQLite is worth a spike.', now() - interval '1 day' + interval '1 minute')`,
    [source.rows[0]!.id],
  );
  // An unrelated DM with more recent activity than the source: a new fork must
  // still list above it, because the server and client rank by creation time too.
  const other = await database.query<{ id: string }>(
    `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at, created_at)
     VALUES ($1, $2, $3, 'Unrelated recent DM', 'active', now() - interval '1 day', now() - interval '1 day') RETURNING id::text`,
    [workspace.id, agent.rows[0]!.id, user.rows[0]!.id],
  );
  await database.query(
    `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, 'assistant', 'Recent unrelated answer', now() - interval '1 hour')`,
    [other.rows[0]!.id],
  );
  return { workspace, sourceId: source.rows[0]!.id, agentId: agent.rows[0]!.id, runtimeId: runtime.rows[0]!.id };
}

async function cleanup(database: pg.Client, seed: { agentId: string; runtimeId: string }) {
  const sessions = await database.query<{ id: string }>(`SELECT id::text FROM chat_session WHERE agent_id = $1`, [seed.agentId]);
  for (const { id } of sessions.rows) {
    await database.query(`DELETE FROM attachment WHERE chat_session_id = $1`, [id]);
    await database.query(`DELETE FROM chat_message WHERE chat_session_id = $1`, [id]);
    await database.query(`DELETE FROM agent_task_queue WHERE chat_session_id = $1`, [id]);
    await database.query(`DELETE FROM chat_session WHERE id = $1`, [id]);
  }
  await database.query(`DELETE FROM agent WHERE id = $1`, [seed.agentId]);
  await database.query(`DELETE FROM agent_runtime WHERE id = $1`, [seed.runtimeId]);
}

async function openSource(page: Page, slug: string, sourceId: string, surface: Surface, token: string) {
  await page.addInitScript(({ authToken, isOpen, key, session }) => {
    localStorage.setItem("multica_token", authToken);
    localStorage.setItem("multica:chat:isOpen", isOpen);
    localStorage.setItem(key, session);
  }, { authToken: token, isOpen: surface === "window" ? "true" : "false", key: `multica:chat:activeSessionId:${slug}`, session: sourceId });
  await page.setViewportSize(surface === "window" ? { width: 1280, height: 900 } : { width: 1440, height: 900 });
  await page.goto(surface === "window" ? `/${slug}/issues` : `/${slug}/chat?session=${sourceId}`, { waitUntil: "domcontentloaded" });
  await expect(page.getByText("Compare Postgres and SQLite for the sync service.").last()).toBeVisible({ timeout: 30_000 });
}

async function shot(page: Page, testInfo: TestInfo, name: string) {
  const path = testInfo.outputPath(`${name}.png`);
  await page.screenshot({ path, fullPage: true });
  await testInfo.attach(name, { path, contentType: "image/png" });
}

for (const surface of ["page", "window"] as const) {
  test.describe(`/fork in the chat ${surface}`, () => {
    let database: pg.Client;
    let api: Awaited<ReturnType<typeof createTestApi>>;
    let seed: Awaited<ReturnType<typeof seedSourceDM>>;

    test.beforeEach(async () => {
      const databaseUrl = process.env.DATABASE_URL;
      if (!databaseUrl || !new URL(databaseUrl).pathname.endsWith("_test")) {
        throw new Error("Fork E2E requires an isolated _test database");
      }
      api = await createTestApi();
      database = new pg.Client(databaseUrl);
      await database.connect();
      seed = await seedSourceDM(database, api);
    });

    test.afterEach(async () => {
      await cleanup(database, seed);
      await database.end();
      await api.cleanup();
    });

    test("with a message opens the fork with the copy, the divider and the message", async ({ page }, testInfo) => {
      const { workspace, sourceId } = seed;
      await openSource(page, workspace.slug, sourceId, surface, api.getToken()!);
      const composer = page.locator('[contenteditable="true"]').last();
      await composer.click();
      await page.keyboard.type("/");
      await expect(page.getByText("Copy this DM into a new one; optional message is sent there")).toBeVisible();
      await shot(page, testInfo, `${surface}-slash-menu`);
      await page.keyboard.type("fork try the variant");
      await page.keyboard.press("ControlOrMeta+Enter");

      const forkedFrom = page.getByRole("link", { name: "Storage engine choice", exact: true });
      await expect(forkedFrom).toBeVisible({ timeout: 30_000 });
      await expect(forkedFrom).toHaveAttribute("href", `/${workspace.slug}/chat?session=${sourceId}`);
      await expect(page.getByRole("separator", { name: /Forked from Storage engine choice/ })).toBeVisible();
      await expect(page.getByText("2 messages copied", { exact: false })).toBeVisible();
      await expect(page.getByText("Compare Postgres and SQLite for the sync service.").last()).toBeVisible();
      await expect(page.getByText("try the variant", { exact: true }).last()).toBeVisible();
      await expect(page.getByText("Send a message to start this branch.")).toHaveCount(0);
      await expect(page.getByText("Storage engine choice (fork: try the variant)").first()).toBeVisible();
      const child = await database.query<{ id: string }>(
        `SELECT id::text FROM chat_session WHERE forked_from_session_id = $1`, [sourceId],
      );
      expect(child.rows).toHaveLength(1);
      expect(Number((await database.query(`SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, [child.rows[0]!.id])).rows[0].count)).toBe(1);
      await shot(page, testInfo, `${surface}-fork-with-message`);

      await forkedFrom.click();
      await expect(page.getByRole("link", { name: "Storage engine choice (fork: try the variant)", exact: true })).toBeVisible({ timeout: 30_000 });
      await expect(page.getByRole("link", { name: "Continued in →", exact: true })).toHaveCount(0);
      await expect(page.locator('[contenteditable="true"]').last()).toBeEditable();
      await shot(page, testInfo, `${surface}-active-source`);
    });

    test("without a message opens the idle fork, and the source lists it", async ({ page }, testInfo) => {
      const { workspace, sourceId } = seed;
      await openSource(page, workspace.slug, sourceId, surface, api.getToken()!);
      const composer = page.locator('[contenteditable="true"]').last();
      await composer.click();
      await page.keyboard.type("/fork");
      await page.keyboard.press("Escape");
      await page.keyboard.press("ControlOrMeta+Enter");

      const forkedFrom = page.getByRole("link", { name: "Storage engine choice", exact: true });
      await expect(page.getByRole("separator", { name: /Forked from Storage engine choice/ })).toBeVisible({ timeout: 30_000 });
      await expect(forkedFrom).toHaveAttribute("href", `/${workspace.slug}/chat?session=${sourceId}`);
      await expect(page.getByText("2 messages copied", { exact: false })).toBeVisible();
      await expect(page.getByText("Send a message to start this branch.")).toBeVisible();
      await expect(page.getByText("Storage engine choice (fork)").first()).toBeVisible();
      // The divider is the only notice: no snackbar or toast.
      await expect(page.locator("[data-sonner-toast]")).toHaveCount(0);
      const child = await database.query<{ id: string }>(
        `SELECT id::text FROM chat_session WHERE forked_from_session_id = $1`, [sourceId],
      );
      expect(child.rows).toHaveLength(1);
      expect(Number((await database.query(`SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, [child.rows[0]!.id])).rows[0].count)).toBe(0);
      if (surface === "page") {
        // The DM list ranks the new idle fork above the more recently active
        // unrelated DM, also after the list refetches.
        // DM list titles; the list renders before the DM header that repeats the title.
        const forkRow = page.getByText("Storage engine choice (fork)", { exact: true }).first();
        const otherRow = page.getByText("Unrelated recent DM", { exact: true });
        // The reload must keep the fork selected: under load it can start before the URL names the fork.
        await expect(page).toHaveURL(new RegExp(`session=${child.rows[0]!.id}`));
        await page.reload({ waitUntil: "domcontentloaded" });
        await expect(otherRow).toBeVisible({ timeout: 30_000 });
        await expect(forkRow).toBeVisible();
        expect((await forkRow.boundingBox())!.y).toBeLessThan((await otherRow.boundingBox())!.y);
      }
      await shot(page, testInfo, `${surface}-idle-fork`);

      await forkedFrom.click();
      const forkedInto = page.getByRole("link", { name: "Storage engine choice (fork)", exact: true });
      await expect(forkedInto).toBeVisible({ timeout: 30_000 });
      await expect(forkedInto).toHaveAttribute("href", `/${workspace.slug}/chat?session=${child.rows[0]!.id}`);
      await shot(page, testInfo, `${surface}-source-after-idle-fork`);
    });

    test("lists each fork under the source message it copied up to, past a late-visible row", async ({ page }, testInfo) => {
      const { workspace, sourceId } = seed;
      const forkSource = async (key: string, title: string) => {
        const response = await fetch(`${process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`}/api/chat/sessions/${sourceId}/fork`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Authorization: `Bearer ${api.getToken()}`, "X-Workspace-Slug": workspace.slug },
          body: JSON.stringify({ idempotency_key: key }),
        });
        expect(response.status).toBe(201);
        const { session } = (await response.json()) as { session: { id: string } };
        await database.query(`UPDATE chat_session SET title = $2 WHERE id = $1`, [session.id, title]);
      };
      const addSourceMessage = (role: string, content: string, ago: string) => database.query(
        `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, $2, $3, now() - $4::interval)`,
        [sourceId, role, content, ago],
      );
      // The first fork copies the two seeded messages. A row that was queued at
      // that moment then surfaces with a time before the fork. The second fork
      // copies it; a newer reply follows both forks.
      await forkSource("e2e-early", "Early branch");
      await addSourceMessage("user", "Late visible question", "1 minute");
      await forkSource("e2e-later", "Later branch");
      await addSourceMessage("assistant", "Newest reply", "0 seconds");

      await openSource(page, workspace.slug, sourceId, surface, api.getToken()!);
      const early = page.getByRole("link", { name: "Early branch", exact: true });
      const later = page.getByRole("link", { name: "Later branch", exact: true });
      await expect(early).toBeVisible({ timeout: 30_000 });
      await expect(later).toBeVisible();
      const rows = page.locator("[data-index]");
      await expect(rows.filter({ hasText: "Postgres fits the current deployment" }).getByRole("link", { name: "Early branch", exact: true })).toBeVisible();
      await expect(rows.filter({ hasText: "Late visible question" }).getByRole("link", { name: "Later branch", exact: true })).toBeVisible();
      const y = async (locator: typeof early) => (await locator.boundingBox())!.y;
      const lateRow = page.getByText("Late visible question").last();
      const newest = page.getByText("Newest reply").last();
      expect(await y(early)).toBeLessThan(await y(lateRow));
      expect(await y(lateRow)).toBeLessThan(await y(later));
      expect(await y(later)).toBeLessThan(await y(newest));
      await shot(page, testInfo, `${surface}-forks-at-copied-messages`);
    });
  });
}
