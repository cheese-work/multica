import { describe, expect, it } from "vitest";
import type { TimelineEntry } from "@multica/core/types";
import { buildStatusHistory } from "./status-history";

function row(id: string, createdAt: string, details: Record<string, unknown>, action = "status_changed"): TimelineEntry {
  return { type: "activity", id, actor_type: "member", actor_id: "u1", created_at: createdAt, action, details };
}

const trigger = {
  v: 1,
  cause: { kind: "ui_or_api", by: { type: "member", id: "u1" } },
  auth_source: "interactive_jwt",
  channel: "web",
};

describe("buildStatusHistory", () => {
  it("keeps only status_changed rows, ordered by (created_at, id)", () => {
    const items = buildStatusHistory([
      row("c", "2026-11-01T10:00:01Z", { from: "b", to: "c", trigger }),
      row("z", "2026-11-01T10:00:00Z", { from: "a", to: "b", trigger }),
      row("a", "2026-11-01T10:00:00Z", { from: "x", to: "a", trigger }),
      row("t", "2026-11-01T09:00:00Z", { title: "n" }, "title_changed"),
    ]);
    expect(items.map((i) => i.id)).toEqual(["a", "z", "c"]);
  });

  it("reads cause, auth source and a valid handoff link from details.trigger", () => {
    const [item] = buildStatusHistory([
      row("a", "2026-11-01T10:00:00Z", {
        from: "todo",
        to: "in_progress",
        handoff_comment_id: "cmt-1",
        trigger: { ...trigger, handoff_comment_id: "cmt-1" },
      }),
    ]);
    expect(item).toMatchObject({
      recorded: true,
      causeKind: "ui_or_api",
      authSource: "interactive_jwt",
      handoffCommentId: "cmt-1",
      refInvalid: false,
    });
  });

  it("marks a row without details.trigger as not recorded", () => {
    const [item] = buildStatusHistory([row("a", "2026-11-01T10:00:00Z", { from: "todo", to: "done" })]);
    expect(item).toMatchObject({ recorded: false, causeKind: null, handoffCommentId: null });
  });

  it("treats a malformed trigger as not recorded and carries ref_invalid", () => {
    const items = buildStatusHistory([
      row("a", "2026-11-01T10:00:00Z", { from: "x", to: "y", trigger: "oops" }),
      row("b", "2026-11-01T10:00:01Z", { from: "y", to: "z", trigger: { ...trigger, ref_invalid: true } }),
    ]);
    expect(items[0]!.recorded).toBe(false);
    expect(items[1]).toMatchObject({ recorded: true, refInvalid: true, handoffCommentId: null });
  });
});
