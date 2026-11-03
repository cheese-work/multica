DROP TRIGGER IF EXISTS capture_comment_insert_wakeup ON comment;
CREATE TRIGGER capture_comment_insert_wakeup AFTER INSERT ON comment FOR EACH ROW
    WHEN (NOT NEW.github_notify_only) EXECUTE FUNCTION capture_comment_wakeup();
