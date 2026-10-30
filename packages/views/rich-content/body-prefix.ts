import { fromMarkdown } from "mdast-util-from-markdown";
import { gfmTableFromMarkdown } from "mdast-util-gfm-table";
import { mathFromMarkdown } from "mdast-util-math";
import { gfmTable } from "micromark-extension-gfm-table";
import { math } from "micromark-extension-math";

/**
 * The longest run of top-level Markdown blocks that ends within `budget`
 * chars. A block is never split: fences, lists and quotes are one block each,
 * whatever blank lines they hold. Same block syntax as RichContent's parse.
 * Only a window of 2x budget is parsed: a block that reaches the window end
 * is cut off and over budget anyway, and the window leaves a full budget of
 * lookahead past the last block that can fit, so later lines decide it as usual.
 */
export function bodyPrefix(content: string, budget: number): string {
  const { children } = fromMarkdown(content.slice(0, budget * 2), {
    extensions: [gfmTable(), math()],
    mdastExtensions: [gfmTableFromMarkdown(), mathFromMarkdown()],
  });
  let cut = 0;
  for (const block of children) {
    const end = block.position?.end.offset;
    if (end == null || end > budget) break;
    cut = end;
  }
  return content.slice(0, cut).trimEnd();
}
