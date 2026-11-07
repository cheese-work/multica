// @vitest-environment jsdom

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { StatusHistoryDialog } from "./status-history-dialog";

afterEach(cleanup);

const trigger = (kind: string, extra: Record<string, unknown> = {}) => ({
  v: 1,
  cause: { kind, by: { type: "member", id: "u1" } },
  auth_source: "interactive_jwt",
  channel: "web",
  ...extra,
});

function row(id: string, createdAt: string, from: string, to: string, t?: unknown): TimelineEntry {
  return {
    type: "activity",
    id,
    actor_type: "member",
    actor_id: "u1",
    created_at: createdAt,
    action: "status_changed",
    details: { from, to, ...(t === undefined ? {} : { trigger: t }) },
  };
}

const KNOWN = new Set(["todo", "in_progress", "done"]);
const resolveStatus = (key: string) => ({ label: key.toUpperCase(), removed: !KNOWN.has(key) });

function open(entries: TimelineEntry[], props: { truncated?: boolean; onOpenChange?: () => void } = {}) {
  return renderWithI18n(
    <StatusHistoryDialog
      open
      onOpenChange={props.onOpenChange ?? (() => {})}
      entries={entries}
      truncated={props.truncated ?? false}
      resolveStatus={resolveStatus}
    />,
  );
}

describe("StatusHistoryDialog", () => {
  it("lists status changes in (created_at, id) order with their trigger", () => {
    open([
      row("b", "2026-11-01T10:00:01Z", "in_progress", "done", trigger("pr_merged")),
      row("a", "2026-11-01T10:00:00Z", "todo", "in_progress", trigger("workflow_edge")),
    ]);
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]!).getByText(/TODO/)).toBeTruthy();
    expect(within(items[0]!).getByText("Workflow edge")).toBeTruthy();
    expect(within(items[1]!).getByText("PR merged")).toBeTruthy();
  });

  it("links a handoff comment and closes the dialog when it is followed", () => {
    const onOpenChange = vi.fn();
    open(
      [row("a", "2026-11-01T10:00:00Z", "todo", "done", trigger("ui_or_api", { handoff_comment_id: "cmt-9" }))],
      { onOpenChange },
    );
    const link = screen.getByRole("link", { name: "Handoff comment" });
    expect(link.getAttribute("href")).toBe("#comment-cmt-9");
    fireEvent.click(link);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("scrubber moves through the changes and selects the position", () => {
    open([
      row("a", "2026-11-01T10:00:00Z", "todo", "in_progress", trigger("workflow_edge")),
      row("b", "2026-11-01T10:00:01Z", "in_progress", "done", trigger("pr_merged")),
    ]);
    // base-ui hides the thumb until it is measured, and jsdom never measures.
    const scrubber = screen.getByRole("slider", { hidden: true });
    // Starts at the latest change.
    expect(scrubber.getAttribute("aria-valuenow")).toBe("1");
    const items = screen.getAllByRole("listitem");
    expect(items[1]!.getAttribute("aria-current")).toBe("step");
    fireEvent.keyDown(scrubber, { key: "ArrowLeft" });
    expect(scrubber.getAttribute("aria-valuenow")).toBe("0");
    expect(screen.getAllByRole("listitem")[0]!.getAttribute("aria-current")).toBe("step");
    expect(screen.getAllByRole("listitem")[1]!.getAttribute("aria-current")).toBeNull();
  });

  it("shows a notice when the history is truncated, and none otherwise", () => {
    const entries = [row("a", "2026-11-01T10:00:00Z", "todo", "done", trigger("ui_or_api"))];
    const { unmount } = open(entries, { truncated: true });
    expect(screen.getByRole("status").textContent).toMatch(/older status changes/i);
    unmount();
    open(entries);
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("renders a status missing from the catalog as removed", () => {
    open([row("a", "2026-11-01T10:00:00Z", "legacy_review", "todo", trigger("ui_or_api"))]);
    expect(screen.getByText("LEGACY_REVIEW (removed)")).toBeTruthy();
    expect(screen.queryByText("TODO (removed)")).toBeNull();
  });

  it('shows "trigger not recorded" for a legacy row', () => {
    open([row("a", "2026-11-01T10:00:00Z", "todo", "done")]);
    expect(screen.getByText("Trigger not recorded")).toBeTruthy();
  });

  it("says so when there are no status changes, and renders no scrubber", () => {
    open([]);
    expect(screen.getByText("No status changes recorded.")).toBeTruthy();
    expect(screen.queryByRole("slider", { hidden: true })).toBeNull();
  });
});
