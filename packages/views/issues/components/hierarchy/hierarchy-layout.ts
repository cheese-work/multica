import type { GroupSummary, HierarchyIssue } from "./hierarchy-model";

export interface Box { x: number; y: number; w: number; h: number }
export interface View { x: number; y: number; scale: number }
export type Mode = "groups" | "cards";
export type LayoutItem =
  | { kind: "issue"; role: "parent" | "current" | "sibling" | "child" | "related"; issue: HierarchyIssue; box: Box }
  | { kind: "group"; role: "siblings" | "children"; summary: GroupSummary; box: Box }
  | { kind: "label"; role: "siblings" | "children" | "related"; total: number; shown: number; box: Box };
export interface Layout { items: LayoutItem[]; edges: [number, number][][]; bounds: Box }
export interface Hierarchy {
  parent: HierarchyIssue | null;
  current: HierarchyIssue;
  siblings: GroupSummary;
  children: GroupSummary;
  siblingIssues: HierarchyIssue[];
  childIssues: HierarchyIssue[];
  /** Wake / mention neighbours, shown only while their toggle is on. */
  relatedIssues: HierarchyIssue[];
}

export const ZOOM_MIN = 0.2;
export const ZOOM_MAX = 2;
/** At and above this zoom a group splits into single issue cards. */
export const ZOOM_SPLIT = 0.7;
const MAX_CARDS = 60;
const CARD = { w: 200, h: 84 };
const CURRENT = { w: 270, h: 104 };
const GROUP = { w: 360, h: 210 };
const GAP = 22;
const ROW_GAP = 96;
const LABEL_H = 22;

export const modeFor = (scale: number): Mode => (scale >= ZOOM_SPLIT ? "cards" : "groups");

const columnsFor = (count: number) => Math.min(4, Math.max(1, Math.ceil(Math.sqrt(count))));
const center = (box: Box) => box.x + box.w / 2;

export function layoutHierarchy(h: Hierarchy, mode: Mode): Layout {
  const items: LayoutItem[] = [];
  const edges: [number, number][][] = [];

  // Group body: a totals block, or a grid of cards sized from the issue count.
  const place = (role: "siblings" | "children", summary: GroupSummary, issues: HierarchyIssue[], x: number, y: number): Box => {
    if (summary.total === 0) return { x, y, w: 0, h: 0 };
    if (mode === "groups") {
      const box = { x, y, w: GROUP.w, h: GROUP.h };
      items.push({ kind: "group", role, summary, box });
      return box;
    }
    const shown = issues.slice(0, MAX_CARDS);
    const columns = columnsFor(shown.length);
    const rows = Math.ceil(shown.length / columns);
    const box = { x, y: y + LABEL_H, w: columns * (CARD.w + GAP) - GAP, h: rows * (CARD.h + GAP) - GAP };
    items.push({ kind: "label", role, total: summary.total, shown: shown.length, box: { x, y, w: box.w, h: LABEL_H } });
    shown.forEach((issue, index) => items.push({
      kind: "issue", role: role === "siblings" ? "sibling" : "child", issue,
      box: { x: x + (index % columns) * (CARD.w + GAP), y: box.y + Math.floor(index / columns) * (CARD.h + GAP), w: CARD.w, h: CARD.h },
    }));
    return { ...box, y, h: box.h + LABEL_H };
  };

  const parentH = h.parent ? CARD.h + ROW_GAP : 0;
  const hasSiblings = h.siblings.total > 0;
  const siblingsWidth = hasSiblings ? (mode === "groups" ? GROUP.w : columnsFor(Math.min(h.siblings.total, MAX_CARDS)) * (CARD.w + GAP) - GAP) : 0;
  const currentX = hasSiblings ? siblingsWidth + GAP * 4 : 0;
  const rowY = parentH;

  const currentBox = { x: currentX, y: rowY, w: CURRENT.w, h: CURRENT.h };
  items.push({ kind: "issue", role: "current", issue: h.current, box: currentBox });
  const siblingsBox = place("siblings", h.siblings, h.siblingIssues, 0, rowY);

  if (h.parent) {
    const parentBox = { x: currentX + CURRENT.w / 2 - CARD.w * 0.75, y: 0, w: CARD.w * 1.5, h: CARD.h };
    items.push({ kind: "issue", role: "parent", issue: h.parent, box: parentBox });
    const busY = parentBox.y + parentBox.h + ROW_GAP / 2;
    edges.push([[center(parentBox), parentBox.y + parentBox.h], [center(parentBox), rowY]]);
    if (hasSiblings) edges.push([[center(parentBox), busY], [center(siblingsBox), busY], [center(siblingsBox), rowY]]);
  }

  const childrenY = rowY + Math.max(CURRENT.h, siblingsBox.h) + ROW_GAP;
  const childrenWidth = h.children.total === 0 ? 0 : mode === "groups" ? GROUP.w : columnsFor(Math.min(h.children.total, MAX_CARDS)) * (CARD.w + GAP) - GAP;
  const childrenBox = place("children", h.children, h.childIssues, center(currentBox) - childrenWidth / 2, childrenY);
  if (h.children.total > 0) edges.push([[center(currentBox), rowY + CURRENT.h], [center(currentBox), childrenBox.y]]);

  if (h.relatedIssues.length > 0) {
    const shown = h.relatedIssues.slice(0, MAX_CARDS);
    const x = Math.max(currentBox.x + CURRENT.w, childrenBox.x + childrenBox.w) + GAP * 4;
    items.push({ kind: "label", role: "related", total: h.relatedIssues.length, shown: shown.length, box: { x, y: rowY, w: CARD.w, h: LABEL_H } });
    shown.forEach((issue, index) => items.push({ kind: "issue", role: "related", issue, box: { x, y: rowY + LABEL_H + index * (CARD.h + GAP), w: CARD.w, h: CARD.h } }));
  }

  const boxes = items.map((item) => item.box);
  const left = Math.min(...boxes.map((b) => b.x));
  const top = Math.min(...boxes.map((b) => b.y));
  const right = Math.max(...boxes.map((b) => b.x + b.w));
  const bottom = Math.max(...boxes.map((b) => b.y + b.h));
  return { items, edges, bounds: { x: left, y: top, w: right - left, h: bottom - top } };
}

const clampScale = (scale: number) => Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, scale));

/** Zoom to `scale`, keeping the world point under `anchor` (screen px) fixed. */
export function zoomAt(view: View, scale: number, anchor: { x: number; y: number }): View {
  const next = clampScale(scale);
  const ratio = next / view.scale;
  return { scale: next, x: anchor.x - (anchor.x - view.x) * ratio, y: anchor.y - (anchor.y - view.y) * ratio };
}

export function centerOn(box: Box, viewport: { w: number; h: number }, scale: number): View {
  return { scale, x: viewport.w / 2 - center(box) * scale, y: viewport.h / 2 - (box.y + box.h / 2) * scale };
}

export function fitView(bounds: Box, viewport: { w: number; h: number }, pad = 32): View {
  const scale = clampScale(Math.min((viewport.w - pad * 2) / bounds.w, (viewport.h - pad * 2) / bounds.h, 1));
  return centerOn(bounds, viewport, scale);
}

export const panBy = (view: View, dx: number, dy: number): View => ({ ...view, x: view.x + dx, y: view.y + dy });

export const intersects = (a: Box, b: Box) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
