import { beforeEach, describe, expect, it } from "vitest";
import {
  selectDescriptionExpanded,
  selectExpandedThreads,
  selectJustFoldedRoots,
  useIssueDisclosureStore,
} from "./issue-disclosure-store";

describe("issue disclosure store", () => {
  beforeEach(() => {
    useIssueDisclosureStore.setState({
      descriptionExpandedIssueIds: new Set(),
      expandedThreadIdsByIssue: {},
      justFoldedRootIdsByIssue: {},
    });
  });

  it("keeps description expansion per issue and makes idempotent writes stable", () => {
    const { setDescriptionExpanded } = useIssueDisclosureStore.getState();
    setDescriptionExpanded("issue-1", true);
    expect(selectDescriptionExpanded("issue-1")(useIssueDisclosureStore.getState())).toBe(true);
    expect(selectDescriptionExpanded("issue-2")(useIssueDisclosureStore.getState())).toBe(false);

    const before = useIssueDisclosureStore.getState().descriptionExpandedIssueIds;
    setDescriptionExpanded("issue-1", true);
    expect(useIssueDisclosureStore.getState().descriptionExpandedIssueIds).toBe(before);

    setDescriptionExpanded("issue-1", false);
    expect(selectDescriptionExpanded("issue-1")(useIssueDisclosureStore.getState())).toBe(false);
  });

  it("keeps thread expansion immutable and isolated by issue", () => {
    const { setThreadExpanded, expandAllThreads } = useIssueDisclosureStore.getState();
    setThreadExpanded("issue-1", "root-1", true);
    const first = selectExpandedThreads("issue-1")(useIssueDisclosureStore.getState());
    expandAllThreads("issue-1", ["root-2", "root-1"]);
    const second = selectExpandedThreads("issue-1")(useIssueDisclosureStore.getState());

    expect([...first]).toEqual(["root-1"]);
    expect([...second].sort()).toEqual(["root-1", "root-2"]);
    expect(selectExpandedThreads("issue-2")(useIssueDisclosureStore.getState()).size).toBe(0);
  });

  it("folds and clears only the requested issue", () => {
    const { setDescriptionExpanded, expandAllThreads, collapseAllThreads, clearIssue } =
      useIssueDisclosureStore.getState();
    setDescriptionExpanded("issue-1", true);
    setDescriptionExpanded("issue-2", true);
    expandAllThreads("issue-1", ["root-1"]);
    expandAllThreads("issue-2", ["root-2"]);

    collapseAllThreads("issue-1");
    expect(selectExpandedThreads("issue-1")(useIssueDisclosureStore.getState()).size).toBe(0);
    expect([...selectExpandedThreads("issue-2")(useIssueDisclosureStore.getState())]).toEqual(["root-2"]);

    clearIssue("issue-1");
    expect(selectDescriptionExpanded("issue-1")(useIssueDisclosureStore.getState())).toBe(false);
    expect(selectDescriptionExpanded("issue-2")(useIssueDisclosureStore.getState())).toBe(true);
  });

  it("returns a stable empty thread selector across unrelated writes", () => {
    const select = selectExpandedThreads("missing");
    const before = select(useIssueDisclosureStore.getState());
    useIssueDisclosureStore.getState().setThreadExpanded("issue-1", "root-1", true);
    expect(select(useIssueDisclosureStore.getState())).toBe(before);
  });

  // CHE-479: collapseAllThreads must record exactly which roots it just
  // un-expanded, so a consumer (issue-detail.tsx's latch effect) can tell
  // "just fold-all'd" apart from "never expanded" for the same tick.
  describe("justFoldedRootIdsByIssue (CHE-479)", () => {
    it("records the exact roots collapseAllThreads just removed", () => {
      const { expandAllThreads, collapseAllThreads } = useIssueDisclosureStore.getState();
      expandAllThreads("issue-1", ["root-1", "root-2"]);

      collapseAllThreads("issue-1");

      expect([...selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState())].sort()).toEqual([
        "root-1",
        "root-2",
      ]);
      // The roots really were cleared, not just mirrored.
      expect(selectExpandedThreads("issue-1")(useIssueDisclosureStore.getState()).size).toBe(0);
    });

    it("does not record anything when there was nothing expanded to fold", () => {
      useIssueDisclosureStore.getState().collapseAllThreads("issue-1");
      expect(selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState()).size).toBe(0);
    });

    it("overwrites, not merges, the just-folded set on a subsequent fold", () => {
      const { expandAllThreads, collapseAllThreads } = useIssueDisclosureStore.getState();
      expandAllThreads("issue-1", ["root-1"]);
      collapseAllThreads("issue-1");
      expect([...selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState())]).toEqual(["root-1"]);

      expandAllThreads("issue-1", ["root-2"]);
      collapseAllThreads("issue-1");
      expect([...selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState())]).toEqual(["root-2"]);
    });

    // The consumer (issue-detail.tsx's latch effect) never clears this
    // signal back to empty — it distinguishes one Fold All occurrence from
    // the next purely by comparing the Set's object identity across
    // renders. This is the store-level guarantee that identity actually
    // changes every time, even when the resulting root membership happens
    // to be identical.
    it("produces a new Set instance on every collapseAllThreads call, even with identical membership", () => {
      const { expandAllThreads, collapseAllThreads } = useIssueDisclosureStore.getState();
      expandAllThreads("issue-1", ["root-1"]);
      collapseAllThreads("issue-1");
      const first = selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState());

      expandAllThreads("issue-1", ["root-1"]);
      collapseAllThreads("issue-1");
      const second = selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState());

      expect(second).not.toBe(first);
      expect([...second]).toEqual(["root-1"]);
    });

    it("isolates just-folded roots by issue", () => {
      const { expandAllThreads, collapseAllThreads } = useIssueDisclosureStore.getState();
      expandAllThreads("issue-1", ["root-1"]);
      expandAllThreads("issue-2", ["root-2"]);

      collapseAllThreads("issue-1");

      expect([...selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState())]).toEqual(["root-1"]);
      expect(selectJustFoldedRoots("issue-2")(useIssueDisclosureStore.getState()).size).toBe(0);
    });

    it("clearIssue also drops any pending just-folded signal for that issue", () => {
      const { expandAllThreads, collapseAllThreads, clearIssue } = useIssueDisclosureStore.getState();
      expandAllThreads("issue-1", ["root-1"]);
      collapseAllThreads("issue-1");
      expect(selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState()).size).toBe(1);

      clearIssue("issue-1");
      expect(selectJustFoldedRoots("issue-1")(useIssueDisclosureStore.getState()).size).toBe(0);
    });

    it("returns a stable empty selector reference across unrelated writes", () => {
      const select = selectJustFoldedRoots("missing");
      const before = select(useIssueDisclosureStore.getState());
      useIssueDisclosureStore.getState().setThreadExpanded("issue-1", "root-1", true);
      expect(select(useIssueDisclosureStore.getState())).toBe(before);
    });
  });
});
