import type { DecisionCommentFields } from "../types";

const DECISION_FIELD_KEYS = [
  "decision_intent_id",
  "decision_request_id",
  "decision_answer_id",
  "decision_comment_kind",
  "decision_human_user_id",
  "decision_recorder_agent_id",
  "decision_class",
  "decision_state",
  "decision_delivered",
  "decision_chosen_option_index",
  "decision_option_label",
  "decision_receipt_answer_id",
  "decision_issue_id",
  "decision_issue_identifier",
  "decision_issue_comment_id",
  "effective_at",
  "disclaimed_at",
  "decision_source_session_id",
  "decision_source_message_id",
  "decision_request_session_id",
  "decision_request_message_id",
  "decision_answered_label",
  "decision_full_message",
  "decision_answered_at",
  "decision_woken_at",
] as const satisfies readonly (keyof DecisionCommentFields)[];

/** Copies the decision card fields a comment carries, so a comment-to-timeline mapper keeps them. */
export function pickDecisionFields(source: DecisionCommentFields): DecisionCommentFields {
  const picked: Record<string, unknown> = {};
  for (const key of DECISION_FIELD_KEYS) {
    if (source[key] !== undefined) picked[key] = source[key];
  }
  return picked;
}

/**
 * A comment snapshot (resolve, unresolve, edit) is broadcast, so it can omit
 * card fields and never carries the owner-only DM pointers. Keep what the
 * cached entry already holds; the snapshot's own values win.
 */
export function keepDecisionFields<T extends DecisionCommentFields>(next: T, prev: DecisionCommentFields): T {
  return { ...next, ...pickDecisionFields(prev), ...pickDecisionFields(next) };
}
