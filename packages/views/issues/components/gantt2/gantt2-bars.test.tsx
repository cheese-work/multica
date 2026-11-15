// @vitest-environment jsdom

import { cleanup, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { IssueTimelineResponse } from "@multica/core/api/issue-graph-schema";
import { renderWithI18n } from "../../../test/i18n";
import { Gantt2Bars, type Gantt2Row } from "./gantt2-bars";

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

type TimelineIssue = IssueTimelineResponse["issues"][number];
type Segment = TimelineIssue["segments"][number];

const DAY = 86_400_000;
const AS_OF = Date.parse("2026-11-13T00:00:00Z");
const window = { from: new Date(AS_OF - 30 * DAY).toISOString(), to: new Date(AS_OF).toISOString() };
const ago = (days: number) => new Date(AS_OF - days * DAY).toISOString();

const seg = (status: string, category: Segment["category"], from: string | null, to: string | null, extra: Partial<Segment> = {}): Segment => ({
  status, category, from, to, clipped_start: false, clipped_end: false, ...extra,
});
const row = (id: string, issue: Partial<TimelineIssue>): Gantt2Row => ({
  identifier: id.toUpperCase(),
  title: `title ${id}`,
  timeline: {
    issue_id: "00000000-0000-4000-8000-000000000001",
    status: "in_progress",
    history: "complete",
    start_known: true,
    actual_start: null,
    actual_end: null,
    presence_from: null,
    presence_to: null,
    start_date: null,
    due_date: null,
    segments: [],
    ...issue,
  },
});
const renderBars = (rows: Gantt2Row[]) => renderWithI18n(<Gantt2Bars rows={rows} window={window} asOf={new Date(AS_OF).toISOString()} />);
const barOf = (container: HTMLElement) => container.querySelector<HTMLElement>("[data-gantt-bar]")!;

describe("Gantt2Bars actual bar (T1–T3)", () => {
  it("T1: an old active issue with no transition in the window is clipped at the window start and runs to now", () => {
    const { container } = renderBars([row("t1", { actual_start: ago(60), segments: [seg("in_progress", "started", ago(30), null, { clipped_start: true })] })]);
    const bar = barOf(container);
    expect(bar.dataset.clippedStart).toBe("true");
    expect(bar.dataset.open).toBe("true");
    expect(bar.style.left).toBe("0%");
    expect(bar.style.width).toBe("100%");
  });

  it("T2: a completed interval that crosses `from` shows the seed segment and ends at actual_end with a done icon (server shape: the done segment runs to now)", () => {
    const { container } = renderBars([
      row("t2", {
        status: "done",
        actual_start: ago(40),
        actual_end: ago(20),
        segments: [seg("in_progress", "started", ago(30), ago(20), { clipped_start: true }), seg("done", "done", ago(20), ago(0))],
      }),
    ]);
    const bar = barOf(container);
    expect(bar.dataset.clippedStart).toBe("true");
    expect(bar.dataset.open).toBe("false");
    expect(bar.style.left).toBe("0%");
    expect(parseFloat(bar.style.width)).toBeCloseTo((10 / 30) * 100, 1);
    expect(within(bar).getAllByTestId("gantt-segment")).toHaveLength(1);
    expect(within(bar).getByTestId("gantt-end-icon").getAttribute("aria-label")).toBe("Done");
  });

  it("T3: a reopened issue is one bar, open ended, with segments after the done segment", () => {
    const { container } = renderBars([
      row("t3", {
        actual_start: ago(20),
        actual_end: null,
        segments: [seg("in_progress", "started", ago(20), ago(12)), seg("done", "done", ago(12), ago(8)), seg("in_progress", "started", ago(8), null)],
      }),
    ]);
    expect(container.querySelectorAll("[data-gantt-bar]")).toHaveLength(1);
    const bar = barOf(container);
    expect(bar.dataset.open).toBe("true");
    const order = within(bar).getAllByTestId("gantt-segment").map((s) => s.dataset.status);
    expect(order).toEqual(["in_progress", "done", "in_progress"]);
    expect(parseFloat(bar.style.left)).toBeCloseTo((10 / 30) * 100, 1);
    expect(parseFloat(bar.style.width)).toBeCloseTo((20 / 30) * 100, 1);
  });

  it("every status segment has an icon and a text label", () => {
    const { container } = renderBars([
      row("t3", {
        actual_start: ago(20),
        segments: [seg("in_progress", "started", ago(20), ago(12)), seg("done", "done", ago(12), ago(11.9)), seg("in_review", "started", ago(11.9), null)],
      }),
    ]);
    const segments = within(barOf(container)).getAllByTestId("gantt-segment");
    expect(segments).toHaveLength(3);
    for (const s of segments) {
      expect(s.querySelector("svg")).not.toBeNull();
      expect((s.textContent ?? "").trim().length).toBeGreaterThan(0);
    }
    // the 0.1 day segment is too narrow to show its label, yet it is still in the DOM for assistive tech
    expect(within(segments[1]!).getByText("Done")).toBeTruthy();
    expect(screen.getAllByText("In Review").length).toBeGreaterThan(0);
  });

  it("draws no bar for an issue without an actual start", () => {
    const { container } = renderBars([row("t4", { history: "none", start_known: false, due_date: ago(5) })]);
    expect(container.querySelector("[data-gantt-bar]")).toBeNull();
    expect(screen.getByText("T4")).toBeTruthy();
  });
});

const leftOf = (el: HTMLElement) => parseFloat(el.style.left);

describe("Gantt2Bars markers and incomplete history (T4, T6, T7)", () => {
  it("T4: markers only, no started row: start_date and due_date draw as markers, no bar", () => {
    const { container } = renderBars([row("t4", { history: "none", start_known: false, start_date: ago(10), due_date: ago(5) })]);
    expect(container.querySelector("[data-gantt-bar]")).toBeNull();
    const start = screen.getByTestId("gantt-marker-start");
    const due = screen.getByTestId("gantt-marker-due");
    expect(leftOf(start)).toBeCloseTo((20 / 30) * 100, 1);
    expect(leftOf(due)).toBeCloseTo((25 / 30) * 100, 1);
    // each marker is named for assistive tech, so shape is never the only signal
    expect(start.getAttribute("aria-label")).toBe("Start date");
    expect(due.getAttribute("aria-label")).toBe("Due date");
    expect(screen.getByTestId("gantt-marker-span")).toBeTruthy();
  });

  it("T4: a marker outside the window is not drawn", () => {
    renderBars([row("t4", { history: "none", start_known: false, due_date: ago(45) })]);
    expect(screen.queryByTestId("gantt-marker-due")).toBeNull();
  });

  it("T4: markers also draw on a row that has a bar", () => {
    const { container } = renderBars([row("t4", { actual_start: ago(20), segments: [seg("in_progress", "started", ago(20), null)], due_date: ago(5) })]);
    expect(barOf(container)).toBeTruthy();
    expect(screen.getByTestId("gantt-marker-due")).toBeTruthy();
    expect(screen.queryByTestId("gantt-marker-span")).toBeNull();
  });

  it("T6: a standalone issue with no history rows shows no bar and says there is no recorded history", () => {
    const { container } = renderBars([row("t6", { history: "none", start_known: false, presence_from: ago(90), presence_to: new Date(AS_OF).toISOString(), segments: [] })]);
    expect(container.querySelector("[data-gantt-bar]")).toBeNull();
    // presence is a selection bound, never drawn
    expect(container.querySelector("[data-gantt-presence]")).toBeNull();
    expect(screen.getByText("No recorded history")).toBeTruthy();
    expect(screen.queryByTestId("gantt-marker-start")).toBeNull();
  });

  it("T7: an old partial-history issue draws a hatched 'before records' segment and no actual start", () => {
    const { container } = renderBars([
      row("t7", {
        status: "in_review",
        history: "partial",
        start_known: false,
        actual_start: null,
        presence_from: ago(70),
        presence_to: new Date(AS_OF).toISOString(),
        segments: [seg("in_progress", "started", null, ago(50)), seg("in_review", "started", ago(50), null)],
      }),
    ]);
    const bar = barOf(container);
    expect(bar.dataset.startKnown).toBe("false");
    // hatched from max(presence_from, window from): presence_from is 70 days ago, so the window edge
    expect(bar.style.left).toBe("0%");
    expect(bar.style.width).toBe("100%");
    const [before, after] = within(bar).getAllByTestId("gantt-segment");
    expect(before!.dataset.beforeRecords).toBe("true");
    expect(after!.dataset.beforeRecords).toBeUndefined();
    expect(within(before!).getByText("Before records")).toBeTruthy();
    expect(screen.queryByText("No recorded history")).toBeNull();
  });

  it("T7: the hatched segment starts at presence_from when that is inside the window", () => {
    const { container } = renderBars([
      row("t7b", {
        history: "partial",
        start_known: false,
        presence_from: ago(20),
        segments: [seg("in_progress", "started", null, ago(10)), seg("in_review", "started", ago(10), null)],
      }),
    ]);
    expect(parseFloat(barOf(container).style.left)).toBeCloseTo((10 / 30) * 100, 1);
  });
});

describe("Gantt2Bars parent/child rows (G3)", () => {
  const node = (id: string, parentId: string | null) => ({ ...row(id, {}), parentId, timeline: { ...row(id, {}).timeline, issue_id: id } });
  const tree = [node("p", null), node("c1", "p"), node("c2", "p"), node("g", "c1"), node("q", null)];
  const ids = () => screen.getAllByTestId("gantt-row").map((r) => r.dataset.issueId);

  it("lists children under their parent in tree order, indented by depth", () => {
    renderWithI18n(<Gantt2Bars rows={[tree[4]!, tree[3]!, tree[2]!, tree[1]!, tree[0]!]} window={window} asOf={new Date(AS_OF).toISOString()} />);
    // K1 order: pre-order, input order among siblings.
    expect(ids()).toEqual(["q", "p", "c2", "c1", "g"]);
  });

  it("collapsing a parent hides its whole subtree; the toggle reports expanded state", () => {
    const onToggle = vi.fn();
    const { rerender } = renderWithI18n(
      <Gantt2Bars rows={tree} window={window} asOf={new Date(AS_OF).toISOString()} collapsed={new Set()} onToggleCollapse={onToggle} />,
    );
    expect(ids()).toEqual(["p", "c1", "g", "c2", "q"]);
    // p and c1 have children; c2, g and q are leaves
    expect(screen.getAllByRole("button", { name: /collapse/i, expanded: true })).toHaveLength(2);
    expect(screen.getAllByTestId("gantt-row")[2]!.dataset.depth).toBe("2");
    rerender(<Gantt2Bars rows={tree} window={window} asOf={new Date(AS_OF).toISOString()} collapsed={new Set(["p"])} onToggleCollapse={onToggle} />);
    expect(ids()).toEqual(["p", "q"]);
    screen.getByRole("button", { name: /expand/i, expanded: false }).click();
    expect(onToggle).toHaveBeenCalledWith("p");
  });

  it("a leaf has no toggle", () => {
    renderWithI18n(<Gantt2Bars rows={[node("q", null)]} window={window} asOf={new Date(AS_OF).toISOString()} collapsed={new Set()} onToggleCollapse={vi.fn()} />);
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("Gantt2Bars wakeup arrows (G4)", () => {
  type Wakeup = IssueTimelineResponse["wakeup_events"][number];
  const node = (id: string, parentId: string | null) => ({ ...row(id, {}), parentId, timeline: { ...row(id, {}).timeline, issue_id: id } });
  // p > c1 > g ; p > c2 ; q > k1 ; r
  const rows = [node("p", null), node("c1", "p"), node("g", "c1"), node("c2", "p"), node("q", null), node("k1", "q"), node("r", null)];
  const event = (id: string, cls: Wakeup["class"], source: string | null, target: string, extra: Partial<Wakeup> = {}): Wakeup => ({
    id, at: ago(5), target_issue_id: target, rule_id: "w", rule_kind: "condition", condition_type: "other_issue",
    source_issue_id: source, class: cls, run_id: "t", ...extra,
  });
  const show = (events: Wakeup[], collapsed: ReadonlySet<string> = new Set()) =>
    renderWithI18n(<Gantt2Bars rows={rows} window={window} asOf={new Date(AS_OF).toISOString()} wakeupEvents={events} collapsed={collapsed} />);
  const arrows = () => screen.queryAllByTestId("gantt-arrow").map((a) => `${a.dataset.class}:${a.dataset.source}>${a.dataset.target}`);

  it("cross: draws one arrow from the source row to the target row", () => {
    show([event("e1", "cross", "r", "g")]);
    expect(arrows()).toEqual(["cross:r>g"]);
    expect(screen.queryByTestId("gantt-target-inside")).toBeNull();
  });

  it("aggregate_children: one arrow from the target's child group, none per child", () => {
    show([event("e2", "aggregate_children", null, "p", { condition_type: "children_done", stage: 2 })]);
    expect(arrows()).toEqual(["aggregate_children:c2>p"]);
  });

  it("both endpoints hidden: the arrow retargets to both visible parents and flags both ends", () => {
    show([event("e3", "cross", "k1", "g")], new Set(["p", "q"]));
    expect(arrows()).toEqual(["cross:q>p"]);
    expect(screen.getAllByTestId("gantt-target-inside")).toHaveLength(2);
  });

  it("one hidden endpoint: the arrow ends at the visible parent with a 'target inside' indicator", () => {
    show([event("e4", "cross", "r", "g")], new Set(["p"]));
    expect(arrows()).toEqual(["cross:r>p"]);
    const flag = screen.getByTestId("gantt-target-inside");
    expect(flag.getAttribute("aria-label")).toMatch(/inside/i);
  });

  it("two firings of one rule draw two separate arrows", () => {
    show([event("e5", "cross", "r", "g"), event("e6", "cross", "r", "g", { at: ago(2) })]);
    expect(arrows()).toEqual(["cross:r>g", "cross:r>g"]);
  });

  it("draws nothing for self, external, unknown_source, an endpoint outside the rows, or a firing outside the window", () => {
    show([
      event("a", "self", null, "g"),
      event("b", "external", null, "g"),
      event("c", "unknown_source", null, "g"),
      event("d", "cross", "not-a-row", "g"),
      event("e", "cross", "r", "g", { at: ago(90) }),
    ]);
    expect(arrows()).toEqual([]);
  });

  it("an aggregate firing whose group is collapsed into its target draws nothing", () => {
    show([event("e7", "aggregate_children", null, "p", { condition_type: "children_done" })], new Set(["p"]));
    expect(arrows()).toEqual([]);
  });
});
