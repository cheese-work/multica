import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderWithI18n } from "../../test/i18n";
import { RunBudgetChip } from "./run-budget-chip";

describe("RunBudgetChip (CHE-5285)", () => {
  it("names a budget-blocked issue next to its status", () => {
    renderWithI18n(<RunBudgetChip issue={{ status: "blocked", run_budget_blocked: true }} />);
    expect(screen.getByText("Budget used")).toBeTruthy();
  });

  it("stays silent for a plain blocked issue", () => {
    const { container } = renderWithI18n(<RunBudgetChip issue={{ status: "blocked" }} />);
    expect(container.textContent).toBe("");
  });

  it("stays silent once the issue left blocked", () => {
    const { container } = renderWithI18n(<RunBudgetChip issue={{ status: "todo", run_budget_blocked: true }} />);
    expect(container.textContent).toBe("");
  });
});
