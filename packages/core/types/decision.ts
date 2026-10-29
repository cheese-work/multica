export type DecisionState = "pending" | "effective" | "disclaimed";

/** One answer button on an open decision card. */
export interface DecisionRequestOption {
  label: string;
  what_happens?: string;
}

export interface DecisionRequestCard {
  id: string;
  issue_id: string;
  issue_identifier: string;
  source_comment_id?: string | null;
  ask: string;
  options?: DecisionRequestOption[];
  state: "open" | "answered" | "withdrawn";
  created_at: string;
  withdrawn_at?: string | null;
  reason?: string;
  /** The issue comment that recorded the answer, set once the request is answered. */
  record_comment_id?: string | null;
}

export type DecisionCommentKind = "ask" | "quote" | "disclaim" | "delivery" | "cancel" | "veto";

/** Display state of one decision answer, carried by the DM bubble it quotes. */
export interface DecisionCard {
  answer_id: string;
  issue_id: string;
  issue_identifier: string;
  issue_comment_id: string;
  class: string;
  state: DecisionState;
  effective_at: string;
  disclaimed_at?: string | null;
  /** Shown on a forked copy: read-only provenance, no Disclaim. */
  inherited: boolean;
}

/**
 * Card fields of a decision-stamped issue comment. The `decision_source_*`
 * pointers are sent only to the request's human: a DM is private, so only its
 * owner can jump to the quoted message.
 */
export interface DecisionCommentFields {
  decision_intent_id?: string | null;
  decision_request_id?: string | null;
  decision_answer_id?: string | null;
  decision_comment_kind?: DecisionCommentKind | null;
  decision_human_user_id?: string;
  decision_recorder_agent_id?: string;
  decision_class?: string;
  decision_state?: DecisionState;
  decision_issue_comment_id?: string;
  effective_at?: string | null;
  disclaimed_at?: string | null;
  decision_source_session_id?: string;
  decision_source_message_id?: string;
  /** The DM message that asked, in the request's own session (a continued DM can differ from the answer's). */
  decision_request_session_id?: string;
  decision_request_message_id?: string;
}
