-- CHE-2831: the delivery policy refusal writes its withdrawal comment inside the
-- delivery transaction and must capture no wakeup. A transaction opts out of the
-- comment-insert capture with multica.skip_comment_wakeup = 'on'.
DROP TRIGGER IF EXISTS capture_comment_insert_wakeup ON comment;
CREATE TRIGGER capture_comment_insert_wakeup AFTER INSERT ON comment FOR EACH ROW
    WHEN (NOT NEW.github_notify_only AND current_setting('multica.skip_comment_wakeup', true) IS DISTINCT FROM 'on') EXECUTE FUNCTION capture_comment_wakeup();
