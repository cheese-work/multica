/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { useRegenerateChatQuickActions } from "./mutations";
import { chatKeys, quickActionsPendingWindowMs } from "./queries";
import { useQuickActionsPendingTimeout } from "./use-quick-actions-pending-timeout";
import type { ChatQuickActionsPendingState } from "../types/chat";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const SESSION = "s1";

describe("quickActionsPendingWindowMs", () => {
  it("is the job's 20 s run deadline while the chain is set", () => {
    expect(quickActionsPendingWindowMs(true)).toBe(20_000);
  });

  it("keeps the old 8 s direct-call budget when no chain is set", () => {
    expect(quickActionsPendingWindowMs(false)).toBe(8_000);
  });
});

describe("useQuickActionsPendingTimeout window", () => {
  let qc: QueryClient;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  const read = () =>
    qc.getQueryData<ChatQuickActionsPendingState | null>(
      chatKeys.quickActionsPending(SESSION),
    );

  beforeEach(() => {
    vi.useFakeTimers();
    qc = new QueryClient();
  });
  afterEach(() => vi.useRealTimers());

  it("holds the skeleton to 20 s when the job is configured, then clears silently", () => {
    // A refresh only reaches 202 when the workspace chain is set, so the marker
    // the mutation stamps carries the chain-set window.
    setApiInstance({
      regenerateChatQuickActions: () => new Promise<void>(() => {}),
    } as unknown as ApiClient);
    const { result } = renderHook(() => useRegenerateChatQuickActions(), { wrapper });
    act(() => result.current.mutate({ sessionId: SESSION, messageId: "m1" }));
    const pending = read()!;
    expect(pending.expires_at - Date.now()).toBe(20_000);
    renderHook(() => useQuickActionsPendingTimeout(SESSION, pending), { wrapper });

    act(() => vi.advanceTimersByTime(19_900));
    expect(read()).toEqual(pending);

    act(() => vi.advanceTimersByTime(200));
    expect(read()).toBeNull();
  });
});
