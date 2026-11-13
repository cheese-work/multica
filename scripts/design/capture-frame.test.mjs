import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";

import { buildFrameHtml, captureFrame, isBlankPng } from "./capture-frame.mjs";

const dir = mkdtempSync(join(tmpdir(), "capture-frame-"));
const fixture = join(dir, "fixture.html");
writeFileSync(
  fixture,
  `<!doctype html><meta charset="utf-8"><body style="margin:0;background:#fff">
   <button style="margin:40px;padding:16px 32px;background:#0a7;color:#fff;font:20px sans-serif">Retry</button>
   <p style="font:16px sans-serif;margin:40px">Something went wrong.</p></body>`,
);
const blank = join(dir, "blank.html");
writeFileSync(blank, `<!doctype html><body style="margin:0;background:#fff"></body>`);

const notes = {
  id: "inline-retry",
  components: ["packages/ui/components/ui/button.tsx"],
  tokens: ["--primary"],
  copy: { action: "chat:needs_you.retry" },
};

test("captures a fixture page at 1440 and 390 as non-blank PNGs plus frame HTML", async () => {
  const out = join(dir, "out");
  const r = await captureFrame({ url: pathToFileURL(fixture).href, outDir: out, notes });
  assert.deepEqual(r.widths, [1440, 390]);
  for (const w of [1440, 390]) {
    const png = readFileSync(join(out, `inline-retry-${w}.png`));
    assert.equal(png.subarray(1, 4).toString(), "PNG");
    assert.equal(isBlankPng(png), false, `${w}px capture is blank`);
  }
  const html = readFileSync(join(out, "inline-retry.html"), "utf8");
  assert.match(html, /^<!doctype html>/i);
  assert.match(html, /data:image\/png;base64,/);
  assert.match(html, /packages\/ui\/components\/ui\/button\.tsx/);
  assert.match(html, /chat:needs_you\.retry/);
  assert.doesNotMatch(html, /(src|href)="https?:/, "frame HTML must not fetch from the network");
});

test("flags a blank capture", async () => {
  const out = join(dir, "blank-out");
  await assert.rejects(
    captureFrame({ url: pathToFileURL(blank).href, outDir: out, notes }),
    /blank/i,
  );
});

test("frame HTML escapes note text", () => {
  const html = buildFrameHtml({ id: "x", components: ["<script>alert(1)</script>"] }, { 1440: "AAAA" });
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /&lt;script&gt;/);
});

test("rejects an id that is not a plain pattern id", async () => {
  await assert.rejects(
    captureFrame({ url: "about:blank", outDir: join(dir, "x"), notes: { ...notes, id: "../evil" } }),
    /id/i,
  );
  assert.equal(existsSync(join(dir, "evil.html")), false);
});
