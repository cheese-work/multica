// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueStatusCategory } from "@multica/core/types";
import { bucketOf, summarizeGroup, type HierarchyIssue } from "./hierarchy-model";
import { centerOn, fitView, intersects, layoutHierarchy, modeFor, panBy, zoomAt, type Box, type Layout } from "./hierarchy-layout";

const categoryOf = (status: string): IssueStatusCategory =>
  ({ todo: "unstarted", backlog: "unstarted", in_progress: "started", blocked: "started", done: "done", cancelled: "closed" } as Record<string, IssueStatusCategory>)[status] ?? "unstarted";
const issue = (n: number, status = "todo"): HierarchyIssue => ({ id: `id-${n}`, identifier: `CHE-${n}`, title: `Issue ${n}`, status });
const many = (from: number, count: number, status = "todo") => Array.from({ length: count }, (_, i) => issue(from + i, status));
const hierarchy = (siblings: HierarchyIssue[], children: HierarchyIssue[], withParent = true) => ({
  parent: withParent ? issue(1, "in_progress") : null,
  current: issue(2, "in_progress"),
  siblings: summarizeGroup(siblings, categoryOf),
  children: summarizeGroup(children, categoryOf),
  siblingIssues: siblings,
  childIssues: children,
  relatedIssues: [] as HierarchyIssue[],
});
const overlaps = (a: Box, b: Box) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
const boxes = (layout: Layout) => layout.items.filter((item) => item.kind !== "label").map((item) => item.box);

describe("bucketOf", () => {
  it("separates blocked from active; closed stays its own bucket", () => {
    expect(bucketOf("blocked", "started")).toBe("blocked");
    expect(bucketOf("in_progress", "started")).toBe("active");
    expect(bucketOf("todo", "unstarted")).toBe("todo");
    expect(bucketOf("done", "done")).toBe("done");
    expect(bucketOf("cancelled", "closed")).toBe("closed");
  });
});

describe("summarizeGroup", () => {
  it("counts every bucket and names at most 3 issues, blocked first", () => {
    const issues = [...many(10, 12, "done"), ...many(30, 2, "in_progress"), ...many(40, 2, "blocked"), ...many(50, 5)];
    const summary = summarizeGroup(issues, categoryOf);
    expect(summary.total).toBe(21);
    expect(summary.counts).toEqual({ done: 12, active: 2, blocked: 2, todo: 5, closed: 0 });
    expect(summary.named.map((n) => n.identifier)).toEqual(["CHE-40", "CHE-41", "CHE-30"]);
    expect(summary.rest).toEqual({ done: 12, active: 1, blocked: 0, todo: 5, closed: 0 });
  });
  it("handles an empty group", () => {
    expect(summarizeGroup([], categoryOf)).toMatchObject({ total: 0, named: [] });
  });
});

describe("modeFor", () => {
  it("splits groups into single issues from 70% zoom", () => {
    expect(modeFor(0.69)).toBe("groups");
    expect(modeFor(0.7)).toBe("cards");
  });
});

describe("layoutHierarchy", () => {
  it("zoomed out: one block per group with totals, current stays a full card", () => {
    const layout = layoutHierarchy(hierarchy(many(100, 21), many(200, 33)), "groups");
    expect(layout.items.filter((i) => i.kind === "group").map((i) => i.kind === "group" && i.summary.total)).toEqual([21, 33]);
    expect(layout.items.some((i) => i.kind === "issue" && i.role === "current")).toBe(true);
    expect(layout.items.some((i) => i.kind === "issue" && i.role === "sibling")).toBe(false);
  });
  it("zoomed in: every issue is its own card and labels carry the totals", () => {
    const layout = layoutHierarchy(hierarchy(many(100, 21), many(200, 33)), "cards");
    expect(layout.items.filter((i) => i.kind === "issue" && i.role === "sibling")).toHaveLength(21);
    expect(layout.items.filter((i) => i.kind === "issue" && i.role === "child")).toHaveLength(33);
    expect(layout.items.filter((i) => i.kind === "label").map((i) => i.kind === "label" && [i.role, i.total, i.shown])).toEqual([["siblings", 21, 21], ["children", 33, 33]]);
  });
  it("caps cards at 60 per group and says how many are shown", () => {
    const layout = layoutHierarchy(hierarchy(many(100, 90), []), "cards");
    expect(layout.items.filter((i) => i.kind === "issue" && i.role === "sibling")).toHaveLength(60);
    expect(layout.items.find((i) => i.kind === "label")).toMatchObject({ total: 90, shown: 60 });
  });
  it.each(["groups", "cards"] as const)("%s: no two blocks overlap, for small and large hierarchies", (mode) => {
    for (const [siblings, children] of [[0, 0], [1, 1], [3, 8], [21, 33], [58, 4]] as const) {
      const all = boxes(layoutHierarchy(hierarchy(many(100, siblings), many(200, children)), mode));
      all.forEach((a, i) => all.slice(i + 1).forEach((b) => expect(overlaps(a, b)).toBe(false)));
    }
  });
  it("root issue has no parent or siblings; leaf has no children group", () => {
    const root = layoutHierarchy(hierarchy([], many(200, 3), false), "cards");
    expect(root.items.some((i) => i.kind === "issue" && i.role === "parent")).toBe(false);
    expect(root.items.some((i) => i.kind === "label" && i.role === "siblings")).toBe(false);
    const leaf = layoutHierarchy(hierarchy(many(100, 3), []), "groups");
    expect(leaf.items.filter((i) => i.kind === "group").map((i) => i.kind === "group" && i.role)).toEqual(["siblings"]);
  });
  it("grows with the hierarchy instead of using fixed positions", () => {
    const small = layoutHierarchy(hierarchy(many(100, 2), many(200, 2)), "cards").bounds;
    const large = layoutHierarchy(hierarchy(many(100, 40), many(200, 40)), "cards").bounds;
    expect(large.w).toBeGreaterThan(small.w);
    expect(large.h).toBeGreaterThan(small.h);
  });
});

describe("related issues (wakes, mentions)", () => {
  it("adds a labelled column of cards to the right of the current issue, without overlap", () => {
    for (const mode of ["groups", "cards"] as const) {
      const layout = layoutHierarchy({ ...hierarchy(many(100, 5), many(200, 5)), relatedIssues: many(300, 4) }, mode);
      const related = layout.items.filter((i) => i.kind === "issue" && i.role === "related");
      const current = layout.items.find((i) => i.kind === "issue" && i.role === "current")!;
      expect(related).toHaveLength(4);
      related.forEach((r) => expect(r.box.x).toBeGreaterThanOrEqual(current.box.x + current.box.w));
      expect(layout.items.find((i) => i.kind === "label" && i.role === "related")).toMatchObject({ total: 4, shown: 4 });
      const all = boxes(layout);
      all.forEach((a, i) => all.slice(i + 1).forEach((b) => expect(overlaps(a, b)).toBe(false)));
    }
  });
});

describe("view math", () => {
  const viewport = { w: 1000, h: 600 };
  it("zoomAt keeps the world point under the anchor fixed", () => {
    const view = { x: 40, y: 20, scale: 1 };
    const anchor = { x: 300, y: 200 };
    const world = { x: (anchor.x - view.x) / view.scale, y: (anchor.y - view.y) / view.scale };
    const next = zoomAt(view, 2, anchor);
    expect(next.scale).toBe(2);
    expect(world.x * next.scale + next.x).toBeCloseTo(anchor.x);
    expect(world.y * next.scale + next.y).toBeCloseTo(anchor.y);
  });
  it("zoomAt is not clamped to a fixed range", () => {
    expect(zoomAt({ x: 0, y: 0, scale: 1 }, 9, { x: 0, y: 0 }).scale).toBe(9);
    expect(zoomAt({ x: 0, y: 0, scale: 1 }, 0.01, { x: 0, y: 0 }).scale).toBe(0.01);
  });
  it("fitView shows all bounds inside the viewport", () => {
    const bounds = { x: 100, y: 50, w: 2000, h: 1500 };
    const view = fitView(bounds, viewport);
    expect(bounds.w * view.scale).toBeLessThanOrEqual(viewport.w);
    expect(bounds.h * view.scale).toBeLessThanOrEqual(viewport.h);
    expect(bounds.x * view.scale + view.x).toBeGreaterThanOrEqual(0);
    expect(bounds.y * view.scale + view.y).toBeGreaterThanOrEqual(0);
  });
  it("centerOn puts the box in the middle of the viewport", () => {
    const box = { x: 500, y: 300, w: 270, h: 100 };
    const view = centerOn(box, viewport, 1);
    expect((box.x + box.w / 2) * view.scale + view.x).toBeCloseTo(500);
    expect((box.y + box.h / 2) * view.scale + view.y).toBeCloseTo(300);
  });
  it("panBy moves the content and intersects flags off-screen boxes", () => {
    const view = panBy({ x: 0, y: 0, scale: 1 }, 100, -50);
    expect(view).toEqual({ x: 100, y: -50, scale: 1 });
    const visible = { x: 0, y: 0, w: viewport.w, h: viewport.h };
    expect(intersects({ x: 10, y: 10, w: 50, h: 50 }, visible)).toBe(true);
    expect(intersects({ x: 5000, y: 10, w: 50, h: 50 }, visible)).toBe(false);
  });
});
