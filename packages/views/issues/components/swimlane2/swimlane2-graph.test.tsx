// @vitest-environment jsdom

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { TimelineEdge } from "@multica/core/issues/graph-core";
import { renderWithI18n } from "../../../test/i18n";
import { Swimlane2Graph, type Swimlane2Node } from "./swimlane2-graph";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({
    entryOf: () => undefined,
    categoryOf: () => "started",
    colorOf: () => null,
    iconOf: () => null,
  }),
}));

afterEach(cleanup);

const node = (id: string, parentId: string | null = null, extra: Partial<Swimlane2Node> = {}): Swimlane2Node => ({
  id,
  identifier: id.toUpperCase(),
  title: `title ${id}`,
  status: "in_progress",
  parentId,
  at: `2026-01-01T00:00:${id.length.toString().padStart(2, "0")}Z`,
  lane: "alice",
  ...extra,
});
const edge = (type: TimelineEdge["type"], source: string, target: string): TimelineEdge => ({ type, source, target });
const drawn = (container: HTMLElement, type: string) => container.querySelectorAll(`[data-edge-type="${type}"]`);

describe("Swimlane2Graph edges", () => {
  const nodes = [node("a"), node("b"), node("c"), node("d")];
  const edges = [edge("wakeup", "a", "b"), edge("dependency", "b", "c"), edge("parent", "a", "c"), edge("mention", "c", "d")];

  it("draws one edge per type with its own line style, so type is never colour alone", () => {
    const { container } = renderWithI18n(<Swimlane2Graph nodes={nodes} edges={edges} />);
    const line = (type: string) => drawn(container, type)[0]!.querySelector("path")!;
    expect(drawn(container, "wakeup")).toHaveLength(1);
    expect(line("wakeup").getAttribute("stroke-dasharray")).toBeNull();
    expect(line("wakeup").getAttribute("marker-end")).toBeTruthy();
    expect(line("dependency").getAttribute("stroke-dasharray")).toBe("6 4");
    expect(line("dependency").getAttribute("marker-end")).toBeTruthy();
    expect(line("parent").getAttribute("marker-end")).toBeNull();
    expect(drawn(container, "parent")[0]!.querySelector("circle")).not.toBeNull();
    expect(line("mention").getAttribute("stroke-dasharray")).toBe("1 4");
    expect(drawn(container, "mention")[0]!.textContent).toContain("@");
  });

  it("names each edge by type and endpoints for assistive tech", () => {
    const { container } = renderWithI18n(<Swimlane2Graph nodes={nodes} edges={edges} />);
    expect(drawn(container, "wakeup")[0]!.getAttribute("aria-label")).toBe("Wakeup: A → B");
    expect(drawn(container, "dependency")[0]!.getAttribute("aria-label")).toBe("Dependency: B → C");
    expect(drawn(container, "parent")[0]!.getAttribute("aria-label")).toBe("Parent / child: A → C");
    expect(drawn(container, "mention")[0]!.getAttribute("aria-label")).toBe("Mention: C → D");
  });

  it("does not draw edge types Swimlane 2.0 has no style for", () => {
    const { container } = renderWithI18n(
      <Swimlane2Graph nodes={nodes} edges={[edge("duplicate", "a", "b"), edge("shared_pr", "a", "b"), edge("unknown", "a", "b")]} />,
    );
    expect(container.querySelectorAll("[data-edge-type]")).toHaveLength(0);
  });

  it("skips an edge whose endpoint is not a loaded node", () => {
    const { container } = renderWithI18n(<Swimlane2Graph nodes={nodes} edges={[edge("wakeup", "a", "gone")]} />);
    expect(container.querySelectorAll("[data-edge-type]")).toHaveLength(0);
  });

  it("shows a legend count per drawn edge type", () => {
    renderWithI18n(<Swimlane2Graph nodes={nodes} edges={[...edges, edge("wakeup", "b", "a")]} />);
    const legend = screen.getByRole("list", { name: "Edge types" });
    expect(within(legend).getByText("Wakeup").parentElement!.textContent).toContain("2");
    expect(within(legend).getByText("Mention").parentElement!.textContent).toContain("1");
  });
});

describe("Swimlane2Graph nodes", () => {
  it("shows a status icon and a text label on every node, never colour alone", () => {
    const nodes = [node("a", null, { status: "in_progress" }), node("b", null, { status: "done", lane: "bob" })];
    renderWithI18n(<Swimlane2Graph nodes={nodes} edges={[]} />);
    const a = screen.getByRole("group", { name: /A.*In Progress/ });
    expect(within(a).getByText("In Progress")).toBeTruthy();
    expect(a.querySelector("svg")).not.toBeNull();
    const b = screen.getByRole("group", { name: /B.*Done/ });
    expect(within(b).getByText("Done")).toBeTruthy();
    expect(b.querySelector("svg")).not.toBeNull();
  });

  // X1 (client part): root A with 45 children plus a disconnected tree B (root and 2 children).
  it("X1: renders 49 nodes and 47 parent edges for two trees in one scope", () => {
    const nodes = [
      node("rootA"),
      ...Array.from({ length: 45 }, (_, i) => node(`a${i}`, "rootA")),
      node("rootB", null, { lane: "bob" }),
      node("b0", "rootB", { lane: "bob" }),
      node("b1", "rootB", { lane: "bob" }),
    ];
    const edges = nodes.filter((n) => n.parentId).map((n) => edge("parent", n.parentId!, n.id));
    const { container } = renderWithI18n(<Swimlane2Graph nodes={nodes} edges={edges} />);
    expect(screen.getAllByRole("group")).toHaveLength(49);
    expect(drawn(container, "parent")).toHaveLength(47);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("Swimlane2Graph incomplete banner", () => {
  const nodes = [node("a"), node("b"), node("c")];
  const edges = [edge("wakeup", "a", "b"), edge("wakeup", "b", "c")];

  // X5: the second edge page fails, so its endpoints are not_loaded until a retry completes the graph.
  it("X5: a failed page shows the banner, draws no edge to a not_loaded endpoint, and a retry completes the graph", () => {
    const onRetry = vi.fn();
    const { container, rerender } = renderWithI18n(
      <Swimlane2Graph nodes={nodes} edges={edges} notLoaded={new Set(["c"])} onRetry={onRetry} />,
    );
    expect(screen.getByRole("alert").textContent).toContain("Relations incomplete");
    expect(drawn(container, "wakeup")).toHaveLength(1);
    expect(container.querySelector("[data-inside]")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);

    rerender(<Swimlane2Graph nodes={nodes} edges={edges} notLoaded={new Set()} onRetry={onRetry} />);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(drawn(container, "wakeup")).toHaveLength(2);
  });

  it("shows a load-more banner while timeline pages are missing", () => {
    const onLoadMore = vi.fn();
    renderWithI18n(<Swimlane2Graph nodes={nodes} edges={edges} hasMoreIssues onLoadMore={onLoadMore} />);
    expect(screen.getByRole("alert").textContent).toContain("More issues not loaded");
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(onLoadMore).toHaveBeenCalledTimes(1);
  });

  it("shows no banner when everything is loaded", () => {
    renderWithI18n(<Swimlane2Graph nodes={nodes} edges={edges} />);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
