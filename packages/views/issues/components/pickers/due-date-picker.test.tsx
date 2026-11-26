import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithI18n } from "../../../test/i18n";
import { DueDatePicker } from "./due-date-picker";

describe("DueDatePicker (issue detail sidebar path)", () => {
  it("opens from the cold trigger, picks a day, and closes without looping", async () => {
    const onUpdate = vi.fn();
    renderWithI18n(<DueDatePicker dueDate="2026-11-27" onUpdate={onUpdate} />);
    fireEvent.click(screen.getByRole("button"));
    await act(() => vi.dynamicImportSettled());
    await screen.findByRole("grid");
    const target = new Date(2026, 10, 28);
    fireEvent.click(document.querySelector(`button[data-day="${target.toLocaleDateString()}"]`)!);
    expect(onUpdate).toHaveBeenCalledTimes(1);
    expect(onUpdate).toHaveBeenCalledWith({ due_date: "2026-11-28" });
    await waitFor(() => expect(screen.queryByRole("grid")).not.toBeInTheDocument());
  });
});
