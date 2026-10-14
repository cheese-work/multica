import type { ChatSession } from "@multica/core/types";

/** `/fork` is offered only in a saved, active web DM — never a draft or a channel-sourced one. */
export function canForkSession(session: ChatSession | null | undefined): boolean {
  return !!session?.id && session.status === "active" && !session.channel_source;
}
