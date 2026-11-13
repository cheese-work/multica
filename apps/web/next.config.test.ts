import { describe, expect, it, vi } from "vitest";
import { hasLocalMatch } from "next/dist/shared/lib/match-local-pattern";

vi.mock("dotenv", () => ({ config: vi.fn() }));
vi.mock("fumadocs-mdx/next", () => ({
  createMDX: () => (config: unknown) => config,
}));

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

describe("image optimization paths", () => {
  it.each([
    "/uploads/workspaces/workspace/attachment.png",
    "/api/attachments/attachment/download",
    "/api/attachments/attachment/signed-download?exp=1&sig=signature",
    "/images/hero.png?url=/uploads/workspaces/workspace/attachment.png",
  ])("rejects private or unexpected sources: %s", (url) => {
    expect(hasLocalMatch(nextConfig.images?.localPatterns, url)).toBe(false);
  });

  it.each(["/images/hero.png", "/usecases/team/screenshot.webp"])(
    "keeps landing images available: %s",
    (url) => {
      expect(hasLocalMatch(nextConfig.images?.localPatterns, url)).toBe(true);
    },
  );
});
