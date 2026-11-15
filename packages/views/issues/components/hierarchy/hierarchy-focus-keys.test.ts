// @vitest-environment node
import { describe, expect, it } from "vitest";
import { focusKeyAction, nextCard } from "./hierarchy-focus-keys";
import type { FocusItem } from "./hierarchy-focus-layout";

describe("focusKeyAction", () => {
  it("+ and = step in; - and _ step out", () => {
    for (const key of ["+", "="]) expect(focusKeyAction({ key }, 1)).toEqual({ type: "step-in" });
    for (const key of ["-", "_"]) expect(focusKeyAction({ key }, 1)).toEqual({ type: "step-out" });
  });

  it("- at the root has nothing to step out to, so it is not handled", () => {
    expect(focusKeyAction({ key: "-" }, 0)).toBeNull();
  });

  it("Escape steps out below the root; at the root it is left to the dialog (close)", () => {
    expect(focusKeyAction({ key: "Escape" }, 2)).toEqual({ type: "step-out" });
    expect(focusKeyAction({ key: "Escape" }, 0)).toBeNull();
  });

  it("arrows move, Enter opens, Space toggles the card's children", () => {
    expect(focusKeyAction({ key: "ArrowLeft" }, 1)).toEqual({ type: "move", direction: "left" });
    expect(focusKeyAction({ key: "ArrowRight" }, 1)).toEqual({ type: "move", direction: "right" });
    expect(focusKeyAction({ key: "ArrowUp" }, 1)).toEqual({ type: "move", direction: "up" });
    expect(focusKeyAction({ key: "ArrowDown" }, 1)).toEqual({ type: "move", direction: "down" });
    expect(focusKeyAction({ key: "Enter" }, 1)).toEqual({ type: "open" });
    expect(focusKeyAction({ key: " " }, 1)).toEqual({ type: "toggle" });
  });

  it("ignores Ctrl, Meta and Alt chords so browser shortcuts keep working", () => {
    for (const mod of ["ctrlKey", "metaKey", "altKey"] as const) expect(focusKeyAction({ key: "+", [mod]: true }, 1)).toBeNull();
  });

  it("ignores unrelated keys", () => {
    expect(focusKeyAction({ key: "a" }, 1)).toBeNull();
    expect(focusKeyAction({ key: "Tab" }, 1)).toBeNull();
  });
});

const card = (x: number, y: number): FocusItem => ({ kind: "card", leaf: true, issue: { id: `${x}-${y}`, identifier: "CHE-1", title: "t", status: "todo" }, box: { x, y, w: 200, h: 84 } });
const count = (x: number, y: number): FocusItem => ({ kind: "count", parentId: "p", summary: {} as never, box: { x, y, w: 200, h: 44 } });
const chain = (x: number): FocusItem => ({ kind: "chain", issue: { id: "c", identifier: "CHE-0", title: "t", status: "todo" }, box: { x, y: 0, w: 150, h: 48 } });

describe("nextCard (2 columns x 2 rows, a chain and a count card among the items)", () => {
  //   chain
  //   [1] [2]
  //   [3] [4]   (count card sits under [1], between the rows)
  const items = [chain(0), card(0, 150), count(0, 262), card(222, 150), card(0, 400), card(222, 400)];
  const [, c1, , c2, c3, c4] = items.map((_, i) => i);

  it("moves to the nearest card in the direction", () => {
    expect(nextCard(items, c1!, "right")).toBe(c2);
    expect(nextCard(items, c2!, "left")).toBe(c1);
    expect(nextCard(items, c1!, "down")).toBe(c3);
    expect(nextCard(items, c4!, "up")).toBe(c2);
  });

  it("skips chain and count cards: only cards are stops", () => {
    expect(nextCard(items, c3!, "up")).toBe(c1);
  });

  it("stays put at an edge", () => {
    expect(nextCard(items, c1!, "left")).toBe(c1);
    expect(nextCard(items, c1!, "up")).toBe(c1);
    expect(nextCard(items, c4!, "right")).toBe(c4);
    expect(nextCard(items, c4!, "down")).toBe(c4);
  });

  it("with no card in the level, returns the start index", () => {
    expect(nextCard([chain(0)], 0, "down")).toBe(0);
  });
});
