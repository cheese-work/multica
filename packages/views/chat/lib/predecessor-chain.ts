import type { ChatSession } from "@multica/core/types";

/**
 * The DMs a continuation came from, oldest first, found by walking
 * `continued_from_session_id` through the session list. Stops at the first
 * session the list does not hold, and at a loop.
 */
export function predecessorChain(sessions: ChatSession[], fromId: string): ChatSession[] {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const chain: ChatSession[] = [];
  const seen = new Set<string>();
  let current = byId.get(fromId);
  while (current && !seen.has(current.id)) {
    seen.add(current.id);
    chain.unshift(current);
    current = current.continued_from_session_id ? byId.get(current.continued_from_session_id) : undefined;
  }
  return chain;
}
