import { filterChatSessions } from "@multica/core/chat/queries";
import type { ChatSession } from "@multica/core/types";

export type ChatScopeView = "all" | "main" | "decisions" | "waiting" | "unseen" | "pinned" | "archived";

/** Scope chips share one type (OR); agent chips are another (AND with scope). */
export type ChatChip = { type: "scope"; view: ChatScopeView } | { type: "agent"; id: string; name: string };

export interface ChipCandidate {
  chip: ChatChip;
  label: string;
}

export const SCOPE_VIEWS: readonly ChatScopeView[] = ["all", "main", "decisions", "waiting", "unseen", "pinned", "archived"];

export const chipKey = (chip: ChatChip) => (chip.type === "scope" ? `scope:${chip.view}` : `agent:${chip.id}`);

const isAll = (chip: ChatChip) => chip.type === "scope" && chip.view === "all";

/** Tất cả is exclusive: it clears every other chip, and any other chip clears it. */
export function toggleChip(chips: readonly ChatChip[], chip: ChatChip): ChatChip[] {
  const key = chipKey(chip);
  if (chips.some((c) => chipKey(c) === key)) return chips.filter((c) => chipKey(c) !== key);
  return isAll(chip) ? [chip] : [...chips.filter((c) => !isAll(c)), chip];
}

/** Same type = OR, different type = AND. Row predicates come from the DM list (`filterChatSessions`). */
export function matchesChips(
  chips: readonly ChatChip[],
  session: ChatSession,
  decisionSessionIds: ReadonlySet<string>,
): boolean {
  const views = chips.flatMap((c) => (c.type === "scope" && c.view !== "all" ? [c.view] : []));
  const agents = chips.flatMap((c) => (c.type === "agent" ? [c.id] : []));
  const scopeOk = views.length === 0 || views.some((v) => filterChatSessions([session], v, decisionSessionIds).length > 0);
  return scopeOk && (agents.length === 0 || agents.includes(session.agent_id));
}

/** The chip Tab adds: the first inactive one whose label starts with the typed text. Empty text skips Tất cả. */
export function chipForTab(
  text: string,
  candidates: readonly ChipCandidate[],
  active: readonly ChatChip[],
): ChatChip | null {
  const typed = text.trim().toLowerCase();
  const on = new Set(active.map(chipKey));
  const found = candidates.find(
    ({ chip, label }) =>
      !on.has(chipKey(chip)) && (typed ? label.toLowerCase().startsWith(typed) : !isAll(chip)),
  );
  return found?.chip ?? null;
}
