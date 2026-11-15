import { defineConfig, devices } from "@playwright/test";

/**
 * The performance scenario runs on its own: one Chromium worker, one test, no
 * retries. A retry would hide the run that mattered, and a second worker would
 * make the two builds under comparison contend for the same machine.
 *
 * The default e2e config ignores this directory, so `pnpm exec playwright test`
 * never picks it up by accident.
 *
 * `desktop` and `phone` are the two fixed measurement profiles (CHE-2976,
 * CHE-2973): the page-load baseline and the layout-stability specs run once per
 * profile, each its own project so the profile name is in every result line.
 * `chromium` keeps the older scenarios exactly as they were.
 */
const PROFILE_SPECS =
  /(chip-stability|chip-retention|compact-thread-reply|page-load-baseline)\.spec/;
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
  projects: [
    { name: "chromium", testIgnore: PROFILE_SPECS, use: { browserName: "chromium" } },
    {
      name: "desktop",
      testMatch: PROFILE_SPECS,
      use: { browserName: "chromium", viewport: { width: 1440, height: 900 }, hasTouch: false },
    },
    {
      // Pixel 7 sets viewport 412x915, isMobile, hasTouch, the phone user agent and scale factor.
      name: "phone",
      testMatch: PROFILE_SPECS,
      use: { ...devices["Pixel 7"], browserName: "chromium" },
    },
  ],
});
