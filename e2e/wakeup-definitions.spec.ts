import "./env";

import { execFile } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { expect, test } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

const run = promisify(execFile);
const API = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const RULE = "pr_merged";

// Run the CLI as a person, not as the agent whose run launched this suite.
const cleanEnv = () => Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("MULTICA_")));

// CHE-1082 L15: the workspace -> project -> issue chain on a real server. The
// handler tests cover the rules in isolation; this walks inherited, override,
// disabled and runtime states through the HTTP API, the issue page and the CLI.
test("workspace, project and issue wakeup defaults resolve and show on every surface", async ({ page }) => {
  test.setTimeout(180_000);
  const api = await createTestApi();
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    const headers = {
      Authorization: `Bearer ${api.getToken()}`,
      "X-Workspace-ID": workspace.id,
      "Content-Type": "application/json",
    };
    const call = (method: string, path: string, body?: unknown) =>
      fetch(`${API}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    const put = (path: string, revision: string, config: Record<string, unknown>) =>
      call("PUT", path, { revision, config: { v: 1, ...config } });

    const project = await api.createProject("Wakeup defaults project");
    const issue = await api.createIssue("Wakeup defaults issue", { project_id: project.id });
    const wsPath = `/api/wakeup-definitions/${RULE}`;
    const projectPath = `/api/projects/${project.id}/wakeup-definitions/${RULE}`;
    const issuePath = `/api/issues/${issue.id}/wakeup-definitions/${RULE}`;
    const effective = async (path = issuePath) => {
      const res = await call("GET", `${path}/effective`);
      expect(res.status).toBe(200);
      return res.json();
    };

    // The gate must be open for definition writes; otherwise the UI hides the list.
    const caps = await (await call("GET", `/api/issues/${issue.id}/wakeup-definitions`)).json();
    expect(caps.capabilities.definition_writes).toBe(true);

    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${issue.id}`, { waitUntil: "domcontentloaded" });
    const row = page.getByRole("list", { name: "Default wakeups" }).getByRole("listitem", { name: "When the linked PR merges" });
    await expect(row).toBeVisible({ timeout: 60_000 });

    // Inherited from the built-in default.
    await expect(row).toContainText("Inherited from the default");
    await expect(row).toContainText("On");

    // Workspace default (owner write).
    expect((await put(wsPath, "0", { enabled: true, instruction: "workspace text" })).status).toBe(200);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(row).toContainText("Inherited from workspace", { timeout: 30_000 });
    let eff = await effective();
    expect(eff.enabled).toBe(true);
    expect(eff.sources.instruction).toBe("workspace");

    // A project override beats the workspace for the fields it sets; the row's
    // source label follows the "enabled" field.
    expect((await put(projectPath, "0", { enabled: true, instruction: "project text" })).status).toBe(200);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(row).toContainText("Inherited from project", { timeout: 30_000 });
    eff = await effective();
    expect(eff.sources.instruction).toBe("project");
    expect(eff.sources.enabled).toBe("project");

    // Issue override from the UI: switch the rule off for this issue only.
    await row.getByRole("button", { name: /^Override/ }).click();
    await page.getByRole("switch").last().click();
    await page.getByRole("button", { name: "Save override" }).click();
    await expect(row).toContainText("Overridden on this issue", { timeout: 30_000 });
    await expect(row).toContainText("Disabled");
    eff = await effective();
    expect(eff.enabled).toBe(false);
    expect(eff.sources.enabled).toBe("issue");
    expect((await effective(wsPath)).enabled).toBe(true);

    // Runtime state: a paused instance of the rule rides on the issue response
    // and on the CLI list, and shows on the issue page.
    // The list joins the agent, so the instance needs one.
    const agentId = (
      await db.query(
        `INSERT INTO agent(workspace_id,name,runtime_mode,visibility) VALUES($1,'Wakeup defaults agent','local','workspace') RETURNING id`,
        [workspace.id],
      )
    ).rows[0].id;
    await db.query(
      `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,event_types,enabled,paused_reason,system_rule)
       VALUES(gen_random_uuid(),$1,$2,$3,'','event','continuous','{pr.merged}',true,'loop',$4)`,
      [workspace.id, issue.id, agentId, RULE],
    );
    eff = await effective();
    expect(eff.execution.paused_reason).toBe("loop");
    expect(eff.execution.instance_id).toBeTruthy();
    expect((await effective(projectPath)).execution ?? null).toBeNull();
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(row).toContainText("Paused", { timeout: 30_000 });

    const cli = process.env.MULTICA_CLI;
    if (cli) {
      const { stdout } = await run(cli, ["issue", "wakeup", "list", issue.id, "--output", "json"], {
        // A cwd outside any agent workdir keeps the CLI from reading a task marker.
        cwd: mkdtempSync(join(tmpdir(), "wakeup-cli-")),
        env: { ...cleanEnv(), MULTICA_SERVER_URL: API, MULTICA_WORKSPACE_ID: workspace.id, MULTICA_TOKEN: api.getToken()! },
      });
      const rows = JSON.parse(stdout) as Array<{ paused_reason?: string }>;
      expect(rows.filter((r) => r.paused_reason === "loop"), stdout).toHaveLength(1);
    }

    // Reset from the UI: the workspace/project value comes back, the instance stays.
    const instanceId = eff.execution.instance_id;
    await row.getByRole("button", { name: /^Reset/ }).click();
    await expect(row).toContainText("Inherited from project", { timeout: 30_000 });
    eff = await effective();
    expect(eff.enabled).toBe(true);
    expect(eff.execution.instance_id).toBe(instanceId);
  } finally {
    await api.cleanup();
    await db.query(`DELETE FROM agent WHERE name = 'Wakeup defaults agent'`);
    await db.query(`DELETE FROM issue_wakeup_definition WHERE rule_key = $1`, [RULE]);
    await db.end();
  }
});
