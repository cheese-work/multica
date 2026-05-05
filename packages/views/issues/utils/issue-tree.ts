import type { Issue } from "@multica/core/types";

export interface IssueTreeNode {
  issue: Issue;
  children: IssueTreeNode[];
}

export interface IssueTree {
  root: IssueTreeNode;
  currentPath: string[];
}

function cloneNodeWithChildren(
  node: IssueTreeNode,
  children: IssueTreeNode[],
): IssueTreeNode {
  return { issue: node.issue, children };
}

function compareIssuePosition(a: Issue, b: Issue) {
  const byPosition = a.position - b.position;
  if (byPosition !== 0) return byPosition;

  const byCreated = new Date(a.created_at).getTime() - new Date(b.created_at).getTime();
  if (byCreated !== 0) return byCreated;

  return a.identifier.localeCompare(b.identifier);
}

export function buildIssueTree(issues: Issue[], currentIssueId: string): IssueTree | null {
  const nodes = new Map<string, IssueTreeNode>();
  for (const issue of issues) {
    nodes.set(issue.id, { issue, children: [] });
  }

  const current = nodes.get(currentIssueId);
  if (!current) return null;

  for (const node of nodes.values()) {
    const parentId = node.issue.parent_issue_id;
    if (!parentId || parentId === node.issue.id) continue;

    const parent = nodes.get(parentId);
    if (parent) {
      parent.children.push(node);
    }
  }

  for (const node of nodes.values()) {
    node.children.sort((a, b) => compareIssuePosition(a.issue, b.issue));
  }

  const path = [currentIssueId];
  const seen = new Set(path);
  let root = current;
  let parentId = current.issue.parent_issue_id;
  while (parentId && !seen.has(parentId)) {
    const parent = nodes.get(parentId);
    if (!parent) break;

    root = parent;
    path.unshift(parentId);
    seen.add(parentId);
    parentId = parent.issue.parent_issue_id;
  }

  const pathSet = new Set(path);
  const prunedById = new Map<string, IssueTreeNode>();
  const getPrunedNode = (nodeId: string, ancestors = new Set<string>()): IssueTreeNode => {
    const cached = prunedById.get(nodeId);
    if (cached) return cached;

    const node = nodes.get(nodeId)!;
    const nextAncestors = new Set(ancestors);
    nextAncestors.add(nodeId);
    const children = node.children
      .filter((child) => !nextAncestors.has(child.issue.id))
      .filter((child) => pathSet.has(child.issue.id) || nodeId === currentIssueId)
      .map((child) =>
        pathSet.has(child.issue.id)
          ? getPrunedNode(child.issue.id, nextAncestors)
          : cloneNodeWithChildren(child, []),
      );
    const pruned = cloneNodeWithChildren(node, children);
    prunedById.set(nodeId, pruned);
    return pruned;
  };

  return { root: getPrunedNode(root.issue.id), currentPath: path };
}
