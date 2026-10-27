// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { StatusSummary, statusSummaryOf } from "./status-summary";

describe("statusSummaryOf", () => {
  it("returns the trimmed string and ignores anything else", () => {
    expect(statusSummaryOf({ status_summary: "  PR open  " })).toBe("PR open");
    expect(statusSummaryOf({ status_summary: 42 })).toBeNull();
    expect(statusSummaryOf({ status_summary: "   " })).toBeNull();
    expect(statusSummaryOf({})).toBeNull();
    expect(statusSummaryOf(undefined)).toBeNull();
  });
});

describe("StatusSummary", () => {
  it("shows the labelled summary as plain text", () => {
    renderWithI18n(<StatusSummary summary="Waiting on <b>review</b>" />);
    expect(screen.getByText("Status summary")).toBeTruthy();
    expect(screen.getByText("Waiting on <b>review</b>")).toBeTruthy();
  });

  it("renders nothing without a summary", () => {
    const { container } = renderWithI18n(<StatusSummary summary={null} />);
    expect(container.textContent).toBe("");
  });
});
