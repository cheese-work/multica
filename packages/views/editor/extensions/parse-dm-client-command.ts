/** DM commands the surface answers itself; none takes an argument. */
type DmClientCommandName = "statistic" | "archive" | "pin" | "unarchive";

/**
 * Classifies composer text as one of the argument-less DM commands: the whole
 * text, trimmed, is exactly `/statistic`, `/archive`, `/pin` or `/unarchive`.
 * Anything else (`/archive now`, `Please /archive`) is an ordinary message.
 */
export function parseDmClientCommand(text: string): DmClientCommandName | null {
  const match = /^\/(statistic|archive|pin|unarchive)$/.exec(text.trim());
  return match ? (match[1] as DmClientCommandName) : null;
}
