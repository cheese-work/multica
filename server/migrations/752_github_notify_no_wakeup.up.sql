DROP TRIGGER IF EXISTS capture_comment_wakeup ON comment;
CREATE TRIGGER capture_comment_insert_wakeup AFTER INSERT ON comment FOR EACH ROW
    WHEN (NOT NEW.github_notify_only) EXECUTE FUNCTION capture_comment_wakeup();
CREATE TRIGGER capture_comment_update_wakeup AFTER UPDATE ON comment FOR EACH ROW
    WHEN (NOT OLD.github_notify_only AND NOT NEW.github_notify_only) EXECUTE FUNCTION capture_comment_wakeup();
CREATE TRIGGER capture_comment_delete_wakeup AFTER DELETE ON comment FOR EACH ROW
    WHEN (NOT OLD.github_notify_only) EXECUTE FUNCTION capture_comment_wakeup();
