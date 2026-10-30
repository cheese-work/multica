// Chat bubble timestamps in the READER's locale and timezone (CHE-1871).
// `locale` / `timeZone` default to the browser's; the params exist so tests
// can pin them. (useLocale() is the UI language, not the browser locale.)

// Same guard as common/format-in-time-zone: an unparseable timestamp prints
// as itself, and a zone this runtime's ICU lacks falls back to local time.
function format(
  iso: string,
  options: Intl.DateTimeFormatOptions,
  locale?: string,
  timeZone?: string,
): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  try {
    return new Intl.DateTimeFormat(locale, { ...options, timeZone }).format(at);
  } catch {
    return new Intl.DateTimeFormat(locale, options).format(at);
  }
}

export const formatMessageTime = (iso: string, locale?: string, timeZone?: string) =>
  format(iso, { timeStyle: "short" }, locale, timeZone);

export const formatMessageDateTime = (iso: string, locale?: string, timeZone?: string) =>
  format(iso, { dateStyle: "medium", timeStyle: "short" }, locale, timeZone);

/** Calendar day of `iso` in the reader's zone, as sortable `YYYY-MM-DD`. */
export const dayKey = (iso: string, timeZone?: string) =>
  format(iso, { year: "numeric", month: "2-digit", day: "2-digit" }, "en-CA", timeZone);

/** "Today" / "Yesterday" (locale wording) or a locale date for older days. */
export function formatDayLabel(
  iso: string,
  now: Date = new Date(),
  locale?: string,
  timeZone?: string,
): string {
  const key = dayKey(iso, timeZone);
  const daysAgo =
    (Date.parse(dayKey(now.toISOString(), timeZone)) - Date.parse(key)) / 86_400_000;
  if (daysAgo === 0 || daysAgo === 1) {
    const word = new Intl.RelativeTimeFormat(locale, { numeric: "auto" }).format(-daysAgo, "day");
    return word.charAt(0).toLocaleUpperCase(locale) + word.slice(1);
  }
  return format(iso, { dateStyle: "medium" }, locale, timeZone);
}

/**
 * `HH:MM ZONE` (24h) in the reader's zone, e.g. "17:00 ICT". Intl names UTC+7
 * "GMT+7"; the workspace writes it as ICT.
 */
export function formatTimeWithZone(iso: string, locale?: string, timeZone?: string): string {
  const time = format(iso, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }, locale, timeZone);
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  let zone = "";
  try {
    zone = new Intl.DateTimeFormat("en-US", { timeZone, timeZoneName: "short" })
      .formatToParts(at)
      .find((part) => part.type === "timeZoneName")?.value ?? "";
  } catch {
    zone = new Intl.DateTimeFormat("en-US", { timeZoneName: "short" })
      .formatToParts(at)
      .find((part) => part.type === "timeZoneName")?.value ?? "";
  }
  return `${time} ${zone === "GMT+7" ? "ICT" : zone}`.trim();
}

/** `formatTimeWithZone` with the reader-local date in front, e.g. "Oct 20, 2026, 17:00 ICT". */
export function formatDateTimeWithZone(iso: string, locale?: string, timeZone?: string): string {
  if (Number.isNaN(new Date(iso).getTime())) return iso;
  const date = format(iso, { dateStyle: "medium" }, locale, timeZone);
  return `${date}, ${formatTimeWithZone(iso, locale, timeZone)}`;
}
