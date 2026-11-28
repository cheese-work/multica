-- sqlc schema hint for 99zzz00_chat_wakeup_fork_convert (CHE-9434 step 8).
--
-- sqlc reads only top-level DDL, so it never sees the DO block in
-- 99zzz00_chat_wakeup_fork_convert replace the fork chat_wakeup table with
-- upstream's shape. Its catalog would keep the fork columns and reject
-- upstream's chat_wakeup.sql queries.
--
-- At runtime this pair is a no-op: rename away, then rename back inside a DO
-- block in the same migration. sqlc applies only the first statement, so its
-- catalog drops the fork shape and takes 99zzz15_chat_wakeup's table.
ALTER TABLE chat_wakeup RENAME TO chat_wakeup_sqlc_fork_shape;
DO $sqlc$ BEGIN ALTER TABLE chat_wakeup_sqlc_fork_shape RENAME TO chat_wakeup; END $sqlc$;
