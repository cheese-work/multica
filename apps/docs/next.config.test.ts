// @vitest-environment node
import { describe, expect, it } from "vitest";

import nextConfig from "./next.config.mjs";
import pkg from "./package.json";

// X99's CI builders run `next build` in a 4GiB, 2-CPU nested container. The
// default Turbopack build, with `cpus` defaulting to the host core count (56),
// OOM-kills the docs image build (CHE-3165). Mirrors apps/web's recipe.
describe("docs build memory", () => {
  it("caps build workers to the CI container's CPU quota", () => {
    expect(nextConfig.experimental?.cpus).toBeLessThanOrEqual(2);
  });

  it("trims the webpack compile footprint", () => {
    expect(nextConfig.experimental?.webpackMemoryOptimizations).toBe(true);
    expect(nextConfig.experimental?.webpackBuildWorker).toBe(true);
  });

  it("builds with webpack, not the memory-hungry default Turbopack", () => {
    expect(pkg.scripts.build).toContain("--webpack");
  });
});
