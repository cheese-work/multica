SET LOCAL lock_timeout = '2s';

DO $$
DECLARE
    request_id uuid := '6094a9bb-0a90-4812-8dff-4d4d842d501b';
    request_found boolean;
BEGIN
    SELECT EXISTS (SELECT 1 FROM decision_request WHERE id = request_id) INTO request_found;
    IF NOT request_found THEN
        RETURN;
    END IF;

    UPDATE chat_message message
    SET decision_answer_id = answer.id
    FROM decision_answer answer
    JOIN decision_request request ON request.id = answer.decision_request_id
    JOIN chat_session session ON session.id = request.chat_session_id
    WHERE request.id = request_id
      AND request.escalation_class <> 'reversible'
      AND request.status = 'answered'
      AND answer.answer_source = 'agent_record'
      AND answer.woken_at IS NULL
      AND answer.disclaimed_at IS NULL
      AND answer.source_comment_id IS NULL
      AND answer.source_chat_session_id = request.chat_session_id
      AND session.creator_id = request.human_user_id
      AND cardinality(answer.chat_message_ids) = 1
      AND message.id = answer.chat_message_ids[1]
      AND message.chat_session_id = request.chat_session_id
      AND message.role = 'user'
      AND message.sent_via = 'interactive_jwt'
      AND message.task_id IS NULL
      AND message.decision_answer_id IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CHE-2976 decision card answer was not repaired; provenance did not match';
    END IF;

    UPDATE decision_answer answer
    SET answer_source = 'card', answered_by_user_id = request.human_user_id,
        answered_by_task_id = NULL, source_chat_session_id = NULL, chat_message_ids = '{}'
    FROM decision_request request
    WHERE request.id = request_id
      AND request.escalation_class <> 'reversible'
      AND request.status = 'answered'
      AND answer.decision_request_id = request.id
      AND answer.answer_source = 'agent_record'
      AND answer.woken_at IS NULL
      AND answer.disclaimed_at IS NULL
      AND answer.source_comment_id IS NULL
      AND answer.source_chat_session_id = request.chat_session_id
      AND EXISTS (SELECT 1 FROM chat_message message WHERE message.decision_answer_id = answer.id);
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CHE-2976 decision card answer changed during repair';
    END IF;
END;
$$;
