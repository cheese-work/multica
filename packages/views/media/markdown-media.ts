import { fromMarkdown } from "mdast-util-from-markdown";
import { gfmTable } from "micromark-extension-gfm-table";
import { gfmTableFromMarkdown } from "mdast-util-gfm-table";
import { math } from "micromark-extension-math";
import { mathFromMarkdown } from "mdast-util-math";
import type { Root, RootContent } from "mdast";
import { computeClosedFences } from "../rich-content/streaming-fence";
import { tableMarkdownFromSource } from "../rich-content/rich-table-model";

export function markdownMedia(content: string): Array<{ kind: "mermaid" | "html" | "table"; content: string }> {
  const tree = fromMarkdown(content, {
    extensions: [gfmTable(), math()],
    mdastExtensions: [gfmTableFromMarkdown(), mathFromMarkdown()],
  });
  const fences = computeClosedFences(content);
  const items: ReturnType<typeof markdownMedia> = [];
  const visit = (node: Root | RootContent) => {
    const start = node.position?.start.offset;
    const end = node.position?.end.offset;
    if (node.type === "code" && (node.lang === "mermaid" || node.lang === "html") && start != null && fences.has(start)) {
      items.push({ kind: node.lang, content: node.value });
    } else if (node.type === "table") {
      items.push({ kind: "table", content: tableMarkdownFromSource(content, content, start, end) });
    } else if ("children" in node) {
      for (const child of node.children) visit(child);
    }
  };
  visit(tree);
  return items;
}
