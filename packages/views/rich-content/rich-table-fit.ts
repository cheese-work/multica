/**
 * Automatic column fitting for the table frame. Measuring lifts the table's own
 * sizing twice and reads layout each time, which is the costly part of mounting
 * a table, so the result is cached per table and skipped for tall tables.
 */

// The editor keeps its header row in <tbody>, so the first row stands for the
// columns in both renderers.
export function headerCells(table: HTMLTableElement): HTMLTableCellElement[] {
  return Array.from(table.rows[0]?.cells ?? []);
}

// Each column's min-content and max-content width, read with the table's own
// sizing lifted and restored within the same task, so the browser paints only
// the restored layout. Only the <table> and its <col>s are touched: in the
// editor, a style written to a cell would reach ProseMirror as an edit.
export function measureColumns(table: HTMLTableElement): { min: number[]; max: number[] } {
  const { style } = table;
  const saved = { layout: style.tableLayout, width: style.width, minWidth: style.minWidth };
  const fixed = table.hasAttribute("data-column-widths");
  const cols = Array.from(table.querySelectorAll<HTMLElement>("colgroup > col"));
  const colWidths = cols.map((col) => col.style.width);
  const read = () => headerCells(table).map((cell) => Math.ceil(cell.getBoundingClientRect().width));

  if (fixed) table.removeAttribute("data-column-widths");
  for (const col of cols) col.style.width = "";
  style.tableLayout = "auto";
  style.minWidth = "0";
  style.width = "max-content";
  const max = read();
  style.width = "min-content";
  const min = read();

  style.tableLayout = saved.layout;
  style.width = saved.width;
  style.minWidth = saved.minWidth;
  cols.forEach((col, i) => {
    col.style.width = colWidths[i] ?? "";
  });
  if (fixed) table.setAttribute("data-column-widths", "");
  return { min, max };
}

// The browser's automatic layout reads well while the columns' minimums fit.
// Once they do not, it shrinks every column to its minimum, which leaves a
// prose column a few characters wide beside a column of unbreakable paths.
// Past that point each column gets its content width, capped so one long cell
// cannot claim the table, and the table scrolls.
const PREFERRED_MAX_COLUMN_WIDTH = 320;

/**
 * Rows past which fitting is skipped: both measuring passes lay out every row,
 * so the cost grows with the table, and a table this tall scrolls as a list
 * rather than being read across its columns.
 */
export const MAX_AUTO_FIT_ROWS = 50;

export function overflowWidths(table: HTMLTableElement, available: number): number[] | null {
  if (table.rows.length > MAX_AUTO_FIT_ROWS) return null;
  const { min, max } = measureColumns(table);
  if (min.length === 0 || sum(min) <= available) return null;
  return min.map((least, i) => Math.max(least, Math.min(max[i] ?? least, PREFERRED_MAX_COLUMN_WIDTH)));
}

function sum(widths: number[]): number {
  return widths.reduce((total, width) => total + width, 0);
}

const FIT_CACHE_LIMIT = 200;
// Keyed by the table's content and the width it was fitted for. A remount
// (Virtuoso scrolling a row back in) finds its fit here instead of measuring.
// Insertion order doubles as recency; the oldest entry goes first.
const fitCache = new Map<string, number[] | null>();

/**
 * `overflowWidths`, remembered per table content and available width.
 * `remeasure` skips the lookup and replaces the entry, for when the content
 * widths themselves changed (a web font replaced its fallback).
 */
export function cachedOverflowWidths(
  key: string,
  table: HTMLTableElement,
  available: number,
  remeasure = false,
): number[] | null {
  const cacheKey = `${key}@${available}`;
  const hit = remeasure ? undefined : fitCache.get(cacheKey);
  if (hit !== undefined) {
    fitCache.delete(cacheKey);
    fitCache.set(cacheKey, hit);
    return hit;
  }
  const fitted = overflowWidths(table, available);
  fitCache.delete(cacheKey);
  fitCache.set(cacheKey, fitted);
  if (fitCache.size > FIT_CACHE_LIMIT) fitCache.delete(fitCache.keys().next().value!);
  return fitted;
}
