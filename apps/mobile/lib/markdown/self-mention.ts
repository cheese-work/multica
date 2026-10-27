import type { MarkdownStyle } from "react-native-enriched-markdown";

// sRGB of --mention-self-bg/-fg in packages/ui/styles/tokens.css (the native
// renderer can't read CSS vars). Kept out of lib/theme.ts so it stays testable
// without loading expo-router.
export const SELF_MENTION_COLORS = {
  light: { bg: "#ffe787", fg: "#282110" },
  dark: { bg: "#eab532", fg: "#161107" },
};

export function selfMentionLinkVariants(
  userId: string | undefined,
  colors: { bg: string; fg: string },
): MarkdownStyle["linkVariants"] {
  if (!userId) return undefined;
  // Regex string tested against the full URL. Ids are UUIDs, but escape anyway.
  const id = userId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return {
    [`^mention://member/${id}$`]: { color: colors.fg, backgroundColor: colors.bg, underline: false },
  };
}
