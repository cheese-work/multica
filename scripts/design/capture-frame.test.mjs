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

test("height option makes a taller capture", async () => {
  const out = join(dir, "tall");
  await captureFrame({ url: pathToFileURL(fixture).href, outDir: out, notes, height: 1200 });
  const png = readFileSync(join(out, "inline-retry-1440.png"));
  assert.equal(png.readUInt32BE(20), 1200, "PNG height from the IHDR chunk");
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

test("launches Chrome with the keyring bypass flags (a page with an input otherwise hangs headless Chrome)", async () => {
  const argsFile = join(dir, "chrome-args.txt");
  const fake = join(dir, "fake-chrome.sh");
  writeFileSync(fake, `#!/usr/bin/env bash\nprintf '%s\\n' "$@" > "${argsFile}"\nexit 0\n`, { mode: 0o755 });
  const prev = process.env.CHROME_BIN;
  process.env.CHROME_BIN = fake;
  try {
    // The fake writes no PNG, so the capture itself fails; only the launch flags matter here.
    await assert.rejects(captureFrame({ url: "file:///x.html", outDir: join(dir, "out-args"), notes, widths: [1440] }));
  } finally {
    if (prev === undefined) delete process.env.CHROME_BIN;
    else process.env.CHROME_BIN = prev;
  }
  const args = readFileSync(argsFile, "utf8").split("\n");
  assert.ok(args.includes("--password-store=basic"), "missing --password-store=basic");
  assert.ok(args.includes("--use-mock-keychain"), "missing --use-mock-keychain");
});
