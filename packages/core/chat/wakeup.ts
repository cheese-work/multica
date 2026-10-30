/**
 * A fired chat wakeup is stored as a user-role message whose content opens
 * with a model-facing header (server/internal/service/chat_wakeup.go). The
 * header is for the agent only and must never reach the screen.
 */
const WAKEUP_HEADER = /^\[Scheduled wakeup — [^\]\n]*\]\n?(?:Instruction: )?/;

export function isWakeupHeaderContent(content: string): boolean {
  return WAKEUP_HEADER.test(content);
}

/** Drops the model-facing header, leaving the instruction text. */
export function stripWakeupHeader(content: string): string {
  return content.replace(WAKEUP_HEADER, "");
}
