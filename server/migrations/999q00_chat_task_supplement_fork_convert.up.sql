-- multica:contract snapshot=chat_task_supplement restore=server/cmd/migrate/recovery/chat_task_supplement_fork_restore.sql
-- Free the name upstream's 999q01 creates. The fork table (875/876) is kept
-- under a new name as the recovery source; the pre-migration hook on the 662
-- target copies its delivered rows into upstream's table. Index names are
-- schema-global, so the fork's two indexes move with it.
ALTER TABLE chat_task_supplement RENAME TO chat_task_supplement_fork_875;
ALTER INDEX chat_task_supplement_pkey RENAME TO chat_task_supplement_fork_875_pkey;
ALTER INDEX chat_task_supplement_task_status_idx RENAME TO chat_task_supplement_fork_875_task_status_idx;
