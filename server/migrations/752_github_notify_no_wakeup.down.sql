DELETE FROM github_webhook_dispatch WHERE state <> 'dispatched';
DROP TRIGGER IF EXISTS capture_comment_insert_wakeup ON comment;
DROP TRIGGER IF EXISTS capture_comment_update_wakeup ON comment;
DROP TRIGGER IF EXISTS capture_comment_delete_wakeup ON comment;
CREATE TRIGGER capture_comment_wakeup AFTER INSERT OR UPDATE OR DELETE ON comment
    FOR EACH ROW EXECUTE FUNCTION capture_comment_wakeup();
