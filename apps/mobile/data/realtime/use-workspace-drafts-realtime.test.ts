import { beforeEach, describe, expect, it, vi } from "vitest";

import { useWorkspaceDraftsRealtime } from "./use-workspace-drafts-realtime";

const { clearScopeDrafts, subscriptionSetups } = vi.hoisted(() => ({
  clearScopeDrafts: vi.fn(async () => {}),
  subscriptionSetups: [] as ((ws: MockWS, wsId: string) => (() => void)[])[],
}));

type EventHandler = (payload: unknown) => void;

interface MockWS {
  on: ReturnType<typeof vi.fn>;
}

vi.mock("@/data/draft-persistence", () => ({ clearScopeDrafts }));
vi.mock("@/data/auth-store", () => ({
  useAuthStore: { getState: () => ({ user: { id: "u1" } }) },
}));
vi.mock("@/lib/use-ws-subscriptions", () => ({
  useWSSubscriptions: (setup: (ws: MockWS, wsId: string) => (() => void)[]) => {
    subscriptionSetups.push(setup);
  },
}));

function connect() {
  useWorkspaceDraftsRealtime();
  const handlers = new Map<string, EventHandler>();
  const ws: MockWS = {
    on: vi.fn((event: string, handler: EventHandler) => {
      handlers.set(event, handler);
      return () => {};
    }),
  };
  subscriptionSetups[0](ws, "ws-1");
  return handlers;
}

describe("useWorkspaceDraftsRealtime", () => {
  beforeEach(() => {
    clearScopeDrafts.mockClear();
    subscriptionSetups.length = 0;
  });

  it("clears the drafts of the deleted workspace for the signed-in user", () => {
    connect().get("workspace:deleted")?.({ workspace_id: "ws-9" });
    expect(clearScopeDrafts).toHaveBeenCalledExactlyOnceWith("u1:ws-9");
  });
});
