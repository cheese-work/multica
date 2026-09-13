import { expect, test } from "@playwright/test";

interface RowMeasurement {
  readonly totalRows: number;
  readonly hiddenRows: number;
}

test("Range geometry distinguishes exactly twelve and thirteen visual lines", async ({ page }) => {
  for (const lineCount of [12, 13]) {
    await page.setContent(`
      <style>
        #fixture { font: 16px/20px monospace; width: 480px; }
        #fixture span { display: block; height: 20px; margin: 0; padding: 0; }
      </style>
      <div id="fixture">${Array.from({ length: lineCount }, (_, index) => `<span>line ${index + 1}</span>`).join("")}</div>
    `);

    const result = await page.locator("#fixture").evaluate((root): RowMeasurement & { oracleRows: number } => {
      const epsilon = 0.5;
      const rootTop = root.getBoundingClientRect().top;
      const lineHeight = Number.parseFloat(getComputedStyle(root).lineHeight);
      const cutoff = rootTop + lineHeight * 12;
      const intervals: Array<{ top: number; bottom: number }> = [];

      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (!node.textContent?.trim()) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        for (const rect of range.getClientRects()) {
          if (rect.width > 0 && rect.height > 0) intervals.push({ top: rect.top, bottom: rect.bottom });
        }
      }
      intervals.sort((a, b) => a.top - b.top || a.bottom - b.bottom);
      const rows: Array<{ top: number; bottom: number }> = [];
      for (const interval of intervals) {
        const previous = rows.at(-1);
        if (previous && interval.top < previous.bottom - epsilon) previous.bottom = Math.max(previous.bottom, interval.bottom);
        else rows.push({ ...interval });
      }

      // Independent, test-only oracle: one Range per non-whitespace character.
      const glyphRows: Array<{ top: number; bottom: number }> = [];
      const characterWalker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = characterWalker.nextNode(); node; node = characterWalker.nextNode()) {
        for (let offset = 0; offset < node.textContent!.length; offset++) {
          if (/\s/.test(node.textContent![offset]!)) continue;
          const range = document.createRange();
          range.setStart(node, offset);
          range.setEnd(node, offset + 1);
          for (const rect of range.getClientRects()) {
            if (rect.width > 0 && rect.height > 0) glyphRows.push({ top: rect.top, bottom: rect.bottom });
          }
        }
      }
      glyphRows.sort((a, b) => a.top - b.top || a.bottom - b.bottom);
      const oracle: Array<{ top: number; bottom: number }> = [];
      for (const interval of glyphRows) {
        const previous = oracle.at(-1);
        if (previous && interval.top < previous.bottom - epsilon) previous.bottom = Math.max(previous.bottom, interval.bottom);
        else oracle.push({ ...interval });
      }

      return {
        totalRows: rows.length,
        hiddenRows: rows.filter((row) => row.bottom > cutoff + epsilon).length,
        oracleRows: oracle.length,
      };
    });

    expect(result.totalRows).toBe(lineCount);
    expect(result.oracleRows).toBe(lineCount);
    expect(result.hiddenRows).toBe(lineCount === 12 ? 0 : 1);
    expect(result.hiddenRows > 0).toBe(lineCount === 13);
    if (lineCount === 13) expect(2).not.toBe(result.hiddenRows);
  }
});
