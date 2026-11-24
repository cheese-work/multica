// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { EMPTY_INBOX_FILTERS, type InboxFilters } from "../inbox/filter-store";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown) {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

describe("archived inbox API", () => {
  it("sends cursor, lookup and every archive filter dimension and maps the page", async () => {
    const fetch = respond({ items: [], next_cursor: null, has_more: false });
    const api = new ApiClient("https://api.example.test");
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
    // The archive has no idle or mention dimension; a selection carried in from the
||||||| 2ae2f08005
    // The archive has no idle dimension; a selection carried in from the
=======
    // The archive has no agent dimension; a selection carried in from the
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
    // active inbox must not reach the server.
    const filters: InboxFilters = {
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
      statuses: ["in_review"], priorities: ["high"], actors: ["system"], unreadOnly: true, idleOnly: true, mentionsOnly: true,
||||||| 2ae2f08005
      statuses: ["in_review"], priorities: ["high"], actors: ["system"], unreadOnly: true, idleOnly: true,
=======
      ...EMPTY_INBOX_FILTERS,
      statuses: ["in_review"], priorities: ["high"], actors: ["system"], unreadOnly: true, agent: "idle",
      labels: ["label-1"], negated: ["label"],
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
    };
    await expect(api.listArchivedInboxPage(filters, { cursor: "cursor-value", groupId: "group" })).resolves.toEqual({ items: [], nextCursor: null, hasMore: false });
    const url = new URL(fetch.mock.calls[0]![0]);
    expect(url.pathname).toBe("/api/inbox/archived/page");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      statuses: "in_review", priorities: "high", actors: "system", unread_only: "true",
      labels: "label-1", negated: "label", limit: "50", cursor: "cursor-value", group_id: "group",
    });
  });

  it.each([
    {}, [], { items: [] }, { items: "wrong", next_cursor: null, has_more: false },
    { items: [], next_cursor: null, has_more: true },
    { items: [], next_cursor: "next", has_more: true },
    { items: [{ id: "broken" }], next_cursor: null, has_more: false },
  ])("rejects malformed pages instead of presenting an empty archive: %j", async (body) => {
    respond(body);
    await expect(new ApiClient("https://api.example.test").listArchivedInboxPage(EMPTY_INBOX_FILTERS))
      .rejects.toThrow("Invalid archived inbox page response");
  });

  it("maps complete facets and rejects incomplete or invalid counts", async () => {
    respond({ statuses: { in_review: 2 }, priorities: {}, actors: { system: 0 }, unread_count: 1 });
    const api = new ApiClient("https://api.example.test");
    // An older server sends no type / project / assignee / label counts.
    await expect(api.getArchivedInboxFacets(EMPTY_INBOX_FILTERS)).resolves.toEqual({
      statuses: { in_review: 2 }, priorities: {}, actors: { system: 0 }, unreadCount: 1,
      types: {}, projects: {}, assignees: {}, labels: {},
    });
    respond({ statuses: {}, priorities: {}, actors: {}, types: { mention: 1 }, projects: { none: 2 }, assignees: {}, labels: { l: 1 }, unread_count: 0 });
    await expect(api.getArchivedInboxFacets(EMPTY_INBOX_FILTERS)).resolves.toMatchObject({
      types: { mention: 1 }, projects: { none: 2 }, labels: { l: 1 },
    });
    for (const body of [{}, { statuses: {}, priorities: {}, actors: {}, unread_count: -1 }]) {
      respond(body);
      await expect(api.getArchivedInboxFacets(EMPTY_INBOX_FILTERS)).rejects.toThrow("Invalid archived inbox facets response");
    }
  });
});
