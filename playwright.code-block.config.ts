import { defineConfig } from "@playwright/test";

/**
 * Local-only config for the self-contained code-block-toolbar spec: it uses
 * page.setContent and needs no app server, and runs against the system Google Chrome (same pattern as
 * playwright.iframe.config.ts). Not used by the main `make check` pipeline.
 */
export default defineConfig({
  testDir: "./e2e",
  testMatch: /code-block-toolbar\.spec\.ts/,
  timeout: 60000,
  workers: 1,
  retries: 0,
  use: { channel: "chrome", headless: true },
  projects: [{ name: "chromium", use: { browserName: "chromium", channel: "chrome" } }],
});
