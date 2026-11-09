import { createServer } from "node:http";
import { readFileSync, mkdirSync, existsSync } from "node:fs";
import { resolve, join, extname } from "node:path";
import { chromium } from "@playwright/test";

// Captures the CHE-3483 decision-card states from the built harness (see README.md). X99 only.
const dist = resolve("docs/ux/CHE-3483/harness/dist");
const out = resolve(process.env.CAPTURE_OUTPUT ?? "/tmp/che3483-captures");
mkdirSync(out, { recursive: true });
const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".woff2": "font/woff2", ".woff": "font/woff", ".wasm": "application/wasm" };
const server = createServer((req, res) => {
  const path = new URL(req.url, "http://x").pathname;
  const file = join(dist, path === "/" ? "index.html" : path);
  if (!file.startsWith(dist) || !existsSync(file)) { res.writeHead(404).end(); return; }
  res.writeHead(200, { "Content-Type": types[extname(file)] ?? "application/octet-stream" }).end(readFileSync(file));
});
await new Promise((ok) => server.listen(0, "127.0.0.1", ok));
const origin = `http://127.0.0.1:${server.address().port}`;

const steps = {
  closed: async () => {},
  changes: async (p) => { await p.getByTestId("config-change-tab-changes").click(); },
  result: async (p) => { await p.getByTestId("config-change-tab-result").click(); },
  refused: async (p) => { await p.getByRole("button", { name: "Approve with passkey" }).click(); await p.getByRole("alert").waitFor(); },
};
const frames = [
  { name: "owner-context-closed", url: "s=context", step: "closed" },
  { name: "owner-context-changes", url: "s=context", step: "changes" },
  { name: "owner-instructions-changes", url: "s=instructions", step: "changes" },
  { name: "owner-settings-result", url: "s=settings", step: "result" },
  { name: "member-context-no-approve", url: "s=context&role=member", step: "changes" },
  { name: "owner-refused-payload-mismatch", url: "s=context&refuse=payload_mismatch", step: "refused" },
  { name: "owner-refused-class-needs-owner", url: "s=context&refuse=class_needs_owner", step: "refused" },
  { name: "owner-details-unavailable", url: "s=bad", step: "closed" },
];
const browser = await chromium.launch({ executablePath: "/usr/bin/google-chrome", headless: true });
const log = [];
try {
  for (const [width, height] of [[1440, 900], [390, 844]]) {
    for (const frame of frames) {
      const context = await browser.newContext({ viewport: { width, height }, reducedMotion: "reduce" });
      const page = await context.newPage();
      page.on("pageerror", (e) => log.push(`${frame.name}@${width} pageerror: ${e.message}`));
      await page.goto(`${origin}/?${frame.url}`);
      await page.getByTestId("decision-request-card").first().waitFor().catch(() => page.locator("main").waitFor());
      await steps[frame.step](page);
      await page.waitForTimeout(300);
      const file = join(out, `${frame.name}-${width}.png`);
      await page.screenshot({ path: file, fullPage: true, animations: "disabled" });
      log.push(`${file}`);
      await context.close();
    }
  }
} finally {
  await browser.close();
  server.close();
}
console.log(log.join("\n"));
