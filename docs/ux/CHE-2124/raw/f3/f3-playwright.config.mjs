import { defineConfig } from "../multica/node_modules/@playwright/test/index.mjs";

export default defineConfig({
  testDir: "../multica/e2e",
  testMatch: "plan-revisions.spec.ts",
  outputDir: process.env.F3_RED === "1" ? "./f3-red-results" : "./f3-green-results",
  timeout: 180000,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: "http://localhost:13954",
    headless: true,
    screenshot: "only-on-failure",
    launchOptions: { executablePath: "/usr/bin/google-chrome", args: ["--no-sandbox"] },
  },
});
