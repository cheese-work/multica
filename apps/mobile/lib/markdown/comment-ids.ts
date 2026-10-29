/**
 * Comment-ID chips for the native prose renderer.
 *
 * Prose is rendered natively, so a chip is a markdown link whose label carries
 * the preview (`CHE-12 · Luna: first line`). A bare comment UUID in prose is
 * rewritten only after the server resolved it (`GET /api/comments/{id}/preview`);
 * an unreadable or unknown id, and a short UUID prefix, stays plain text.
 *
 * Mirrors web's `preprocessCommentIds` (packages/ui/markdown/comment-ids.ts):
 * code spans, markdown links and bare URLs are skipped.
 */
import type { CommentPreview } from "@multica/core/types";

const UUID = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";

// Alternation order matters: spans we must not touch win over the UUID group.
const SCAN_RE = new RegExp(
  `(\`[^\`\\n]*\`)|(\\[[^\\]]*\\]\\([^)]*\\))|(https?://\\S+)|(?<![A-Za-z0-9_-])(${UUID})(?![A-Za-z0-9_-])`,
  "g",
);
const MENTION_RE = new RegExp(`^mention://comment/(${UUID})\\?issue=([^&#]+)$`);

export function findCommentIds(text: string): string[] {
  const ids = new Set<string>();
  for (const m of text.matchAll(SCAN_RE)) if (m[4]) ids.add(m[4]);
  return [...ids];
}

const escapeLabel = (s: string) => s.replace(/([\\[\]])/g, "\\$1");

export function linkifyCommentIds(
  text: string,
  resolved: ReadonlyMap<string, CommentPreview>,
): string {
  return text.replace(SCAN_RE, (whole, _code, _link, _url, id?: string) => {
    const p = id ? resolved.get(id) : undefined;
    if (!p) return whole;
    const who = p.author_name ? `${p.author_name}: ` : "";
    const label = escapeLabel(`${p.issue_identifier} · ${who}${p.preview}`.trim());
    return `[${label}](mention://comment/${p.comment_id}?issue=${p.issue_id})`;
  });
}

export function parseCommentMention(
  url: string,
): { commentId: string; issueId: string } | null {
  const m = MENTION_RE.exec(url);
  return m ? { commentId: m[1]!, issueId: m[2]! } : null;
}
