export type DecisionState = "pending" | "effective" | "disclaimed";

export interface OpenDecisionRequest {
  id: string;
  issue_id: string;
  issue_identifier: string;
  asking_agent_id: string;
  asking_agent_name: string;
  ask: string;
  escalation_class: string;
  created_at: string;
  chat_session_id: string;
  message_id: string | null;
  source_comment_id?: string | null;
  options?: DecisionRequestOption[];
  /** Per-issue cards a batch card answers for; shown with `covered_cards` before an answer. */
  covers_request_ids?: string[];
  covered_cards?: DecisionCoveredCard[];
  /** The pending executable action behind the card, so a page can find a budget request (CHE-5285). */
  action?: DecisionRequestAction;
}

/** One answer button on an open decision card. */
export interface DecisionRequestOption {
  label: string;
  what_happens?: string;
  pros?: string[];
  cons?: string[];
  cost_or_time?: string;
}

export interface DecisionRequestCard {
  id: string;
  issue_id: string;
  issue_identifier: string;
  /** The source issue's title for the Source row; absent on an older server. */
  issue_title?: string;
  source_comment_id?: string | null;
  ask: string;
  /** The asking agent's name; absent when the agent is gone or the server is older. */
  asking_agent_name?: string;
  /** Answer buttons; empty for a request stored without a structured ask. */
  options?: DecisionRequestOption[];
  /** 0-based into `options`: the option the ask's recommendation names. */
  recommended_option_index?: number;
  state: "open" | "answered" | "withdrawn";
  created_at: string;
  withdrawn_at?: string | null;
  reason?: string;
  /** The issue comment that recorded the answer, set once the request is answered. */
  record_comment_id?: string | null;
  /** Latest answer, kept after a disclaim so the card can say so. */
  answer?: DecisionRequestAnswer;
  /** The pending executable action behind an open request, if any. */
  action?: DecisionRequestAction;
  /** A batch card: the per-issue cards its answer also closes. */
  covers_request_ids?: string[];
  /** What each covered card asks, so the human sees every decision the batch answer closes. */
  covered_cards?: DecisionCoveredCard[];
  /** Set when a batch card's answer closed this card. */
  answered_by_batch_request_id?: string | null;
}

/** One per-issue card a batch card covers, with the decision it asks. */
export interface DecisionCoveredCard {
  id: string;
  issue_id: string;
  issue_identifier: string;
  issue_title?: string;
  decision: string;
  /** The covered card's own options with their meanings. */
  options?: DecisionRequestOption[];
}

/** What an executable action would change, rendered by the server from the values its executor checks. */
export interface DecisionActionPreview {
  fields: { label: string; current: string; proposed: string }[];
  sha256: string;
}

/** One hunk of a config-change diff; every line starts with `+`, `-` or a space. */
export interface DecisionConfigDiffHunk {
  old_start: number;
  new_start: number;
  lines: string[];
}

/** The stored server preview of a config-change action: what the approver is shown and approves against. */
export interface DecisionConfigChange {
  field: string;
  target_id: string;
  keys: string[];
  /** `json`, `text` or `lines`; a newer server may send another kind, which this client cannot approve. */
  diff_kind: "json" | "text" | "lines" | (string & {});
  diff: DecisionConfigDiffHunk[];
  /** The value the change writes. A string for text, an array of lines for lines, JSON otherwise. */
  after: unknown;
  before_sha256: string;
  after_sha256: string;
  meta?: Record<string, unknown>;
  /** Digest of the stored preview; sent back as `preview_sha256` on approval. */
  sha256: string;
}

export interface DecisionRequestAction {
  id: string;
  action_type: string;
  payload_sha256: string;
  /** Action state. Only a pending action carries a preview; an older server omits it. */
  state?: string;
  /** Absent for an unknown action type, a stale window or an older server: the card then offers no approval. */
  preview?: DecisionActionPreview;
  /** Set for a config-change action; absent when the preview is malformed, so the card offers no approval. */
  config_change?: DecisionConfigChange;
  /** Only the workspace owner may approve. Absent on an older server: the client then fails closed. */
  owner_only?: boolean;
  /** Readback of a decided action (B v9 S9). `applied`, `not_applied` or `expired`; absent while pending or on an older server. */
  effect?: "applied" | "not_applied" | "expired" | (string & {});
  effect_reason?: string;
  decided_at?: string;
  decided_by_name?: string;
  on_behalf_of_name?: string;
  /** True while a revoke would be accepted; the card shows Revoke only then. */
  revocable?: boolean;
  revoke?: { at: string; by_name?: string; reason: string };
}

/** The run that resumed the issue after the answer was recorded. */
export interface DecisionRequestRun {
  id: string;
  agent_name: string;
}

/** Receipt half of a request card: recorded, delivered once the asking agent was woken, then picked up once a run started. */
export interface DecisionRequestAnswer {
  state: "recorded" | "delivered" | "picked_up" | "disclaimed";
  /** Set with `picked_up`. */
  run?: DecisionRequestRun;
  /** 0-based into the card's options; absent for a free-text answer. */
  chosen_option_index?: number;
  option_label?: string;
  answered_at: string;
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
  /** True once the asking agent was woken with the answer. */
  delivered?: boolean;
  /** 0-based into the card's options; absent for a free-text answer. */
  chosen_option_index?: number;
  option_label?: string;
  /** The ask card in its own session, so the receipt can jump back to it. */
  request_session_id?: string;
  request_message_id?: string;
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
  decision_delivered?: boolean;
  decision_chosen_option_index?: number;
  decision_option_label?: string;
  decision_issue_comment_id?: string;
  /** Set on a member's own comment that a task recorded as the answer: that comment carries the receipt. */
  decision_receipt_answer_id?: string;
  /** The issue that accepted the answer; a receipt on a comment elsewhere names it. */
  decision_issue_id?: string;
  decision_issue_identifier?: string;
  effective_at?: string | null;
  disclaimed_at?: string | null;
  decision_source_session_id?: string;
  decision_source_message_id?: string;
  /** The DM message that asked, in the request's own session (a continued DM can differ from the answer's). */
  decision_request_session_id?: string;
  decision_request_message_id?: string;
  /** The offered-option label (for example D1) an excerpted record answers. */
  decision_answered_label?: string;
  /** The whole source message, sent only for an excerpted record so the card can disclose it. */
  decision_full_message?: string;
  /** When the record was made; `effective_at` minus this is the disclaim window. */
  decision_answered_at?: string;
  /** When the answer became effective; absent while pending. */
  decision_woken_at?: string | null;
}

/** Reviewer signature carried by an approved file. */
export interface DecisionArtifactSignature {
  reviewer: string;
  comment_id: string;
}

/** One changed region of a text diff. Each line starts with " ", "-" or "+". */
export interface DecisionArtifactHunk {
  old_start: number;
  new_start: number;
  lines: string[];
}

/** `problem` is set when no hunks can be shown. */
export interface DecisionArtifactDiff {
  previous_filename: string;
  hunks: DecisionArtifactHunk[];
  problem?: "not_text" | "too_large" | "unavailable";
}

/** A file the human approves, from `GET /api/decisions/{id}/artifacts`. */
export interface DecisionArtifact {
  attachment_id: string;
  label: string;
  sha256: string;
  supersedes_attachment_id?: string;
  signed_by?: DecisionArtifactSignature;
  /** Signature of the superseded revision; the diff is measured against that file. */
  supersedes_signed_by?: DecisionArtifactSignature;
  /** Absent when the file was deleted after the ask was sent. */
  attachment?: {
    id: string;
    filename: string;
    content_type: string;
    size_bytes: number;
    download_url: string;
    attachment_download_url?: string;
  };
  diff?: DecisionArtifactDiff;
}
