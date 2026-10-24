/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { useCancelChatWakeup } from "./mutations";
import { chatKeys, chatWakeupsOptions } from "./queries";
import type { ChatWakeup } from "../types";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const wakeup = (id: string): ChatWakeup => ({
  id,
  chat_session_id: "s1",
  instruction: "Check back.",
  fire_at: "2026-10-21T04:40:00Z",
  status: "pending",
  task_id: null,
  condition: null,
});

function wrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("chat wakeups queries and cancel", () => {
  let qc: QueryClient;
  const listChatWakeups = vi.fn<(sessionId: string) => Promise<{ wakeups: ChatWakeup[] }>>();
  const cancelChatWakeup = vi.fn<(sessionId: string, wakeupId: string) => Promise<void>>();

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    listChatWakeups.mockReset();
    cancelChatWakeup.mockReset();
    setApiInstance({ listChatWakeups, cancelChatWakeup } as unknown as ApiClient);
  });
  afterEach(() => qc.clear());

  it("keys the list by session", () => {
    expect(chatKeys.wakeups("s1")).not.toEqual(chatKeys.wakeups("s2"));
    expect(chatKeys.wakeups("s1")).toEqual([...chatKeys.wakeupsAll(), "s1"]);
  });

  it("loads the pending wakeups of a session", async () => {
    listChatWakeups.mockResolvedValue({ wakeups: [wakeup("w1")] });
    const data = await qc.fetchQuery(chatWakeupsOptions("s1"));
    expect(listChatWakeups).toHaveBeenCalledWith("s1");
    expect(data.wakeups.map((w) => w.id)).toEqual(["w1"]);
  });

  it("refreshes the list after a successful cancel", async () => {
    cancelChatWakeup.mockResolvedValue(undefined);
    qc.setQueryData(chatKeys.wakeups("s1"), { wakeups: [wakeup("w1")] });
    const { result } = renderHook(() => useCancelChatWakeup("s1"), { wrapper: wrapper(qc) });
    await act(async () => {
      await result.current.mutateAsync("w1");
    });
    expect(cancelChatWakeup).toHaveBeenCalledWith("s1", "w1");
    expect(qc.getQueryState(chatKeys.wakeups("s1"))?.isInvalidated).toBe(true);
  });

  it("keeps the follow-up in the list when the cancel fails", async () => {
    cancelChatWakeup.mockRejectedValue(new Error("boom"));
    qc.setQueryData(chatKeys.wakeups("s1"), { wakeups: [wakeup("w1")] });
    const { result } = renderHook(() => useCancelChatWakeup("s1"), { wrapper: wrapper(qc) });
    await act(async () => {
      await result.current.mutateAsync("w1").catch(() => undefined);
    });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(qc.getQueryData<{ wakeups: ChatWakeup[] }>(chatKeys.wakeups("s1"))?.wakeups).toHaveLength(1);
  });
});
