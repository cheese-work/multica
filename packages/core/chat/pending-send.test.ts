// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import { setApiInstance } from "../api";
import { classifySendFailure, sendWithPendingId, usePendingSendStore } from "./pending-send";

beforeEach(() => {
  usePendingSendStore.setState({ bySession: {} });
  localStorage.clear();
});

describe("classifySendFailure", () => {
  it("treats 409 and other 4xx as a definite rejection", () => {
    expect(classifySendFailure(new ApiError("conflict", 409, "Conflict"))).toBe("rejected");
    expect(classifySendFailure(new ApiError("forbidden", 403, "Forbidden"))).toBe("rejected");
  });
  it("treats timeouts, network errors and 5xx as unknown delivery", () => {
    expect(classifySendFailure(new TypeError("Failed to fetch"))).toBe("unknown");
    expect(classifySendFailure(new ApiError("gw", 504, "Gateway Timeout"))).toBe("unknown");
    expect(classifySendFailure(new ApiError("to", 408, "Request Timeout"))).toBe("unknown");
  });
});

describe("usePendingSendStore", () => {
  it("reuses the client_message_id for a retry of the same content", () => {
    const { begin, markUnknown } = usePendingSendStore.getState();
    const first = begin("s1", "hello");
    markUnknown("s1");
    expect(usePendingSendStore.getState().bySession.s1?.status).toBe("unknown");
    expect(begin("s1", "hello")).toBe(first);
  });

  it("mints a new id when the draft text changed (A22: one message per id)", () => {
    const { begin } = usePendingSendStore.getState();
    const first = begin("s1", "hello");
    expect(begin("s1", "hello!")).not.toBe(first);
  });

  it("mints a new id after the send settled", () => {
    const { begin, settle } = usePendingSendStore.getState();
    const first = begin("s1", "hello");
    settle("s1");
    expect(usePendingSendStore.getState().bySession.s1).toBeUndefined();
    expect(begin("s1", "hello")).not.toBe(first);
  });

  it("keeps the pending record across reload (A12/A22)", async () => {
    const { begin, markUnknown } = usePendingSendStore.getState();
    const id = begin("s1", "hello");
    markUnknown("s1");
    // Simulate a reload: the saved copy survives, memory starts empty.
    const saved = localStorage.getItem("multica_chat_pending_send")!;
    usePendingSendStore.setState({ bySession: {} });
    localStorage.setItem("multica_chat_pending_send", saved);
    await usePendingSendStore.persist.rehydrate();
    expect(usePendingSendStore.getState().bySession.s1).toMatchObject({
      clientMessageId: id,
      status: "unknown",
    });
  });
});

describe("sendWithPendingId", () => {
  const ok = { message_id: "m1", task_id: "t1", created_at: "x" };
  const useApi = (sendChatMessage: ReturnType<typeof vi.fn>) =>
    setApiInstance({ sendChatMessage } as never);

  it("A22: a timeout then a retry sends the same client_message_id", async () => {
    const send = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("timeout"))
      .mockResolvedValueOnce(ok);
    useApi(send);
    await expect(sendWithPendingId("s1", "hello")).rejects.toThrow("timeout");
    expect(usePendingSendStore.getState().bySession.s1?.status).toBe("unknown");
    await sendWithPendingId("s1", "hello");
    const ids = send.mock.calls.map((c) => c[4].clientMessageId);
    expect(ids[0]).toBe(ids[1]);
    expect(usePendingSendStore.getState().bySession.s1).toBeUndefined();
  });

  it("A12: a 409 forgets the id, so the retry is a new message", async () => {
    const send = vi
      .fn()
      .mockRejectedValueOnce(new ApiError("conflict", 409, "Conflict"))
      .mockResolvedValueOnce(ok);
    useApi(send);
    await expect(sendWithPendingId("s1", "hello")).rejects.toThrow("conflict");
    expect(usePendingSendStore.getState().bySession.s1).toBeUndefined();
    await sendWithPendingId("s1", "hello");
    expect(send.mock.calls[0]![4].clientMessageId).not.toBe(send.mock.calls[1]![4].clientMessageId);
  });

  it("forwards attachments, decision id and ask ids", async () => {
    const send = vi.fn().mockResolvedValue(ok);
    useApi(send);
    await sendWithPendingId("s1", "hi", { attachmentIds: ["a"], decisionRequestId: "d", askIds: ["k"] });
    expect(send).toHaveBeenCalledWith("s1", "hi", ["a"], "d", expect.objectContaining({ askIds: ["k"] }));
  });
});
