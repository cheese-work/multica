DROP TRIGGER IF EXISTS dm_progress_epic_new_child ON dm_progress_follow;
DROP TRIGGER IF EXISTS dm_progress_epic_reopen ON issue;
DROP FUNCTION IF EXISTS dm_progress_epic_new_child();
DROP FUNCTION IF EXISTS dm_progress_epic_reopen();
DROP FUNCTION IF EXISTS dm_progress_epic_unfinished(uuid, text);
DROP TABLE IF EXISTS dm_progress_epic_episode;
