type Chip = { color: string; backgroundColor: string; underline: boolean };

/**
 * Solid brand chip for mentions of the viewer: their own `mention://member/<id>`
 * and `@all`. Mirrors web's `.mention-self` (brand fill, background-coloured
 * text). enriched-markdown's link variants cannot set font weight, so the
 * web's bold is not reproduced. Patterns match the full URL; the id is a UUID
 * but is escaped anyway.
 */
export function selfMentionLinkVariants(
  userId: string | null | undefined,
  colors: { brand: string; background: string },
): Record<string, Chip> {
  if (!userId) return {};
  const chip: Chip = { color: colors.background, backgroundColor: colors.brand, underline: false };
  const escapedId = userId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return {
    [`^mention://member/${escapedId}$`]: chip,
    "^mention://all/": chip,
  };
}
