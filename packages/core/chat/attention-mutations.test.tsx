/**
 * @vitest-environment jsdom
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import {
  useChatAttentionLater,
  useChatAttentionUndoXongAll,
  useChatAttentionXong,
  useChatAttentionXongAll,
} from "./attention-mutations";
import { chatKeys } from "./queries";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const VIEW = { attention: [] };

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  const api = {
    chatAttentionXong: vi.fn().mockResolvedValue(VIEW),
    chatAttentionLater: vi.fn().mockResolvedValue(VIEW),
    chatAttentionXongAll: vi.fn().mockResolvedValue({ attention: [], batch_id: "b1" }),
    chatAttentionUndoXongAll: vi.fn().mockResolvedValue({ attention: [], batch_id: "b1" }),
  };
  setApiInstance(api as unknown as ApiClient);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, invalidate, api, wrapper };
}

const sessionsInvalidated = (invalidate: ReturnType<typeof setup>["invalidate"]) =>
  invalidate.mock.calls.some(([f]) => JSON.stringify((f as { queryKey: unknown }).queryKey) === JSON.stringify(chatKeys.sessions("ws-1")));

describe("attention mutations", () => {
  let ctx: ReturnType<typeof setup>;
  beforeEach(() => {
    ctx = setup();
  });

  it("Xong sends the displayed keys minus decision keys (T3: a decision is never acked)", async () => {
    const { result } = renderHook(() => useChatAttentionXong(), { wrapper: ctx.wrapper });
    await act(() => result.current.mutateAsync({ sessionId: "s1", keys: ["item:1", "decision:2@1", "turn:3"] }));
    expect(ctx.api.chatAttentionXong).toHaveBeenCalledWith("s1", ["item:1", "turn:3"]);
    await waitFor(() => expect(sessionsInvalidated(ctx.invalidate)).toBe(true));
  });

  it("Xong with only decision keys makes no request", async () => {
    const { result } = renderHook(() => useChatAttentionXong(), { wrapper: ctx.wrapper });
    await act(() => result.current.mutateAsync({ sessionId: "s1", keys: ["decision:2@1"] }));
    expect(ctx.api.chatAttentionXong).not.toHaveBeenCalled();
  });

  it("Để sau sends all keys (decision snooze is presentation-only, T5) with the request id and duration", async () => {
    const { result } = renderHook(() => useChatAttentionLater(), { wrapper: ctx.wrapper });
    await act(() =>
      result.current.mutateAsync({ sessionId: "s1", keys: ["item:1", "decision:2@1"], durationSeconds: 7200, requestId: "r1" }),
    );
    expect(ctx.api.chatAttentionLater).toHaveBeenCalledWith("s1", ["item:1", "decision:2@1"], {
      durationSeconds: 7200,
      requestId: "r1",
    });
    await waitFor(() => expect(sessionsInvalidated(ctx.invalidate)).toBe(true));
  });

  it("Xong tất cả returns the batch id and undo (A11) reopens it", async () => {
    const all = renderHook(() => useChatAttentionXongAll(), { wrapper: ctx.wrapper });
    let batch: { batch_id?: string } | undefined;
    await act(async () => {
      batch = await all.result.current.mutateAsync(["item:1", "turn:3"]);
    });
    expect(ctx.api.chatAttentionXongAll).toHaveBeenCalledWith(["item:1", "turn:3"]);
    expect(batch?.batch_id).toBe("b1");

    const undo = renderHook(() => useChatAttentionUndoXongAll(), { wrapper: ctx.wrapper });
    await act(() => undo.result.current.mutateAsync("b1"));
    expect(ctx.api.chatAttentionUndoXongAll).toHaveBeenCalledWith("b1");
    await waitFor(() => expect(sessionsInvalidated(ctx.invalidate)).toBe(true));
  });
});
