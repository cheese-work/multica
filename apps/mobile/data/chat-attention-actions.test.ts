import { beforeEach, describe, expect, it, vi } from "vitest";

const apiMock = vi.hoisted(() => ({
  chatAttentionXong: vi.fn(),
  chatAttentionLater: vi.fn(),
}));
vi.mock("@/data/api", () => ({ api: apiMock }));

import { xongSession, laterSession } from "./chat-attention-actions";

beforeEach(() => vi.clearAllMocks());

describe("Xong", () => {
  it("sends the exact displayed non-decision keys", async () => {
    await xongSession("s1", ["decision:7@2", "item:1", "turn:3"]);
    expect(apiMock.chatAttentionXong).toHaveBeenCalledWith("s1", ["item:1", "turn:3"]);
  });

  it("does not call the server for a decision-only snapshot (server would 422)", async () => {
    await xongSession("s1", ["decision:7@2"]);
    expect(apiMock.chatAttentionXong).not.toHaveBeenCalled();
  });
});

describe("Để sau", () => {
  it("snoozes every displayed key, decisions included, with the request id", async () => {
    await laterSession("s1", ["decision:7@2", "item:1"], "req-1");
    expect(apiMock.chatAttentionLater).toHaveBeenCalledWith("s1", ["decision:7@2", "item:1"], {
      requestId: "req-1",
    });
  });

  it("does not call the server without keys", async () => {
    await laterSession("s1", [], "req-1");
    expect(apiMock.chatAttentionLater).not.toHaveBeenCalled();
  });
});
