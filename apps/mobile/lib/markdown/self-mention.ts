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

// Only real link tokens are bolded. Code (inline, fenced, indented, nested in
// lists or quotes) is displayed and copied as written, and regexes can't tell
// it apart, so ask the markdown lexer which matches are links.
export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  const self = () =>
    new RegExp(
      String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
      "g",
    );
  // One entry per match in source order: is it a link token, or text/code?
  const isLink: boolean[] = [];
  marked.walkTokens(marked.lexer(markdown), (token) => {
    const raw = (token as { raw?: string }).raw ?? "";
    if (token.type === "link") {
      for (const m of raw.matchAll(self())) isLink.push(m[0] === raw);
    } else if (!("tokens" in token) && !("items" in token) && token.type !== "table") {
      isLink.push(...Array.from(raw.matchAll(self()), () => false));
    }
  });
  // Lexer and source disagree (unexpected syntax): leave the text alone.
  if (isLink.length !== [...markdown.matchAll(self())].length) return markdown;
  let i = 0;
  return markdown.replace(self(), (all, label, url) => (isLink[i++] ? `[**${label}**](${url})` : all));
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
