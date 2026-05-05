import { describe, it, expect } from "vitest";
import type { Issue } from "@multica/core/types";
import { buildIssueTree, type IssueTreeNode } from "./issue-tree";

function makeIssue(overrides: Partial<Issue> = {}): Issue {
  return {
    id: "issue-1",
    workspace_id: "ws-1",
    number: 1,
    identifier: "TES-1",
    title: "Test issue",
    description: null,
    status: "todo",
    priority: "medium",
    assignee_type: null,
    assignee_id: null,
    creator_type: "member",
    creator_id: "user-1",
    parent_issue_id: null,
    project_id: null,
    position: 0,
    due_date: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function flattenIds(node: IssueTreeNode): string[] {
  return [node.issue.id, ...node.children.flatMap(flattenIds)];
}

describe("buildIssueTree", () => {
  it("renders only the current branch plus direct children of the current issue", () => {
    const root = makeIssue({ id: "root", identifier: "TES-1", title: "Root" });
    const sibling = makeIssue({
      id: "sibling",
      identifier: "TES-2",
      title: "Sibling branch",
      parent_issue_id: root.id,
      position: 0,
    });
    const siblingChild = makeIssue({
      id: "sibling-child",
      identifier: "TES-3",
      title: "Sibling child",
      parent_issue_id: sibling.id,
    });
    const current = makeIssue({
      id: "current",
      identifier: "TES-4",
      title: "Current",
      parent_issue_id: root.id,
      position: 1,
    });
    const child = makeIssue({
      id: "child",
      identifier: "TES-5",
      title: "Current child",
      parent_issue_id: current.id,
      position: 0,
    });
    const grandchild = makeIssue({
      id: "grandchild",
      identifier: "TES-6",
      title: "Current grandchild",
      parent_issue_id: child.id,
    });

    const tree = buildIssueTree(
      [root, sibling, siblingChild, current, child, grandchild],
      current.id,
    );

    expect(tree?.currentPath).toEqual([root.id, current.id]);
    expect(tree?.root.issue.id).toBe(root.id);
    expect(flattenIds(tree!.root)).toEqual([root.id, current.id, child.id]);
  });

  it("sorts siblings by position, created time, then identifier", () => {
    const current = makeIssue({ id: "current", identifier: "TES-1", title: "Current" });
    const byIdentifierA = makeIssue({
      id: "by-identifier-a",
      identifier: "TES-2",
      parent_issue_id: current.id,
      position: 1,
      created_at: "2026-01-03T00:00:00Z",
    });
    const byIdentifierB = makeIssue({
      id: "by-identifier-b",
      identifier: "TES-3",
      parent_issue_id: current.id,
      position: 1,
      created_at: "2026-01-03T00:00:00Z",
    });
    const byCreated = makeIssue({
      id: "by-created",
      identifier: "TES-4",
      parent_issue_id: current.id,
      position: 1,
      created_at: "2026-01-02T00:00:00Z",
    });
    const byPosition = makeIssue({
      id: "by-position",
      identifier: "TES-5",
      parent_issue_id: current.id,
      position: 0,
      created_at: "2026-01-04T00:00:00Z",
    });

    const tree = buildIssueTree(
      [current, byIdentifierB, byCreated, byIdentifierA, byPosition],
      current.id,
    );

    expect(tree?.root.children.map((child) => child.issue.id)).toEqual([
      "by-position",
      "by-created",
      "by-identifier-a",
      "by-identifier-b",
    ]);
  });

  it("returns null when the current issue is missing", () => {
    expect(buildIssueTree([makeIssue({ id: "other" })], "missing")).toBeNull();
  });

  it("stops root walking at a cycle instead of recursing forever", () => {
    const current = makeIssue({ id: "current", parent_issue_id: "parent" });
    const parent = makeIssue({ id: "parent", parent_issue_id: "current" });

    const tree = buildIssueTree([current, parent], current.id);

    expect(tree?.currentPath).toEqual([parent.id, current.id]);
    expect(flattenIds(tree!.root)).toEqual([parent.id, current.id]);
  });
});
