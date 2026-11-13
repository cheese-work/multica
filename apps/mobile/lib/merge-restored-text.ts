/**
 * Text to put back in the input after a failed send. `latest` is what the
 * input holds now: anything typed while the send was pending must survive, so
 * the failed text goes first and the newer text follows it.
 */
export function mergeRestoredText(snapshot: string, latest: string): string {
  if (!latest || latest === snapshot) return snapshot;
  if (!snapshot) return latest;
  return `${snapshot}\n${latest}`;
}
