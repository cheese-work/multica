import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import BalancedSequencer from "./test/balanced-sequencer";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./test/setup.ts"],
    include: ["**/*.test.{ts,tsx}"],
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
    // The default 5s is too tight for the heaviest interaction tests: alone,
    // two of them take 3 to 4s, and under CPU contention (a busy CI runner, or
    // the 520 files this package runs in parallel) they cross 5s and fail with
    // "Test timed out in 5000ms" although nothing is wrong.
    testTimeout: 15_000,
||||||| 2ae2f08005
=======
    sequence: { sequencer: BalancedSequencer },
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  },
});
