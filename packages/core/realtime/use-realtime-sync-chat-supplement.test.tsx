/** @vitest-environment jsdom */
import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { chatKeys } from "../chat/queries";
import { createQueryClient } from "../query-client";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));
vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

// CHE-2820: a receipt change (read by agent, uncertain, failed) must refresh the
// bubble and the queue head state, which only the server can author.
describe("useRealtimeSync — chat:supplement_updated", () => {
  afterEach(cleanup);

  it("refetches the DM's messages and pending task", () => {
    const queryClient = createQueryClient();
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const handlers: Record<string, (payload: unknown) => void> = {};
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => {
        handlers[event] = handler;
        return () => {};
      },
      onAny: () => () => {},
      onReconnect: () => () => {},
    } as unknown as WSClient;
    const stores = {
      authStore: { getState: () => ({ user: { id: "u1" } }) },
    } as unknown as RealtimeSyncStores;
    renderHook(() => useRealtimeSync(ws, stores), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    });

    expect(handlers["chat:supplement_updated"]).toBeTypeOf("function");
    invalidate.mockClear();
    act(() => {
      handlers["chat:supplement_updated"]!({
        supplement: { message_id: "m1", chat_session_id: "dm-1", status: "delivered" },
      });
    });

    const keys = invalidate.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(keys).toContain(JSON.stringify(chatKeys.messages("dm-1")));
    expect(keys).toContain(JSON.stringify(chatKeys.messagesPage("dm-1")));
    expect(keys).toContain(JSON.stringify(chatKeys.pendingTask("dm-1")));
  });

  it("ignores a payload without a session id", () => {
    const queryClient = createQueryClient();
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const handlers: Record<string, (payload: unknown) => void> = {};
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => {
        handlers[event] = handler;
        return () => {};
      },
      onAny: () => () => {},
      onReconnect: () => () => {},
    } as unknown as WSClient;
    renderHook(
      () => useRealtimeSync(ws, { authStore: { getState: () => ({ user: { id: "u1" } }) } } as unknown as RealtimeSyncStores),
      {
        wrapper: ({ children }: { children: ReactNode }) => (
          <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
        ),
      },
    );
    invalidate.mockClear();
    expect(() => act(() => handlers["chat:supplement_updated"]!({ supplement: "junk" }))).not.toThrow();
    expect(invalidate).not.toHaveBeenCalled();
  });
});
