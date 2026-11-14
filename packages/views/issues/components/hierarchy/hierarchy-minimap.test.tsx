import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Issue, IssueStatusCategory } from "@multica/core/types";
import enIssues from "../../../locales/en/issues.json";
import { IssueHierarchy } from "./hierarchy-minimap";
import type { HierarchyIssue } from "./hierarchy-model";

const push = vi.hoisted(() => vi.fn());
const state = vi.hoisted(() => ({ loading: false }));
vi.mock("@multica/core/paths", () => ({ useWorkspacePaths: () => ({ issueDetail: (id: string) => `/issues/${id}` }) }));
vi.mock("../../../navigation", () => ({
  useNavigation: () => ({ push }),
  AppLink: ({ href, children, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => <a href={href} {...props}>{children}</a>,
}));
vi.mock("./use-hierarchy", async () => {
  const { summarizeGroup } = await import("./hierarchy-model");
  const categoryOf = (status: string): IssueStatusCategory => (({ done: "done", in_progress: "started", blocked: "started" }) as Record<string, IssueStatusCategory>)[status] ?? "unstarted";
  const node = (n: number, status = "todo", title = `Issue ${n}`): HierarchyIssue => ({ id: `id-${n}`, identifier: `CHE-${n}`, title, status });
  const siblings = Array.from({ length: 21 }, (_, i) => node(100 + i, i < 12 ? "done" : "todo"));
  const children = Array.from({ length: 33 }, (_, i) => node(200 + i, i < 12 ? "done" : "blocked"));
  const hierarchy = {
    parent: node(1, "in_progress", "Parent plan"),
    current: node(2, "in_progress", "A very long title that must wrap onto two lines and clamp in a narrow sidebar"),
    siblings: summarizeGroup(siblings, categoryOf),
    children: summarizeGroup(children, categoryOf),
    siblingIssues: siblings,
    childIssues: children,
    relatedIssues: [],
  };
  return {
    useHierarchy: () => ({
      hierarchy: state.loading ? null : hierarchy,
      loading: state.loading,
      error: false,
      catalog: { categoryOf, labelOf: (status: string) => status },
      wakeCount: { value: 27, partial: false },
      mentionCount: null,
      mentionsLoading: false,
      refetch: vi.fn(),
    }),
  };
});

const issue = { id: "id-2", identifier: "CHE-2", title: "x", status: "in_progress", parent_issue_id: "id-1" } as Issue;
const ui = () => <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueHierarchy issue={issue} /></I18nProvider>;
const world = () => screen.getByTestId("hierarchy-world").style.transform;
const percent = () => screen.getByRole("status", { name: /^Zoom/ }).textContent;
const press = (key: string, times = 1) => { for (let i = 0; i < times; i++) fireEvent.keyDown(screen.getByTestId("hierarchy-canvas"), { key }); };
const openCanvas = () => fireEvent.click(screen.getByRole("button", { name: "Expand hierarchy" }));

beforeEach(() => {
  state.loading = false;
  push.mockClear();
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({ width: 1000, height: 600, x: 0, y: 0, top: 0, left: 0, right: 1000, bottom: 600, toJSON: () => ({}) });
});
afterEach(() => vi.restoreAllMocks());

describe("minimap", () => {
  it("shows parent, sibling and child totals, the current issue and the wake count", () => {
    render(ui());
    expect(screen.getByRole("link", { name: /parent CHE-1/i })).toHaveAttribute("href", "/issues/id-1");
    expect(screen.getByText("21 siblings")).toBeInTheDocument();
    expect(screen.getByText("33 children")).toBeInTheDocument();
    expect(screen.getByText("you are here")).toBeInTheDocument();
    expect(screen.getByText("CHE-2").closest("[aria-current]")).not.toBeNull();
    expect(screen.getByText("27 wakes")).toBeInTheDocument();
  });
  it("clamps titles to two lines", () => {
    render(ui());
    // `block` would override line-clamp's `display: -webkit-box` and let the title run past two lines.
    const title = screen.getByText(/A very long title/);
    expect(title).toHaveClass("line-clamp-2");
    expect(title).not.toHaveClass("block");
  });
  it("shows a busy skeleton in the same section while the target loads", () => {
    state.loading = true;
    render(ui());
    const skeleton = screen.getByRole("status", { name: "Loading hierarchy…" });
    expect(skeleton.parentElement).toHaveAttribute("aria-busy", "true");
    expect(screen.getByTestId("issue-hierarchy")).toContainElement(skeleton);
    // D5 shared states: the mode switch and the expand button stay usable while the card loads.
    expect(screen.getByRole("button", { name: "Expand hierarchy" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Swimlane 2.0" })).toBeEnabled();
  });
  it("takes focus on the heading when it mounts after a dialog navigation", async () => {
    const { requestHierarchyFocus } = await import("./hierarchy-minimap");
    requestHierarchyFocus();
    render(ui());
    expect(screen.getByRole("heading", { name: "Hierarchy" })).toHaveFocus();
  });
});

describe("canvas dialog", () => {
  it("opens focused on the canvas, zoomed out to group totals", async () => {
    render(ui());
    openCanvas();
    const canvas = screen.getByTestId("hierarchy-canvas");
    await waitFor(() => expect(canvas).toHaveFocus());
    expect(canvas).toHaveAttribute("data-mode", "groups");
    expect(within(canvas).getByText("21 siblings")).toBeInTheDocument();
  });
  it("zooms with + and -, and splits groups into single issues from 70%", () => {
    render(ui());
    openCanvas();
    const start = Number.parseInt(percent()!, 10);
    press("+");
    expect(Number.parseInt(percent()!, 10)).toBeGreaterThan(start);
    press("+", 5);
    expect(screen.getByTestId("hierarchy-canvas")).toHaveAttribute("data-mode", "cards");
    expect(within(screen.getByTestId("hierarchy-canvas")).getAllByRole("link").length).toBeGreaterThan(50);
    press("-", 8);
    expect(screen.getByTestId("hierarchy-canvas")).toHaveAttribute("data-mode", "groups");
  });
  it("pans with the arrow keys", () => {
    render(ui());
    openCanvas();
    const before = world();
    press("ArrowRight");
    expect(world()).not.toBe(before);
  });
  it("buttons zoom, Fit restores the whole hierarchy and Me centres the current issue", () => {
    render(ui());
    openCanvas();
    const fitted = percent();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(percent()).not.toBe(fitted);
    fireEvent.click(screen.getByRole("button", { name: "Me" }));
    expect(percent()).toBe("100%");
    fireEvent.click(screen.getByRole("button", { name: "Fit" }));
    expect(percent()).toBe(fitted);
  });
  it("highlights off-screen cards in the overview after panning away", () => {
    render(ui());
    openCanvas();
    expect(screen.queryAllByTestId("overview-offscreen")).toHaveLength(0);
    press("ArrowRight", 40);
    expect(screen.getAllByTestId("overview-offscreen").length).toBeGreaterThan(0);
    expect(screen.getByText(/off-screen/)).toBeInTheDocument();
  });
  it("draws the overview blocks with real SVG geometry (width and height)", () => {
    render(ui());
    openCanvas();
    const svg = screen.getByRole("img", { name: "Overview" });
    const blocks = Array.from(svg.querySelectorAll("rect"));
    expect(blocks.length).toBeGreaterThan(2);
    blocks.forEach((rect) => {
      expect(Number(rect.getAttribute("width"))).toBeGreaterThan(0);
      expect(Number(rect.getAttribute("height"))).toBeGreaterThan(0);
    });
  });
  it("animates button moves but never when reduced motion is asked for", () => {
    render(ui());
    openCanvas();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(screen.getByTestId("hierarchy-world").className).toContain("motion-reduce:transition-none");
  });
  const openCard = () => {
    openCanvas();
    press("+", 5);
    fireEvent.click(within(screen.getByTestId("hierarchy-canvas")).getByRole("link", { name: /CHE-100/ }));
  };
  it("shrinks the dialog toward the minimap for ~180ms before navigating, then focuses the minimap heading", async () => {
    render(ui());
    openCard();
    const popup = screen.getByTestId("hierarchy-dialog");
    expect(push).not.toHaveBeenCalled();
    expect(popup.style.opacity).toBe("0");
    expect(popup.style.scale).not.toBe("");
    expect(popup.style.transition).toContain("180ms");
    await waitFor(() => expect(push).toHaveBeenCalledWith("/issues/id-100"));
    await waitFor(() => expect(screen.queryByTestId("hierarchy-dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("heading", { name: "Hierarchy" })).toHaveFocus());
  });
  it("navigates as soon as the shrink transition ends, without waiting for the fallback timer", () => {
    render(ui());
    openCard();
    fireEvent.transitionEnd(screen.getByTestId("hierarchy-dialog"), { propertyName: "opacity" });
    expect(push).toHaveBeenCalledWith("/issues/id-100");
    expect(push).toHaveBeenCalledTimes(1);
  });
  it("navigates at once when reduced motion is asked for", async () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    render(ui());
    openCard();
    expect(push).toHaveBeenCalledWith("/issues/id-100");
    await waitFor(() => expect(screen.queryByTestId("hierarchy-dialog")).not.toBeInTheDocument());
    vi.unstubAllGlobals();
  });
  it("cancels the pending navigation when the dialog is dismissed mid-collapse", async () => {
    render(ui());
    openCard();
    fireEvent.keyDown(screen.getByTestId("hierarchy-dialog"), { key: "Escape" });
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(push).not.toHaveBeenCalled();
  });
  it("leaves modifier clicks to the browser", () => {
    render(ui());
    openCanvas();
    press("+", 5);
    fireEvent.click(within(screen.getByTestId("hierarchy-canvas")).getByRole("link", { name: /CHE-100/ }), { metaKey: true });
    expect(push).not.toHaveBeenCalled();
    expect(screen.getByTestId("hierarchy-dialog")).toBeInTheDocument();
  });
});
