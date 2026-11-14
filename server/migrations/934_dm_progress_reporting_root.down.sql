DROP TABLE IF EXISTS dm_progress_data_incident;
DROP TRIGGER IF EXISTS dm_progress_follow_root_immutable ON dm_progress_follow;
DROP FUNCTION IF EXISTS dm_progress_follow_root_immutable();
ALTER TABLE dm_progress_follow DROP COLUMN IF EXISTS reporting_root_id;
