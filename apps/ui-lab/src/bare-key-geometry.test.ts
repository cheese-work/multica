// Real-browser geometry for a bare issue key (CHE-1829). jsdom has no layout, so
// this drives the production render path in headless Chromium through
// `geometry/main.tsx` and compares the box before and after the lookup settles.
// Skipped, with a reason, when no Chromium can be launched.
import { afterAll, beforeAll, describe, expect, it } from "vitest";
// Root devDependency; the browser binary is the one the e2e suite already uses.
// eslint-disable-next-line import-x/no-extraneous-dependencies
import { chromium, type Browser } from "@playwright/test";
import { createServer, type ViteDevServer } from "vite";

declare global {
  interface Window {
    __release: () => void;
    __getIssueCalls: string[];
  }
}

const TITLES = {
  short: "Short title",
  medium: "fix(db): resolve duplicate migration prefix",
  long: "A very long issue title that will certainly exceed the eighteen rem cap of the chip and must truncate",
};
const VIEWPORTS = { desktop: 1280, mobile: 390 } as const;
const SURFACES = ["chat", "description"] as const;

interface Box {
  readonly kind: "loading" | "chip" | "none";
  readonly inLink: boolean;
  readonly width: number;
  readonly height: number;
  readonly top: number;
  readonly hostHeight: number;
  readonly trailingTextX: number;
  readonly trailingTextTop: number;
}

/** Runs in the page. Reads the box and where the text after it landed. */
function measure(): Box {
  const host = document.getElementById("host")!;
  const hostRect = host.getBoundingClientRect();
  const loading = document.querySelector("[data-issue-chip-loading]");
  const el = loading ?? document.querySelector("a .issue-mention");
  const rect = el?.getBoundingClientRect();
  const walker = document.createTreeWalker(host, NodeFilter.SHOW_TEXT);
  let trailing: Text | null = null;
  while (walker.nextNode()) {
    if (walker.currentNode.textContent?.includes("now and after")) trailing = walker.currentNode as Text;
  }
  const range = document.createRange();
  if (trailing) {
    const start = trailing.textContent!.indexOf("now");
    range.setStart(trailing, start);
    range.setEnd(trailing, start + 3);
  }
  const trailingRect = range.getBoundingClientRect();
  return {
    kind: loading ? "loading" : el ? "chip" : "none",
    inLink: !!el?.closest("a"),
    width: rect?.width ?? 0,
    height: rect?.height ?? 0,
    top: (rect?.top ?? 0) - hostRect.top,
    hostHeight: hostRect.height,
    trailingTextX: trailingRect.left,
    trailingTextTop: trailingRect.top - hostRect.top,
  };
}

let server: ViteDevServer | undefined;
let browser: Browser | undefined;
let origin = "";

beforeAll(async () => {
  try {
    browser = await chromium.launch();
  } catch {
    return;
  }
  server = await createServer({
    configFile: new URL("../geometry/vite.config.ts", import.meta.url).pathname,
    server: { port: 0, host: "127.0.0.1" },
    logLevel: "error",
  });
  await server.listen();
  origin = server.resolvedUrls!.local[0]!;
}, 60_000);

afterAll(async () => {
  await browser?.close();
  await server?.close();
});

async function render(width: number, query: Record<string, string>) {
  const page = await browser!.newPage({ viewport: { width, height: 800 } });
  await page.goto(`${origin}index.html?${new URLSearchParams(query)}`);
  await page.waitForSelector("[data-issue-chip-loading]", { timeout: 20_000 });
  return page;
}

describe("bare issue key box", () => {
  for (const [viewport, width] of Object.entries(VIEWPORTS)) {
    for (const surface of SURFACES) {
      for (const [name, title] of Object.entries(TITLES)) {
        it(`keeps its size and the text after it when the lookup resolves: ${viewport} ${surface} ${name} title`, async (ctx) => {
          ctx.skip(!browser);
          const page = await render(width, { surface, title });
          const pending = await page.evaluate(measure);
          await page.evaluate(() => window.__release());
          await page.waitForSelector("a .issue-mention", { timeout: 20_000 });
          await page.waitForTimeout(100);
          const resolved = await page.evaluate(measure);
          const calls = await page.evaluate(() => window.__getIssueCalls);
          await page.close();

          expect(pending.kind).toBe("loading");
          expect(pending.inLink).toBe(false);
          expect(resolved.kind).toBe("chip");
          expect(calls).toHaveLength(1);
          expect(resolved.width).toBeCloseTo(pending.width, 0);
          expect(resolved.height).toBeCloseTo(pending.height, 0);
          expect(resolved.top).toBeCloseTo(pending.top, 0);
          expect(resolved.hostHeight).toBeCloseTo(pending.hostHeight, 0);
          expect(resolved.trailingTextX).toBeCloseTo(pending.trailingTextX, 0);
          expect(resolved.trailingTextTop).toBeCloseTo(pending.trailingTextTop, 0);
          // Existing narrow-container bound stays.
          expect(resolved.width).toBeLessThanOrEqual(width);
        }, 60_000);
      }
    }
  }

  it("settles a missing key to plain text with no box and no link", async (ctx) => {
    ctx.skip(!browser);
    const page = await render(1280, { outcome: "missing" });
    await page.evaluate(() => window.__release());
    await page.waitForFunction(() => !document.querySelector("[data-issue-chip-loading]"), undefined, { timeout: 20_000 });
    const text = await page.locator("#host").innerText();
    const hasBoxOrLink = await page.evaluate(() => !!document.querySelector("#host a, #host .issue-mention"));
    await page.close();

    expect(text).toBe("see MUL-7 now and after.");
    expect(hasBoxOrLink).toBe(false);
  }, 60_000);
});
