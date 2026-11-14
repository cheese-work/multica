import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import {
  CHAT_FIRST_ITEM_INDEX_BASE,
  useChatFirstItemIndex,
} from "./use-chat-first-item-index";

const ids = (from: number, to: number) =>
  Array.from({ length: to - from + 1 }, (_, i) => `m${from + i}`);

function setup(initial: string[]) {
  return renderHook(({ keys }) => useChatFirstItemIndex(keys), {
    initialProps: { keys: initial },
  });
}

describe("useChatFirstItemIndex", () => {
  it("starts at the base index", () => {
    expect(setup(ids(11, 20)).result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE);
  });

  it("keeps the index when a refetch only re-pages the same messages", () => {
    // Page boundary moved by one: the same rows, one more new row at the end.
    const { result, rerender } = setup(ids(11, 20));
    rerender({ keys: ids(11, 21) });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE);
  });

  it("decreases the index by exactly the rows prepended", () => {
    const { result, rerender } = setup(ids(11, 20));
    rerender({ keys: ids(6, 20) });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE - 5);
    rerender({ keys: ids(1, 20) });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE - 10);
  });

  it("counts only the rows that render, not hidden ones", () => {
    // The caller passes rendered keys; a hidden row never reaches the hook,
    // so an older page of 5 messages with 2 hidden moves the index by 3.
    const { result, rerender } = setup(ids(11, 20));
    rerender({ keys: ["m6", "m8", "m10", ...ids(11, 20)] });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE - 3);
  });

  it("raises the index when a refetch slides the loaded window and drops the oldest rows", () => {
    // Full pages re-fetched after a new message: one row enters at the end,
    // the oldest loaded row falls off the front. Surviving rows keep their index.
    const { result, rerender } = setup(ids(1, 150));
    rerender({ keys: ids(2, 151) });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE + 1);
    rerender({ keys: ids(4, 152) });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE + 3);
  });

  it("keeps surviving rows at their index when older rows arrive and the tail moves", () => {
    const { result, rerender } = setup(ids(11, 20));
    rerender({ keys: [...ids(6, 19), "task:t1"] });
    expect(result.current).toBe(CHAT_FIRST_ITEM_INDEX_BASE - 5);
  });
});
