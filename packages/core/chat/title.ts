const TITLE_MAX = 80;
// Word boundary only when it keeps >= 2/3 of the limit; else a long token (URL) is dropped whole.
const MIN_WORD_CUT = Math.floor((TITLE_MAX * 2) / 3);

// Deterministic DM title from the first message: first non-empty line, markdown
// stripped, whitespace collapsed, at most 80 code points, cut at a word boundary
// when that keeps at least 2/3 of the limit (else a hard cut). No ellipsis is stored; the UI truncates at the container edge.
// Server twin: chattitle.Derive (server/internal/chattitle/title.go).
export function deriveChatTitle(content: string): string {
  const firstLine = (content.split("\n").find((l) => l.trim()) ?? content).trim();
  const cleaned = firstLine
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/[#*`>~_]/g, "")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1") // markdown links/images → their text
    .replace(/\s+/g, " ")
    .trim();
  const cps = Array.from(cleaned); // code points, not UTF-16 units
  if (cps.length <= TITLE_MAX) return cleaned;
  let cut = cps.slice(0, TITLE_MAX);
  if (cps[TITLE_MAX] !== " ") {
    const i = cut.lastIndexOf(" ");
    if (i >= MIN_WORD_CUT) cut = cut.slice(0, i);
  }
  return cut.join("").trimEnd();
}
