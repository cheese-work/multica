/** @vitest-environment jsdom */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { renderWithI18n } from "../test/i18n";
import { LintMarks } from "./workflow-lint-marks";

afterEach(cleanup);

describe("LintMarks", () => {
  it("shows a count with a word for each level that has findings, never colour alone", () => {
    renderWithI18n(<LintMarks counts={{ errors: 2, warnings: 1, infos: 0 }} />);
    expect(screen.getByLabelText("2 errors").textContent).toBe("2");
    expect(screen.getByLabelText("1 warning").textContent).toBe("1");
    expect(screen.queryByLabelText(/note/)).toBeNull();
  });

  it("renders nothing for a clean workflow", () => {
    const { container } = renderWithI18n(<LintMarks counts={{ errors: 0, warnings: 0, infos: 0 }} />);
    expect(container.textContent).toBe("");
  });
});
