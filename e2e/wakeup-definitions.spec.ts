import "./env";

import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { promisify } from "node:util";
import { _electron, expect, test, type ElectronApplication, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

const run = promisify(execFile);
const API = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const RULE = "pr_merged";

// Run the CLI as a person, not as the agent whose run launched this suite.
const cleanEnv = () => Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("MULTICA_")));

// CHE-1082 L15: the workspace -> project -> issue chain on a real server. The
// handler tests cover the rules in isolation; this walks inherited, override,
// disabled and runtime states through the HTTP API, the issue page and the CLI
// (`multica wakeup-definition get` for definitions, `multica issue wakeup list`
// for runtime instances).
test("workspace, project and issue wakeup defaults resolve and show on every surface", async ({ page }) => {
  test.setTimeout(180_000);
  const api = await createTestApi();
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  // Everything the test writes outside the API client is tracked here so the
  // cleanup removes exactly that, in this workspace, and nothing else.
  let workspaceId: string | undefined;
  let settingsBefore: unknown;
  let hadWorkspaceDefinition = true;
  let projectId: string | undefined;
  let issueId: string | undefined;
  let agentId: string | undefined;
  let wakeupId: string | undefined;
  let app: ElectronApplication | undefined;
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    workspaceId = workspace.id;
    // A built-in rule write at workspace scope merges into workspace.settings.
    settingsBefore = (await db.query(`SELECT settings FROM workspace WHERE id=$1`, [workspace.id])).rows[0].settings;
    hadWorkspaceDefinition =
      (await db.query(`SELECT 1 FROM issue_wakeup_definition WHERE workspace_id=$1 AND scope_kind='workspace' AND rule_key=$2`, [workspace.id, RULE]))
        .rowCount! > 0;
    // The test writes with revision 0, so it needs a clean slate to create.
    expect(hadWorkspaceDefinition).toBe(false);
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
    projectId = project.id;
    issueId = issue.id;
    const wsPath = `/api/wakeup-definitions/${RULE}`;
    const projectPath = `/api/projects/${project.id}/wakeup-definitions/${RULE}`;
    const issuePath = `/api/issues/${issue.id}/wakeup-definitions/${RULE}`;
    const effective = async (path = issuePath) => {
      const res = await call("GET", `${path}/effective`);
      expect(res.status).toBe(200);
      return res.json();
    };

    // The CLI runs as a person from a cwd outside any agent workdir, so it does not
    // read a task marker. It is optional: without MULTICA_CLI the CLI steps are skipped.
    const cli = process.env.MULTICA_CLI;
    const cliRun = async (args: string[]) => {
      const { stdout } = await run(cli!, args, {
        cwd: mkdtempSync(join(tmpdir(), "wakeup-cli-")),
        env: { ...cleanEnv(), MULTICA_SERVER_URL: API, MULTICA_WORKSPACE_ID: workspace.id, MULTICA_TOKEN: api.getToken()! },
      });
      return JSON.parse(stdout);
    };
    // `wakeup-definition get` must return exactly what the API returns for the same scope.
    const cliEffective = async (scope: "workspace" | "project" | "issue") => {
      const flags = scope === "workspace" ? [] : scope === "project" ? ["--project", project.id] : ["--issue", issue.id];
      const fromCli = await cliRun(["wakeup-definition", "get", RULE, ...flags, "--output", "json"]);
      expect(fromCli).toEqual(await effective({ workspace: wsPath, project: projectPath, issue: issuePath }[scope]));
      return fromCli;
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

    // Surfaces that render the issue page: the web page, plus the built Electron
    // client when WAKEUP_DESKTOP=1 (after `electron-vite build` with VITE_API_URL
    // set), with an isolated profile against the same server.
    const surfaces: Page[] = [page];
    if (process.env.WAKEUP_DESKTOP === "1") {
      const home = mkdtempSync(join(tmpdir(), "wakeup-desktop-"));
      mkdirSync(join(home, ".multica"), { recursive: true });
      writeFileSync(join(home, ".multica", "desktop_prefs.json"), JSON.stringify({ autoStart: false, autoStop: false }));
      const env = { ...cleanEnv(), HOME: home, XDG_CONFIG_HOME: join(home, "config"), DESKTOP_APP_SUFFIX: "CHE3667", ELECTRON_DISABLE_SECURITY_WARNINGS: "true" } as Record<string, string>;
      app = await _electron.launch({
        executablePath: createRequire(resolve("apps/desktop/package.json"))("electron"),
        args: [resolve("apps/desktop"), "--ozone-platform=headless", "--no-sandbox", "--disable-gpu"],
        env,
      });
      const win = await app.firstWindow();
      await win.waitForLoadState("domcontentloaded");
      // On a loaded host the first window can still be about:blank, which has no storage.
      await win.waitForFunction(() => location.protocol !== "about:", undefined, { timeout: 60_000 });
      const path = `/${workspace.slug}/issues/${issue.id}`;
      await win.evaluate(({ token, slug, path }) => {
        localStorage.setItem("multica_token", token);
        localStorage.setItem("multica_tabs", JSON.stringify({ version: 5, state: {
          activeWorkspaceSlug: slug, byWorkspace: { [slug]: {
            activeTabId: "wakeups", tabs: [{ id: "wakeups", url: path, resourceKey: path, title: "Wakeup defaults issue", pinned: false, history: { stack: [path], index: 0 }, memento: { scroll: {}, view: {} } }],
            browsingHistory: [path], browsingHistoryTitles: {}, recentTabIds: [],
          } },
        } }));
      }, { token: api.getToken()!, slug: workspace.slug, path });
      surfaces.push(win);
    }
    const rowOf = (p: Page) => p.getByRole("list", { name: "Default wakeups" }).getByRole("listitem", { name: "When the linked PR merges" });
    // Reload every surface and check the same row text on each.
    const expectState = async (...texts: string[]) => {
      for (const p of surfaces) {
        await p.reload({ waitUntil: "domcontentloaded" });
        for (const text of texts) await expect(rowOf(p)).toContainText(text, { timeout: 90_000 });
      }
    };
    // Drive an issue override or reset through one surface's UI.
    const override = async (p: Page) => {
      await rowOf(p).getByRole("button", { name: /^Override/ }).click();
      await p.getByRole("switch").last().click();
      await p.getByRole("button", { name: "Save override" }).click();
      await expect(rowOf(p)).toContainText("Overridden on this issue", { timeout: 30_000 });
    };
    const reset = async (p: Page) => {
      await rowOf(p).getByRole("button", { name: /^Reset/ }).click();
      await expect(rowOf(p)).toContainText("Inherited from project", { timeout: 30_000 });
    };
    const web = page;
    const last = surfaces[surfaces.length - 1]!;

    // Inherited from the built-in default.
    await expectState("Inherited from the default", "On");
    if (cli) {
      const fromCli = await cliEffective("issue");
      expect(fromCli.enabled).toBe(true);
      expect(Object.values(fromCli.sources)).not.toContain("workspace");
      expect(Object.values(fromCli.sources)).not.toContain("project");
    }

    // Workspace default (owner write).
    expect((await put(wsPath, "0", { enabled: true, instruction: "workspace text" })).status).toBe(200);
    await expectState("Inherited from workspace");
    let eff = await effective();
    expect(eff.enabled).toBe(true);
    expect(eff.sources.instruction).toBe("workspace");
    if (cli) {
      expect((await cliEffective("workspace")).sources.instruction).toBe("workspace");
      expect((await cliEffective("issue")).sources.instruction).toBe("workspace");
    }

    // A project override beats the workspace for the fields it sets; the row's
    // source label follows the "enabled" field.
    expect((await put(projectPath, "0", { enabled: true, instruction: "project text" })).status).toBe(200);
    await expectState("Inherited from project");
    eff = await effective();
    expect(eff.sources.instruction).toBe("project");
    expect(eff.sources.enabled).toBe("project");
    if (cli) {
      const fromCli = await cliEffective("project");
      expect(fromCli.sources.instruction).toBe("project");
      expect(fromCli.overrides).toEqual(expect.arrayContaining(["instruction"]));
      expect((await cliEffective("issue")).sources.instruction).toBe("project");
    }

    // Issue override from the web UI: switch the rule off for this issue only.
    await override(web);
    await expectState("Overridden on this issue", "Disabled");
    eff = await effective();
    expect(eff.enabled).toBe(false);
    expect(eff.sources.enabled).toBe("issue");
    expect((await effective(wsPath)).enabled).toBe(true);
    if (cli) {
      const fromCli = await cliEffective("issue");
      expect(fromCli.enabled).toBe(false);
      expect(fromCli.sources.enabled).toBe("issue");
      expect((await cliEffective("workspace")).enabled).toBe(true);
      expect((await cliEffective("project")).enabled).toBe(true);
    }

    // Runtime state: a paused instance of the rule rides on the issue response
    // and on the CLI list, and shows on the issue page.
    // The list joins the agent, so the instance needs one.
    agentId = (
      await db.query(
        `INSERT INTO agent(workspace_id,name,runtime_mode,visibility) VALUES($1,'Wakeup defaults agent','local','workspace') RETURNING id`,
        [workspace.id],
      )
    ).rows[0].id;
    wakeupId = (
      await db.query(
        `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,event_types,enabled,paused_reason,system_rule)
         VALUES(gen_random_uuid(),$1,$2,$3,'','event','continuous','{pr.merged}',true,'loop',$4) RETURNING id`,
        [workspace.id, issue.id, agentId, RULE],
      )
    ).rows[0].id;
    eff = await effective();
    expect(eff.execution.paused_reason).toBe("loop");
    expect(eff.execution.instance_id).toBeTruthy();
    expect((await effective(projectPath)).execution ?? null).toBeNull();
    await expectState("Overridden on this issue", "Paused");

    // Runtime instances come from `issue wakeup list`; the definition's execution block comes from `get`.
    const cliList = async () =>
      (await cliRun(["issue", "wakeup", "list", issue.id, "--output", "json"]) as Array<{ id?: string; paused_reason?: string }>).filter(
        (r) => r.paused_reason === "loop",
      );
    if (cli) {
      const fromCli = await cliEffective("issue");
      expect(fromCli.execution.paused_reason).toBe("loop");
      expect(fromCli.execution.instance_id).toBe(eff.execution.instance_id);
      expect((await cliEffective("project")).execution ?? null).toBeNull();
    }
    if (cli) expect(await cliList()).toHaveLength(1);

    // Reset through the last surface's UI (desktop when present): the project
    // value comes back and the paused instance stays.
    const instanceId = eff.execution.instance_id;
    await reset(last);
    await expectState("Inherited from project", "Paused");
    eff = await effective();
    expect(eff.enabled).toBe(true);
    expect(eff.execution.instance_id).toBe(instanceId);
    // The same instance is still the one the CLI lists after the reset, and `get` reads the project value again.
    if (cli) {
      expect((await cliList()).map((r) => r.id)).toEqual([instanceId]);
      const fromCli = await cliEffective("issue");
      expect(fromCli.enabled).toBe(true);
      expect(fromCli.sources.enabled).toBe("project");
      expect(fromCli.execution.instance_id).toBe(instanceId);
    }

    // Override again through the last surface, then reset through the web.
    // The row shows the paused runtime state over the disabled setting.
    await override(last);
    await expectState("Overridden on this issue", "Paused");
    expect((await effective()).sources.enabled).toBe("issue");
    if (cli) expect((await cliEffective("issue")).sources.enabled).toBe("issue");
    await reset(web);
    await expectState("Inherited from project");
    expect((await effective()).enabled).toBe(true);
    if (cli) expect((await cliEffective("issue")).sources.enabled).toBe("project");
  } finally {
    await app?.close();
    if (workspaceId) {
      if (wakeupId) await db.query(`DELETE FROM issue_wakeup WHERE id=$1 AND workspace_id=$2`, [wakeupId, workspaceId]);
      if (agentId) await db.query(`DELETE FROM agent WHERE id=$1 AND workspace_id=$2`, [agentId, workspaceId]);
      for (const scopeId of [issueId, projectId]) {
        if (scopeId) {
          await db.query(`DELETE FROM issue_wakeup_definition WHERE workspace_id=$1 AND scope_id=$2 AND rule_key=$3`, [workspaceId, scopeId, RULE]);
        }
      }
      // The workspace row exists only because this run wrote it.
      if (!hadWorkspaceDefinition) {
        await db.query(`DELETE FROM issue_wakeup_definition WHERE workspace_id=$1 AND scope_kind='workspace' AND scope_id=$1 AND rule_key=$2`, [workspaceId, RULE]);
      }
      if (settingsBefore !== undefined) {
        await db.query(`UPDATE workspace SET settings=$1::jsonb WHERE id=$2`, [JSON.stringify(settingsBefore), workspaceId]);
      }
    }
    await api.cleanup();
    await db.end();
  }
});
