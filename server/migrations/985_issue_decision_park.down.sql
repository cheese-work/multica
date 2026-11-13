SET LOCAL lock_timeout = '2s';

-- Write each park back under issue.metadata.decision_park, then drop the table.
-- Optional fields stay absent when null, as the original record omitted them.
UPDATE issue i SET metadata = jsonb_set(i.metadata, '{decision_park}',
    jsonb_build_object('status', p.prior_status, 'to', p.to_status)
    || CASE WHEN p.readiness_id IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('readiness_id', p.readiness_id) END
    || CASE WHEN p.prior_readiness IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('readiness', p.prior_readiness) END
    || CASE WHEN p.set_value IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('set', p.set_value) END)
FROM issue_decision_park p
WHERE p.issue_id = i.id;

DROP TABLE IF EXISTS issue_decision_park;
