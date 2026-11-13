import { toast } from "sonner";
import { invalidAgentMentionRanges, invalidAgentMentions } from "@multica/core/issues/invalid-agent-mentions";
import type { useT } from "../i18n";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

// Composer submit results carry this instead of `false` so the composer can
// keep the draft AND flag the refused chips.
export interface InvalidMentionRejection {
  invalidMentionIds: string[];
}

// The server refused the comment (422 invalid_agent_mentions) and saved
// nothing. Toasts a localized sentence naming the mention typed in `content`
// (the wire carries only offsets, by design) and returns the ids to flag.
// Returns undefined for any other error so callers fall through to their own toast.
export function rejectInvalidAgentMentions(
  err: unknown,
  content: string,
  t: IssuesT,
): InvalidMentionRejection | undefined {
  const ranges = invalidAgentMentionRanges(err);
  if (!ranges) return undefined;
  const mentions = invalidAgentMentions(content, ranges);
  const only = mentions.length === 1 ? mentions[0]!.label : undefined;
  toast.error(
    only
      ? t(($) => $.comment.invalid_agent_mention_named, { name: only })
      : t(($) => $.comment.invalid_agent_mentions, { count: Math.max(ranges.length, 1) }),
  );
  return { invalidMentionIds: mentions.map((m) => m.id) };
}
