import {
  detectLinks,
  findCodeRanges,
  findMarkdownLinkRanges,
  isInsideCode,
  rangesOverlap,
} from './linkify'

/**
 * Autolink bare comment UUIDs for markdown preprocessing.
 *
 * Rewrites a full comment UUID into `[<uuid>](mention://comment/<uuid>)` so the
 * readonly renderer can resolve it to a chip. Only a FULL UUID matches: a short
 * prefix (`01a1769e-060e`) is ambiguous and stays plain text.
 *
 * Like `preprocessIssueIdentifiers` this is pure detection. Any UUID in prose
 * is a candidate; whether it is a comment the viewer can read is decided at
 * render time, and a miss falls back to the plain UUID text.
 */
const COMMENT_ID_RE =
  /(?<![A-Za-z0-9_-])([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?![A-Za-z0-9_-])/g

export const COMMENT_ID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

export function preprocessCommentIds(text: string): string {
  if (!/[0-9a-f]{8}-[0-9a-f]{4}-/.test(text)) return text

  const codeRanges = findCodeRanges(text)
  const linkRanges = findMarkdownLinkRanges(text)
  const detectedLinks = detectLinks(text)

  COMMENT_ID_RE.lastIndex = 0
  let result = ''
  let lastIndex = 0
  let match: RegExpExecArray | null

  while ((match = COMMENT_ID_RE.exec(text)) !== null) {
    const id = match[1]
    if (!id) continue
    const start = match.index
    const range = { start, end: start + id.length }

    if (isInsideCode(start, codeRanges)) continue
    if (linkRanges.some((r) => rangesOverlap(range, r))) continue
    if (detectedLinks.some((l) => rangesOverlap(range, l))) continue

    result += text.slice(lastIndex, start)
    result += `[${id}](mention://comment/${id})`
    lastIndex = range.end
  }

  if (lastIndex === 0) return text
  return result + text.slice(lastIndex)
}
