// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const entry = {
  type: "activity",
  id: "a",
  actor_type: "system",
  actor_id: "system",
  created_at: "2026-11-01T10:00:00Z",
  action: "status_changed",
  details: { from: "todo", to: "done" },
};

function stub(body: unknown, truncated?: string) {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (truncated !== undefined) headers["X-Timeline-Truncated"] = truncated;
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200, headers })));
}

const read = () => new ApiClient("https://api.example.test").listTimelineWithMeta("issue-1");

describe("listTimelineWithMeta", () => {
  it("returns the entries and is not truncated without the header", async () => {
    stub([entry]);
    const out = await read();
    expect(out.entries).toHaveLength(1);
    expect(out.truncated).toBe(false);
  });

  it.each(["activity", "activity,comment"])("is truncated when the header names activity (%s)", async (h) => {
    stub([entry], h);
    expect((await read()).truncated).toBe(true);
  });

  it("is not truncated when only comments were cut", async () => {
    stub([entry], "comment");
    expect((await read()).truncated).toBe(false);
  });

  it("falls back to no entries on a malformed body", async () => {
    stub({ not: "a list" });
    expect((await read()).entries).toEqual([]);
  });
});
