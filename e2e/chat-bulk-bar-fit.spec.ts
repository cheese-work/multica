import { createRequire } from "node:module";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { build } from "esbuild";
import postcss from "postcss";

/**
 * Real-Chromium layout test for the DM list bulk bar (CHE-5883). jsdom has no
 * layout, so this compiles the real Tailwind, bundles the real ChatBulkBar and
 * mounts it in a column of a given width with `page.setContent` (no app
 * server). The bar must never overflow its column: when the labelled buttons
 * do not fit it falls back to the lock's icon-only split buttons.
 * Run: `pnpm exec playwright test -c playwright.dm-bulk-bar.config.ts`.
 */

const root = path.resolve(import.meta.dirname, "..");
const chat = JSON.parse(readFileSync(path.join(root, "packages/views/locales/en/chat.json"), "utf8"));
const webRequire = createRequire(path.join(root, "apps/web/package.json"));

// Narrowest desktop viewport = the `useIsMobile` breakpoint (768); the list column caps at 40vw.
const NARROWEST_DESKTOP = 768;
const WIDTHS = [280, 320, Math.round(NARROWEST_DESKTOP * 0.4)];

async function compileCss(): Promise<string> {
  const entry = `@import "tailwindcss";
@source "${root}/packages/views/chat/components/chat-bulk-bar.tsx";
@source "${root}/packages/ui/components/ui";
@source "${root}/packages/ui/components/common/tooltip-hint.tsx";
${readFileSync(path.join(root, "packages/ui/styles/tokens.css"), "utf8")}`;
  const out = await postcss([webRequire("@tailwindcss/postcss")()]).process(entry, {
    from: path.join(root, "apps/web/app/globals.css"),
  });
  return out.css;
}

async function bundle(): Promise<string> {
  const outfile = path.join(mkdtempSync(path.join(tmpdir(), "bulk-bar-")), "mount.js");
  await build({
    stdin: {
      contents: `
        import { createElement } from "react";
        import { createRoot } from "react-dom/client";
        import { I18nProvider } from "@multica/core/i18n/react";
        import { ChatBulkBar } from "./chat/components/chat-bulk-bar";
        const s = (id, o = {}) => ({ id, status: "active", has_unread: false, unread_count: 0, pinned: false, ...o });
        window.mount = (width) => {
          const host = document.getElementById("root");
          host.style.width = width + "px";
          createRoot(host).render(
            createElement(I18nProvider, { locale: "en", resources: { en: { chat: window.__chat } } },
              createElement(ChatBulkBar, { sessions: [s("1", { has_unread: true }), s("2", { pinned: true }), s("3", { status: "archived" })], onAction: () => {} })));
        };`,
      resolveDir: path.join(root, "packages/views"),
      loader: "tsx",
    },
    bundle: true,
    platform: "browser",
    format: "iife",
    jsx: "automatic",
    outfile,
    loader: { ".css": "empty" },
    absWorkingDir: root,
    logLevel: "error",
  });
  return readFileSync(outfile, "utf8");
}

async function load(page: Page, width: number) {
  const [css, js] = await Promise.all([compileCss(), bundle()]);
  await page.setViewportSize({ width: NARROWEST_DESKTOP, height: 600 });
  await page.setContent(`<!doctype html><html><head><style>${css}</style></head><body style="margin:0"><div id="root"></div></body></html>`);
  await page.evaluate((c) => ((window as unknown as { __chat: unknown }).__chat = c), chat);
  await page.addScriptTag({ content: js });
  await page.evaluate((w) => (window as unknown as { mount: (w: number) => void }).mount(w), width);
  await page.getByRole("toolbar").waitFor();
}

const geometry = (page: Page) =>
  page.evaluate(() => {
    const bar = document.querySelector<HTMLElement>('[role="toolbar"]')!;
    const box = bar.getBoundingClientRect();
    const buttons = [...bar.querySelectorAll<HTMLElement>("button")].map((b) => {
      const r = b.getBoundingClientRect();
      return {
        name: b.getAttribute("aria-label") ?? b.textContent?.trim() ?? "",
        inside: r.left >= box.left - 0.5 && r.right <= box.right + 0.5,
        height: r.height,
        fontSize: getComputedStyle(b).fontSize,
      };
    });
    return { overflows: bar.scrollWidth > bar.clientWidth, buttons };
  });

for (const width of WIDTHS) {
  test(`${width}px column: every bulk button lies inside the bar`, async ({ page }) => {
    await load(page, width);
    const g = await geometry(page);
    expect(g.buttons).toHaveLength(6);
    expect(g.overflows).toBe(false);
    for (const b of g.buttons) expect(b.inside, `${b.name} is clipped`).toBe(true);
  });

  test(`${width}px column: all six buttons are keyboard reachable and named`, async ({ page }) => {
    await load(page, width);
    const g = await geometry(page);
    for (const b of g.buttons) expect(b.name).not.toBe("");
    // Mixed selection: every split has something to apply and an opposite, so all six are enabled.
    expect(await page.locator('[role="toolbar"] button:enabled').count()).toBe(6);
    const seen: string[] = [];
    for (let i = 0; i < 6; i++) {
      await page.keyboard.press("Tab");
      seen.push(await page.evaluate(() => document.activeElement?.getAttribute("aria-label") ?? ""));
    }
    expect(new Set(seen).size, seen.join(" | ")).toBe(6);
    expect(seen).not.toContain("");
  });
}

test("330px and wider: split buttons are 28px high with 12px text, as in the lock", async ({ page }) => {
  await load(page, 330);
  const g = await geometry(page);
  expect(g.overflows).toBe(false);
  for (const b of g.buttons) {
    expect(b.height).toBe(28);
    expect(b.fontSize).toBe("12px");
  }
});

// Evidence screenshots for the PR: set BULK_BAR_SHOTS=<dir> to write them.
test("screenshots", async ({ page }) => {
  test.skip(!process.env.BULK_BAR_SHOTS, "set BULK_BAR_SHOTS to write screenshots");
  for (const width of [280, 320, 330]) {
    await load(page, width);
    await page.locator("#root").screenshot({ path: path.join(process.env.BULK_BAR_SHOTS!, `bulk-bar-${width}.png`), scale: "device" });
  }
});
