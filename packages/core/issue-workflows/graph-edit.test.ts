// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflowStep } from "../types";
import {
  addStepEdge,
  EDITABLE_GUARDS,
  removeStepEdge,
  stepEdges,
  updateStepEdge,
  validateWorkflowGraph,
} from "./graph-edit";

const step = (
  status_key: string,
  rest: Partial<IssueWorkflowStep> = {},
): IssueWorkflowStep => ({
  status_key,
  handler: { type: "none" },
  instructions: "",
  ...rest,
});

const base = (mid: Partial<IssueWorkflowStep> = {}) => [
  step("todo"),
  step("in_progress", mid),
  step("in_review"),
  step("blocked"),
  step("done"),
  step("cancelled"),
];

const codes = (steps: IssueWorkflowStep[]) =>
  validateWorkflowGraph(steps).map((i) => `${i.from}:${i.code}`);

describe("validateWorkflowGraph", () => {
  it("accepts a valid workflow", () => {
    expect(
      codes(
        base({
          edges: [
            { to: "in_review", guard: "agent_done" },
            { to: "done", guard: "pr.merged", timeout: "72h" },
          ],
        }),
      ),
    ).toEqual([]);
  });

  it("rejects more than 8 edges on a step", () => {
    const edges = Array.from({ length: 9 }, () => ({
      to: "done",
      guard: "agent_done",
    }));
    expect(codes(base({ edges }))).toContain("in_progress:too_many_edges");
  });

  it("rejects an edge to a missing status or to itself", () => {
    expect(
      codes(
        base({
          edges: [
            { to: "ghost", guard: "agent_done" },
            { to: "in_progress", guard: "agent_done" },
          ],
        }),
      ),
    ).toEqual(["in_progress:missing_target", "in_progress:missing_target"]);
  });

  it.each(["pr.review_changes_requested", "pr.review_approved"])(
    "accepts the %s guard and requires a timeout on it",
    (guard) => {
      expect(EDITABLE_GUARDS).toContain(guard);
      expect(codes(base({ edges: [{ to: "in_review", guard, timeout: "72h" }] }))).toEqual([]);
      expect(codes(base({ edges: [{ to: "in_review", guard }] }))).toEqual([
        "in_progress:bad_timeout",
      ]);
    },
  );

  it("rejects a guard the server does not know", () => {
    expect(
      codes(
        base({
          edges: [
            { to: "done", guard: "webhook" },
            { to: "done", guard: "agent_verdict(MAYBE)" },
            { to: "done", guard: "issue_status(CHE-1)" },
          ],
        }),
      ),
    ).toEqual([
      "in_progress:bad_guard",
      "in_progress:bad_guard",
      "in_progress:bad_guard",
    ]);
  });

  it("requires a timeout between 1m and 720h on PR guards", () => {
    const edge = (timeout?: string) =>
      codes(base({ edges: [{ to: "done", guard: "pr.merged", timeout }] }));
    expect(edge(undefined)).toEqual(["in_progress:bad_timeout"]);
    expect(edge("soon")).toEqual(["in_progress:bad_timeout"]);
    expect(edge("30s")).toEqual(["in_progress:bad_timeout"]);
    expect(edge("721h")).toEqual(["in_progress:bad_timeout"]);
    expect(edge("1h30m")).toEqual([]);
  });

  it("needs on_timeout only when the workflow has no blocked status", () => {
    const edges = [{ to: "done", guard: "pr.merged", timeout: "72h" }];
    const noBlocked = base({ edges }).filter((s) => s.status_key !== "blocked");
    expect(codes(noBlocked)).toEqual(["in_progress:bad_on_timeout"]);
    expect(
      codes(
        noBlocked.map((s) =>
          s.status_key === "in_progress"
            ? { ...s, edges: [{ ...edges[0]!, on_timeout: "in_review" }] }
            : s,
        ),
      ),
    ).toEqual([]);
    expect(
      codes(
        base({ edges: [{ ...edges[0]!, on_timeout: "in_progress" }] }),
      ),
    ).toEqual(["in_progress:bad_on_timeout"]);
  });

  it("does not hold the save for a handler without explicit exits", () => {
    const agent = { type: "agent" as const, id: "a-1" };
    expect(codes(base({ handler: agent }))).toEqual([]);
    expect(codes(base({ handler: { type: "project_lead" } }))).toEqual([]);
  });

  it("holds a CI-green exit out of a status the server refuses it from", () => {
    const ci = { to: "todo", guard: "pr.checks_green", timeout: "72h", on_timeout: "todo" };
    expect(
      codes(
        [step("backlog", { edges: [ci] }), step("todo"), step("blocked", { edges: [ci] }), step("done", { edges: [ci] }), step("cancelled")],
      ),
    ).toEqual(["backlog:ci_green_source", "blocked:ci_green_source", "done:ci_green_source"]);
  });

  it("does not ask a legacy CI advance for a timeout", () => {
    expect(codes(base({ advance_on_ci_green: true, next_status_key: "in_review" }))).toEqual([]);
  });
});

describe("stepEdges", () => {
  it("falls back to the exits the legacy fields stand for", () => {
    expect(
      stepEdges(base({ next_status_key: "in_review", back_status_key: "todo" }), "in_progress"),
    ).toEqual([
      { to: "in_review", guard: "agent_done" },
      { to: "todo", guard: "agent_verdict(CHANGES)" },
    ]);
  });
});

describe("editing edges", () => {
  it("adds an edge after materialising derived exits and drops the legacy fields", () => {
    const out = addStepEdge(
      base({ next_status_key: "in_review", advance_on_ci_green: true }),
      "in_progress",
      "done",
    );
    const s = out.find((x) => x.status_key === "in_progress")!;
    expect(s.edges).toEqual([
      { to: "in_review", guard: "agent_done" },
      { to: "in_review", guard: "pr.checks_green", timeout: "72h", on_timeout: "blocked" },
      { to: "done", guard: "agent_done" },
    ]);
    expect(s.next_status_key).toBeUndefined();
    expect(s.advance_on_ci_green).toBeFalsy();
  });

  it("does not add an edge to itself, to a missing status, or past the cap", () => {
    const steps = base();
    expect(addStepEdge(steps, "in_progress", "in_progress")).toBe(steps);
    expect(addStepEdge(steps, "in_progress", "ghost")).toBe(steps);
    const full = base({
      edges: Array.from({ length: 8 }, () => ({ to: "done", guard: "agent_done" })),
    });
    expect(addStepEdge(full, "in_progress", "in_review")).toBe(full);
  });

  it("gives a PR guard a default timeout and on_timeout, and drops them for others", () => {
    const start = base({ edges: [{ to: "done", guard: "agent_done" }] });
    const pr = updateStepEdge(start, "in_progress", 0, { guard: "pr.merged" });
    expect(stepEdges(pr, "in_progress")).toEqual([
      { to: "done", guard: "pr.merged", timeout: "72h", on_timeout: "blocked" },
    ]);
    const back = updateStepEdge(pr, "in_progress", 0, { guard: "agent_done" });
    expect(stepEdges(back, "in_progress")).toEqual([
      { to: "done", guard: "agent_done" },
    ]);
  });

  it("removing the last edge leaves no ghost exit from legacy fields", () => {
    const out = removeStepEdge(
      base({ next_status_key: "in_review" }),
      "in_progress",
      0,
    );
    expect(stepEdges(out, "in_progress")).toEqual([]);
    expect(
      out.find((x) => x.status_key === "in_progress")!.next_status_key,
    ).toBeUndefined();
  });
});
