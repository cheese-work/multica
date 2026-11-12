// @vitest-environment node
import { describe, expect, it } from "vitest";
import { isStoppedByBudget, runBudgetTone } from "./state";

describe("runBudgetTone", () => {
  it.each([
    ["ok", "ok"],
    ["near", "warn"],
    ["at_limit", "danger"],
    ["over", "danger"],
  ] as const)("maps %s to %s", (state, tone) => {
    expect(runBudgetTone(state)).toBe(tone);
  });

  it("treats a missing state as ok", () => {
    expect(runBudgetTone(null)).toBe("ok");
  });
});

describe("isStoppedByBudget", () => {
  it("is true only for a blocked issue the server marks as budget-blocked", () => {
    expect(isStoppedByBudget({ status: "blocked", run_budget_blocked: true })).toBe(true);
    expect(isStoppedByBudget({ status: "blocked" })).toBe(false);
    expect(isStoppedByBudget({ status: "todo", run_budget_blocked: true })).toBe(false);
  });
});
