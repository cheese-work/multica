import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";

const toastError = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { error: toastError } }));

import { useChatMessageJump } from "./use-chat-message-jump";

beforeEach(() => toastError.mockReset());

describe("useChatMessageJump", () => {
  it("targets the message only while its session is the open one", () => {
    const { result, rerender } = renderHook(({ active }) => useChatMessageJump(active, "not found"), {
      initialProps: { active: "s1" as string | null },
    });
    expect(result.current.jumpToMessageId).toBeNull();

    act(() => result.current.request("s2", "m-9"));
    expect(result.current.jumpToMessageId).toBeNull();

    rerender({ active: "s2" });
    expect(result.current.jumpToMessageId).toBe("m-9");
  });

  it("clears the target once the list reports it landed", () => {
    const { result } = renderHook(() => useChatMessageJump("s1", "not found"));
    act(() => result.current.request("s1", "m-1"));
    expect(result.current.jumpToMessageId).toBe("m-1");

    act(() => result.current.settle("landed"));
    expect(result.current.jumpToMessageId).toBeNull();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("tells the reader when the message is gone, then clears the target", () => {
    const { result } = renderHook(() => useChatMessageJump("s1", "not found"));
    act(() => result.current.request("s1", "m-1"));

    act(() => result.current.settle("missing"));
    expect(toastError).toHaveBeenCalledWith("not found");
    expect(result.current.jumpToMessageId).toBeNull();
  });

  it("lets the same message be jumped to twice", () => {
    const { result } = renderHook(() => useChatMessageJump("s1", "not found"));
    act(() => result.current.request("s1", "m-1"));
    act(() => result.current.settle("landed"));
    act(() => result.current.request("s1", "m-1"));
    expect(result.current.jumpToMessageId).toBe("m-1");
  });
});
