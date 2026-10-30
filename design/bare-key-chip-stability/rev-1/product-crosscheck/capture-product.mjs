// Product-render cross-check for the bare-key sizing design (CHE-1829).
// Run from apps/ui-lab of a cheese-work/multica checkout that has geometry/ (source head
// 8187ea0ccf245bfa8dad12b0efe2850896c66968): PROBE_OUT=<dir> node capture-product.mjs <label>
// Isolated harness (production RichContent + real styles, stubbed API). Not the supported
// product route, and not independent verification.
import { createServer } from "vite";
import { chromium } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
const label = process.argv[2], out = process.env.PROBE_OUT ?? ".";
mkdirSync(out, { recursive: true });
const server = await createServer({ configFile: `${process.cwd()}/geometry/vite.config.ts`, server: { port: 0, host: "127.0.0.1" }, logLevel: "error" });
await server.listen();
const origin = server.resolvedUrls.local[0];
const browser = await chromium.launch();
const titles = { short: "Short title", long: "A very long issue title that will certainly exceed the eighteen rem cap of the chip and must truncate" };
const measure = () => {
  const host = document.getElementById("host"), h = host.getBoundingClientRect();
  const el = document.querySelector("[data-issue-chip-loading]") ?? document.querySelector("a .issue-mention"), r = el.getBoundingClientRect();
  return { w: +r.width.toFixed(1), h: +r.height.toFixed(1), hostH: +h.height.toFixed(1) };
};
const rows = [];
for (const [vp, width] of Object.entries({ desktop: 1280, mobile: 390 })) for (const [tn, title] of Object.entries(titles)) {
  const page = await browser.newPage({ viewport: { width, height: 800 } });
  await page.goto(`${origin}index.html?${new URLSearchParams({ surface: "chat", title })}`);
  await page.waitForSelector("[data-issue-chip-loading]");
  const pending = await page.evaluate(measure);
  await page.screenshot({ path: `${out}/${label}-${vp}-${tn}-1-pending.png`, clip: { x: 0, y: 0, width, height: 80 } });
  await page.evaluate(() => window.__release());
  await page.waitForSelector("a .issue-mention"); await page.waitForTimeout(150);
  const resolved = await page.evaluate(measure);
  await page.screenshot({ path: `${out}/${label}-${vp}-${tn}-2-resolved.png`, clip: { x: 0, y: 0, width, height: 80 } });
  rows.push({ vp, title: tn, pending, resolved });
  await page.close();
}
await browser.close(); await server.close();
writeFileSync(`${out}/product-${label}.json`, JSON.stringify(rows, null, 1));
for (const r of rows) console.log(`${label} ${r.vp} ${r.title}: pending ${r.pending.w}x${r.pending.h} host ${r.pending.hostH} -> resolved ${r.resolved.w}x${r.resolved.h} host ${r.resolved.hostH}`);
