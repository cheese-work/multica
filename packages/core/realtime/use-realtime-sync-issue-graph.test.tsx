/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { issueGraphKeys } from "../issues/graph-queries";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));
vi.mock("../paths", () => ({ useHasOnboarded: () => true, resolvePostAuthDestination: () => "/" }));

function createRecordingWs() {
  const handlers: Record<string, (p: unknown) => void> = {};
  const ws = {
    on: vi.fn((event: string, handler: (p: unknown) => void) => { handlers[event] = handler; return () => {}; }),
    onAny: vi.fn(() => () => {}),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
  return { ws, handlers };
}
const stores = { authStore: Object.assign(() => ({}), { getState: () => ({ user: { id: "u1" } }), subscribe: () => () => {}, setState: () => {}, destroy: () => {} }) } as unknown as RealtimeSyncStores;

describe("useRealtimeSync — timeline and relations invalidation", () => {
  let qc: QueryClient;
  const timelineKey = [...issueGraphKeys.all("ws-1"), "timeline", {}] as const;
  const relationsKey = issueGraphKeys.relations("ws-1", ["a"], {});
  const otherWs = [...issueGraphKeys.all("ws-2"), "timeline", {}] as const;

  beforeEach(() => {
    vi.useFakeTimers();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    for (const key of [timelineKey, relationsKey, otherWs]) qc.setQueryData(key, {});
  });
  afterEach(() => { vi.useRealTimers(); qc.clear(); });

  it.each([
    ["issue:updated", { issue: { id: "i1", status: "done" }, status_changed: true }],
    ["issue:created", { issue: { id: "i2" } }],
    ["issue:deleted", { issue_id: "i1" }],
    ["activity:created", { issue_id: "i1", entry: { type: "activity" } }],
  ])("%s invalidates this workspace's graph queries (debounced), not another workspace's", (event, payload) => {
    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, stores), { wrapper: ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider> });
    handlers[event]!(payload);
    vi.advanceTimersByTime(150);
    expect(qc.getQueryState(timelineKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(relationsKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(otherWs)?.isInvalidated).toBe(false);
  });
});
