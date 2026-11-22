-- Loses no fork row. Rows written to upstream's table after the deploy are not
-- copied back; recovery after a deploy is chat_task_supplement_fork_restore.sql.
ALTER INDEX chat_task_supplement_fork_875_task_status_idx RENAME TO chat_task_supplement_task_status_idx;
ALTER INDEX chat_task_supplement_fork_875_pkey RENAME TO chat_task_supplement_pkey;
ALTER TABLE chat_task_supplement_fork_875 RENAME TO chat_task_supplement;
-- 999q01's down restores NOT NULL; the fork 875 state is nullable.
ALTER TABLE task_supplement_capability ALTER COLUMN issue_id DROP NOT NULL;
