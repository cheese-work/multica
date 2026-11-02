SET LOCAL lock_timeout = '2s';

UPDATE decision_answer answer
SET answer_source = 'agent_record', answered_by_user_id = NULL,
    source_chat_session_id = request.chat_session_id,
    chat_message_ids = ARRAY(SELECT message.id FROM chat_message message
                             WHERE message.decision_answer_id = answer.id
                               AND message.role = 'user'
                               AND message.sent_via = 'interactive_jwt'
                               AND message.task_id IS NULL)
FROM decision_request request
WHERE request.id = '6094a9bb-0a90-4812-8dff-4d4d842d501b'
  AND answer.decision_request_id = request.id
  AND answer.answer_source = 'card'
  AND answer.woken_at IS NULL
  AND answer.disclaimed_at IS NULL
  AND answer.answered_by_task_id IS NULL
  AND answer.source_comment_id IS NULL
  AND answer.source_chat_session_id IS NULL
  AND cardinality(answer.chat_message_ids) = 0
  AND EXISTS (
      SELECT 1 FROM chat_message message
      WHERE message.decision_answer_id = answer.id
        AND message.chat_session_id = request.chat_session_id
        AND message.role = 'user'
        AND message.sent_via = 'interactive_jwt'
      AND message.task_id IS NULL
  );

UPDATE chat_message message
SET decision_answer_id = NULL
FROM decision_answer answer
WHERE answer.decision_request_id = '6094a9bb-0a90-4812-8dff-4d4d842d501b'
  AND answer.answer_source = 'agent_record'
  AND answer.woken_at IS NULL
  AND message.decision_answer_id = answer.id
  AND message.id = ANY(answer.chat_message_ids)
  AND message.role = 'user'
  AND message.sent_via = 'interactive_jwt'
  AND message.task_id IS NULL;
