import { describe, expect, it } from "vitest";
import { buildGraph, type TimelineEdge, type TimelineNode } from "./graph-core";

const node = (id: string, parentId: string | null, extra: Partial<TimelineNode> = {}): TimelineNode =>
  ({ id, identifier: id.toUpperCase(), title: id, status: "todo", parentId, ...extra });
const edge = (source: string, target: string, type: TimelineEdge["type"] = "wakeup"): TimelineEdge => ({ type, source, target });

// root > a > a1, a2 ; root > b > b1
const tree = [node("root", null), node("a", "root"), node("a1", "a"), node("a2", "a"), node("b", "root"), node("b1", "b")];

describe("buildGraph tree", () => {
  it("lists visible nodes in pre-order and detects leaves", () => {
    const g = buildGraph({ nodes: tree, edges: [] });
    expect(g.roots).toEqual(["root"]);
    expect(g.visible).toEqual(["root", "a", "a1", "a2", "b", "b1"]);
    expect(g.isLeaf("a1")).toBe(true);
    expect(g.isLeaf("a")).toBe(false);
  });

  it("treats a node whose parent is not in the response as a root", () => {
    const g = buildGraph({ nodes: [node("x", "gone"), node("y", "x")], edges: [] });
    expect(g.roots).toEqual(["x"]);
  });

  it("collapse hides descendants but keeps the collapsed node", () => {
    const g = buildGraph({ nodes: tree, edges: [], collapsed: new Set(["a"]) });
    expect(g.visible).toEqual(["root", "a", "b", "b1"]);
    expect([...g.hidden].sort()).toEqual(["a1", "a2"]);
  });
});

describe("buildGraph retarget", () => {
  it("retargets an edge into a hidden child to the visible parent and flags it", () => {
    const g = buildGraph({ nodes: tree, edges: [edge("b1", "a1")], collapsed: new Set(["a"]) });
    expect(g.edges).toMatchObject([{ source: "b1", target: "a", targetInside: true, sourceInside: false }]);
  });

  it("retargets both ends when both endpoints are hidden", () => {
    const g = buildGraph({ nodes: tree, edges: [edge("a1", "b1")], collapsed: new Set(["a", "b"]) });
    expect(g.edges).toMatchObject([{ source: "a", target: "b", sourceInside: true, targetInside: true }]);
  });

  it("drops an edge that falls inside one collapsed group and dedupes merged edges", () => {
    const g = buildGraph({ nodes: tree, edges: [edge("a1", "a2"), edge("b1", "a1"), edge("b1", "a2")], collapsed: new Set(["a"]) });
    expect(g.edges.map((e) => `${e.source}>${e.target}`)).toEqual(["b1>a"]);
  });

  it("keeps the inside flag when a retargeted edge merges with an ordinary one, in any input order", () => {
    const direct = edge("b1", "a");
    const viaChild = edge("b1", "a1");
    for (const edges of [[direct, viaChild], [viaChild, direct]]) {
      const g = buildGraph({ nodes: tree, edges, collapsed: new Set(["a"]) });
      expect(g.edges).toMatchObject([{ source: "b1", target: "a", targetInside: true }]);
    }
  });

  it("uses the nearest visible ancestor under nested collapse", () => {
    const nested = [...tree, node("a11", "a1")];
    const g = buildGraph({ nodes: nested, edges: [edge("b1", "a11")], collapsed: new Set(["a", "a1"]) });
    expect(g.edges[0]?.target).toBe("a");
  });

  it("retargets to an out-of-scope (context) ancestor", () => {
    const nodes = [node("ctx", null, { context: true }), node("c1", "ctx"), node("c2", "ctx"), node("o", null)];
    const g = buildGraph({ nodes, edges: [edge("o", "c1")], collapsed: new Set(["ctx"]) });
    expect(g.edges).toMatchObject([{ source: "o", target: "ctx", targetState: "in_response", targetInside: true }]);
  });

  it("marks a context endpoint and never retargets it", () => {
    const nodes = [node("ctx", null, { context: true }), node("a", null)];
    const g = buildGraph({ nodes, edges: [edge("a", "ctx")], collapsed: new Set(["ctx"]) });
    expect(g.edges).toMatchObject([{ target: "ctx", targetState: "context", targetInside: false }]);
  });

  it("never retargets a not_loaded endpoint", () => {
    const g = buildGraph({ nodes: tree, edges: [edge("b1", "zz"), edge("b1", "a1")], collapsed: new Set(["a"]), notLoaded: new Set(["a1"]) });
    expect(g.edges).toMatchObject([
      { target: "zz", targetState: "not_loaded", targetInside: false },
      { target: "a1", targetState: "not_loaded", targetInside: false },
    ]);
  });
});

describe("buildGraph edge id (repeated firings)", () => {
  it("keeps two edges with the same (type, source, target) apart when they carry distinct ids", () => {
    const g = buildGraph({ nodes: tree, edges: [{ ...edge("b1", "a1"), id: "e1" }, { ...edge("b1", "a1"), id: "e2" }] });
    expect(g.edges.map((e) => e.id)).toEqual(["e1", "e2"]);
  });

  it("still merges retargeted edges of one id, and edges without an id keep the old merge", () => {
    const withId = buildGraph({ nodes: tree, edges: [{ ...edge("b1", "a1"), id: "e1" }, { ...edge("b1", "a2"), id: "e1" }], collapsed: new Set(["a"]) });
    expect(withId.edges).toHaveLength(1);
    const noId = buildGraph({ nodes: tree, edges: [edge("b1", "a1"), edge("b1", "a1")] });
    expect(noId.edges).toHaveLength(1);
  });
});

describe("buildGraph cycle", () => {
  const cyc = [node("n3", "n1"), node("n1", "n2"), node("n2", "n3"), node("n4", "n3")];

  it("breaks a parent cycle at the smallest id, whatever the input order", () => {
    const a = buildGraph({ nodes: cyc, edges: [] });
    const b = buildGraph({ nodes: [...cyc].reverse(), edges: [] });
    expect(a.roots).toEqual(["n1"]);
    expect(b.roots).toEqual(["n1"]);
    expect([...a.visible].sort()).toEqual(["n1", "n2", "n3", "n4"]);
    expect(b.visible).toEqual(buildGraph({ nodes: [...cyc].reverse(), edges: [] }).visible);
  });
});
