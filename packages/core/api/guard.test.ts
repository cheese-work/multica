// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");

it("parses guard stats and fills a missing count with zero", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          days: 7,
          tz: "Asia/Bangkok",
          since: "2026-11-08",
          plugins: [
            { plugin: "gh-pr", fixed: 42, refused: 3, errors: 0, daily: [{ day: "2026-11-08", fixed: 5, refused: 0, errors: 0 }] },
            { plugin: "ci-wait", refused: 18 },
          ],
        }),
      ),
    ),
  );
  const stats = await client.getGuardStats(7);
  expect(stats.since).toBe("2026-11-08");
  expect(stats.plugins[0]).toMatchObject({ plugin: "gh-pr", fixed: 42, refused: 3 });
  expect(stats.plugins[1]).toMatchObject({ plugin: "ci-wait", fixed: 0, errors: 0, refused: 18, daily: [] });
});

it("does not present a malformed stats body as no plugins", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('{"plugins":"nope"}')));
  await expect(client.getGuardStats(7)).rejects.toThrow("Could not load guard stats");
});

it("lists decisions with the plugin and limit in the query, and reads an unknown action as is", async () => {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify({ decisions: [{ id: "d1", plugin: "gh-pr", action: "fixed", time: "2026-11-13T15:41:00Z", line: "title tag [squash] added", issue: "CHE-5189", runtime: "claude" }, { id: "d2", action: "weird" }] })),
  );
  vi.stubGlobal("fetch", fetchMock);
  const rows = await client.listGuardDecisions({ plugin: "gh-pr", limit: 10 });
  expect(String(fetchMock.mock.calls[0]![0])).toBe("https://api.example.test/api/guard/decisions?plugin=gh-pr&limit=10");
  expect(rows[0]).toMatchObject({ id: "d1", action: "fixed", issue: "CHE-5189" });
  expect(rows[1]).toMatchObject({ id: "d2", plugin: "", line: "", action: "weird" });
});

it("does not present a malformed decision list as empty", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('{"decisions":3}')));
  await expect(client.listGuardDecisions({})).rejects.toThrow("Could not load guard decisions");
});
