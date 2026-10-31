/**
 * Comment-ID chips for the native prose renderer.
 *
 * Prose is rendered natively, so a chip is a markdown link whose label carries
 * the preview (`CHE-12 · Luna: first line`). A bare comment UUID, or an in-app
 * issue link ending in `#comment-<uuid>`, is rewritten only after the server
 * resolved it (`GET /api/comments/{id}/preview`); an unreadable or unknown id,
 * and a short UUID prefix, stays as the author wrote it.
 *
 * Mirrors web's `preprocessCommentIds` + `parseWorkspaceCommentLink`: code
 * spans and other links are skipped, and an anchor URL counts only when it
 * points at this app and the current workspace. Fenced code is never touched:
 * callers use the `*Segments` helpers, which rewrite prose segments only.
 */
import type { CommentPreview } from "@multica/core/types";
import type { MarkdownSegment } from "./split-markdown";

export interface CommentLinkContext {
  /** Web app origin (`EXPO_PUBLIC_WEB_URL`); anchor URLs only match this origin. */
  webOrigin: string | null;
  /** Current workspace slug; anchor URLs of another workspace are ignored. */
  slug: string | null;
}

const UUID = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";

// Alternation order matters: spans we must not touch win over the UUID group.
// Groups: 1 code, 2 labeled-link URL, 3 bare URL, 4 bare UUID.
const SCAN_RE = new RegExp(
  `(\`[^\`\\n]*\`)|\\[[^\\]]*\\]\\(([^)]*)\\)|(https?://[^\\s<>()]*[^\\s<>().,;:!?])|(?<![A-Za-z0-9_-])(${UUID})(?![A-Za-z0-9_-])`,
  "g",
);
const MENTION_RE = new RegExp(`^mention://comment/(${UUID})\\?issue=([^&#]+)$`);
const ANCHOR_PATH_RE = new RegExp(`^/([^/?#]+)/issues/[^/?#]+#comment-(${UUID})$`);

function commentIdFromUrl(url: string, ctx: CommentLinkContext): string | null {
  if (!ctx.webOrigin || !ctx.slug) return null;
  const origin = ctx.webOrigin.replace(/\/+$/, "");
  if (!url.startsWith(`${origin}/`)) return null;
  const m = ANCHOR_PATH_RE.exec(url.slice(origin.length));
  return m && m[1] === ctx.slug ? m[2]! : null;
}

/** The comment id a scan match stands for, or null when it must be left alone. */
function matchedId(
  m: { url?: string; bare?: string; uuid?: string },
  ctx: CommentLinkContext,
): string | null {
  const url = m.url ?? m.bare;
  return url !== undefined ? commentIdFromUrl(url, ctx) : (m.uuid ?? null);
}

export function findCommentIds(text: string, ctx: CommentLinkContext): string[] {
  const ids = new Set<string>();
  for (const m of text.matchAll(SCAN_RE)) {
    const id = matchedId({ url: m[2], bare: m[3], uuid: m[4] }, ctx);
    if (id) ids.add(id);
  }
  return [...ids];
}

const escapeLabel = (s: string) => s.replace(/([\\[\]])/g, "\\$1");

export function linkifyCommentIds(
  text: string,
  resolved: ReadonlyMap<string, CommentPreview>,
  ctx: CommentLinkContext,
): string {
  return text.replace(
    SCAN_RE,
    (whole, _code, url?: string, bare?: string, uuid?: string) => {
      const id = matchedId({ url, bare, uuid }, ctx);
      const p = id ? resolved.get(id) : undefined;
      if (!p) return whole;
      const who = p.author_name ? `${p.author_name}: ` : "";
      const label = escapeLabel(`${p.issue_identifier} · ${who}${p.preview}`.trim());
      return `[${label}](mention://comment/${p.comment_id}?issue=${p.issue_id})`;
    },
  );
}

export function findSegmentCommentIds(
  segments: readonly MarkdownSegment[],
  ctx: CommentLinkContext,
): string[] {
  const ids = new Set<string>();
  for (const seg of segments) {
    if (seg.type === "prose") for (const id of findCommentIds(seg.content, ctx)) ids.add(id);
  }
  return [...ids];
}

export function linkifySegments(
  segments: MarkdownSegment[],
  resolved: ReadonlyMap<string, CommentPreview>,
  ctx: CommentLinkContext,
): MarkdownSegment[] {
  if (resolved.size === 0) return segments;
  return segments.map((seg) =>
    seg.type === "prose"
      ? { ...seg, content: linkifyCommentIds(seg.content, resolved, ctx) }
      : seg,
  );
}

export function parseCommentMention(
  url: string,
): { commentId: string; issueId: string } | null {
  const m = MENTION_RE.exec(url);
  return m ? { commentId: m[1]!, issueId: m[2]! } : null;
}
