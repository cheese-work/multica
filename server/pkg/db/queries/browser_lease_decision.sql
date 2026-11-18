-- name: GetBrowserLeaseDecision :one
-- A lease decision with its live (not disclaimed) answer and the answerer's workspace role. The
-- answer and role are NULL while the request is open or after the answer was disclaimed.
SELECT request.issue_id, request.asking_agent_id, request.authority, request.status, request.options,
       request.ask_json, request.expires_at, request.withdrawn_at,
       answer.content_snapshot AS answer_text, answer.effective_at AS answer_effective_at,
       answer.chosen_option_index AS answer_chosen_option_index, answer.reply_text AS answer_reply_text,
       answer.answered_by_user_id AS answered_by_user_id, member.role AS answerer_role
FROM decision_request request
LEFT JOIN decision_answer answer ON answer.decision_request_id = request.id AND answer.disclaimed_at IS NULL
LEFT JOIN member ON member.workspace_id = request.workspace_id AND member.user_id = answer.answered_by_user_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
ORDER BY answer.created_at DESC NULLS LAST
LIMIT 1;
