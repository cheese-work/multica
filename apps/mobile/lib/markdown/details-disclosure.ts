import type { DetailsSegment, MarkdownSegment } from "./split-markdown";

/** Body segments to render: none while collapsed, all of them when expanded. */
export function detailsBodyFor(
  segment: DetailsSegment,
  expanded: boolean,
): MarkdownSegment[] | null {
  return expanded ? segment.body : null;
}
