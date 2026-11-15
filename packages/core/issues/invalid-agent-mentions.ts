import { ApiError } from "../api/client";
import { parseMentions, type ParsedMention } from "./comment-trigger-outcomes";

export interface ByteRange {
  start: number;
  end: number;
}

// The server refuses a comment create/edit whose explicit agent mention cannot
// be invoked (422 `invalid_agent_mentions`) and saves nothing. The body carries
// only byte offsets into the content we sent, never a name or id. Returns the
// ranges for that error, [] when the code matches but no usable range came
// back, and undefined for anything else.
export function invalidAgentMentionRanges(err: unknown): ByteRange[] | undefined {
  if (!(err instanceof ApiError) || err.status !== 422) return undefined;
  const body = err.body as { code?: unknown; invalid_mentions?: unknown } | undefined;
  if (body?.code !== "invalid_agent_mentions") return undefined;
  const raw = Array.isArray(body.invalid_mentions) ? body.invalid_mentions : [];
  return raw.filter(
    (r): r is ByteRange =>
      !!r && Number.isInteger(r.start) && Number.isInteger(r.end) && r.start >= 0 && r.end > r.start,
  );
}

// The refused mentions (label + id), parsed from the caller's own content by
// UTF-8 byte offset. A range that is out of bounds or not a mention is skipped,
// so callers fall back to a count.
export function invalidAgentMentions(content: string, ranges: ByteRange[]): ParsedMention[] {
  const bytes = new TextEncoder().encode(content);
  const decoder = new TextDecoder();
  return ranges.flatMap(({ start, end }) => {
    if (end > bytes.length) return [];
    const mention = parseMentions(decoder.decode(bytes.slice(start, end)))[0];
    return mention ? [mention] : [];
  });
}
