SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS options jsonb NOT NULL DEFAULT '[]';
-- A card answer is made by the human on the card, not by a chat task.
ALTER TABLE decision_answer ALTER COLUMN answered_by_task_id DROP NOT NULL;
