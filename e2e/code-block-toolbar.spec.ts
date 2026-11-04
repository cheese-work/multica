import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { build } from "esbuild";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import postcss from "postcss";

/**
 * Self-contained real-Chromium test for the code-block copy toolbar (CHE-2939).
 * jsdom cannot evaluate `@media (hover: none)` or layout, so this compiles the
 * real Tailwind + code.css, renders the real CodeBlockShell with
 * `page.setContent` (no app server), and checks visibility without hover and
 * clearance of the first code line on a touch (hover: none) device and on a
 * hover desktop. Run: `pnpm exec playwright test -c playwright.code-block.config.ts`.
 */

const root = path.resolve(import.meta.dirname, "..");
const editor = JSON.parse(readFileSync(path.join(root, "packages/views/locales/en/editor.json"), "utf8"));
const webRequire = createRequire(path.join(root, "apps/web/package.json"));

async function compileCss(): Promise<string> {
  const entry = `@import "tailwindcss";
@source "${root}/packages/views/rich-content/rich-code-block.tsx";
${readFileSync(path.join(root, "packages/ui/styles/tokens.css"), "utf8")}
${readFileSync(path.join(root, "packages/views/editor/styles/code.css"), "utf8")}`;
  const tailwind = webRequire("@tailwindcss/postcss");
  const out = await postcss([tailwind()]).process(entry, {
    from: path.join(root, "apps/web/app/globals.css"),
  });
  return out.css;
}

// The component graph imports CSS, which Playwright's TS loader cannot parse,
// so bundle a tiny render entry with esbuild (CSS imports become empty).
async function markup(): Promise<string> {
  const outfile = path.join(mkdtempSync(path.join(tmpdir(), "code-block-")), "render.cjs");
  await build({
    stdin: {
      contents: `
        import { createElement } from "react";
        import { renderToStaticMarkup } from "react-dom/server";
        import i18next from "i18next";
        import { I18nextProvider, initReactI18next } from "react-i18next";
        import { CodeBlockShell } from "./rich-content/rich-code-block";
        export async function render(editor) {
          const i18n = i18next.createInstance();
          await i18n.use(initReactI18next).init({ lng: "en", ns: ["editor"], defaultNS: "editor", resources: { en: { editor } } });
          return renderToStaticMarkup(
            createElement(I18nextProvider, { i18n },
              createElement("div", { className: "rich-text-editor", style: { width: "480px" } },
                createElement(CodeBlockShell, { language: "bash", code: "echo hi" },
                  createElement("code", null, "echo hi")))));
        }`,
      resolveDir: path.join(root, "packages/views"),
      loader: "tsx",
    },
    bundle: true,
    platform: "node",
    format: "cjs",
    jsx: "automatic",
    outfile,
    loader: { ".css": "empty" },
    absWorkingDir: root,
    logLevel: "error",
  });
  return webRequire(outfile).render(editor);
}

async function load(page: Page) {
  const css = await compileCss();
  const html = await markup();
  await page.setContent(`<!doctype html><html><head><style>${css}</style></head><body>${html}</body></html>`);
}

const toolbarOpacity = (page: Page) =>
  page.getByRole("button", { name: /copy/i }).evaluate((b) => Number(getComputedStyle(b.parentElement!).opacity));

const clearance = (page: Page) =>
  page.evaluate(() => {
    const code = document.querySelector("pre code")!.getBoundingClientRect();
    const bar = document.querySelector("button")!.parentElement!.getBoundingClientRect();
    return code.top - bar.bottom; // >= 0 means the toolbar does not overlap the first line
  });

test.describe("code-block copy toolbar (real Chromium)", () => {
  test.describe("touch screen (hover: none)", () => {
    test.use({ hasTouch: true, isMobile: true });

    test("toolbar is visible without hover and clears the first line", async ({ page }) => {
      await load(page);
      expect(await page.evaluate(() => matchMedia("(hover: none)").matches)).toBe(true);
      expect(await toolbarOpacity(page)).toBe(1);
      expect(await clearance(page)).toBeGreaterThanOrEqual(0);
    });
  });

  test.describe("desktop (hover: hover)", () => {
    test("toolbar is hidden until hover and clears the first line", async ({ page }) => {
      await load(page);
      expect(await page.evaluate(() => matchMedia("(hover: hover)").matches)).toBe(true);
      expect(await toolbarOpacity(page)).toBe(0);
      await page.locator(".code-block-wrapper").hover();
      await expect.poll(() => toolbarOpacity(page)).toBe(1);
      expect(await clearance(page)).toBeGreaterThanOrEqual(0);
    });
  });
});
