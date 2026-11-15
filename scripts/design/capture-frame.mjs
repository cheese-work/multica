#!/usr/bin/env node
// Pattern frame capture (CHE-5420 S4a). Run on X99 only (headless Chrome, never a laptop).
//
//   node scripts/design/capture-frame.mjs --url <page> --id <pattern id> --out <dir> \
//        [--component <path>]... [--token <name>]... [--copy <label=ns:key>]...
//
// Writes <out>/<id>-1440.png, <out>/<id>-390.png and <out>/<id>.html. The HTML is one
// self-contained doop frame: both screenshots inlined as data URIs, plus the frame notes
// (component paths, tokens, i18n keys). No network fetch, no external font or script.
// Fails when a capture is blank. Zero dependencies: Node 22 and google-chrome.
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { inflateSync } from "node:zlib";

export const WIDTHS = [1440, 390];
const HEIGHT = 900;
const ID_RE = /^[a-z0-9][a-z0-9-]*$/;
const esc = (s) =>
  String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// Decode an 8-bit non-interlaced PNG and report whether every pixel is identical.
export function isBlankPng(png) {
  if (png.readUInt32BE(12) !== 0x49484452) throw new Error("not a PNG: no IHDR");
  const w = png.readUInt32BE(16);
  const h = png.readUInt32BE(20);
  const [depth, colorType, , , interlace] = [png[24], png[25], png[26], png[27], png[28]];
  const channels = { 0: 1, 2: 3, 4: 2, 6: 4 }[colorType];
  if (depth !== 8 || !channels || interlace !== 0) throw new Error("unsupported PNG layout");
  const idat = [];
  for (let p = 8; p < png.length; ) {
    const len = png.readUInt32BE(p);
    if (png.toString("latin1", p + 4, p + 8) === "IDAT") idat.push(png.subarray(p + 8, p + 8 + len));
    p += 12 + len;
  }
  const raw = inflateSync(Buffer.concat(idat));
  const bpp = channels;
  const stride = w * bpp;
  let prev = Buffer.alloc(stride);
  let first = null;
  for (let y = 0; y < h; y++) {
    const filter = raw[y * (stride + 1)];
    const line = Buffer.from(raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1)));
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? line[i - bpp] : 0;
      const b = prev[i];
      const c = i >= bpp ? prev[i - bpp] : 0;
      const pa = Math.abs(b - c), pb = Math.abs(a - c), pc = Math.abs(a + b - 2 * c);
      const paeth = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      line[i] = (line[i] + [0, a, b, (a + b) >> 1, paeth][filter]) & 255;
    }
    for (let x = 0; x < w; x++) {
      const px = line.subarray(x * bpp, x * bpp + bpp);
      if (first === null) first = Buffer.from(px);
      else if (!first.equals(px)) return false;
    }
    prev = line;
  }
  return true;
}

export function buildFrameHtml(notes, pngBase64ByWidth) {
  const list = (items) => (items?.length ? `<ul>${items.map((i) => `<li><code>${esc(i)}</code></li>`).join("")}</ul>` : "<p>none</p>");
  const copy = Object.entries(notes.copy ?? {}).map(([label, key]) => `${label}: ${key}`);
  const shots = Object.entries(pngBase64ByWidth)
    .sort(([a], [b]) => b - a)
    .map(([w, b64]) => `<figure><figcaption>${esc(w)}px</figcaption><img alt="${esc(notes.id)} at ${esc(w)}px" src="data:image/png;base64,${b64}"></figure>`)
    .join("");
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>${esc(notes.id)}</title>
<style>body{margin:0;padding:24px;font:14px/1.5 system-ui,sans-serif;color:#111;background:#fff}
h1{font-size:18px}h2{font-size:14px;margin:16px 0 4px}figure{margin:16px 0}figcaption{font-weight:600}
img{max-width:100%;border:1px solid #ddd}code{font:12px ui-monospace,monospace}</style></head>
<body><h1>${esc(notes.id)}</h1>
${shots}
<h2>Components</h2>${list(notes.components)}
<h2>Tokens</h2>${list(notes.tokens)}
<h2>i18n keys</h2>${list(copy)}
</body></html>
`;
}

const chromeBin = () => process.env.CHROME_BIN || "google-chrome";

function shoot(url, width, file, height = HEIGHT) {
  const profile = mkdtempSync(join(tmpdir(), "capture-chrome-"));
  try {
    const r = spawnSync(
      chromeBin(),
      ["--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars",
        // A page with an input makes Chrome ask the OS keyring; with no keyring on X99 that blocks forever.
        "--password-store=basic", "--use-mock-keychain", `--user-data-dir=${profile}`,
        `--window-size=${width},${height}`, "--virtual-time-budget=5000", `--screenshot=${file}`, url],
      { encoding: "utf8", timeout: 60_000 },
    );
    if (r.status !== 0) throw new Error(`chrome failed (${r.status ?? r.signal}): ${r.stderr?.slice(-300)}`);
  } finally {
    rmSync(profile, { recursive: true, force: true });
  }
}

export async function captureFrame({ url, outDir, notes, widths = WIDTHS, height = HEIGHT }) {
  if (!ID_RE.test(notes?.id ?? "")) throw new Error(`invalid pattern id: ${notes?.id}`);
  mkdirSync(outDir, { recursive: true });
  const b64 = {};
  for (const w of widths) {
    const file = join(outDir, `${notes.id}-${w}.png`);
    shoot(url, w, file, height);
    const png = readFileSync(file);
    if (isBlankPng(png)) throw new Error(`blank capture at ${w}px: ${url}`);
    b64[w] = png.toString("base64");
  }
  writeFileSync(join(outDir, `${notes.id}.html`), buildFrameHtml(notes, b64));
  return { widths, html: join(outDir, `${notes.id}.html`) };
}

function parseArgs(argv) {
  const o = { components: [], tokens: [], copy: {} };
  for (let i = 0; i < argv.length; i += 2) {
    const [k, v] = [argv[i], argv[i + 1]];
    if (v === undefined) throw new Error(`missing value for ${k}`);
    if (k === "--url") o.url = v;
    else if (k === "--id") o.id = v;
    else if (k === "--out") o.out = v;
    else if (k === "--height") o.height = Number(v); // taller viewport for a page that outgrows 900px
    else if (k === "--component") o.components.push(v);
    else if (k === "--token") o.tokens.push(v);
    else if (k === "--copy") { const [l, ...key] = v.split("="); o.copy[l] = key.join("="); }
    else throw new Error(`unknown flag ${k}`);
  }
  if (!o.url || !o.id || !o.out) throw new Error("usage: capture-frame.mjs --url <page> --id <pattern id> --out <dir>");
  return o;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    const o = parseArgs(process.argv.slice(2));
    const r = await captureFrame({
      url: o.url, outDir: o.out, ...(o.height > 0 ? { height: o.height } : {}),
      notes: { id: o.id, components: o.components, tokens: o.tokens, copy: o.copy },
    });
    console.log(`wrote ${r.html}`);
  } catch (e) {
    console.error(e.message);
    process.exit(1);
  }
}
