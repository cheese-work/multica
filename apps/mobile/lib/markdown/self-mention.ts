import type { MarkdownStyle } from "react-native-enriched-markdown";
import { marked } from "marked";
import type { MarkdownSegment } from "./split-markdown";

// sRGB of --mention-self-bg/-fg in packages/ui/styles/tokens.css (the native
// renderer can't read CSS vars). Kept out of lib/theme.ts so it stays testable
// without loading expo-router.
export const SELF_MENTION_COLORS = {
  light: { bg: "#ffe787", fg: "#282110" },
  dark: { bg: "#eab532", fg: "#161107" },
};

// `@all` counts as a mention of every viewer, like web's `.mention-self`.
// Link variants can't set font weight, so bold the label in the markdown.
// Labels may hold underscores and backslash escapes (`@john_doe`, `@David\[TF\]`);
// a bare `*`, `[`, `]` or backtick would break the wrapping, so those stay unbolded.
const SELF_LABEL = /@(?:\\.|[^\]\[\n\\*`])*/.source;

function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// Links whose first child is bold: what `emphasizeSelfMentions` produces.
function countBoldLinks(markdown: string) {
  let count = 0;
  marked.walkTokens(marked.lexer(markdown), (token) => {
    if (token.type === "link" && token.tokens?.[0]?.type === "strong") count++;
  });
  return count;
}

// Only real links are bolded. Code (inline, fenced, indented, nested in lists
// or quotes) is displayed and copied as written, and escaped or image look-alikes
// aren't links, but regexes can't tell them apart. So try each match on its own
// and keep it only if the markdown lexer then sees one more bold link.
export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  const self = new RegExp(
    String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
    "g",
  );
  const matches = [...markdown.matchAll(self)];
  if (!matches.length) return markdown;
  const before = countBoldLinks(markdown);
  const boldOf = (m: RegExpMatchArray) => `[**${m[1]}**](${m[2]})`;
  const splice = (source: string, m: RegExpMatchArray) =>
    source.slice(0, m.index) + boldOf(m) + source.slice(m.index! + m[0].length);
  // Back to front so earlier offsets stay valid.
  return matches
    .filter((m) => countBoldLinks(splice(markdown, m)) === before + 1)
    .reduceRight(splice, markdown);
}

// Top-level code blocks are their own segments: leave them untouched.
export function emphasizeSelfMentionSegments(segments: MarkdownSegment[], userId: string | undefined) {
  return segments.map((segment) =>
    segment.type === "prose"
      ? { ...segment, content: emphasizeSelfMentions(segment.content, userId) }
      : segment,
  );
}

export function selfMentionLinkVariants(
  userId: string | undefined,
  colors: { bg: string; fg: string },
): MarkdownStyle["linkVariants"] {
  if (!userId) return undefined;
  // Regex strings tested against the full URL. Ids are UUIDs, but escape anyway.
  const style = { color: colors.fg, backgroundColor: colors.bg, underline: false };
  return {
    [`^mention://member/${escapeRegex(userId)}$`]: style,
    "^mention://all/": style,
  };
}
