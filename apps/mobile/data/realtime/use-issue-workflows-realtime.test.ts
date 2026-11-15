import { QueryClient } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { issueWorkflowKeys } from "@/data/queries/issue-workflows";
import { useIssueWorkflowsRealtime } from "./use-issue-workflows-realtime";

type EventHandler = (payload: unknown) => void;
type SubscriptionSetup = (ws: unknown, wsId: string) => (() => void)[];

const state = vi.hoisted(() => ({
  qc: undefined as unknown as QueryClient,
  subscriptionSetups: [] as SubscriptionSetup[],
}));

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQueryClient: () => state.qc,
}));

// The key lives beside the query, whose module reaches react-native via the API client.
vi.mock("@/data/api", () => ({ api: {} }));

vi.mock("@/lib/use-ws-subscriptions", () => ({
  useWSSubscriptions: (setup: SubscriptionSetup) => {
    state.subscriptionSetups.push(setup);
  },
}));

const wsId = "workspace-1";

function connect() {
  const handlers = new Map<string, EventHandler>();
  let reconnect: (() => void) | undefined;
  const ws = {
    on: vi.fn((event: string, handler: EventHandler) => {
      handlers.set(event, handler);
      return () => {};
    }),
    onReconnect: vi.fn((cb: () => void) => {
      reconnect = cb;
      return () => {};
    }),
  };
  state.subscriptionSetups[0](ws, wsId);
  return { handlers, reconnect: () => reconnect?.() };
}

describe("useIssueWorkflowsRealtime", () => {
  beforeEach(() => {
    state.qc = new QueryClient();
    state.subscriptionSetups = [];
    useIssueWorkflowsRealtime();
  });

  it("invalidates the workflow list on issue_workflow:changed", () => {
    const invalidate = vi.spyOn(state.qc, "invalidateQueries");
    connect().handlers.get("issue_workflow:changed")?.({ action: "updated" });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: issueWorkflowKeys.list(wsId) });
  });

  it("invalidates the workflow list after a reconnect", () => {
    const invalidate = vi.spyOn(state.qc, "invalidateQueries");
    connect().reconnect();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: issueWorkflowKeys.list(wsId) });
  });
});
