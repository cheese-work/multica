// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { AgentTask, TimelineEntry } from "@multica/core/types";
import { projectThreadDisplay } from "./thread-disclosure";
import type { CommentRun } from "./comment-runs";

function reply(id: string, createdAt: string): TimelineEntry {
  return { type: "comment", id, actor_type: "member", actor_id: "user-1", content: id, created_at: createdAt };
}

function task(id: string, createdAt = "2026-09-07T00:00:00Z"): AgentTask {
  return { id, agent_id: "agent", runtime_id: "runtime", issue_id: "issue", status: "running", priority: 0,
    created_at: createdAt, started_at: null, dispatched_at: null, completed_at: null, result: null, error: null };
}

function replyRun(taskId: string, commentId: string, anchorCommentId: string, createdAt?: string): CommentRun {
  return { task: task(taskId, createdAt), commentId, anchorCommentId, hasReply: true };
}

function runOnly(taskId: string, anchorCommentId: string, createdAt?: string): CommentRun {
  return { task: task(taskId, createdAt), anchorCommentId, hasReply: false };
}

const commentIds = (slots: ReturnType<typeof projectThreadDisplay>["slots"]) =>
  slots.filter((s) => s.kind === "comment").map((s) => s.comment.id);

describe("projectThreadDisplay", () => {
  it("shows every reply in canonical order when the thread has three or fewer", () => {
    const replies = [reply("r1", "2026-09-07T00:01:00Z"), reply("r2", "2026-09-07T00:02:00Z")];
    const projection = projectThreadDisplay(replies, [], false);
    expect(commentIds(projection.slots)).toEqual(["r1", "r2"]);
    expect(projection.hiddenCount).toBe(0);
  });

  it("compact mode shows exactly the final three with the correct hidden count (r1..r10 -> r8,r9,r10, hidden 7)", () => {
    const replies = Array.from({ length: 10 }, (_, i) =>
      reply(`r${i + 1}`, `2026-09-07T00:${String(i + 1).padStart(2, "0")}:00Z`));
    const projection = projectThreadDisplay(replies, [], false);
    expect(commentIds(projection.slots)).toEqual(["r8", "r9", "r10"]);
    expect(projection.hiddenCount).toBe(7);
  });

  it("expanded mode yields r1..r10 once, in order", () => {
    const replies = Array.from({ length: 10 }, (_, i) =>
      reply(`r${i + 1}`, `2026-09-07T00:${String(i + 1).padStart(2, "0")}:00Z`));
    const projection = projectThreadDisplay(replies, [], true);
    expect(commentIds(projection.slots)).toEqual(Array.from({ length: 10 }, (_, i) => `r${i + 1}`));
    expect(projection.hiddenCount).toBe(0);
  });

  it("a run anchored to a hidden reply must appear once, on its own visible comment slot, not on the anchor", () => {
    // r8 is anchored (as association metadata) to hidden r2 — the run's reply
    // (r8 itself) owns its own chronological slot regardless.
    const replies = Array.from({ length: 10 }, (_, i) =>
      reply(`r${i + 1}`, `2026-09-07T00:${String(i + 1).padStart(2, "0")}:00Z`));
    const runs = [replyRun("task-1", "r8", "r2")];
    const projection = projectThreadDisplay(replies, runs, false);
    expect(commentIds(projection.slots)).toEqual(["r8", "r9", "r10"]);
    const r8Slot = projection.slots.find((s) => s.kind === "comment" && s.comment.id === "r8");
    expect(r8Slot?.kind).toBe("comment");
    expect((r8Slot as { runs: CommentRun[] }).runs.map((r) => r.task.id)).toEqual(["task-1"]);
  });

  it("hidden r2 anchored to root or a visible reply (r9) must be absent from output entirely", () => {
    const replies = Array.from({ length: 10 }, (_, i) =>
      reply(`r${i + 1}`, `2026-09-07T00:${String(i + 1).padStart(2, "0")}:00Z`));
    // r2's own run has no published reply and is anchored to r9 (visible) —
    // r2 itself never appears because it is outside the visible window, and
    // must not be synthesized via the anchor either.
    const runs = [runOnly("task-2", "r9")];
    const projection = projectThreadDisplay(replies, runs, false);
    expect(commentIds(projection.slots)).toEqual(["r8", "r9", "r10"]);
    expect(projection.hiddenCount).toBe(7);
    // The run-only slot attaches after r9, its actual visible anchor.
    const r9Index = projection.slots.findIndex((s) => s.kind === "comment" && s.comment.id === "r9");
    expect(projection.slots[r9Index + 1]).toEqual({ kind: "run", run: runs[0] });
  });

  it("chain r1->r2->r8->r9->r10 retains exactly r8..r10 in compact mode", () => {
    const replies = [
      reply("r1", "2026-09-07T00:01:00Z"), reply("r2", "2026-09-07T00:02:00Z"),
      reply("r8", "2026-09-07T00:08:00Z"), reply("r9", "2026-09-07T00:09:00Z"),
      reply("r10", "2026-09-07T00:10:00Z"),
    ];
    const projection = projectThreadDisplay(replies, [], false);
    expect(commentIds(projection.slots)).toEqual(["r8", "r9", "r10"]);
    expect(projection.hiddenCount).toBe(2);
  });

  it("a visible->hidden->visible chain must not leak the hidden middle reply", () => {
    const replies = Array.from({ length: 10 }, (_, i) =>
      reply(`r${i + 1}`, `2026-09-07T00:${String(i + 1).padStart(2, "0")}:00Z`));
    // r9's run-only anchor metadata points at hidden r5; the run itself must
    // never surface r5's body, and must not attach after r5 either (r5 is not
    // in the visible window at all).
    const runs = [runOnly("task-mid", "r5")];
    const projection = projectThreadDisplay(replies, runs, false);
    expect(commentIds(projection.slots)).toEqual(["r8", "r9", "r10"]);
    expect(projection.slots.some((s) => s.kind === "run")).toBe(false);
  });

  it("multi-hop A->B->C anchor association: each loaded visible comment owns exactly one slot, no recursive emission", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const b = reply("b", "2026-09-07T00:02:00Z");
    const c = reply("c", "2026-09-07T00:03:00Z");
    // Three independent runs form an anchor chain a -> b -> c via metadata
    // only; each run's own commentId still owns exactly its own slot.
    const runs = [replyRun("t-a", "a", "a"), replyRun("t-b", "b", "a"), replyRun("t-c", "c", "b")];
    const projection = projectThreadDisplay([a, b, c], runs, true);
    expect(commentIds(projection.slots)).toEqual(["a", "b", "c"]);
    expect(projection.slots).toHaveLength(3);
  });

  it("self-anchor and cyclic anchor metadata do not cause infinite recursion or duplicate slots", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const b = reply("b", "2026-09-07T00:02:00Z");
    // a's run anchors to itself; b's run anchors to a, and a "conceptually"
    // anchors back to b via a different run — cyclic metadata that a
    // recursive walker would loop on. Direct-lookup projection is immune.
    const runs = [replyRun("t-self", "a", "a"), replyRun("t-b", "b", "a"), replyRun("t-cycle", "a", "b")];
    const projection = projectThreadDisplay([a, b], runs, true);
    expect(commentIds(projection.slots)).toEqual(["a", "b"]);
    const aSlot = projection.slots.find((s) => s.kind === "comment" && s.comment.id === "a");
    expect((aSlot as { runs: CommentRun[] }).runs.map((r) => r.task.id).sort()).toEqual(["t-cycle", "t-self"]);
  });

  it("duplicate task/comment association: multiple runs on the same reply dedupe into one comment body", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const runs = [replyRun("t-1", "a", "a", "2026-09-07T00:00:01Z"), replyRun("t-2", "a", "a", "2026-09-07T00:00:02Z")];
    const projection = projectThreadDisplay([a], runs, true);
    expect(projection.slots).toHaveLength(1);
    const slot = projection.slots[0];
    expect(slot?.kind).toBe("comment");
    expect((slot as { runs: CommentRun[] }).runs.map((r) => r.task.id)).toEqual(["t-1", "t-2"]);
  });

  it("no task is counted as a comment: a run without a published reply never adds a comment slot", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const runs = [runOnly("t-1", "a")];
    const projection = projectThreadDisplay([a], runs, true);
    expect(commentIds(projection.slots)).toEqual(["a"]);
    expect(projection.slots.filter((s) => s.kind === "run")).toHaveLength(1);
  });

  it("active no-reply run at a visible anchor gets one run-only slot ordered by task created_at/id", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const runs = [runOnly("t-late", "a", "2026-09-07T00:00:02Z"), runOnly("t-early", "a", "2026-09-07T00:00:01Z")];
    const projection = projectThreadDisplay([a], runs, true);
    const runSlots = projection.slots.filter((s) => s.kind === "run") as { kind: "run"; run: CommentRun }[];
    expect(runSlots.map((s) => s.run.task.id)).toEqual(["t-early", "t-late"]);
  });

  it("delayed/missing reply metadata: a run whose anchor or reply is not yet loaded is inert, not synthesized", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const runs = [replyRun("t-1", "missing-comment", "a"), runOnly("t-2", "missing-anchor")];
    const projection = projectThreadDisplay([a], runs, true);
    expect(commentIds(projection.slots)).toEqual(["a"]);
    expect(projection.slots.filter((s) => s.kind === "run")).toHaveLength(0);
    const slot = projection.slots[0];
    expect((slot as { runs: CommentRun[] }).runs).toHaveLength(0);
  });

  it("deduplicates repeated logical comment IDs in the input before selecting the visible window", () => {
    const a = reply("a", "2026-09-07T00:01:00Z");
    const projection = projectThreadDisplay([a, a], [], false);
    expect(commentIds(projection.slots)).toEqual(["a"]);
    expect(projection.hiddenCount).toBe(0);
  });

  it("deterministic ties: input order (created_at then id, as collectThreadReplies returns) is preserved verbatim", () => {
    const same = "2026-09-07T00:01:00Z";
    const a = reply("a", same);
    const b = reply("b", same);
    const projection = projectThreadDisplay([a, b], [], true);
    expect(commentIds(projection.slots)).toEqual(["a", "b"]);
  });

  it("nested chronology: a reply nested under an earlier sibling still occupies its chronological slot, not a nested one", () => {
    // collectThreadReplies already resolves depth-first vs chronological
    // ordering (#3691) before this function runs; projectThreadDisplay must
    // not re-nest by parent_id, only slot in the given order.
    const r1 = reply("r1", "2026-09-07T00:01:00Z");
    const r2 = reply("r2", "2026-09-07T00:02:00Z");
    const nested = { ...reply("d", "2026-09-07T00:03:00Z"), parent_id: "r1" };
    const projection = projectThreadDisplay([r1, r2, nested], [], true);
    expect(commentIds(projection.slots)).toEqual(["r1", "r2", "d"]);
  });

  describe("excludeFromVisible", () => {
    it("drops an excluded reply's own comment slot and keeps it out of hiddenCount", () => {
      const a = reply("a", "2026-09-07T00:01:00Z");
      const b = reply("b", "2026-09-07T00:02:00Z");
      const projection = projectThreadDisplay([a, b], [], false, new Set(["a"]));
      expect(commentIds(projection.slots)).toEqual(["b"]);
      expect(projection.hiddenCount).toBe(0);
    });

    it("does not let an excluded reply consume a compact-window slot, so a fourth ordinary reply becomes visible instead", () => {
      const replies = Array.from({ length: 4 }, (_, i) =>
        reply(`r${i + 1}`, `2026-09-07T00:0${i + 1}:00Z`));
      // Without exclusion this would be r2,r3,r4 hidden=1; excluding r1 must
      // not change which of the remaining three are chosen — r1 was never a
      // window candidate to begin with once excluded.
      const projection = projectThreadDisplay(replies, [], false, new Set(["r1"]));
      expect(commentIds(projection.slots)).toEqual(["r2", "r3", "r4"]);
      expect(projection.hiddenCount).toBe(0);
    });

    it("still resolves a downstream run-only slot anchored to an excluded reply", () => {
      const a = reply("a", "2026-09-07T00:01:00Z");
      const b = reply("b", "2026-09-07T00:02:00Z");
      // A queued follow-up run anchors to excluded reply "a" (comment-card.tsx's
      // root-anchored-run case: "a" is rendered elsewhere by the caller, but a
      // later run still targets it as its most recent covered input).
      const runs = [runOnly("t-follow", "a")];
      const projection = projectThreadDisplay([a, b], runs, false, new Set(["a"]));
      expect(commentIds(projection.slots)).toEqual(["b"]);
      const runSlots = projection.slots.filter((s) => s.kind === "run");
      expect(runSlots).toHaveLength(1);
      expect((runSlots[0] as { run: CommentRun }).run.task.id).toBe("t-follow");
    });

    it("with no excludeFromVisible argument, behaves exactly as before (empty-set default)", () => {
      const a = reply("a", "2026-09-07T00:01:00Z");
      const projection = projectThreadDisplay([a], [], true);
      expect(commentIds(projection.slots)).toEqual(["a"]);
    });
  });
});
