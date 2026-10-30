import { parseChatCommandInput } from "@multica/core/chat/commands";

/** DM commands the surface answers itself; none takes an argument. */
export type DmClientCommandName = "statistic" | "archive" | "pin" | "unarchive";

/**
 * Classifies composer text as one of the argument-less DM commands: the whole
 * text, trimmed and case-insensitively, is `/statistic`, `/archive`, `/pin`
 * or `/unarchive`.
 */
export function parseDmClientCommand(text: string): DmClientCommandName | null {
  const parsed = parseChatCommandInput(text);
  if (parsed && parsed.status === "ok" && parsed.command.args === "none") {
    return parsed.command.id as DmClientCommandName;
  }
  return null;
}

/**
 * Parses an argument-less DM command attempt, including invalid extra text so
 * the composer can reject extra arguments with an inline hint while preserving
 * the draft.
 */
export function parseDmClientCommandAttempt(
  text: string,
): { command: DmClientCommandName; extraArgs: string } | null {
  const parsed = parseChatCommandInput(text);
  if (parsed && parsed.command.args === "none") {
    return {
      command: parsed.command.id as DmClientCommandName,
      extraArgs: parsed.status === "invalid" ? parsed.rawArgs : "",
    };
  }
  return null;
}
