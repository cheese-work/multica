import { describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { ChatSession } from "@multica/core/types";
import { planBulk, useChatSelection } from "./use-chat-selection";

const s = (id: string, o: Partial<ChatSession> = {}) => ({ id, status: "active", has_unread: false, unread_count: 0, pinned: false, ...o }) as ChatSession;

describe("useChatSelection", () => {
  it("is off until a row enters it, and exit clears it", () => {
    const { result } = renderHook(() => useChatSelection(["a", "b", "c"]));
    expect(result.current.active).toBe(false);
    act(() => result.current.enter("b"));
    expect(result.current.active).toBe(true);
    expect([...result.current.selected]).toEqual(["b"]);
    act(() => result.current.exit());
    expect(result.current.active).toBe(false);
    expect(result.current.selected.size).toBe(0);
  });

  it("reports a group as all / some / none and toggling selects or clears it", () => {
    const { result } = renderHook(() => useChatSelection(["a", "b", "c", "d"]));
    act(() => result.current.enter("a"));
    const group = ["a", "b"];
    expect(result.current.groupState(group)).toBe("some");
    act(() => result.current.toggleGroup(group)); // some -> all
    expect(result.current.groupState(group)).toBe("all");
    act(() => result.current.toggleGroup(group)); // all -> none
    expect(result.current.groupState(group)).toBe("none");
    expect(result.current.groupState(["c", "d"])).toBe("none");
  });

  it("drops selected ids that left the visible list", () => {
    const { result, rerender } = renderHook(({ ids }) => useChatSelection(ids), { initialProps: { ids: ["a", "b"] } });
    act(() => result.current.enter("a"));
    act(() => result.current.toggle("b"));
    rerender({ ids: ["b"] });
    expect([...result.current.selected]).toEqual(["b"]);
  });

  it("selectAll takes every visible id", () => {
    const { result } = renderHook(() => useChatSelection(["a", "b", "c"]));
    act(() => result.current.enter("a"));
    act(() => result.current.selectAll());
    expect(result.current.selected.size).toBe(3);
  });
});

describe("planBulk", () => {
  it("2 unseen + 1 seen: primary marks the 2 seen, the opposite offers the 1 as unseen", () => {
    const plan = planBulk([s("1", { has_unread: true }), s("2", { unread_count: 3 }), s("3")]);
    expect(plan.seen.primary).toEqual({ action: "seen", ids: ["1", "2"] });
    expect(plan.seen.opposite).toEqual({ action: "unseen", ids: ["3"] });
  });

  it("flips the primary to the majority and keeps the forward action on a tie", () => {
    expect(planBulk([s("1", { pinned: true }), s("2", { pinned: true }), s("3")]).pin.primary.action).toBe("unpin");
    expect(planBulk([s("1", { pinned: true }), s("2")]).pin.primary.action).toBe("pin");
    expect(planBulk([s("1", { status: "archived" }), s("2", { status: "archived" })]).archive.primary).toEqual({
      action: "unarchive",
      ids: ["1", "2"],
    });
  });

  it("never pins or marks seen/unseen an archived DM", () => {
    const plan = planBulk([s("1", { status: "archived", pinned: false, has_unread: true }), s("2")]);
    expect(plan.pin.primary.ids).toEqual(["2"]);
    expect(plan.seen.primary).toEqual({ action: "unseen", ids: ["2"] });
    expect(plan.seen.opposite.ids).toEqual([]);
  });
});
