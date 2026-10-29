import { describe, expect, it } from "vitest";
import type { IssueRelations } from "../types";
import { filterRelations } from "./relations";

const node = (id: string) => ({ id, identifier: id.toUpperCase(), title: id, status: "todo" });
const graph: IssueRelations = {
  depth: 2, omitted_count: 0,
  nodes: [node("root"), node("child"), node("ref"), node("far")],
  edges: [
    { source: "root", target: "child", type: "parent" },
    { source: "root", target: "ref", type: "mention" },
    { source: "ref", target: "far", type: "wakeup" },
  ],
};

describe("filterRelations", () => {
  it("keeps everything when nothing is hidden", () => {
    expect(filterRelations(graph, "root", [])).toEqual(graph);
  });

  it("hiding mentions drops mention edges and nodes reachable only through them", () => {
    const filtered = filterRelations(graph, "root", ["mention"]);
    expect(filtered.edges.map((edge) => edge.type)).toEqual(["parent"]);
    expect(filtered.nodes.map((n) => n.id)).toEqual(["root", "child"]);
  });

  it("keeps a node still reachable through another visible edge type", () => {
    const withWake = { ...graph, edges: [...graph.edges, { source: "child", target: "ref", type: "wakeup" as const }] };
    expect(filterRelations(withWake, "root", ["mention"]).nodes.map((n) => n.id)).toEqual(["root", "child", "ref", "far"]);
  });

  it("treats children_done as a wake edge and never hides unfiltered types", () => {
    const g = { ...graph, edges: [{ source: "root", target: "child", type: "children_done" as const }, { source: "root", target: "ref", type: "shared_pr" as const }] };
    expect(filterRelations(g, "root", ["wakeup"]).edges.map((edge) => edge.type)).toEqual(["shared_pr"]);
  });
});
