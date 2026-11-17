-- Restores the 1,000 local workspace ceiling of 583. Existing rows are kept; only new enables are refused.
CREATE OR REPLACE FUNCTION guard_issue_wakeup_capacity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT NEW.enabled OR NEW.system_rule IS NOT NULL THEN RETURN NEW; END IF;
 IF TG_OP='UPDATE' AND OLD.enabled AND OLD.issue_id=NEW.issue_id AND OLD.workspace_id=NEW.workspace_id
  AND (OLD.default_rule_key IS NULL)=(NEW.default_rule_key IS NULL)
  AND OLD.default_scope_kind IS NOT DISTINCT FROM NEW.default_scope_kind
  AND OLD.default_scope_id IS NOT DISTINCT FROM NEW.default_scope_id THEN RETURN NEW; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('issue-wakeup-capacity:'||NEW.workspace_id::text,0));
 IF NEW.default_rule_key IS NULL THEN
  IF (SELECT count(*) FROM (SELECT 1 FROM issue_wakeup WHERE workspace_id=NEW.workspace_id AND issue_id=NEW.issue_id AND enabled AND system_rule IS NULL AND default_rule_key IS NULL AND id<>NEW.id LIMIT 32) slots)>=32 THEN
   RAISE EXCEPTION 'An issue can have at most 32 enabled wakeups' USING ERRCODE='23514',CONSTRAINT='issue_wakeup_active_limit';
  END IF;
  IF (SELECT count(*) FROM (SELECT 1 FROM issue_wakeup WHERE workspace_id=NEW.workspace_id AND enabled AND system_rule IS NULL AND default_rule_key IS NULL AND id<>NEW.id LIMIT 1000) slots)>=1000 THEN
   RAISE EXCEPTION 'A workspace can have at most 1000 enabled wakeups' USING ERRCODE='23514',CONSTRAINT='issue_wakeup_active_limit';
  END IF;
  RETURN NEW;
 END IF;
 IF (SELECT count(*) FROM (SELECT 1 FROM issue_wakeup WHERE workspace_id=NEW.workspace_id AND issue_id=NEW.issue_id AND enabled AND system_rule IS NULL AND default_rule_key IS NOT NULL AND id<>NEW.id LIMIT 32) slots)>=32 THEN
  RAISE EXCEPTION 'Default capacity reached' USING ERRCODE='23514',CONSTRAINT='issue_wakeup_default_capacity',DETAIL='issue';
 END IF;
 IF (SELECT count(*) FROM (SELECT 1 FROM issue_wakeup WHERE workspace_id=NEW.workspace_id AND default_scope_kind=NEW.default_scope_kind AND default_scope_id=NEW.default_scope_id AND enabled AND system_rule IS NULL AND default_rule_key IS NOT NULL AND id<>NEW.id LIMIT 1000) slots)>=1000 THEN
  RAISE EXCEPTION 'Default capacity reached' USING ERRCODE='23514',CONSTRAINT='issue_wakeup_default_capacity',DETAIL='scope';
 END IF;
 IF (SELECT count(*) FROM (SELECT 1 FROM issue_wakeup WHERE workspace_id=NEW.workspace_id AND enabled AND system_rule IS NULL AND default_rule_key IS NOT NULL AND id<>NEW.id LIMIT 5000) slots)>=5000 THEN
  RAISE EXCEPTION 'Default capacity reached' USING ERRCODE='23514',CONSTRAINT='issue_wakeup_default_capacity',DETAIL='workspace';
 END IF;
 RETURN NEW;
END $$;
