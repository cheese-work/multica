import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { IssueRelations } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import { useRelationsFilterStore } from "@multica/core/issues/stores/relations-filter-store";
import enIssues from "../../locales/en/issues.json";
import { IssueRelationsGraph } from "./issue-relations-graph";

const mermaid = vi.hoisted(() => ({ render: vi.fn() }));
vi.mock("../../editor/mermaid-diagram", () => ({ getMermaid: () => mermaid }));
const catalog = {
  categoryOf: (key: string) => key === "done" || key === "custom-done" ? "done" as const : "unstarted" as const,
  colorOf: (key: string) => key === "custom-done" ? "#aa00cc" : null,
  labelOf: (key: string) => key,
};
const graph: IssueRelations = {
  depth: 1, omitted_count: 0,
  nodes: [{ id: "root", identifier: "CHE-1", title: "Root", status: "todo" }, { id: "child", identifier: "CHE-2", title: "Child", status: "done" }],
  edges: [{ source: "root", target: "child", type: "parent" }],
};
function show(data = graph, onNavigate = vi.fn()) {
  return render(<I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueRelationsGraph graph={data} currentIssueId="root" catalog={catalog} onNavigate={onNavigate} hrefForIssue={(id) => `/issues/${id}`} /></I18nProvider>);
}

beforeEach(() => {
  useRelationsFilterStore.setState({ hidden: ["mention"] });
  mermaid.render.mockClear();
});
describe("issue relations graph", () => {
  it("renders native issue links immediately without Mermaid", () => {
    show();
    expect(screen.getByRole("link", { name: "CHE-2: Child (done)" }).tagName).toBe("A");
    expect(mermaid.render).not.toHaveBeenCalled();
  });
  it("escapes hostile titles and keeps the full title accessible", () => {
    const title = '<img src=x onerror=alert(1)> Very long title that must remain in the accessible name';
    const view = show({ ...graph, nodes: [graph.nodes[0]!, { ...graph.nodes[1]!, title }] });
    const link = screen.getByRole("link", { name: `CHE-2: ${title} (done)` });
    expect(link).toHaveAttribute("title", `CHE-2: ${title} (done)`);
    expect(view.container.querySelector("img,script,foreignObject")).toBeNull();
  });
  it("navigates from native links, preserves modifier clicks and moves focus with arrow keys", () => {
    const navigate = vi.fn();
    show(graph, navigate);
    const root = screen.getByRole("link", { name: "CHE-1: Root (todo)" });
    const child = screen.getByRole("link", { name: "CHE-2: Child (done)" });
    expect(root).toHaveAttribute("aria-current", "page");
    root.focus();
    fireEvent.keyDown(root, { key: "ArrowRight" });
    expect(child).toHaveFocus();
    fireEvent.keyDown(child, { key: "ArrowLeft" });
    expect(root).toHaveFocus();
    fireEvent.click(child, { ctrlKey: true });
    expect(navigate).not.toHaveBeenCalled();
    fireEvent.click(child);
    expect(navigate).toHaveBeenCalledWith("child");
    expect(child).toHaveAttribute("href", "/issues/child");
  });
  it("orders phase suffixes naturally and displays phase chips", () => {
    show({ ...graph, nodes: [graph.nodes[0]!, { id: "late", identifier: "CHE-9", title: "Late (CHE-1 a10)", status: "done" }, { id: "early", identifier: "CHE-8", title: "Early (CHE-1 a2)", status: "done" }], edges: [{ source: "root", target: "late", type: "parent" }, { source: "root", target: "early", type: "parent" }] });
    expect(screen.getByText("a2")).toBeInTheDocument();
    expect(screen.getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual(["/issues/root", "/issues/early", "/issues/late"]);
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "2");
  });
  it("uses custom status categories and colors for completion", () => {
    show({ ...graph, nodes: [graph.nodes[0]!, { ...graph.nodes[1]!, status: "custom-done" }] });
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "1");
    expect(screen.getByRole("link", { name: "CHE-2: Child (custom-done)" }).querySelector("[data-status-dot]")).toHaveStyle({ backgroundColor: "#aa00cc" });
  });
  it("hides mention-only nodes and persists the chip preference", () => {
    const mentioned: IssueRelations = { ...graph, nodes: [...graph.nodes, { id: "ref", identifier: "CHE-3", title: "Ref", status: "done" }], edges: [...graph.edges, { source: "root", target: "ref", type: "mention" }] };
    show(mentioned);
    expect(screen.queryByRole("link", { name: "CHE-3: Ref (done)" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /mentions/i }));
    expect(screen.getByRole("link", { name: "CHE-3: Ref (done)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /mentions/i })).toHaveAttribute("aria-pressed", "true");
    expect(JSON.parse(localStorage.getItem("multica_relations_filter")!).state.hidden).toEqual([]);
  });
  it("keeps non-hierarchy relations visible, ignores dangling edges, and handles cycles", () => {
    show({ ...graph, nodes: [...graph.nodes, { id: "ref", identifier: "CHE-3", title: "Ref", status: "todo" }], edges: [...graph.edges, { source: "child", target: "root", type: "parent" }, { source: "root", target: "missing", type: "parent" }, { source: "root", target: "ref", type: "unknown" }] });
    expect(screen.getAllByRole("link")).toHaveLength(3);
    expect(screen.getAllByText("related").length).toBeGreaterThan(0);
  });
  it("switches to a readable list and adds a column at depth two", () => {
    const view = show({ ...graph, depth: 2, nodes: [...graph.nodes, { id: "grandchild", identifier: "CHE-3", title: "Grandchild", status: "todo" }], edges: [...graph.edges, { source: "child", target: "grandchild", type: "parent" }] });
    expect(view.container.querySelector('[data-relations-column="2"]')).not.toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "List" }));
    expect(screen.getByRole("tabpanel")).toHaveAttribute("data-view", "list");
    expect(screen.getAllByRole("link")).toHaveLength(3);
    expect(screen.getByRole("link", { name: "CHE-3: Grandchild (todo)" })).toBeInTheDocument();
  });
});
