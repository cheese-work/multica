// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueStatusCategory } from "@multica/core/types";
import { summarizeGroup, type HierarchyIssue } from "./hierarchy-model";
import { layoutFocus, type FocusItem, type FocusLayout, type FocusNode } from "./hierarchy-focus-layout";

const categoryOf = (): IssueStatusCategory => "unstarted";
const issue = (n: number): HierarchyIssue => ({ id: `id-${n}`, identifier: `CHE-${n}`, title: `Issue ${n}`, status: "todo" });
const node = (n: number, kids = 0): FocusNode => ({ issue: issue(n), children: summarizeGroup(Array.from({ length: kids }, (_, i) => issue(n * 1000 + i)), categoryOf) });
const level = (from: number, count: number, kids = 0) => Array.from({ length: count }, (_, i) => node(from + i, kids));
const chainOf = (...ns: number[]) => ns.map(issue);
const kinds = (l: FocusLayout, kind: FocusItem["kind"]) => l.items.filter((i) => i.kind === kind);
const ids = (l: FocusLayout, kind: FocusItem["kind"]) => kinds(l, kind).map((i) => (i as { issue: HierarchyIssue }).issue.identifier);
const overlaps = (a: FocusItem["box"], b: FocusItem["box"]) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

describe("layoutFocus per depth (CHE-2797 shape: 4 depths, 3 levels of children)", () => {
  it("depth 0: root alone, no ancestors, one count card below it, no fan edge", () => {
    const l = layoutFocus({ chain: [], level: [node(2797, 2)] });
    expect(kinds(l, "chain")).toHaveLength(0);
    expect(ids(l, "card")).toEqual(["CHE-2797"]);
    expect(kinds(l, "count")).toMatchObject([{ parentId: "id-2797", summary: { total: 2 } }]);
    expect(l.edges).toHaveLength(1); // card -> its count card
    const [card] = kinds(l, "card");
    const [count] = kinds(l, "count");
    expect(count!.box.y).toBeGreaterThan(card!.box.y + card!.box.h);
  });

  it("depth 1: chain holds the root; children are cards, only those with children get a count card", () => {
    const l = layoutFocus({ chain: chainOf(2797), level: [node(2798, 41), node(2949, 12), node(3000)] });
    expect(ids(l, "chain")).toEqual(["CHE-2797"]);
    expect(ids(l, "card")).toEqual(["CHE-2798", "CHE-2949", "CHE-3000"]);
    expect(kinds(l, "card").map((c) => (c as { leaf: boolean }).leaf)).toEqual([false, false, true]);
    expect(kinds(l, "count").map((c) => (c as { parentId: string }).parentId)).toEqual(["id-2798", "id-2949"]);
    const [chain] = kinds(l, "chain");
    kinds(l, "card").forEach((c) => expect(c.box.y).toBeGreaterThan(chain!.box.y + chain!.box.h));
  });

  it("depth 2: chain is root then parent, left to right in order; every card sits below the chain", () => {
    const l = layoutFocus({ chain: chainOf(2797, 2798), level: level(2824, 5) });
    expect(ids(l, "chain")).toEqual(["CHE-2797", "CHE-2798"]);
    const [a, b] = kinds(l, "chain");
    expect(b!.box.x).toBeGreaterThanOrEqual(a!.box.x + a!.box.w);
    expect(kinds(l, "card")).toHaveLength(5);
    expect(kinds(l, "count")).toHaveLength(0);
  });

  it("depth 3 (leaf level): no count cards at all", () => {
    const l = layoutFocus({ chain: chainOf(2797, 2798, 2838), level: level(3039, 2) });
    expect(ids(l, "chain")).toEqual(["CHE-2797", "CHE-2798", "CHE-2838"]);
    expect(kinds(l, "count")).toHaveLength(0);
    expect(kinds(l, "card").every((c) => (c as { leaf: boolean }).leaf)).toBe(true);
  });
});

describe("layoutFocus dashed leaf edges", () => {
  const into = (l: FocusLayout, c: FocusItem) => l.edges.filter((e) => e.points.at(-1)![0] === c.box.x + c.box.w / 2 && e.points.at(-1)![1] === c.box.y);

  it("the edge into a leaf card is dashed; into a card with children it is solid", () => {
    const l = layoutFocus({ chain: chainOf(1), level: [node(10, 3), node(11), node(12, 1), node(13)] });
    const cards = kinds(l, "card") as Extract<FocusItem, { kind: "card" }>[];
    expect(cards.map((c) => c.leaf)).toEqual([false, true, false, true]);
    cards.forEach((c) => {
      const edges = into(l, c);
      expect(edges).toHaveLength(1);
      expect(edges[0]!.dashed).toBe(c.leaf);
    });
  });

  it("every other edge stays solid: fan, trunk and card-to-count", () => {
    const l = layoutFocus({ chain: chainOf(1), level: [node(10, 3), node(11), node(12, 1)] });
    const leafIntoEdges = kinds(l, "card").filter((c) => (c as { leaf: boolean }).leaf).flatMap((c) => into(l, c));
    expect(l.edges.filter((e) => e.dashed)).toEqual(leafIntoEdges);
    expect(l.edges.filter((e) => e.dashed)).toHaveLength(1);
  });

  it("a level of only leaves (depth 3) is all dashed card edges", () => {
    const l = layoutFocus({ chain: chainOf(1, 2, 3), level: level(50, 4) });
    const cards = kinds(l, "card");
    cards.forEach((c) => expect(into(l, c).every((e) => e.dashed)).toBe(true));
  });

  it("a root leaf at depth 0 has no edge at all", () => {
    expect(layoutFocus({ chain: [], level: [node(1)] }).edges).toEqual([]);
  });
});

describe("layoutFocus rules", () => {
  it("fan edges connect the last chain card to every first-row card", () => {
    const l = layoutFocus({ chain: chainOf(1, 2), level: level(10, 3) });
    const chain = kinds(l, "chain").at(-1)!;
    const cards = kinds(l, "card");
    const ends = l.edges.map((e) => e.points.at(-1)!);
    cards.forEach((c) => expect(ends).toContainEqual([c.box.x + c.box.w / 2, c.box.y]));
    expect(l.edges.some((e) => e.points[0]![0] === chain.box.x + chain.box.w / 2 && e.points[0]![1] === chain.box.y + chain.box.h)).toBe(true);
  });

  it("caps the level at 60 cards and adds a '+N' tile; count cards do not count toward the cap", () => {
    const l = layoutFocus({ chain: chainOf(1), level: level(100, 74, 3) });
    expect(kinds(l, "card")).toHaveLength(60);
    expect(kinds(l, "count")).toHaveLength(60);
    expect(kinds(l, "more")).toMatchObject([{ hidden: 14 }]);
    expect(ids(l, "card")[59]).toBe("CHE-159");
  });

  it("exactly 60 cards: no '+N' tile", () => {
    expect(kinds(layoutFocus({ chain: chainOf(1), level: level(100, 60) }), "more")).toHaveLength(0);
  });

  it("deep chain folds the middle into one '… N' card, keeping first and last two", () => {
    const l = layoutFocus({ chain: chainOf(1, 2, 3, 4, 5, 6, 7), level: level(10, 2) });
    expect(ids(l, "chain")).toEqual(["CHE-1", "CHE-6", "CHE-7"]);
    expect(kinds(l, "fold")).toMatchObject([{ hidden: [issue(2), issue(3), issue(4), issue(5)] }]);
  });

  it("a chain at the limit is not folded", () => {
    const l = layoutFocus({ chain: chainOf(1, 2, 3, 4), level: level(10, 1) });
    expect(ids(l, "chain")).toHaveLength(4);
    expect(kinds(l, "fold")).toHaveLength(0);
  });

  it("empty level yields only the chain", () => {
    const l = layoutFocus({ chain: chainOf(1), level: [] });
    expect(l.items.map((i) => i.kind)).toEqual(["chain"]);
  });

  it("no two blocks overlap and every block is inside bounds, for small and large levels", () => {
    for (const [chain, n, kids] of [[[], 1, 2], [[1], 5, 0], [[1, 2], 12, 3], [[1, 2, 3, 4, 5, 6], 74, 2]] as const) {
      const l = layoutFocus({ chain: chainOf(...chain), level: level(100, n, kids) });
      l.items.forEach((a, i) => {
        l.items.slice(i + 1).forEach((b) => expect(overlaps(a.box, b.box)).toBe(false));
        expect(a.box.x).toBeGreaterThanOrEqual(l.bounds.x);
        expect(a.box.y).toBeGreaterThanOrEqual(l.bounds.y);
        expect(a.box.x + a.box.w).toBeLessThanOrEqual(l.bounds.x + l.bounds.w);
        expect(a.box.y + a.box.h).toBeLessThanOrEqual(l.bounds.y + l.bounds.h);
      });
    }
  });

  it("is deterministic", () => {
    const input = { chain: chainOf(1, 2), level: level(10, 9, 2) };
    expect(layoutFocus(input)).toEqual(layoutFocus(input));
  });
});
