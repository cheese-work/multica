// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseRunBudget, UNBUDGETED } from "./schema";

const budgeted = {
  budgeted: true,
  runs_used: 8,
  runs_left: 2,
  runs_limit: 10,
  window_started_at: "2026-11-01T05:22:54.123456789Z",
  window_id: "2026-11-01T05:22:54.123456789Z",
  runs_state: "near",
  state: "near",
  blocking: [],
  can_raise: true,
  tokens_active: false,
  raise_preview: { add_runs: 5, balance_after: 7, limit_after: 15, max_allowed_add: 30 },
};

describe("run-budget meter response boundary (CHE-2851)", () => {
  it("maps a budgeted snapshot, keeping window_id opaque", () => {
    expect(parseRunBudget(budgeted)).toEqual({
      budgeted: true,
      runsUsed: 8,
      runsLeft: 2,
      runsLimit: 10,
      state: "near",
      windowId: "2026-11-01T05:22:54.123456789Z",
      windowStartedAt: "2026-11-01T05:22:54.123456789Z",
      blocking: [],
      canRaise: true,
      raisePreview: { addRuns: 5, limitAfter: 15 },
    });
  });

  it("has no raise preview when the server sends none or a malformed one", () => {
    const { raise_preview: _drop, ...bare } = budgeted;
    expect(parseRunBudget(bare).raisePreview).toBeNull();
    expect(parseRunBudget({ ...budgeted, raise_preview: { add_runs: "x" } }).raisePreview).toBeNull();
  });

  it("accepts every server state, including negative runs_left", () => {
    for (const state of ["ok", "near", "at_limit", "over"] as const) {
      expect(parseRunBudget({ ...budgeted, state, runs_state: state, runs_left: -1 }).state).toBe(state);
    }
    expect(parseRunBudget({ ...budgeted, runs_left: -1 }).runsLeft).toBe(-1);
  });

  it("treats an unbudgeted snapshot as unbudgeted", () => {
    expect(parseRunBudget({ budgeted: false, runs_used: 0, runs_left: 0, runs_limit: 0, window_started_at: "0001-01-01T00:00:00Z", blocking: [], tokens_active: false, tokens_balance: 0, tokens_overshoot: 0, can_raise: false })).toEqual(UNBUDGETED);
  });

  it("tolerates a server without window_id (A1b2 not shipped)", () => {
    const { window_id: _omit, ...legacy } = budgeted;
    expect(parseRunBudget(legacy)).toMatchObject({ budgeted: true, windowId: null, runsLeft: 2 });
  });

  it("falls back to unbudgeted on malformed responses, never inventing numbers", () => {
    for (const bad of [
      null,
      undefined,
      "oops",
      [],
      {},
      { budgeted: true },
      { ...budgeted, runs_left: "2" },
      { ...budgeted, runs_left: null },
      { ...budgeted, runs_limit: 1.5 },
      { ...budgeted, state: "bogus" },
      { ...budgeted, state: undefined },
      { ...budgeted, window_id: 7 },
      { ...budgeted, blocking: "runs" },
    ]) {
      expect(parseRunBudget(bad)).toEqual(UNBUDGETED);
    }
  });
});
