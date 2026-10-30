// Static, design-only measurement of why rev-1/B-390.png wraps "after." while the
// product-harness screenshot keeps "now and after." on one line (CHE-1827).
// Loads the retained rev-1/B.html from disk in headless Chromium and varies ONE
// factor at a time by injected CSS. No product app, API, database or fixture.
// Usage (from a checkout with node_modules): node measure-b-wrap.mjs <b.html> <inter.woff2> <out.json>
import { chromium } from "@playwright/test";
import { writeFileSync } from "node:fs";
const [bHtml, interFont, out] = process.argv.slice(2);
const FONT = `@font-face{font-family:"Inter Variable";src:url("file://${interFont}") format("woff2");font-weight:100 900}`;
const NO_CHROME = ".bubble{padding:10px 0;border:0;border-radius:0;background:none}";
const variants = {
  "as-retained": "",
  "bubble-chrome-removed": NO_CHROME,
  "inter-font": `${FONT} body,.bubble{font-family:"Inter Variable"}`,
  "bubble-chrome-removed+inter": `${FONT} body,.bubble{font-family:"Inter Variable"} ${NO_CHROME}`,
};
const measure = () => {
  const bubble = document.querySelectorAll(".bubble")[0]; // state 1, pending
  const p = bubble.querySelector("p");
  const cs = getComputedStyle(p);
  const pr = p.getBoundingClientRect();
  const chip = p.querySelector(".issue-mention");
  const chipRect = chip.getBoundingClientRect();
  const chipCs = getComputedStyle(chip);
  const chipBox = chipRect.width + parseFloat(chipCs.marginLeft) + parseFloat(chipCs.marginRight);
  const texts = [...p.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE);
  const before = texts[0];
  const after = texts[1];
  const lineTop = (node, needle) => {
    const r = document.createRange();
    const i = node.textContent.indexOf(needle);
    r.setStart(node, i);
    r.setEnd(node, i + needle.length);
    return Math.round(r.getBoundingClientRect().top);
  };
  const lastWordOnFirstLine = lineTop(after, "after.") === lineTop(before, "see");
  // Text widths are measured with wrapping off, so a wrapped fragment is not undercounted.
  const probe = document.createElement("span");
  probe.style.cssText = "white-space:pre;position:absolute;visibility:hidden";
  p.appendChild(probe);
  const textWidth = (t) => {
    probe.textContent = t;
    return probe.getBoundingClientRect().width;
  };
  const natural = textWidth(before.textContent) + chipBox + textWidth(after.textContent);
  probe.remove();
  const q = (n) => Math.round(n * 10) / 10;
  return { family: cs.fontFamily.slice(0, 40), fontSize: cs.fontSize, innerWidth: q(pr.width), chipWidth: q(chipRect.width), sentenceNeeds: q(natural), lastWordOnFirstLine, paragraphHeight: q(pr.height) };
};
const browser = await chromium.launch();
const rows = [];
for (const [name, css] of Object.entries(variants)) {
  const page = await browser.newPage({ viewport: { width: 390, height: 960 } });
  await page.goto(`file://${bHtml}`);
  if (css) await page.addStyleTag({ content: css });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(150);
  rows.push({ variant: name, ...(await page.evaluate(measure)) });
  await page.close();
}
await browser.close();
writeFileSync(out, JSON.stringify(rows, null, 1));
for (const r of rows) {
  console.log(`${r.variant}: ${r.family} ${r.fontSize}; inner ${r.innerWidth}px; sentence needs ${r.sentenceNeeds}px; "after." on first line=${r.lastWordOnFirstLine}; paragraph ${r.paragraphHeight}px`);
}
