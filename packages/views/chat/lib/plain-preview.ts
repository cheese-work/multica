const NAMED_ENTITIES: Record<string, string> = { nbsp: " ", amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };

/** One-line plain text for a collapsed preview of Markdown the reader never meant to see raw. */
export function plainPreview(content: string): string {
  return content
    .replace(/&(?:#(\d+)|#x([0-9a-f]+)|(\w+));/gi, (entity, dec, hex, name) => {
      if (dec || hex) return String.fromCodePoint(parseInt(dec ?? hex, dec ? 10 : 16));
      return NAMED_ENTITIES[name.toLowerCase()] ?? entity;
    })
    .replace(/^[ \t]{0,3}(?:>[ \t]?)+/gm, "")
    .replace(/^[ \t]{0,3}#{1,6}[ \t]+/gm, "")
    .replace(/^[ \t]*[-*+][ \t]+/gm, "")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[*`~]+/g, "")
    .replace(/\s+/g, " ")
    .trim();
}
