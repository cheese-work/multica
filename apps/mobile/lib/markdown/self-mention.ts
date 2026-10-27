import type { MarkdownStyle } from "react-native-enriched-markdown";
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

// Fenced blocks (``` or ~~~, optionally quoted) and inline code spans.
// Code must stay verbatim: it is displayed and copied as written.
// (`\x60` is a backtick: the pieces share group numbers, so they stay strings.)
const CODE = new RegExp(
  [
    String.raw`^[ \t>]*(\x60{3,}|~{3,})[^\n]*\n[\s\S]*?(?:\n[ \t>]*\1[\x60~]*[ \t]*(?=\n|$)|(?![\s\S]))`,
    String.raw`(\x60+)(?!\x60)(?:(?!\n[ \t]*\n)[\s\S])*?(?<!\x60)\2(?!\x60)`,
  ].join("|"),
  "gm",
);

function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  const self = new RegExp(
    String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
    "g",
  );
  const bold = (text: string) => text.replace(self, "[**$1**]($2)");
  let out = "";
  let last = 0;
  for (const code of markdown.matchAll(CODE)) {
    out += bold(markdown.slice(last, code.index)) + code[0];
    last = code.index + code[0].length;
  }
  return out + bold(markdown.slice(last));
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
