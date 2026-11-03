import { beforeEach, describe, expect, it, vi } from "vitest";

import { useCommentPreviewRealtime } from "./use-comment-preview-realtime";

const { invalidateQueries, subscriptionSetups } = vi.hoisted(() => ({
  invalidateQueries: vi.fn(),
  subscriptionSetups: [] as ((ws: MockWS, wsId: string) => (() => void)[])[],
}));

type EventHandler = (payload: unknown) => void;

interface MockWS {
  on: ReturnType<typeof vi.fn>;
  onReconnect: ReturnType<typeof vi.fn>;
}

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ invalidateQueries }),
}));

vi.mock("@/lib/use-ws-subscriptions", () => ({
  useWSSubscriptions: (setup: (ws: MockWS, wsId: string) => (() => void)[]) => {
    subscriptionSetups.push(setup);
  },
}));

function connect() {
  useCommentPreviewRealtime();
  const handlers = new Map<string, EventHandler>();
  let reconnect: (() => void) | undefined;
  const ws: MockWS = {
    on: vi.fn((event: string, handler: EventHandler) => {
      handlers.set(event, handler);
      return () => {};
    }),
    onReconnect: vi.fn((handler: () => void) => {
      reconnect = handler;
      return () => {};
    }),
  };
  subscriptionSetups[0](ws, "ws-1");
  return { handlers, reconnect: () => reconnect?.() };
}

const keys = () => invalidateQueries.mock.calls.map(([q]) => q.queryKey);

describe("useCommentPreviewRealtime", () => {
  beforeEach(() => {
    invalidateQueries.mockReset();
    subscriptionSetups.length = 0;
  });

  it("refreshes one preview when its comment is edited", () => {
    const { handlers } = connect();
    handlers.get("comment:updated")?.({ comment: { id: "c1", issue_id: "i1" } });
    expect(keys()).toEqual([["issues", "ws-1", "comment-preview", "c1"]]);
  });

  it("refreshes one preview when its comment is deleted", () => {
    const { handlers } = connect();
    handlers.get("comment:deleted")?.({ comment_id: "c2", issue_id: "i1" });
    expect(keys()).toEqual([["issues", "ws-1", "comment-preview", "c2"]]);
  });

  it("refreshes every preview on reconnect", () => {
    const { reconnect } = connect();
    reconnect();
    expect(invalidateQueries).toHaveBeenCalledTimes(1);
    const arg = invalidateQueries.mock.calls[0]![0];
    expect(arg.predicate({ queryKey: ["issues", "ws-1", "comment-preview", "c1"] })).toBe(true);
    expect(arg.predicate({ queryKey: ["issues", "ws-1", "detail", "i1"] })).toBe(false);
  });
});
