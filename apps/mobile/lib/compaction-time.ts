// ICT (UTC+7, no DST) wall time of an in-place compaction (CHE-2169). Computed
// from the UTC clock rather than Intl `timeZone` so every RN runtime agrees
// and the divider matches web's "Compacted at HH:MM ICT".
const ICT_OFFSET_MS = 7 * 60 * 60 * 1000;

/** "HH:MM" in ICT, or `null` when the timestamp does not parse. */
export function formatCompactionTime(iso: string): string | null {
  const at = new Date(iso).getTime();
  if (Number.isNaN(at)) return null;
  const ict = new Date(at + ICT_OFFSET_MS);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(ict.getUTCHours())}:${pad(ict.getUTCMinutes())}`;
}
