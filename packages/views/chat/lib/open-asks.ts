import type { ChatMessage } from "@multica/core/types";

export interface OpenAsk {
  messageId: string;
  ask: string;
}

const MAX_OPEN_ASKS = 5;

/** Longer lines are prose, not a question to repeat; skipping them bounds the work per message. */
const MAX_QUESTION_SCAN = 1_000;

/** Question lines (outside code fences) of a message, bullet markers stripped. */
export function questionLines(content: string): string[] {
  // ponytail: a line ending in "?" is a question; no sentence splitting.
  // Linear scan only: no regex over unbounded assistant text (ReDoS).
  const asks: string[] = [];
  let fenced = false;
  for (const raw of content.split("\n")) {
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

/**
 * Open asks of the last assistant message before `messages[index]`: its open
 * decision cards, then its question lines. Cards stay open until answered, so
 * they repeat whatever a member said since; a member reply after the message
 * answers its plain questions.
 */
export function openAsksBefore(messages: readonly ChatMessage[], index: number): OpenAsk[] {
  let replied = false;
  for (let i = index - 1; i >= 0; i--) {
    const m = messages[i]!;
    if (m.role === "user") {
      replied = true;
      continue;
    }
    if (m.role !== "assistant" || m.message_kind === "compact_notice" || m.message_kind === "compaction_summary") continue;
    const cards = m.decision_requests ?? (m.decision_request ? [m.decision_request] : []);
    const asks = cards.filter((card) => card.state === "open").map((card) => ({ messageId: m.id, ask: card.ask }));
    if (!replied) {
      for (const line of questionLines(m.content)) {
        if (!asks.some((a) => a.ask === line)) asks.push({ messageId: m.id, ask: line });
      }
    }
    return asks.slice(0, MAX_OPEN_ASKS);
  }
  return [];
}
