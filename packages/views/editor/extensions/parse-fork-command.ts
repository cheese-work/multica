/**
 * Classifies composer text as the `/fork` DM command: the first word of the
 * first non-blank line is exactly `/fork`. The rest of the input, trimmed, is
 * the optional first message for the new DM ("" = none).
 */
export function parseForkCommand(text: string): { message: string } | null {
  const trimmed = text.trimStart();
  if (!/^\/fork(?:\s|$)/.test(trimmed)) return null;
  return { message: trimmed.slice("/fork".length).trim() };
}
