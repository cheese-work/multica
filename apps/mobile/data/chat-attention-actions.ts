import { snoozableKeys } from "@multica/core/chat/attention";
import { api } from "@/data/api";

/** Xong: ack `turn:` keys, resolve `item:` keys. `decision:` keys are dropped
 *  because the server answers 422 for them (T3); a decision-only row sends nothing. */
export async function xongSession(sessionId: string, displayedKeys: readonly string[]) {
  const keys = snoozableKeys(displayedKeys);
  if (keys.length === 0) return null;
  return api.chatAttentionXong(sessionId, keys);
}

/** Để sau: snooze every displayed key (T5). `requestId` makes a retry idempotent. */
export async function laterSession(
  sessionId: string,
  displayedKeys: readonly string[],
  requestId: string,
) {
  if (displayedKeys.length === 0) return null;
  return api.chatAttentionLater(sessionId, [...displayedKeys], { requestId });
}
