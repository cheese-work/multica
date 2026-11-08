import { describe, expect, it } from "vitest";
import { layoutSwimlanes, type SwimEdge, type SwimNode } from "./swimlane-layout";

const n = (id: string, at: string, lane: string | null = "a"): SwimNode => ({ id, at, lane });
const e = (source: string, target: string): SwimEdge => ({ source, target });
const rank = (nodes: SwimNode[], edges: SwimEdge[]) => Object.fromEntries(layoutSwimlanes({ nodes, edges }).rank);

describe("layoutSwimlanes rank", () => {
  it("ranks by longest causal path from a start node, not by clock", () => {
    // x (latest clock) causes y; z depends on both x and y.
    const nodes = [n("z", "2026-01-01"), n("y", "2026-01-02"), n("x", "2026-01-09")];
    expect(rank(nodes, [e("x", "y"), e("y", "z"), e("x", "z")])).toEqual({ x: 0, y: 1, z: 2 });
  });

  it("ignores self loops, duplicate edges and edges to unknown nodes", () => {
    const nodes = [n("a", "1"), n("b", "2")];
    expect(rank(nodes, [e("a", "a"), e("a", "b"), e("a", "b"), e("b", "ghost"), e("ghost", "a")])).toEqual({ a: 0, b: 1 });
  });

  it("is independent of node and edge input order", () => {
    const nodes = [n("a", "1"), n("b", "2"), n("c", "3"), n("d", "4")];
    const edges = [e("a", "b"), e("b", "c"), e("c", "a"), e("c", "d")];
    const base = layoutSwimlanes({ nodes, edges });
    const flipped = layoutSwimlanes({ nodes: [...nodes].reverse(), edges: [...edges].reverse() });
    expect(flipped.lanes).toEqual(base.lanes);
    expect([...flipped.rank].sort()).toEqual([...base.rank].sort());
    expect(base.rank.size).toBe(4);
  });
});

describe("layoutSwimlanes cycles", () => {
  it("breaks a cycle at its latest (at, id) node: the earliest node is the start", () => {
    const nodes = [n("c", "3"), n("b", "2"), n("a", "1")];
    expect(rank(nodes, [e("a", "b"), e("b", "c"), e("c", "a")])).toEqual({ a: 0, b: 1, c: 2 });
  });

  it("breaks equal timestamps by id", () => {
    const nodes = [n("q", "1"), n("p", "1")];
    expect(rank(nodes, [e("q", "p"), e("p", "q")])).toEqual({ p: 0, q: 1 });
  });

  it("keeps the acyclic part of a graph that contains a cycle", () => {
    const nodes = [n("s", "0"), n("a", "1"), n("b", "2"), n("t", "3")];
    expect(rank(nodes, [e("s", "a"), e("a", "b"), e("b", "a"), e("b", "t")])).toEqual({ s: 0, a: 1, b: 2, t: 3 });
  });

  it("starts a cycle entered through a later member at its earliest node", () => {
    // s -> b -> a -> b with s < a < b: the cycle {a, b} starts at a, so b -> a is the cut edge.
    const nodes = [n("b", "3"), n("a", "2"), n("s", "1")];
    expect(rank(nodes, [e("s", "b"), e("b", "a"), e("a", "b")])).toEqual({ s: 0, a: 0, b: 1 });
  });

  it("handles a long chain without recursion limits", () => {
    const nodes = Array.from({ length: 20000 }, (_, i) => n(`n${String(i).padStart(5, "0")}`, String(i).padStart(5, "0")));
    const edges = nodes.slice(1).map((x, i) => e(nodes[i]!.id, x.id));
    expect(layoutSwimlanes({ nodes, edges }).rank.get("n19999")).toBe(19999);
  });
});

describe("layoutSwimlanes lanes", () => {
  it("sorts lanes by key with the empty lane last and orders nodes by rank, at, id", () => {
    const nodes = [n("u", "1", null), n("b2", "5", "bob"), n("b1", "4", "bob"), n("a1", "9", "ann"), n("b0", "6", "bob")];
    const { lanes } = layoutSwimlanes({ nodes, edges: [e("b1", "b0")] });
    expect(lanes).toEqual([
      { key: "ann", nodeIds: ["a1"] },
      { key: "bob", nodeIds: ["b1", "b2", "b0"] },
      { key: null, nodeIds: ["u"] },
    ]);
  });

  it("returns no lanes for no nodes", () => {
    expect(layoutSwimlanes({ nodes: [], edges: [] }).lanes).toEqual([]);
  });
});
