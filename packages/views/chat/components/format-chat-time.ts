// IM-style timestamp: today → clock, this year → M/D, else full date.
export function formatChatTime(dateStr: string, locale: string): string {
  const d = new Date(dateStr);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit" });
  }
  if (d.getFullYear() === now.getFullYear()) {
    return d.toLocaleDateString(locale, { month: "numeric", day: "numeric" });
  }
  return d.toLocaleDateString(locale);
}

// Clock time only. A row sits under a day header, so it never repeats the date.
export function formatChatClock(date: string | number, locale: string): string {
  return new Date(date).toLocaleTimeString(locale, { hour: "numeric", minute: "2-digit" });
}
