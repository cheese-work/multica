import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Stepper, StepperItem, StepperNav, StepperTrigger } from "../stepper";

afterEach(cleanup);

function Steps({ firstKey }: { firstKey: string }) {
  return (
    <Stepper>
      <StepperNav>
        <StepperItem step={1}>
          <StepperTrigger render={(props) => <button key={firstKey} data-testid="first" {...props} />}>One</StepperTrigger>
        </StepperItem>
        <StepperItem step={2}>
          <StepperTrigger data-testid="second">Two</StepperTrigger>
        </StepperItem>
      </StepperNav>
    </Stepper>
  );
}

describe("StepperTrigger keyboard navigation", () => {
  it("moves focus to the next trigger", () => {
    render(<Steps firstKey="a" />);
    screen.getByTestId("first").focus();
    fireEvent.keyDown(screen.getByTestId("first"), { key: "ArrowRight" });
    expect(document.activeElement).toBe(screen.getByTestId("second"));
  });

  it("focuses the replacement node after render swaps the trigger's button", () => {
    const { rerender } = render(<Steps firstKey="a" />);
    const before = screen.getByTestId("first");
    rerender(<Steps firstKey="b" />);
    expect(screen.getByTestId("first")).not.toBe(before);
    screen.getByTestId("second").focus();
    fireEvent.keyDown(screen.getByTestId("second"), { key: "ArrowLeft" });
    expect(document.activeElement).toBe(screen.getByTestId("first"));
  });
});
