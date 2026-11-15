import { defineConfig } from "@playwright/test";

/**
 * Local config for the self-contained DM bulk-bar layout spec (CHE-5883): it
 * uses page.setContent and needs no app server. Same pattern as
 * playwright.code-block.config.ts. Not used by the main `make check` pipeline.
 */
export default defineConfig({
  testDir: "./e2e",
  testMatch: /chat-bulk-bar-fit\.spec\.ts/,
  timeout: 60000,
  workers: 1,
  retries: 0,
  use: { headless: true },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
