import { renderHook } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { useOpenChatMessageHit } from "./use-open-chat-message-hit";

const push = vi.hoisted(() => vi.fn());
vi.mock("../../navigation", () => ({ useNavigation: () => ({ push }) }));
vi.mock("@multica/core/paths", () => ({ useWorkspacePaths: () => ({ chat: () => "/acme/chat" }) }));

describe("useOpenChatMessageHit", () => {
  it("pushes the chat page entry that scrolls to the message", () => {
    const { result } = renderHook(() => useOpenChatMessageHit());
    result.current("s-1", "m-1");
    expect(push).toHaveBeenCalledTimes(1);
    expect(push).toHaveBeenCalledWith("/acme/chat?session=s-1&message=m-1");
  });
});
