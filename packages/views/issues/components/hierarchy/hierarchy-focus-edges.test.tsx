import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FocusEdges } from "./hierarchy-focus-edges";

describe("FocusEdges", () => {
  it("dashed edges carry a dash pattern; solid edges carry none", () => {
    const { container } = render(<FocusEdges edges={[
      { points: [[0, 0], [0, 10]], dashed: true },
      { points: [[5, 0], [5, 10]], dashed: false },
    ]} />);
    const lines = container.querySelectorAll("polyline");
    expect(lines).toHaveLength(2);
    expect(lines[0]!.getAttribute("stroke-dasharray")).toBe("6 4");
    expect(lines[0]!.getAttribute("points")).toBe("0,0 0,10");
    expect(lines[1]!.hasAttribute("stroke-dasharray")).toBe(false);
  });
});
