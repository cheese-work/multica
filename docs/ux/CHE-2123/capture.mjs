import { createServer } from "node:http";
import { execFileSync, spawn } from "node:child_process";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { resolve, join } from "node:path";
import { chromium, _electron } from "@playwright/test";

const root = process.cwd();
const output = resolve(root, process.env.CAPTURE_OUTPUT ?? "evidence/che-2123/captures");
const revision = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
const sourcePhase = process.env.CAPTURE_SOURCE_PHASE;
const home = "/tmp/che-2123-capture-home";
mkdirSync(output, { recursive: true });
mkdirSync(home, { recursive: true });
const apiOrigin = "http://127.0.0.1:31954";
const webOrigin = "http://127.0.0.1:31953";
const chart = "flowchart LR\n  A[Open dialog] --> B[Check source access]\n  B --> C[Media only]\n  C --> D[Copy link]";
const table = "| Media | Web | Desktop |\n| --- | --- | --- |\n| Diagram | New tab | New window |\n| Table | Sort and filter | Sort and filter |\n| Image | Zoom and fit | Zoom and fit |\n| HTML | Sandboxed | Sandboxed |";
const html = '<!doctype html><html><head><style>body{font:16px system-ui;margin:0;padding:56px;background:#f7f8fa;color:#172033}main{max-width:760px;margin:auto;background:white;border:1px solid #dde2e9;border-radius:16px;padding:36px}h1{font-size:28px;margin:0 0 12px}.row{display:flex;align-items:center;gap:16px;margin:24px 0}.bar{height:24px;background:#5d78e7;border-radius:5px}span{width:120px}small{color:#586477}</style></head><body><main><small>VIEWER MEDIA</small><h1>One item. Its own page.</h1><p>No issue thread or application chrome.</p><div class="row"><span>Web tab</span><div class="bar" style="width:320px"></div></div><div class="row"><span>App window</span><div class="bar" style="width:260px;background:#3c9f81"></div></div><p>Reload and share the same permission-checked link.</p></main></body></html>';
const imageUrl = `${apiOrigin}/fixture/image.png`;
const content = `# Capture fixture\n\nSurrounding issue text must not appear on a media page.\n\n\`\`\`mermaid\n${chart}\n\`\`\`\n\n${table}\n\n![Media fixture](${imageUrl})\n\n\`\`\`html\n${html}\n\`\`\``;
const issue = { id: "issue-1", workspace_id: "ws-1", number: 1, identifier: "CAP-1", title: "Viewer media fixture", description: `Saved issue-description media.\n\n\`\`\`mermaid\n${chart}\n\`\`\`\n\n\`\`\`html\n${html}\n\`\`\``, status: "in_progress", priority: "medium", assignee_type: null, assignee_id: null, creator_type: "member", creator_id: "user-1", parent_issue_id: null, project_id: null, position: 0, start_date: null, due_date: null, metadata: {}, properties: {}, created_at: "2026-10-27T00:00:00Z", updated_at: "2026-10-27T00:00:00Z" };
const comment = { id: "comment-1", issue_id: issue.id, type: "comment", author_type: "member", author_id: "user-1", content, parent_id: null, attachments: [], reactions: [], revision: 1, created_at: issue.created_at, updated_at: issue.updated_at };
const timeline = { ...comment, type: "comment", actor_type: "member", actor_id: "user-1", actor_name: "Capture fixture" };
const user = { id: "user-1", name: "Capture fixture", email: "capture@example.invalid", onboarded_at: "2026-10-27T00:00:00Z", language: "en" };
const workspace = { id: "ws-1", slug: "capture", name: "Capture fixture", description: null, context: null, settings: {}, repos: [], issue_prefix: "CAP", avatar_url: null, created_at: "", updated_at: "" };
const requests = [];
let denied = false;
async function waitForMedia(surface, kind) {
  if (kind === "mermaid") await surface.frameLocator("iframe").locator("svg").waitFor({ state: "visible", timeout: 120000 });
  else if (kind === "html") await surface.frameLocator("iframe").getByRole("heading", { name: "One item. Its own page.", exact: true }).waitFor();
  else if (kind === "image") await surface.locator("img").first().evaluate((image) => image.decode());
  else await surface.getByText("Diagram", { exact: true }).waitFor();
}
const server = createServer((request, response) => {
  const url = new URL(request.url, apiOrigin);
  requests.push(url.pathname);
  response.setHeader("Access-Control-Allow-Origin", request.headers.origin ?? "*");
  response.setHeader("Access-Control-Allow-Credentials", "true");
  response.setHeader("Access-Control-Allow-Headers", request.headers["access-control-request-headers"] ?? "Authorization, Content-Type, X-Workspace-ID");
  response.setHeader("Access-Control-Allow-Methods", "GET,POST,PATCH,OPTIONS");
  if (request.method === "OPTIONS") { response.writeHead(204).end(); return; }
  if (url.pathname === "/fixture/image.png") {
    response.setHeader("Content-Type", "image/png");
    response.end(readFileSync(join(root, "design/viewer-media-pages/rev-1/A-1440.png")));
    return;
  }
  response.setHeader("Content-Type", "application/json");
  url.pathname = url.pathname.replace("/api/issues/CAP-1", "/api/issues/issue-1");
  if (denied && url.pathname.startsWith("/api/issues/issue-1")) { response.writeHead(404).end(JSON.stringify({ error: "not found" })); return; }
  let body = [];
  if (url.pathname === "/api/me") body = user;
  else if (url.pathname === "/api/auth/refresh") body = { renewed: false, expires_at: "", check_again_in_seconds: 3600 };
  else if (url.pathname === "/api/config") body = { auth: { google_enabled: false }, cdn_domain: "", features: {}, environment: "self_hosted" };
  else if (url.pathname === "/api/workspaces") body = [workspace];
  else if (url.pathname === "/api/workspaces/ws-1") body = workspace;
  else if (url.pathname === "/api/issues/issue-1") body = issue;
  else if (url.pathname === "/api/issues/issue-1/comments") body = [comment];
  else if (url.pathname === "/api/issues/issue-1/timeline") body = [timeline];
  else if (url.pathname === "/api/issues") body = { issues: [issue], total: 1, limit: 50, offset: 0 };
  response.end(JSON.stringify(body));
});
const digest = (text) => createHash("sha256").update(text.trim()).digest("hex");
const items = [["mermaid", chart], ["table", table], ["image", imageUrl], ["html", html]].map(([kind, value]) => ({ kind, path: `/capture/media/issues/issue-1/${kind}/${digest(value)}` }));
let web;
let browser;
let electron;
let logs = "";
try {
  await new Promise((ready) => server.listen(31954, "127.0.0.1", ready));
  if (process.env.CAPTURE_WEB !== "0") {
  web = spawn("pnpm", ["--filter", "@multica/web", "dev"], { cwd: root, env: { ...process.env, FRONTEND_PORT: "31953", REMOTE_API_URL: apiOrigin, NEXT_PUBLIC_API_URL: apiOrigin, CORS_ALLOWED_ORIGINS: "http://127.0.0.1" }, stdio: ["ignore", "pipe", "pipe"], detached: true });
  web.stdout.on("data", (data) => { logs += data; });
  web.stderr.on("data", (data) => { logs += data; });
  const deadline = Date.now() + 180000;
  while (!logs.includes("Ready in")) {
    if (web.exitCode != null || Date.now() > deadline) throw new Error(`Web startup failed: ${logs}`);
    await new Promise((ready) => setTimeout(ready, 250));
  }
  writeFileSync(join(output, "web-startup.txt"), logs);
  browser = await chromium.launch({ executablePath: "/usr/bin/google-chrome", headless: true, args: ["--no-sandbox"] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, reducedMotion: "reduce", permissions: ["clipboard-read", "clipboard-write"] });
  await context.addInitScript(() => { try { localStorage.setItem("multica_token", "capture-fixture-only"); localStorage.setItem("multica-theme", "light"); } catch {} });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => { if (message.type() === "error") console.log("WEB console", message.text().slice(0, 300)); });
  page.on("requestfailed", (request) => console.log("WEB request failed", request.url(), request.failure()?.errorText));
  if (sourcePhase) {
    const widths = process.env.CAPTURE_SOURCE_WIDTH ? [Number(process.env.CAPTURE_SOURCE_WIDTH)] : [1440, 390];
    const sourceItems = process.env.CAPTURE_SOURCE_KIND ? items.filter((item) => item.kind === process.env.CAPTURE_SOURCE_KIND) : items;
    if (!sourceItems.length) throw new Error("Unknown source capture kind");
    for (const width of widths) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      for (const item of sourceItems) {
        console.log("SOURCE", sourcePhase, width, item.kind);
        await page.goto(`${webOrigin}/capture/issues/CAP-1`, { timeout: 1200000, waitUntil: "domcontentloaded" });
        const source = page.locator("[data-comment-content='comment-1']").first();
        await source.waitFor({ timeout: 180000 });
        if (item.kind === "mermaid" || item.kind === "html") {
          await source.locator("[data-rich-block-shell]").nth(item.kind === "mermaid" ? 0 : 1).scrollIntoViewIfNeeded();
        }
        const trigger = item.kind === "table" ? source.getByRole("button", { name: "Expand table", exact: true })
          : item.kind === "image" ? source.getByAltText("Media fixture", { exact: true })
          : source.locator(`[data-dynamic-block='${item.kind}']`).getByRole("button", { name: "Fullscreen", exact: true });
        await trigger.waitFor({ timeout: 180000 });
        if (item.kind === "mermaid" || item.kind === "html") await waitForMedia(source.locator(`[data-dynamic-block='${item.kind}']`), item.kind);
        await trigger.click();
        const dialog = page.getByRole("dialog");
        await dialog.waitFor({ timeout: 120000 });
        await waitForMedia(dialog, item.kind);
        await page.screenshot({ path: join(output, `source-${item.kind}-${width}-${sourcePhase}.png`), animations: "disabled" });
        const more = dialog.locator("summary[aria-label='More']");
        if (sourcePhase === "after" && width === 390) {
          const close = dialog.getByRole("button", { name: "Close", exact: true });
          if (await close.evaluate((button) => !!button.closest("details"))) throw new Error("Source Close is hidden inside More");
          await close.focus();
          if (!(await close.evaluate((button) => document.activeElement === button))) throw new Error("Source Close is not focusable");
          await more.focus();
          await page.keyboard.press("Enter");
          await page.keyboard.press("Tab");
          if (!(await page.evaluate(() => document.activeElement?.tagName === "BUTTON" && !!document.activeElement.closest("details")))) throw new Error("Source More lacks a keyboard path");
          await page.screenshot({ path: join(output, `source-${item.kind}-${width}-menu-after.png`) });
          await page.keyboard.press("Escape");
          if (!(await more.evaluate((element) => document.activeElement === element && !element.closest("details").open))) throw new Error("Source More did not close and restore focus");
        }
        if (sourcePhase === "after" && width === 1440) {
          const [target] = await Promise.all([
            page.waitForEvent("popup", { timeout: 1200000 }),
            dialog.getByRole("button", { name: "Open in new tab", exact: true }).click(),
          ]);
          await target.getByTestId("media-toolbar").waitFor({ timeout: 120000 });
          if (target.url() !== `${webOrigin}${item.path}`) throw new Error(`Wrong source link: ${target.url()}`);
          await target.close();
        }
        await page.keyboard.press("Escape");
        await dialog.waitFor({ state: "hidden" });
      }
      if (sourcePhase === "after") {
        for (const item of items.filter((item) => item.kind === "mermaid" || item.kind === "html")) {
          console.log("DESCRIPTION", width, item.kind);
          await page.goto(`${webOrigin}/capture/issues/CAP-1`, { timeout: 1200000, waitUntil: "domcontentloaded" });
          const description = page.locator("[data-comment-content^='description:']").first();
          await description.waitFor({ timeout: 180000 });
          const expand = page.getByRole("button", { name: /^Show more/ }).first();
          if (await expand.isVisible()) await expand.click();
          const block = description.locator(".code-block-wrapper").nth(item.kind === "mermaid" ? 0 : 1);
          const trigger = block.getByRole("button", { name: item.kind === "mermaid" ? "Open diagram viewer" : "Fullscreen", exact: true });
          await trigger.scrollIntoViewIfNeeded();
          await trigger.focus();
          await trigger.press("Enter");
          const dialog = page.getByRole("dialog");
          await waitForMedia(dialog, item.kind);
          if (width === 390) await dialog.locator("summary[aria-label='More']").click();
          const action = dialog.getByRole("button", { name: "Open in new tab", exact: true });
          await action.waitFor();
          await page.screenshot({ path: join(output, `description-${item.kind}-${width}-after.png`), animations: "disabled" });
          const [target] = await Promise.all([
            page.waitForEvent("popup", { timeout: 1200000 }),
            action.click(),
          ]);
          await target.getByTestId("media-toolbar").waitFor({ timeout: 120000 });
          await waitForMedia(target, item.kind);
          if (target.url() !== `${webOrigin}${item.path}`) throw new Error(`Wrong saved description link: ${target.url()}`);
          await target.close();
          await dialog.getByRole("button", { name: "Close", exact: true }).click();
          await dialog.waitFor({ state: "hidden" });
        }
      }
    }
    console.log(`SOURCE PASS: ${sourcePhase}; ${sourceItems.length} kinds; widths ${widths.join(",")}; Escape dismissal`);
    await page.setViewportSize({ width: 1440, height: 900 });
  }
  if (process.env.CAPTURE_MEDIA !== "0") {
  for (const item of items) {
    console.log("WEB", item.kind, item.path);
    await page.goto(`${webOrigin}${item.path}`, { timeout: 600000, waitUntil: "domcontentloaded" });
    await page.getByTestId("media-toolbar").waitFor({ timeout: 120000 }).catch(async (error) => {
      console.log("WEB failure", await page.locator("body").innerText(), errors, requests);
      await page.screenshot({ path: join(output, "web-diagnostic.png") });
      throw error;
    });
    await waitForMedia(page, item.kind);
    await page.screenshot({ path: join(output, `web-${item.kind}-1440-after.png`) });
    await page.getByRole("button", { name: "Copy link", exact: true }).click();
    await page.getByRole("button", { name: "Link copied", exact: true }).waitFor();
    const copied = await page.evaluate(() => navigator.clipboard.readText());
    if (copied !== `${webOrigin}${item.path}`) throw new Error(`Wrong web copy link: ${copied}`);
    await page.reload();
    await page.getByTestId("media-toolbar").waitFor();
    await waitForMedia(page, item.kind);
    await page.setViewportSize({ width: 390, height: 844 });
    const more = page.locator("summary[aria-label='More']");
    await more.waitFor();
    await more.focus();
    await page.keyboard.press("Enter");
    await page.keyboard.press("Tab");
    if (!(await page.evaluate(() => document.activeElement?.tagName === "BUTTON" && !!document.activeElement.closest("details")))) throw new Error("More has no keyboard path to its buttons");
    await page.screenshot({ path: join(output, `web-${item.kind}-390-menu-after.png`) });
    await page.keyboard.press("Escape");
    if (!(await more.evaluate((element) => document.activeElement === element))) throw new Error("More Escape did not restore focus");
    await page.screenshot({ path: join(output, `web-${item.kind}-390-after.png`) });
    await page.setViewportSize({ width: 1440, height: 900 });
  }
  const beforeDenied = requests.filter((path) => path === "/fixture/image.png").length;
  denied = true;
  await page.goto(`${webOrigin}${items.find((item) => item.kind === "image").path}`);
  await page.getByRole("heading", { name: "Page not found" }).waitFor();
  await page.getByRole("link", { name: "Back to Multica" }).waitFor();
  if (await page.locator("img,iframe").count()) throw new Error("Denied route rendered media");
  if (requests.filter((path) => path === "/fixture/image.png").length !== beforeDenied) throw new Error("Denied route fetched image bytes");
  await page.screenshot({ path: join(output, "web-denied-1440.png") });
  denied = false;
  console.log("WEB PASS: four kinds; reload; copy link; phone keyboard; denied route zero media requests");
  }
  writeFileSync(join(output, "web-page-errors.json"), JSON.stringify(errors, null, 2));
  }
  if (process.env.CAPTURE_DESKTOP !== "0") {
    electron = await _electron.launch({ executablePath: createRequire(resolve(root, "apps/desktop/package.json"))("electron"), args: ["--no-sandbox", "--disable-gpu", "--ozone-platform=headless", resolve(root, "apps/desktop/out/main/index.js")], env: { ...process.env, HOME: home, XDG_CONFIG_HOME: join(home, ".config"), DESKTOP_APP_SUFFIX: "CHE-2123-Capture" }, timeout: 300000 });
    const main = await electron.firstWindow();
    main.on("console", (message) => console.log("DESKTOP", message.type(), message.text().slice(0, 180)));
    await main.waitForURL(/^file:/, { timeout: 60000 });
    await main.waitForLoadState("domcontentloaded");
    await main.evaluate(() => localStorage.setItem("multica_token", "capture-fixture-only"));
    await main.reload();
    await main.waitForFunction(() => typeof window.desktopAPI?.openMediaWindow === "function", undefined, { timeout: 60000 });
    console.log("DESKTOP main body", (await main.locator("body").innerText()).slice(0, 1800));
    await main.screenshot({ path: join(output, "desktop-main-diagnostic.png") });
    for (const item of items) {
      console.log("DESKTOP", item.kind);
      const opened = electron.waitForEvent("window");
      const result = await main.evaluate((item) => window.desktopAPI.openMediaWindow({ path: item.path, title: ({ mermaid: "Mermaid diagram", table: "Table", image: "View image", html: "HTML preview" })[item.kind] }), item);
      if (!result.ok) throw new Error(`Desktop open rejected ${item.path}`);
      const target = await opened;
      await target.setViewportSize({ width: 960, height: 760 });
      await target.getByTestId("media-toolbar").waitFor({ timeout: 60000 }).catch(async (error) => {
        console.log("DESKTOP target failure", await target.locator("body").innerText(), requests);
        await target.screenshot({ path: join(output, "desktop-target-diagnostic.png") });
        throw error;
      });
      await waitForMedia(target, item.kind);
      await target.screenshot({ path: join(output, `desktop-${item.kind}-960-after.png`) });
      const more = target.locator("summary[aria-label='More']");
      if (await more.isVisible()) await more.click();
      await target.getByRole("button", { name: "Copy link", exact: true }).click();
      await target.getByRole("button", { name: "Link copied", exact: true }).waitFor();
      const copied = await electron.evaluate(({ clipboard }) => clipboard.readText());
      if (copied !== `${webOrigin}${item.path}`) throw new Error(`Wrong desktop copy link: ${copied}`);
      await target.reload();
      await target.getByTestId("media-toolbar").waitFor();
      await target.evaluate(() => window.desktopAPI.closeWindow());
    }
    console.log("DESKTOP PASS: native IPC windows; four kinds; reload; public copy links");
  }
  writeFileSync(join(output, "receipt.json"), JSON.stringify({ revision, sourcePhase: sourcePhase ?? "NOT-RUN", sourceKind: process.env.CAPTURE_SOURCE_KIND ?? "all", sourceWidth: process.env.CAPTURE_SOURCE_WIDTH ?? "both", savedDescription: sourcePhase === "after" ? "PASS" : "NOT-RUN", mediaPages: process.env.CAPTURE_MEDIA !== "0", items, web: browser ? "PASS" : "NOT-RUN", desktop: electron ? "PASS" : "NOT-RUN", fixtures: true, daemonAndUpdaterDisabled: true, requests }, null, 2));
} catch (error) {
  writeFileSync(join(output, "failure-requests.json"), JSON.stringify(requests, null, 2));
  for (const [index, page] of (browser?.contexts()[0]?.pages() ?? []).entries()) {
    await page.screenshot({ path: join(output, `failure-${index}.png`) });
    writeFileSync(join(output, `failure-${index}.txt`), `${page.url()}\n${await page.locator("body").innerText()}`);
  }
  throw error;
} finally {
  writeFileSync(join(output, "web-startup.txt"), logs);
  if (electron) await electron.close();
  if (browser) await browser.close();
  if (web && web.exitCode == null) process.kill(-web.pid, "SIGTERM");
  await new Promise((done) => server.close(done));
}
