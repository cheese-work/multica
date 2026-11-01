SET LOCAL lock_timeout = '2s';

-- External-read mark (CHE-2216 plan 3.7). A task is marked once it contacts a
-- host other than the Multica server and the provider API, or uses a GitHub
-- read token. What a marked task writes carries the mark so an elevated run
-- can be shown a stub instead of the body. The mark only grows.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS external_read boolean NOT NULL DEFAULT false;
ALTER TABLE comment ADD COLUMN IF NOT EXISTS external_read boolean NOT NULL DEFAULT false;
ALTER TABLE attachment ADD COLUMN IF NOT EXISTS external_read boolean NOT NULL DEFAULT false;
ALTER TABLE issue ADD COLUMN IF NOT EXISTS description_external_read boolean NOT NULL DEFAULT false;

-- A comment written by a marked task stores the mark. Grow-only, like taint:
-- an update cannot clear it.
CREATE OR REPLACE FUNCTION comment_inherit_external_read() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.external_read := NEW.external_read OR OLD.external_read;
    END IF;
    IF NEW.source_task_id IS NOT NULL AND NOT NEW.external_read THEN
        NEW.external_read := COALESCE((SELECT external_read FROM agent_task_queue WHERE id = NEW.source_task_id), false);
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS comment_inherit_external_read ON comment;
CREATE TRIGGER comment_inherit_external_read BEFORE INSERT OR UPDATE OF external_read, source_task_id ON comment
    FOR EACH ROW EXECUTE FUNCTION comment_inherit_external_read();
