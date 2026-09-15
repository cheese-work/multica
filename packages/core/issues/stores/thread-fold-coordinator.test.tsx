// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { useCommentCollapseStore } from "./comment-collapse-store";
import { useResolvedExpandStore } from "./resolved-expand-store";
import { useIssueDisclosureStore } from "./issue-disclosure-store";
import { foldAllCommentThreads, unfoldAllCommentThreads } from "./thread-fold-coordinator";

const ISSUE = "issue-1";
const ROOTS = ["root-1", "root-2"];

function resetStores() {
  useCommentCollapseStore.setState({ collapsedByIssue: {} });
  useResolvedExpandStore.setState({ expandedByIssue: {} });
  useIssueDisclosureStore.setState({
    descriptionExpandedIssueIds: new Set(),
    expandedThreadIdsByIssue: {},
    justFoldedRootIdsByIssue: {},
  });
}

/**
 * Mirrors the real cross-store read shape in this codebase (the only actual
 * consumer of all three fold stores, comment-card.tsx / issue-detail.tsx):
 * one store read directly via the hook, two read via props derived from a
 * parent's own hook reads. A torn render is one where these three values
 * disagree about whether root-1's thread is folded.
 */
function Consumer() {
  const collapsed = useCommentCollapseStore((s) => s.isCollapsed(ISSUE, "root-1"));
  const resolvedFolded = !useResolvedExpandStore((s) => s.expandedByIssue[ISSUE]?.has("root-1"));
  const threadFolded = !useIssueDisclosureStore((s) => s.expandedThreadIdsByIssue[ISSUE]?.has("root-1"));
  const state = collapsed && resolvedFolded && threadFolded
    ? "all-folded"
    : !collapsed && !resolvedFolded && !threadFolded
      ? "all-open"
      : `torn:${String(collapsed)}/${String(resolvedFolded)}/${String(threadFolded)}`;
  return <div data-testid="state">{state}</div>;
}

describe("thread fold coordinator", () => {
  beforeEach(() => {
    resetStores();
    useResolvedExpandStore.getState().expandAll(ISSUE, ["root-1"]);
    useIssueDisclosureStore.getState().expandAllThreads(ISSUE, ["root-1"]);
  });

  it("folds all three stores together — no rendered snapshot is ever torn", () => {
    render(<Consumer />);
    expect(screen.getByTestId("state").textContent).toBe("all-open");

    act(() => {
      foldAllCommentThreads(ISSUE, ROOTS);
    });

    // The only render this produces must land directly on "all-folded" — a
    // torn intermediate ("torn:...") would mean a real component in this app
    // displayed some threads folded and others not for the same user action.
    expect(screen.getByTestId("state").textContent).toBe("all-folded");
  });

  it("unfolds all three stores together — no rendered snapshot is ever torn", () => {
    act(() => {
      foldAllCommentThreads(ISSUE, ROOTS);
    });
    render(<Consumer />);
    expect(screen.getByTestId("state").textContent).toBe("all-folded");

    act(() => {
      unfoldAllCommentThreads(ISSUE, ROOTS, ["root-1"]);
    });

    expect(screen.getByTestId("state").textContent).toBe("all-open");
  });

  it("documents the known gap: a raw (non-React) subscriber reading across stores can still observe an intermediate write", () => {
    // foldAllCommentThreads applies three independent stores' setState calls
    // sequentially. React's automatic batching (verified above) coalesces
    // them into a single consistent render for every real consumer in this
    // codebase today (all of them go through the useXStore() hook). A
    // hypothetical subscriber that calls store.subscribe(...) directly
    // (bypassing React) is NOT protected by that batching, because each
    // store's set() notifies its own listeners synchronously and
    // independently — this is a Zustand-level property this coordinator does
    // not change. No such subscriber exists in this codebase; if one is ever
    // added, it must not assume cross-store atomicity from this module.
    const seen: boolean[] = [];
    const unsub = useCommentCollapseStore.subscribe(() => {
      seen.push(!!useResolvedExpandStore.getState().expandedByIssue[ISSUE]?.has("root-1"));
    });
    foldAllCommentThreads(ISSUE, ROOTS);
    unsub();
    // At the moment comment-collapse's listener fires, resolved-expand has not
    // folded yet — demonstrating the gap this test documents.
    expect(seen).toEqual([true]);
  });

  // CHE-479: foldAllCommentThreads's call into collapseAllThreads must leave
  // a short-lived record of exactly which roots it just cleared, so
  // issue-detail.tsx's latch effect can skip re-latching them on the same
  // tick. This is the coordinator-level proof that the signal is actually
  // produced by the real fold-all entry point, not just by calling the
  // store method directly (already covered in issue-disclosure-store.test.ts).
  it("records the folded roots as just-folded so a consumer can suppress re-latching them", () => {
    act(() => {
      foldAllCommentThreads(ISSUE, ROOTS);
    });

    const justFolded = useIssueDisclosureStore.getState().justFoldedRootIdsByIssue[ISSUE];
    expect(justFolded).toBeDefined();
    // Only root-1 had been in expandedThreadIdsByIssue (from beforeEach);
    // collapseAllThreads only ever records what it actually had to clear.
    expect([...justFolded!]).toEqual(["root-1"]);
  });
});
