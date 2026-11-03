import { defineConfig, devices } from "@playwright/test";

/**
 * The performance scenario runs on its own: one Chromium worker, one test, no
 * retries. A retry would hide the run that mattered, and a second worker would
 * make the two builds under comparison contend for the same machine.
 *
 * The default e2e config ignores this directory, so `pnpm exec playwright test`
 * never picks it up by accident.
 */
export default defineConfig({
  testDir: "./e2e/perf",
  timeout: 180_000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? "http://localhost:3000",
    headless: true,
  },
  // `desktop` and `phone` are the two fixed measurement profiles (CHE-2976);
  // only the page-load baseline runs under them.
  projects: [
    { name: "chromium", testIgnore: /page-load-baseline\.spec/, use: { browserName: "chromium" } },
    {
      name: "desktop",
      testMatch: /page-load-baseline\.spec/,
      use: { browserName: "chromium", viewport: { width: 1440, height: 900 }, hasTouch: false },
    },
    {
      name: "phone",
      testMatch: /page-load-baseline\.spec/,
      use: { browserName: "chromium", ...devices["Pixel 7"] },
    },
  ],
});
