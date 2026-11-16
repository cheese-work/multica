// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it } from "vitest";
import { createStore } from "zustand/vanilla";
import { viewStoreSlice, type IssueViewState } from "@multica/core/issues/stores/view-store";
import { ViewStoreProvider } from "@multica/core/issues/stores/view-store-context";
import { useGantt2Collapse } from "./use-gantt2-collapse";

describe("useGantt2Collapse", () => {
  it("toggles the gantt collapse set in the view store", () => {
    const store = createStore<IssueViewState>()((set) => viewStoreSlice(set));
    const wrapper = ({ children }: { children: ReactNode }) => <ViewStoreProvider store={store}>{children}</ViewStoreProvider>;
    const { result } = renderHook(() => useGantt2Collapse(), { wrapper });
    expect(result.current.collapsed.size).toBe(0);
    act(() => result.current.onToggleCollapse("a"));
    expect([...result.current.collapsed]).toEqual(["a"]);
    expect(store.getState().collapsedIssues.swimlane2).toEqual([]);
    act(() => result.current.onToggleCollapse("a"));
    expect(result.current.collapsed.size).toBe(0);
  });
});
