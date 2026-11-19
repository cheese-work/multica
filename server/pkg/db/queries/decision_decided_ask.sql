-- name: ListDecidedAskCandidates :many
-- The human-authority decision and action requests a new ask may repeat
-- (CHE-5682): same workspace, same recipient, on the new ask's issue or one of
-- its ancestors, outside the proposal path. Each row carries its newest answer
-- and, for an action request, the stored action. The caller keys and judges.
SELECT request.id, request.issue_id, request.status, request.kind, request.escalation_class,
       request.ask_json, request.origin, request.options,
       answer.id AS answer_id, answer.answer_source, answer.chosen_option_index, answer.answered_label,
       answer.answered_by_user_id, answer.effective_at AS answer_effective_at,
       answer.disclaimed_at AS answer_disclaimed_at, answer.created_at AS answer_created_at,
       request.answered_by_batch_request_id,
       action.id AS action_id, action.action_type, action.action_version, action.params AS action_params,
       action.secret_slots AS action_secret_slots, action.state AS action_state, action.revoked_at AS action_revoked_at
FROM decision_request request
LEFT JOIN LATERAL (
    SELECT * FROM decision_answer a WHERE a.decision_request_id = request.id
    ORDER BY a.created_at DESC, a.id DESC LIMIT 1
) answer ON TRUE
LEFT JOIN decision_action action ON action.decision_request_id = request.id
WHERE request.workspace_id = sqlc.arg('workspace_id')
  AND request.authority = 'human'
  AND request.human_user_id = sqlc.arg('human_user_id')
  AND request.issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
  AND request.proposal_fingerprint IS NULL;
