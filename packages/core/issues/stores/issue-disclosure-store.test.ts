import { beforeEach, describe, expect, it } from "vitest";
import {
  selectDescriptionExpanded,
  selectExpandedThreads,
  useIssueDisclosureStore,
} from "./issue-disclosure-store";

describe("issue disclosure store", () => {
  beforeEach(() => {
    useIssueDisclosureStore.setState({
      descriptionExpandedIssueIds: new Set(),
      expandedThreadIdsByIssue: {},
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
});
