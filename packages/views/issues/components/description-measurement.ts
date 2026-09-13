export const DESCRIPTION_PREVIEW_LINES = 12;
const ROW_OVERLAP_EPSILON = 0.5;

export interface DescriptionMeasurement {
  readonly totalRows: number;
  readonly hiddenRows: number;
  readonly hasOverflow: boolean;
  readonly lineHeight: number;
  readonly previewText: string;
}

interface VerticalInterval {
  top: number;
  bottom: number;
}

interface TextFragment {
  readonly node: Text;
  readonly rects: readonly DOMRect[];
  readonly block: Element | null;
}

function rectsForRange(range: Range): DOMRect[] {
  return Array.from(range.getClientRects()).filter((rect) => rect.width > 0 && rect.height > 0);
}

function isIgnored(node: Text, root: HTMLElement): boolean {
  const parent = node.parentElement;
  if (!parent) return true;
  for (let element: Element | null = parent; element && element !== root; element = element.parentElement) {
    if (
      element.hasAttribute("hidden") ||
      element.matches("script, style, [data-description-measure-ignore]") ||
      getComputedStyle(element).display === "none"
    ) {
      return true;
    }
  }
  return false;
}

function nearestBlock(node: Text, root: HTMLElement): Element | null {
  for (let element = node.parentElement; element && element !== root; element = element.parentElement) {
    const display = getComputedStyle(element).display;
    if (display === "block" || display === "list-item" || display === "table-cell") return element;
  }
  return null;
}

function collectTextFragments(root: HTMLElement): TextFragment[] {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const fragments: TextFragment[] = [];
  let node = walker.nextNode();
  while (node) {
    const text = node as Text;
    if (text.data.trim().length > 0 && !isIgnored(text, root)) {
      const range = document.createRange();
      range.selectNodeContents(text);
      const rects = rectsForRange(range);
      if (rects.length > 0) fragments.push({ node: text, rects, block: nearestBlock(text, root) });
    }
    node = walker.nextNode();
  }
  return fragments;
}

function mergeRows(intervals: VerticalInterval[]): VerticalInterval[] {
  const sorted = intervals.sort((a, b) => a.top - b.top || a.bottom - b.bottom);
  const rows: VerticalInterval[] = [];
  for (const interval of sorted) {
    const previous = rows.at(-1);
    // A row can have separated table cells and mixed-font fragments. Overlap
    // is transitive, but touching rows must remain distinct.
    if (previous && interval.top < previous.bottom - ROW_OVERLAP_EPSILON) {
      previous.bottom = Math.max(previous.bottom, interval.bottom);
    } else {
      rows.push({ ...interval });
    }
  }
  return rows;
}

function visibleOffset(node: Text, cutoff: number): number {
  let low = 0;
  let high = node.data.length;
  while (low < high) {
    const middle = Math.ceil((low + high) / 2);
    const range = document.createRange();
    range.setStart(node, 0);
    range.setEnd(node, middle);
    const fits = rectsForRange(range).every((rect) => rect.bottom <= cutoff + ROW_OVERLAP_EPSILON);
    if (fits) low = middle;
    else high = middle - 1;
  }
  return low;
}

function visibleText(fragments: readonly TextFragment[], cutoff: number): string {
  const pieces: string[] = [];
  let previousBlock: Element | null | undefined;
  for (const fragment of fragments) {
    const end = visibleOffset(fragment.node, cutoff);
    if (end === 0) continue;
    if (pieces.length > 0 && previousBlock !== fragment.block) pieces.push(" ");
    pieces.push(fragment.node.data.slice(0, end));
    previousBlock = fragment.block;
  }
  return pieces.join("").replace(/\s+/g, " ").trim();
}

function visibleImageAlt(root: HTMLElement, cutoff: number): string[] {
  return Array.from(root.querySelectorAll("img"))
    .filter((image) => {
      const rect = image.getBoundingClientRect();
      return rect.width > 0 && rect.height > 0 && rect.bottom <= cutoff + ROW_OVERLAP_EPSILON;
    })
    .map((image) => image.alt.trim())
    .filter(Boolean);
}

/**
 * Measure rendered rows from browser Range rectangles. This intentionally does
 * not interpret Markdown source or infer rows from scroll height.
 */
export function measureDescription(root: HTMLElement): DescriptionMeasurement {
  const rootRect = root.getBoundingClientRect();
  const computed = getComputedStyle(root);
  const parsedLineHeight = Number.parseFloat(computed.lineHeight);
  const lineHeight = Number.isFinite(parsedLineHeight)
    ? parsedLineHeight
    : Number.parseFloat(computed.fontSize) * 1.2;
  const cutoff = rootRect.top + lineHeight * DESCRIPTION_PREVIEW_LINES;
  const fragments = collectTextFragments(root);
  const rows = mergeRows(
    fragments.flatMap((fragment) =>
      fragment.rects.map((rect) => ({ top: rect.top, bottom: rect.bottom })),
    ),
  );
  const textOverflow = rows.some((row) => row.bottom > cutoff + ROW_OVERLAP_EPSILON);
  const nonTextOverflow = Array.from(root.querySelectorAll("img, video, canvas, svg"))
    .map((element) => element.getBoundingClientRect())
    .some((rect) => rect.width > 0 && rect.height > 0 && rect.bottom > cutoff + ROW_OVERLAP_EPSILON);
  const prefix = visibleText(fragments, cutoff);
  const imageAlt = visibleImageAlt(root, cutoff);

  return {
    totalRows: rows.length,
    hiddenRows: rows.filter((row) => row.bottom > cutoff + ROW_OVERLAP_EPSILON).length,
    hasOverflow: textOverflow || nonTextOverflow,
    lineHeight,
    previewText: [prefix, ...imageAlt].filter(Boolean).join(" "),
  };
}
