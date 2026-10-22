import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./test/setup.ts"],
    include: ["**/*.test.{ts,tsx}"],
    // The default 5s is too tight for the heaviest interaction tests: alone,
    // two of them take 3 to 4s, and under CPU contention (a busy CI runner, or
    // the 520 files this package runs in parallel) they cross 5s and fail with
    // "Test timed out in 5000ms" although nothing is wrong.
    testTimeout: 15_000,
  },
});
