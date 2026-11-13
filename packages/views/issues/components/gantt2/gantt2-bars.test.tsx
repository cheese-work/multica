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

  it("T2: a completed interval that crosses `from` shows the seed segment and ends at actual_end with a done icon", () => {
    const { container } = renderBars([
      row("t2", {
        status: "done",
        actual_start: ago(40),
        actual_end: ago(10),
        segments: [seg("in_progress", "started", ago(30), ago(20), { clipped_start: true }), seg("done", "done", ago(20), ago(10))],
      }),
    ]);
    const bar = barOf(container);
    expect(bar.dataset.clippedStart).toBe("true");
    expect(bar.dataset.open).toBe("false");
    expect(bar.style.left).toBe("0%");
    expect(parseFloat(bar.style.width)).toBeCloseTo((20 / 30) * 100, 1);
    expect(within(bar).getAllByTestId("gantt-segment")).toHaveLength(2);
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
