import { defineConfig } from "@playwright/test";

// The skill's own config: no global-setup (it compiles 18 routes under `next dev`,
// minutes of work this drive does not need), evidence written outside the run dir.
// The e2e helpers read the web origin from this variable; unset, they use localhost:3000.
process.env.PLAYWRIGHT_BASE_URL ??= process.env.FRONTEND_ORIGIN ?? `http://localhost:${process.env.FRONTEND_PORT ?? 3000}`;
const evidence = process.env.EVIDENCE_DIR ?? "evidence";

export default defineConfig({
  testDir: ".",
  testMatch: "*.spec.ts",
  timeout: 300_000,
  workers: 1,
  retries: 0,
  outputDir: `${evidence}/pw-output`,
  reporter: [["list"], ["json", { outputFile: `${evidence}/report.json` }]],
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL,
    headless: true,
    screenshot: "on",
    trace: "off",
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
