import type { MarkdownStyle } from "react-native-enriched-markdown";

// sRGB of --mention-self-bg/-fg in packages/ui/styles/tokens.css (the native
// renderer can't read CSS vars). Kept out of lib/theme.ts so it stays testable
// without loading expo-router.
export const SELF_MENTION_COLORS = {
  light: { bg: "#ffe787", fg: "#282110" },
  dark: { bg: "#eab532", fg: "#161107" },
};

// `@all` counts as a mention of every viewer, like web's `.mention-self`.
// Link variants can't set font weight, so bold the label in the markdown.
const SELF_LABEL = String.raw`@[^\]\n*_\\]*`;

function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

export function emphasizeSelfMentions(markdown: string, userId: string | undefined) {
  if (!userId) return markdown;
  const self = new RegExp(
    String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(userId)}|all/[^)\s]*))\)`,
    "g",
  );
  return markdown.replace(self, "[**$1**]($2)");
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
