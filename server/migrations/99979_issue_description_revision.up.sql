-- Description revision history (CHE-5230). issue.revision bumps on every write,
-- so a plan that agents rewrite shows only a number. This keeps the description
-- text as of each revision that changed it. One row per (issue, revision); the
-- trigger captures every write path, and the actor comes from the same
-- transaction settings the wakeup capture reads (NULL when a path sets none).
CREATE TABLE IF NOT EXISTS issue_description_revision (
    issue_id     UUID        NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    revision     BIGINT      NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    actor_type   TEXT,
    actor_id     UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, revision)
);

CREATE OR REPLACE FUNCTION record_issue_description_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO issue_description_revision (issue_id, revision, description, actor_type, actor_id)
    VALUES (NEW.id, NEW.revision, COALESCE(NEW.description, ''),
            NULLIF(current_setting('multica.actor_type', true), ''),
            NULLIF(current_setting('multica.actor_id', true), '')::uuid)
    ON CONFLICT (issue_id, revision) DO UPDATE SET description = EXCLUDED.description;
    RETURN NULL;
END $$;

CREATE TRIGGER issue_description_revision_ins AFTER INSERT ON issue
    FOR EACH ROW EXECUTE FUNCTION record_issue_description_revision();
CREATE TRIGGER issue_description_revision_upd AFTER UPDATE OF description ON issue
    FOR EACH ROW WHEN (OLD.description IS DISTINCT FROM NEW.description)
    EXECUTE FUNCTION record_issue_description_revision();

-- Baseline: current text at current revision, so the first diff has a left side.
INSERT INTO issue_description_revision (issue_id, revision, description, created_at)
SELECT id, revision, COALESCE(description, ''), updated_at FROM issue
ON CONFLICT DO NOTHING;
