import type { ChatMessage } from "@multica/core/types";

export interface OpenAsk {
  messageId: string;
  ask: string;
}

const MAX_OPEN_ASKS = 5;

/** Longer lines are prose, not a question to repeat; skipping them bounds the work per message. */
const MAX_QUESTION_SCAN = 1_000;

/** Questions examined per message, and how many member messages / characters of each count as replies. */
const MAX_QUESTIONS = 20;
const MAX_REPLIES = 50;
const MAX_REPLY_CHARS = 20_000;

/** Question lines (outside code fences) of a message, bullet markers stripped; stops after `limit`. */
export function questionLines(content: string, limit = Infinity): string[] {
  // ponytail: a line ending in "?" is a question; no sentence splitting.
  // Linear scan only: no regex over unbounded assistant text (ReDoS).
  const asks: string[] = [];
  let fenced = false;
  for (const raw of content.split("\n")) {
    if (asks.length >= limit) break;
    const line = raw.trim();
    if (line.startsWith("```")) {
      fenced = !fenced;
      continue;
    }
    if (fenced || line.length > MAX_QUESTION_SCAN) continue;
    let end = line.length;
    while (end > 0 && (line[end - 1] === "*" || line[end - 1] === "_")) end--;
    const text = line.slice(0, end).replace(/^(?:[-*>]|\d{1,3}[.)])\s+/, "");
    if (!text.endsWith("?")) continue;
    asks.push(text.length > 200 ? `${text.slice(0, 199)}…` : text);
  }
  return asks;
}

/** Lowercase, without emphasis/code markers or a truncation ellipsis, so a quote matches the stored line. */
const normalize = (text: string) => text.replace(/[*_`…]/g, "").toLowerCase();

/**
 * Open asks of the last assistant message before `messages[index]`: its open
 * decision cards, then its question lines. Cards stay open until answered, so
 * they repeat whatever a member said since. A plain question counts as answered
 * only when a later member message quotes it; an unrelated message answers nothing.
 */
export function openAsksBefore(messages: readonly ChatMessage[], index: number): OpenAsk[] {
  const replies: string[] = [];
  for (let i = index - 1; i >= 0; i--) {
    const m = messages[i]!;
    if (m.role === "user") {
      if (replies.length < MAX_REPLIES) replies.push(normalize(m.content.slice(0, MAX_REPLY_CHARS)));
      continue;
    }
    if (m.role !== "assistant" || m.message_kind === "compact_notice" || m.message_kind === "compaction_summary") continue;
    const cards = m.decision_requests ?? (m.decision_request ? [m.decision_request] : []);
    const asks = cards.filter((card) => card.state === "open").map((card) => ({ messageId: m.id, ask: card.ask }));
    const seen = new Set(asks.map((a) => a.ask));
    for (const line of questionLines(m.content, MAX_QUESTIONS)) {
      if (asks.length >= MAX_OPEN_ASKS) break;
      if (seen.has(line)) continue;
      seen.add(line);
      const key = normalize(line);
      if (!replies.some((reply) => reply.includes(key))) asks.push({ messageId: m.id, ask: line });
    }
    return asks.slice(0, MAX_OPEN_ASKS);
  }
  return [];
}
