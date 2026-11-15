import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Issue, IssueStatusCategory } from "@multica/core/types";
import enIssues from "../../../locales/en/issues.json";
import { IssueHierarchy } from "./hierarchy-minimap";
import type { HierarchyIssue } from "./hierarchy-model";

const push = vi.hoisted(() => vi.fn());
const state = vi.hoisted(() => ({ loading: false, levels: {} as Record<number, { nodes: unknown[]; status: string }>, expand: vi.fn() }));
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

// Root id-2 → CHE-100 (2 kids), CHE-101 (leaf), CHE-102 (1 kid) → CHE-300/301 under 100, CHE-302 under 102; depth 3 is empty.
vi.mock("./use-hierarchy-levels", async (importActual) => ({
  ...(await importActual<typeof import("./use-hierarchy-levels")>()),
  useHierarchyLevels: () => ({ levels: state.levels, expand: state.expand }),
}));
const tn = (n: number, parentId: string) => ({ id: `id-${n}`, identifier: `CHE-${n}`, title: `Issue ${n}`, status: "todo", parentId, context: false });
const fullLevels = () => ({
  1: { nodes: [tn(100, "id-2"), tn(101, "id-2"), tn(102, "id-2")], status: "ready" },
  2: { nodes: [tn(300, "id-100"), tn(301, "id-100"), tn(302, "id-102")], status: "ready" },
  3: { nodes: [], status: "ready" },
});

const issue = { id: "id-2", identifier: "CHE-2", title: "x", status: "in_progress", parent_issue_id: "id-1" } as Issue;
const ui = () => <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueHierarchy issue={issue} /></I18nProvider>;
const press = (key: string, times = 1) => { for (let i = 0; i < times; i++) fireEvent.keyDown(screen.getByTestId("hierarchy-canvas"), { key }); };
const openCanvas = () => fireEvent.click(screen.getByRole("button", { name: "Expand hierarchy" }));

beforeEach(() => {
  state.loading = false;
  state.levels = fullLevels();
  state.expand.mockClear();
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

const canvas = () => screen.getByTestId("hierarchy-canvas");
const ready = () => canvas().getAttribute("data-level-ready");
const ids = () => within(canvas()).queryAllByRole("link").map((a) => a.textContent?.match(/CHE-\d+/)?.[0]);
const marks = () => (performance.mark as unknown as ReturnType<typeof vi.fn>).mock.calls.map((c) => c[0]);

describe("canvas dialog: focus-depth levels (CHE-5715)", () => {
  beforeEach(() => {
    vi.spyOn(performance, "mark").mockImplementation((() => undefined) as never);
  });
  it("opens on depth 0: the root card with one count card, canvas focused, level ready", async () => {
    render(ui());
    openCanvas();
    await waitFor(() => expect(canvas()).toHaveFocus());
    expect(ready()).toBe("0");
    expect(within(canvas()).getByText("CHE-2")).toBeInTheDocument();
    expect(within(canvas()).getByText("3 children")).toBeInTheDocument();
    expect(screen.getByText("Depth 0 / 2")).toBeInTheDocument();
  });
  it("+ steps in to the children level, - and Escape step back out, Escape at the root closes the dialog", () => {
    render(ui());
    openCanvas();
    press("+");
    expect(ready()).toBe("1");
    expect(ids()).toEqual(["CHE-100", "CHE-101", "CHE-102"]);
    press("-");
    expect(ready()).toBe("0");
    press("+");
    press("Escape");
    expect(ready()).toBe("0");
    expect(screen.getByTestId("hierarchy-dialog")).toBeInTheDocument();
    press("Escape");
    expect(screen.queryByTestId("hierarchy-dialog")).not.toBeInTheDocument();
  });
  it("+ stops at the deepest level; the button is disabled there", () => {
    render(ui());
    openCanvas();
    press("+", 5);
    expect(ready()).toBe("2");
    expect(screen.getByRole("button", { name: "Step in" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Step out" })).toBeEnabled();
  });
  it("Space steps in, and steps out at the deepest level", () => {
    render(ui());
    openCanvas();
    press(" ");
    expect(ready()).toBe("1");
    press(" ");
    expect(ready()).toBe("2");
    press(" ");
    expect(ready()).toBe("1");
  });
  it("marks level-start then level-ready for every level change", () => {
    render(ui());
    openCanvas();
    press("+");
    press("-");
    expect(marks().filter((m) => m === "level-start")).toHaveLength(2);
    expect(marks().filter((m) => m === "level-ready")).toHaveLength(2);
    expect(marks().slice(-2)).toEqual(["level-start", "level-ready"]);
  });
  it("requests the count level on mount and the next one when the focus steps in", () => {
    state.levels = {};
    const first = render(ui());
    openCanvas();
    expect(state.expand).toHaveBeenCalledWith(0);
    first.unmount();
    state.levels = { 1: fullLevels()[1] };
    state.expand.mockClear();
    render(ui());
    openCanvas();
    expect(state.expand).not.toHaveBeenCalled();
    press("+");
    expect(state.expand).toHaveBeenCalledWith(1);
  });
  it("is not ready while the count level is still loading", () => {
    state.levels = { 1: { nodes: [], status: "loading" } };
    render(ui());
    openCanvas();
    expect(canvas()).not.toHaveAttribute("data-level-ready");
    expect(screen.getByRole("status", { name: "Loading hierarchy…" })).toBeInTheDocument();
  });
  it("a failed level shows a retry that reloads it", () => {
    state.levels = { 1: { nodes: [], status: "failed" } };
    render(ui());
    openCanvas();
    state.expand.mockClear();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(state.expand).toHaveBeenCalledWith(0);
  });
  it("a count card steps into that card only: its children, with it last in the chain", () => {
    render(ui());
    openCanvas();
    press("+");
    expect(ids()).toEqual(["CHE-100", "CHE-101", "CHE-102"]);
    fireEvent.click(within(canvas()).getByText("1 child"));
    expect(ready()).toBe("2");
    expect(ids()).toEqual(["CHE-302"]);
    expect(within(canvas()).getByRole("button", { name: /CHE-102/ })).toBeInTheDocument();
    expect(within(canvas()).queryByRole("button", { name: /CHE-100/ })).not.toBeInTheDocument();
    press("-");
    expect(ids()).toEqual(["CHE-100", "CHE-101", "CHE-102"]);
    expect(within(canvas()).getByText("CHE-102").closest("a")).toHaveFocus();
  });
  it("+ steps into the active card, and is off on a leaf", () => {
    render(ui());
    openCanvas();
    press("+");
    const card = (n: number) => within(canvas()).getByText(`CHE-${n}`).closest("a")!;
    fireEvent.focus(card(101));
    expect(screen.getByRole("button", { name: "Step in" })).toBeDisabled();
    fireEvent.focus(card(102));
    press("+");
    expect(ids()).toEqual(["CHE-302"]);
  });
  it("animates a level change over 240 ms, but not with reduced motion", () => {
    const animate = vi.fn();
    HTMLElement.prototype.animate = animate;
    render(ui());
    openCanvas();
    press("+");
    expect(animate).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ duration: 240 }));
    animate.mockClear();
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    press("-");
    expect(animate).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
    // @ts-expect-error test cleanup
    delete HTMLElement.prototype.animate;
  });
});

describe("canvas dialog: opening an issue", () => {
  const openCard = () => {
    openCanvas();
    press("+");
    fireEvent.click(within(canvas()).getByRole("link", { name: /CHE-100/ }));
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
    press("+");
    fireEvent.click(within(canvas()).getByRole("link", { name: /CHE-100/ }), { metaKey: true });
    expect(push).not.toHaveBeenCalled();
    expect(screen.getByTestId("hierarchy-dialog")).toBeInTheDocument();
  });
});
