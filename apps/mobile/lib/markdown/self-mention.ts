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

// A marker (`#sm`, plus a few characters when the source already holds it) that
// the source never contains, so only our own text can carry it. Each "#sm" in the
// source rules out one value per length, so a value of ceil(log36(n)) characters is
// always free. Plain string search: no pattern grows with the source.
function freeMarker(markdown: string) {
  const base = "#sm";
  if (!markdown.includes(base)) return base;
  for (let length = 1; ; length++) {
    const taken = new Set<string>();
    for (let i = markdown.indexOf(base); i >= 0; i = markdown.indexOf(base, i + 1)) {
      taken.add(markdown.slice(i + base.length, i + base.length + length));
    }
    for (let k = 0; k < 36 ** length; k++) {
      const suffix = k.toString(36).padStart(length, "0");
      if (!taken.has(suffix)) return base + suffix;
    }
  }
}

// Only real links are bolded. Code (inline, fenced, indented, nested in lists
// or quotes) is displayed and copied as written, and escaped or image look-alikes
// aren't links, but regexes can't tell them apart. So bold every match in a
// scratch copy, tagging each URL with its match number (`<marker><n>`), and lex
// that copy once. A match is a real link when a link token comes back with exactly
// the tagged text; the rest stay as written. Any failure leaves the text unbolded.
export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  try {
    const self = new RegExp(
      String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
      "g",
    );
    const matches = [...markdown.matchAll(self)];
    if (!matches.length) return markdown;
    const bold = (m: RegExpMatchArray, suffix = "") => `[**${m[1]}**](${m[2]}${suffix})`;
    const marker = freeMarker(markdown);
    const tagged = matches.map((m, n) => bold(m, `${marker}${n}`));
    let scratch = "";
    let end = 0;
    matches.forEach((m, n) => {
      scratch += markdown.slice(end, m.index) + tagged[n];
      end = m.index! + m[0].length;
    });
    scratch += markdown.slice(end);
    const indexOf = new Map(tagged.map((raw, n) => [raw, n]));
    const real = new Set<number>();
    marked.walkTokens(marked.lexer(scratch), (token) => {
      const n = token.type === "link" ? indexOf.get(token.raw) : undefined;
      if (n !== undefined) real.add(n);
    });
    // Back to front so earlier offsets stay valid.
    return matches.reduceRight(
      (out, m, n) => (real.has(n) ? out.slice(0, m.index) + bold(m) + out.slice(m.index! + m[0].length) : out),
      markdown,
    );
  } catch {
    return markdown;
  }
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
