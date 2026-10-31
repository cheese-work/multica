// @vitest-environment node
import { describe, expect, it } from "vitest";

import nextConfig from "./next.config";

describe("next.config build memory", () => {
  it("includes the immutable fork entries in the standalone changelog image", () => {
    expect(nextConfig.outputFileTracingIncludes?.["/changelog"]).toEqual(["./.fork-changelog/entries.json"]);
  });
  // The custom `webpack` hook turns Next's default build worker off. Without
  // the worker, `next build` OOM-kills in X99's 4GiB CI builders (CHE-702).
  it("keeps the webpack build worker on alongside the custom webpack hook", () => {
    expect(nextConfig.webpack).toBeTypeOf("function");
    expect(nextConfig.experimental?.webpackBuildWorker).toBe(true);
  });
});

describe("next.config dev route retention", () => {
  // The E2E global setup compiles every route once, minutes before the specs
  // that use them. Next evicts a dev page after 60s idle and keeps a handful,
  // so a warmed route recompiled (10 to 75s) mid-suite and timed out specs.
  it("keeps compiled dev routes for the whole suite", () => {
    expect(nextConfig.onDemandEntries?.maxInactiveAge).toBeGreaterThanOrEqual(60 * 60 * 1000);
    expect(nextConfig.onDemandEntries?.pagesBufferLength).toBeGreaterThanOrEqual(30);
  });
});

describe("next.config service worker headers", () => {
  // A cached worker script would keep old push handling alive after a deploy.
  it("serves /sw.js revalidated on every load", async () => {
    const rules = (await nextConfig.headers?.()) ?? [];
    const rule = rules.find((r) => r.source === "/sw.js");
    expect(rule?.headers).toContainEqual({ key: "Cache-Control", value: "no-cache" });
  });
});
