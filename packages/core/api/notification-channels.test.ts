// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown) {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

// DM switches default to off (CHE-1848).
const DEFAULT_CHANNELS = {
  inbox: { in_app: true, push: false },
  dm: { in_app: false, push: false },
};

describe("notification channel API", () => {
  it("reads the channel switches of a specific workspace", async () => {
    const fetch = respond({
      workspace_id: "ws-1",
      channels: { inbox: { in_app: true, push: false }, dm: { in_app: false, push: true } },
    });
    const api = new ApiClient("https://api.example.test");
    await expect(api.getNotificationChannels("ws-slug")).resolves.toEqual({
      workspace_id: "ws-1",
      channels: { inbox: { in_app: true, push: false }, dm: { in_app: false, push: true } },
    });
    expect(fetch.mock.calls[0]![0]).toBe("https://api.example.test/api/notification-channels");
    expect(fetch.mock.calls[0]![1]).toEqual(
      expect.objectContaining({ headers: expect.objectContaining({ "X-Workspace-Slug": "ws-slug" }) }),
    );
  });

  it("patches one source and channel at a time", async () => {
    const fetch = respond({ workspace_id: "ws-1", channels: DEFAULT_CHANNELS });
    const api = new ApiClient("https://api.example.test");
    await api.updateNotificationChannel({ source: "dm", channel: "push", enabled: true }, "ws-slug");
    expect(fetch.mock.calls[0]![1]).toEqual(
      expect.objectContaining({
        method: "PATCH",
        headers: expect.objectContaining({ "X-Workspace-Slug": "ws-slug" }),
        body: JSON.stringify({ source: "dm", channel: "push", enabled: true }),
      }),
    );
  });

  it.each([
    ["channels is an array", { workspace_id: "ws-1", channels: [] }],
    ["a switch is not a boolean", { workspace_id: "ws-1", channels: { inbox: { in_app: "yes", push: 1 } } }],
    ["the body is not an object", "nope"],
  ])("falls back to the safe defaults when %s", async (_name, body) => {
    respond(body);
    const api = new ApiClient("https://api.example.test");
    await expect(api.getNotificationChannels()).resolves.toEqual({ workspace_id: "", channels: DEFAULT_CHANNELS });
  });

  it("fills a missing source or switch with its default and keeps unknown fields out of the way", async () => {
    respond({ workspace_id: "ws-1", channels: { dm: { push: true }, sms: { in_app: true } }, extra: 1 });
    const api = new ApiClient("https://api.example.test");
    const got = await api.getNotificationChannels();
    expect(got.channels).toEqual({ inbox: DEFAULT_CHANNELS.inbox, dm: { in_app: false, push: true } });
  });
});
