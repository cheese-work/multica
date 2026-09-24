// @vitest-environment node
import { describe, expect, it } from "vitest";

import nextConfig from "./next.config";

describe("next.config build memory", () => {
  // The custom `webpack` hook turns Next's default build worker off. Without
  // the worker, `next build` OOM-kills in X99's 4GiB CI builders (CHE-702).
  it("keeps the webpack build worker on alongside the custom webpack hook", () => {
    expect(nextConfig.webpack).toBeTypeOf("function");
    expect(nextConfig.experimental?.webpackBuildWorker).toBe(true);
  });
});
