import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { DRAFT_NEW_SESSION, createChatStore, registerChatStore, useChatStore } from "@multica/core/chat";

const push = vi.fn();

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: string[] }) => ({
    data: opts.queryKey.includes("agents")
      ? [{ id: "agent-1", archived_at: null }]
      : [{ user_id: "user-1", role: "owner" }],
  }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: unknown) => unknown) => selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({ useWorkspacePaths: () => ({ chat: () => "/ws/chat" }) }));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("../issues/components/pickers/assignee-picker", () => ({ canAssignAgent: () => true }));
vi.mock("../navigation", () => ({ useNavigation: () => ({ push }) }));

import { useStartChatWithDraft } from "./use-start-chat-with-draft";

describe("useStartChatWithDraft", () => {
  beforeEach(() => {
    push.mockClear();
    const m = new Map<string, string>();
    registerChatStore(
      createChatStore({
        storage: {
          getItem: (k) => m.get(k) ?? null,
          setItem: (k, v) => void m.set(k, v),
          removeItem: (k) => void m.delete(k),
        },
      }),
    );
  });

  it("seeds the canned prompt into an empty new-chat draft", () => {
    const { result } = renderHook(() => useStartChatWithDraft());
    result.current?.("canned");
    expect(useChatStore.getState().inputDrafts[DRAFT_NEW_SESSION]).toBe("canned");
    expect(push).toHaveBeenCalledWith("/ws/chat?agent=agent-1");
  });

  it("keeps an unsent new-chat draft instead of overwriting it", () => {
    useChatStore.getState().setInputDraft(DRAFT_NEW_SESSION, "my unsent text");
    const { result } = renderHook(() => useStartChatWithDraft());
    result.current?.("canned");
    expect(useChatStore.getState().inputDrafts[DRAFT_NEW_SESSION]).toBe("my unsent text");
    expect(push).toHaveBeenCalledWith("/ws/chat?agent=agent-1");
  });
});
