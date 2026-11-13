import type { Box } from "../canvas/canvas-math";
import type { GroupSummary, HierarchyIssue } from "./hierarchy-model";

/** One card at the focus depth; `children` summarises the level below it. */
export interface FocusNode { issue: HierarchyIssue; children: GroupSummary }
export interface FocusInput {
  /** Ancestors from the root down to the parent of the focus level. Empty at depth 0. */
  chain: HierarchyIssue[];
  /** Every loaded card at the focus depth. */
  level: FocusNode[];
}
export type FocusItem =
  | { kind: "chain"; issue: HierarchyIssue; box: Box }
  | { kind: "fold"; hidden: HierarchyIssue[]; box: Box }
  | { kind: "card"; issue: HierarchyIssue; leaf: boolean; box: Box }
  | { kind: "count"; parentId: string; summary: GroupSummary; box: Box }
  | { kind: "more"; hidden: number; box: Box };
export interface FocusLayout { items: FocusItem[]; edges: [number, number][][]; bounds: Box }

/** Cards per depth; count cards ride along with their card and are not counted. */
export const FOCUS_CAP = 60;
/** Chains longer than this fold their middle into one "… N" card. */
export const CHAIN_LIMIT = 4;
const CHAIN = { w: 150, h: 48 };
const CHAIN_GAP = 36;
const CARD = { w: 200, h: 84 };
const COUNT = { w: 200, h: 44 };
const COUNT_GAP = 28;
const GAP = 22;
const ROW_GAP = 96;
const MAX_COLUMNS = 6;

const columnsFor = (count: number) => Math.min(MAX_COLUMNS, Math.max(1, Math.ceil(Math.sqrt(count))));
const CHAIN_BOX: Box = { x: 0, y: 0, w: 0, h: 0 };
const mid = (box: Box) => box.x + box.w / 2;

export function layoutFocus({ chain, level }: FocusInput): FocusLayout {
  const items: FocusItem[] = [];
  const edges: [number, number][][] = [];

  // Ancestor chain: one row, root first. Long chains keep the root and the last two.
  const folded = chain.length > CHAIN_LIMIT;
  const shown: FocusItem[] = folded
    ? [
        { kind: "chain", issue: chain[0]!, box: CHAIN_BOX },
        { kind: "fold", hidden: chain.slice(1, -2), box: CHAIN_BOX },
        ...chain.slice(-2).map((issue): FocusItem => ({ kind: "chain", issue, box: CHAIN_BOX })),
      ]
    : chain.map((issue): FocusItem => ({ kind: "chain", issue, box: CHAIN_BOX }));
  shown.forEach((item, index) => items.push({ ...item, box: { ...CHAIN, x: index * (CHAIN.w + CHAIN_GAP), y: 0 } }));
  const last = items.at(-1);
  const chainBottom = chain.length > 0 ? CHAIN.h : 0;
  const top = chain.length > 0 ? chainBottom + ROW_GAP : 0;

  // Focus level: a grid of cards, each non-leaf card with its count card below.
  const cards = level.slice(0, FOCUS_CAP);
  const columns = columnsFor(cards.length);
  const trunkX = -GAP * 2;
  const rowBusYs: number[] = [];
  let rowY = top;
  let lastRowY = top;
  for (let start = 0; start < cards.length; start += columns) {
    const row = cards.slice(start, start + columns);
    rowBusYs.push(rowY - ROW_GAP / 2);
    lastRowY = rowY;
    row.forEach((node, index) => {
      const box = { x: index * (CARD.w + GAP), y: rowY, w: CARD.w, h: CARD.h };
      const leaf = node.children.total === 0;
      items.push({ kind: "card", issue: node.issue, leaf, box });
      if (chain.length > 0) edges.push([[mid(box), rowY - ROW_GAP / 2], [mid(box), rowY]]);
      if (!leaf) {
        const countBox = { x: box.x, y: rowY + CARD.h + COUNT_GAP, w: COUNT.w, h: COUNT.h };
        items.push({ kind: "count", parentId: node.issue.id, summary: node.children, box: countBox });
        edges.push([[mid(box), rowY + CARD.h], [mid(countBox), countBox.y]]);
      }
    });
    const rowHeight = CARD.h + (row.some((n) => n.children.total > 0) ? COUNT_GAP + COUNT.h : 0);
    if (chain.length > 0) edges.push([[trunkX, rowY - ROW_GAP / 2], [(row.length - 1) * (CARD.w + GAP) + CARD.w / 2, rowY - ROW_GAP / 2]]);
    rowY += rowHeight + ROW_GAP;
  }
  if (level.length > FOCUS_CAP) {
    // Next free cell: the rest of the last row, or a new row when it is full.
    const tileIndex = cards.length % columns;
    const tileY = tileIndex === 0 ? rowY : lastRowY;
    const x = tileIndex * (CARD.w + GAP);
    items.push({ kind: "more", hidden: level.length - FOCUS_CAP, box: { x, y: tileY, w: CARD.w, h: CARD.h } });
  }

  // Fan: down from the last chain card, along the left gutter, into each row.
  if (chain.length > 0 && last && rowBusYs.length > 0) {
    const fanY = rowBusYs[0]!;
    edges.push([[mid(last.box), chainBottom], [mid(last.box), fanY], [trunkX, fanY]]);
    if (rowBusYs.length > 1) edges.push([[trunkX, fanY], [trunkX, rowBusYs.at(-1)!]]);
  }

  const points = [...items.flatMap((i) => [[i.box.x, i.box.y], [i.box.x + i.box.w, i.box.y + i.box.h]]), ...edges.flat()] as [number, number][];
  if (points.length === 0) return { items, edges, bounds: { x: 0, y: 0, w: 0, h: 0 } };
  const left = Math.min(...points.map((p) => p[0]));
  const topMost = Math.min(...points.map((p) => p[1]));
  return { items, edges, bounds: { x: left, y: topMost, w: Math.max(...points.map((p) => p[0])) - left, h: Math.max(...points.map((p) => p[1])) - topMost } };
}
