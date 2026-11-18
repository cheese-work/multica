import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionSummary } from "./decision-summary";

vi.mock("@multica/core/api", async (importActual) => ({ ...(await importActual<typeof import("@multica/core/api")>()), api: {} }));

const raise = { runs_used: 25, runs_limit: 25, runs_limit_after: 30 };
const topUp = { runs_used: 44, runs_limit: 25, runs_limit_after: 25, new_window: true };
const at = "2026-11-11T06:41:00Z";
const render = (action: Record<string, unknown>) => {
  const decision = {
    id: "request", issue_id: "issue", issue_identifier: "CHE-3461", ask: "Raise?", state: "answered",
    created_at: "2026-11-11T06:20:00Z", action: { id: "a", action_type: "issue.run_budget.top_up", payload_sha256: "p", state: "executed", effect: "applied", decided_at: "2026-11-11T06:44:00Z", ...action },
  } as unknown as RequestCard;
  renderWithI18n(<QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionSummary decision={decision} /></WorkspaceSlugProvider></QueryClientProvider>);
  return {
    effect: screen.getByTestId("decision-request-effect").textContent,
    runs: screen.queryByTestId("decision-request-runs")?.textContent ?? null,
    outcome: screen.queryByTestId("decision-request-receipt")?.textContent ?? null,
    meta: screen.getByTestId("decision-request-meta").textContent ?? "",
  };
};

// One row per line of the plan section 4 table (CHE-6943).
describe("DecisionSummary budget receipt", () => {
  it.each([
    ["cap_applied", null],
    ["run_queued", "Run queued."],
    ["run_started", "Run started."],
    ["status_restored", "Status restored."],
    ["still_blocked:runs", "Still blocked: no runs left."],
    ["still_blocked:tokens", "Still blocked: token budget used up."],
    ["still_blocked:future", "Still blocked."],
    ["superseded:later_window", "Not resumed: a newer budget window replaced this one."],
    ["superseded:status_moved", "Not resumed: the issue moved to another status."],
    ["superseded:window_changed", "Not resumed: the budget window changed."],
    ["superseded:issue_closed", "Not resumed: the issue is closed."],
    ["superseded:agent_archived", "Not resumed: the agent is archived."],
    ["superseded:restore_refused", "Not resumed: the status could not be restored."],
    ["superseded:unknown_origin", "Not resumed: the status before the block is unknown. Move the status by hand."],
    ["superseded:assignee_changed", "Not resumed: the assignee changed."],
    ["superseded:source_unknown", "Not resumed: the original trigger is unknown."],
    ["superseded:source_not_delivered", "Not resumed: the original trigger has no work left."],
    ["superseded:future", "Not resumed."],
    ["future_token", null],
  ])("%s", (outcome, line) => {
    const got = render({ receipt: { outcome, applied_at: at, ...topUp } });
    expect(got.effect).toBe("Applied. CHE-3461 now shows 0 of 25 runs.");
    expect(got.runs).toBe("44 of 25 → 0 of 25 (new window)");
    expect(got.outcome).toBe(line);
  });

  it("a raise shows the new cap and no new-window note", () => {
    const got = render({ action_type: "issue.run_budget.raise", receipt: { outcome: "cap_applied", applied_at: at, ...raise } });
    expect(got.effect).toBe("Applied. CHE-3461 now shows 25 of 30 runs.");
    expect(got.runs).toBe("25 of 25 → 25 of 30");
  });

  it("without numbers: plain Applied. and no before → after row, outcome line still shown", () => {
    const got = render({ receipt: { outcome: "run_queued", applied_at: at } });
    expect(got.effect).toBe("Applied.");
    expect(got.runs).toBeNull();
    expect(got.outcome).toBe("Run queued.");
  });

  it("no receipt: today's card", () => {
    const got = render({});
    expect(got.effect).toBe("Applied.");
    expect(got.runs).toBeNull();
    expect(got.outcome).toBeNull();
  });

  it("the Decided row shows receipt.applied_at in ICT", () => {
    const got = render({ decided_at: "2026-11-11T05:00:00Z", receipt: { outcome: "cap_applied", applied_at: at, ...topUp } });
    expect(got.meta).toContain("11 Nov 2026, 13:41 ICT");
    expect(got.meta).not.toContain("12:00 ICT");
  });

  it("a revoked or not-applied action shows no receipt line", () => {
    const got = render({ state: "failed", effect: "not_applied", effect_reason: "boom", receipt: { outcome: "run_queued", applied_at: at, ...topUp } });
    expect(got.outcome).toBeNull();
    expect(got.runs).toBeNull();
  });
});
