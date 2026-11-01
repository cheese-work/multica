-- Rollout receipt for migration 870_decision_wake_policy: non-reversible
-- agent_record answers woken in the 24 h before the migration. They stay as they
-- are; a person reads the list. Run it just before the migration:
--   psql "$DATABASE_URL" -f scripts/decision-wake-receipt.sql
SELECT a.id AS answer_id, r.workspace_id, r.id AS request_id, r.issue_id, r.escalation_class, a.woken_at, a.wake_outcome
FROM decision_answer a
JOIN decision_request r ON r.id = a.decision_request_id
WHERE a.answer_source = 'agent_record' AND r.escalation_class <> 'reversible'
  AND a.woken_at > now() - interval '24 hours' AND a.wake_outcome IS DISTINCT FROM 'skipped_ineligible'
ORDER BY a.woken_at
