// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown, status = 200) {
  const fetch = vi.fn().mockResolvedValue(
    status === 204 ? new Response(null, { status }) : new Response(JSON.stringify(body), { status }),
  );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

describe("push API", () => {
  it("reads the push config", async () => {
    const fetch = respond({ enabled: true, public_key: "BKey" });
    const api = new ApiClient("https://api.example.test");
    await expect(api.getPushConfig()).resolves.toEqual({ enabled: true, public_key: "BKey" });
    expect(fetch.mock.calls[0]![0]).toBe("https://api.example.test/api/push/config");
  });

  it.each([
    ["the body is not an object", "nope"],
    ["enabled is not a boolean", { enabled: "yes", public_key: 1 }],
  ])("treats push as off when %s", async (_n, body) => {
    respond(body);
    const api = new ApiClient("https://api.example.test");
    await expect(api.getPushConfig()).resolves.toEqual({ enabled: false, public_key: "" });
  });

  it("registers a device with POST", async () => {
    const fetch = respond({ id: "sub-1" });
    const api = new ApiClient("https://api.example.test");
    const body = { endpoint: "https://push.example/x", keys: { p256dh: "P", auth: "A" } };
    await api.registerPushSubscription(body);
    expect(fetch.mock.calls[0]![0]).toBe("https://api.example.test/api/push/subscriptions");
    expect(fetch.mock.calls[0]![1]).toEqual(
      expect.objectContaining({ method: "POST", body: JSON.stringify(body) }),
    );
  });

  it("deletes a device by endpoint", async () => {
    const fetch = respond(null, 204);
    const api = new ApiClient("https://api.example.test");
    await expect(api.deletePushSubscription("https://push.example/x")).resolves.toBeUndefined();
    expect(fetch.mock.calls[0]![1]).toEqual(
      expect.objectContaining({ method: "DELETE", body: JSON.stringify({ endpoint: "https://push.example/x" }) }),
    );
  });
});
