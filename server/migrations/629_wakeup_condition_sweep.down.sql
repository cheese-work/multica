-- Restore the L9 probe (migration 597) and drop the activation capture.
DROP TRIGGER IF EXISTS capture_issue_activation_wakeup ON issue;
DROP FUNCTION IF EXISTS capture_issue_activation_wakeup();
CREATE OR REPLACE FUNCTION wakeup_scoped_event_probe(uuid, text, uuid, text) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
BEGIN
 RETURN EXISTS (SELECT 1 FROM issue_wakeup_definition d WHERE d.workspace_id=$1 AND d.scope_kind=$2 AND d.scope_id=$3 AND (CASE WHEN $4 IS NULL THEN cardinality(d.event_types)>0 ELSE $4=ANY(d.event_types) END));
END $$;
ALTER TABLE issue_wakeup_definition
 DROP COLUMN IF EXISTS sweep_revision,
 DROP COLUMN IF EXISTS sweep_cursor,
 DROP COLUMN IF EXISTS sweep_done,
 DROP COLUMN IF EXISTS sweep_baseline,
 DROP COLUMN IF EXISTS sweep_attempts,
 DROP COLUMN IF EXISTS sweep_retry_at,
 DROP COLUMN IF EXISTS sweep_error;
