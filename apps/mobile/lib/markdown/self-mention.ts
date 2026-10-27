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

// Only real links are bolded. Code (inline, fenced, indented, nested in lists
// or quotes) is displayed and copied as written, and escaped or image look-alikes
// aren't links, but regexes can't tell them apart. So bold every match in a
// scratch copy, tagging each URL with its match number (`<tag><n>`), and lex
// that copy once. The tag is absent from the source, so only our own text can
// carry it. A match is a real link when a link token comes back with exactly the
// tagged text; the rest stay as written.
export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  const self = new RegExp(
    String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
    "g",
  );
  const matches = [...markdown.matchAll(self)];
  if (!matches.length) return markdown;
  const bold = (m: RegExpMatchArray, suffix = "") => `[**${m[1]}**](${m[2]}${suffix})`;
  let tag = "#sm";
  while (markdown.includes(tag)) tag += "x";
  const tagged = matches.map((m, n) => bold(m, `${tag}${n}`));
  let scratch = "";
  let end = 0;
  matches.forEach((m, n) => {
    scratch += markdown.slice(end, m.index) + tagged[n];
    end = m.index! + m[0].length;
  });
  scratch += markdown.slice(end);
  const tagIndex = new RegExp(`${tag}(\\d+)\\)$`);
  const real = new Set<number>();
  marked.walkTokens(marked.lexer(scratch), (token) => {
    if (token.type !== "link") return;
    const n = Number(tagIndex.exec(token.raw)?.[1]);
    if (tagged[n] === token.raw) real.add(n);
  });
  // Back to front so earlier offsets stay valid.
  return matches.reduceRight(
    (out, m, n) => (real.has(n) ? out.slice(0, m.index) + bold(m) + out.slice(m.index! + m[0].length) : out),
    markdown,
  );
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
