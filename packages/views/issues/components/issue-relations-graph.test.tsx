import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { IssueRelations } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../locales/en/issues.json";
import enEditor from "../../locales/en/editor.json";
import { useRelationsFilterStore } from "@multica/core/issues/stores/relations-filter-store";
import { buildRelationsChart, IssueRelationsGraph } from "./issue-relations-graph";

const mermaid = vi.hoisted(() => ({
  initialize: vi.fn(),
  render: vi.fn(async (diagramId: string, _chart?: string) => ({ svg: `<svg viewBox="0 0 200 100"><g class="node" id="${diagramId}-flowchart-n0-0"><text>CHE&amp;#45;1&amp;#58; Root &amp;#40;todo&amp;#41;</text></g><g class="node" id="${diagramId}-flowchart-n1-1"><text>CHE&amp;#45;2&amp;#58; Child &amp;#40;done&amp;#41;</text></g></svg>` })),
}));
vi.mock("../../editor/mermaid-diagram", () => ({
  getMermaid: async () => mermaid,
  getMermaidLayout: () => ({ width: 200, height: 100 }),
  getMermaidThemeVariables: () => ({}),
}));
vi.mock("../../editor/hooks/use-theme-version", () => ({ useThemeVersion: () => 0 }));

const catalog = { categoryOf: (key: string) => (key === "custom-done" ? "done" as const : "unstarted" as const), colorOf: (key: string) => (key === "custom-done" ? "#aa00cc" : null) };

const graph: IssueRelations = {
  depth: 1, omitted_count: 0,
  nodes: [{ id: "root", identifier: "CHE-1", title: "Root", status: "todo" }, { id: "child", identifier: "CHE-2", title: "Child", status: "done" }],
  edges: [{ source: "root", target: "child", type: "parent" }],
};

describe("issue relations graph", () => {
  it("escapes untrusted labels and uses controlled node IDs without click directives", () => {
    const hostile = structuredClone(graph);
    hostile.nodes[1]!.title = '"]\nclick n0 href "javascript:alert(1)"\n%%{init: {"securityLevel":"loose"}}%% <img onerror=alert(1)>';
    const chart = buildRelationsChart(hostile, "root");
    expect(chart).not.toContain("\nclick");
    expect(chart).not.toContain("%%{");
    expect(chart).not.toContain("<img");
    expect(chart).toContain("#34;");
    expect(chart).toContain("class n0 current");
  });

  it("distinguishes edge types, ignores dangling edges and keeps cycles", () => {
    const chart = buildRelationsChart({ ...graph, edges: [
      { source: "root", target: "child", type: "wakeup" },
      { source: "child", target: "root", type: "mention" },
      { source: "root", target: "missing", type: "parent" },
    ] }, "root");
    expect(chart).toContain("stroke-dasharray:8 4");
    expect(chart).toContain("stroke-dasharray:2 4");
    expect(chart).not.toContain("undefined");
  });

  it("navigates from mouse and keyboard nodes with Mermaid security strict", async () => {
    const navigate = vi.fn();
    render(<I18nProvider locale="en" resources={{ en: { issues: enIssues, editor: enEditor } }}><IssueRelationsGraph graph={graph} currentIssueId="root" catalog={catalog} onNavigate={navigate} /></I18nProvider>);
    const child = await screen.findByRole("link", { name: "CHE-2: Child (done)" });
    expect(child).toHaveTextContent("CHE-2: Child (done)");
    fireEvent.click(child);
    expect(navigate).toHaveBeenLastCalledWith("child");
    fireEvent.keyDown(child, { key: "Enter" });
    expect(navigate).toHaveBeenCalledTimes(2);
    expect(mermaid.initialize).toHaveBeenCalledWith(expect.objectContaining({ securityLevel: "strict", htmlLabels: false }));
  });

  it("shows a render error and retries without changing security", async () => {
    mermaid.render.mockRejectedValueOnce(new Error("render failed"));
    render(<I18nProvider locale="en" resources={{ en: { issues: enIssues, editor: enEditor } }}><IssueRelationsGraph graph={graph} currentIssueId="root" catalog={catalog} onNavigate={vi.fn()} /></I18nProvider>);
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /retry|try again/i }));
    expect(await screen.findByRole("link", { name: "CHE-2: Child (done)" })).toBeInTheDocument();
  });

  it("builds a chart without edge text, with two-line titles and no mention edges when filtered", () => {
    const long = { ...graph, nodes: [graph.nodes[0]!, { ...graph.nodes[1]!, title: "A very long issue title that keeps going well past two short lines of text" }] };
    const chart = buildRelationsChart(long, "root");
    expect(chart).not.toContain("|");
    expect(chart).toContain("CHE-2<br/>");
    expect(chart).toContain("…");
    expect(chart.split("<br/>").length).toBeLessThanOrEqual(5);
  });

  it("hides mention edges and mention-only nodes from the chip filter, and the choice persists", async () => {
    const mentioned: IssueRelations = { ...graph, nodes: [...graph.nodes, { id: "ref", identifier: "CHE-3", title: "Ref", status: "done" }], edges: [...graph.edges, { source: "root", target: "ref", type: "mention" }] };
    useRelationsFilterStore.setState({ hidden: [] });
    mermaid.render.mockClear();
    render(<I18nProvider locale="en" resources={{ en: { issues: enIssues, editor: enEditor } }}><IssueRelationsGraph graph={mentioned} currentIssueId="root" catalog={catalog} onNavigate={vi.fn()} /></I18nProvider>);
    await screen.findByRole("link", { name: "CHE-2: Child (done)" });
    expect(mermaid.render.mock.lastCall?.[1]).toContain("CHE-3");
    fireEvent.click(screen.getByRole("button", { name: /mentions/i }));
    await waitFor(() => expect(mermaid.render.mock.lastCall?.[1]).not.toContain("CHE-3"));
    expect(useRelationsFilterStore.getState().hidden).toEqual(["mention"]);
    expect(screen.getByRole("button", { name: /mentions/i })).toHaveAttribute("aria-pressed", "false");
    expect(JSON.parse(localStorage.getItem("multica_relations_filter")!).state.hidden).toEqual(["mention"]);
  });

  it("gives duplicate and unknown edges their own dash pattern, not the parent look", () => {
    const edge = (type: IssueRelations["edges"][number]["type"]) => buildRelationsChart({ ...graph, edges: [{ source: "root", target: "child", type }] }, "root");
    expect(edge("duplicate")).not.toEqual(edge("parent"));
    expect(edge("unknown")).not.toEqual(edge("parent"));
    expect(edge("unknown")).not.toEqual(edge("duplicate"));
  });

  it("lists duplicate, shared-PR and unknown edges in the legend only when they are drawn", async () => {
    const related: IssueRelations = { ...graph, edges: [...graph.edges, { source: "child", target: "root", type: "duplicate" }] };
    const { unmount } = render(<I18nProvider locale="en" resources={{ en: { issues: enIssues, editor: enEditor } }}><IssueRelationsGraph graph={related} currentIssueId="root" catalog={catalog} onNavigate={vi.fn()} /></I18nProvider>);
    await screen.findByRole("link", { name: "CHE-2: Child (done)" });
    expect(screen.getByText("duplicate of")).toBeInTheDocument();
    expect(screen.queryByText("shared PR")).not.toBeInTheDocument();
    unmount();
  });

  it("paints the stripe from the workspace catalog for custom statuses", async () => {
    mermaid.render.mockImplementationOnce(async (diagramId: string) => ({ svg: `<svg viewBox="0 0 200 100"><g class="node" id="${diagramId}-flowchart-n0-0"><rect x="0" y="0" width="100" height="50" /><text>a</text></g><g class="node" id="${diagramId}-flowchart-n1-1"><rect x="0" y="0" width="100" height="50" /><text>b</text></g></svg>` }));
    const custom: IssueRelations = { ...graph, nodes: [graph.nodes[0]!, { ...graph.nodes[1]!, status: "custom-done" }] };
    render(<I18nProvider locale="en" resources={{ en: { issues: enIssues, editor: enEditor } }}><IssueRelationsGraph graph={custom} currentIssueId="root" catalog={catalog} onNavigate={vi.fn()} /></I18nProvider>);
    const link = await screen.findByRole("link", { name: "CHE-2: Child (custom-done)" });
    expect(link.querySelector('rect[width="6"]')).toHaveStyle({ fill: "#aa00cc" });
    expect(screen.getByRole("link", { name: "CHE-1: Root (todo)" }).querySelector('rect[width="6"]')).toHaveStyle({ fill: "var(--muted-foreground)" });
  });
});
