/**
 * Self-mention highlight for the mobile markdown renderer (CHE-2115).
 *
 * Web styles the viewer's own mentions (and `@all`, which counts as a mention
 * of everyone) with a bold, filled, rounded chip. enriched-markdown cannot
 * inject React per leaf, so mobile gets the same signal from two native hooks:
 *   - `linkVariants` (per-URL-pattern link style) → filled background, no
 *     underline. The library has no corner radius for link backgrounds, so
 *     the fill is square — a known divergence from web.
 *   - bold emphasis inside the link label → `font-weight: 700` equivalent;
 *     variants cannot set weight.
 */

const SELF_LABEL = String.raw`@[^\]\n*_\\]*`;

function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

export function emphasizeSelfMentions(markdown: string, viewerId: string | null) {
  if (!viewerId) return markdown;
  const self = new RegExp(
    String.raw`\[(${SELF_LABEL})\]\((mention://(?:member/${escapeRegex(viewerId)}|all/[^)\s]*))\)`,
    "g",
  );
  return markdown.replace(self, "[**$1**]($2)");
}

export function selfMentionLinkVariants(
  viewerId: string | null,
  colors: { color: string; backgroundColor: string },
): Record<string, { color: string; backgroundColor: string; underline: boolean }> {
  if (!viewerId) return {};
  const style = { ...colors, underline: false };
  return {
    [`^mention://member/${escapeRegex(viewerId)}$`]: style,
    "^mention://all/": style,
  };
}
