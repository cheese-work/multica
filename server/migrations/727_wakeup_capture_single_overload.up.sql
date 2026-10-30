-- One capture_issue_wakeup (CHE-1199, L9). Migration 636 replaced the 6-argument
-- function with a 7-argument one (p_only, scoped-event capture). Migrations 663
-- and 666 run after it and CREATE OR REPLACE the 6-argument signature, which
-- is a separate function in PostgreSQL, so both overloads existed and every
-- 6-argument call from the event triggers failed with SQLSTATE 42725 (not
-- unique). This migration drops the 6-argument overload and keeps the 7-argument
-- function with main's multica.handoff_write tag (666) added, so one function
-- carries both the scoped capture and the handoff tag. Callers pass six
-- arguments and p_only takes its default.
DROP FUNCTION IF EXISTS capture_issue_wakeup(uuid, text, text, uuid, uuid, jsonb);
CREATE OR REPLACE FUNCTION capture_issue_wakeup(p_issue uuid, p_type text, p_key text, p_agent uuid, p_task uuid, p_payload jsonb, p_only uuid[] DEFAULT NULL)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE w issue_wakeup; owner_workspace uuid; owner_project uuid; evidence jsonb; violated_constraint text; legacy boolean; scoped boolean; delivered text[] := '{}'; chain text[];
BEGIN
 legacy := EXISTS (SELECT 1 FROM issue_wakeup WHERE issue_id=p_issue AND enabled AND kind='event' AND p_type=ANY(event_types));
 -- A drain replay (p_only) targets given instances and never captures again.
 scoped := p_only IS NULL AND wakeup_scoped_event_wanted(p_issue,p_type);
 IF NOT legacy AND NOT scoped THEN RETURN; END IF;
 IF p_agent IS NULL AND current_setting('multica.actor_type',true)='agent' THEN
  p_agent := NULLIF(current_setting('multica.actor_id',true),'')::uuid;
 END IF;
 SELECT i.workspace_id,i.project_id INTO owner_workspace,owner_project FROM issue i
 WHERE i.id=p_issue AND i.status NOT IN ('done','cancelled') AND NOT EXISTS
 (SELECT 1 FROM issue_status s WHERE s.workspace_id=i.workspace_id AND s.key=i.status AND s.category IN ('done','closed'));
 IF owner_workspace IS NULL THEN RETURN; END IF;
 evidence := jsonb_build_object('event_id',p_key,'event_type',p_type,'version',1,
  'occurred_at',clock_timestamp(),'workspace_id',owner_workspace,'issue_id',p_issue,
  'source_task_id',p_task,'agent_id',p_agent,
  'actor_type',COALESCE(NULLIF(current_setting('multica.actor_type',true),''),CASE WHEN p_agent IS NOT NULL THEN 'agent' ELSE 'system' END),
  'actor_id',COALESCE(NULLIF(current_setting('multica.actor_id',true),''),p_agent::text)) || p_payload;
 IF NULLIF(current_setting('multica.handoff_write',true),'') IS NOT NULL THEN
  evidence := evidence || jsonb_build_object('handoff_write',current_setting('multica.handoff_write'));
 END IF;
 IF legacy THEN
  FOR w IN SELECT * FROM issue_wakeup WHERE issue_id=p_issue AND workspace_id=owner_workspace AND enabled AND kind='event'
    AND (p_only IS NULL OR id=ANY(p_only))
    AND p_type=ANY(event_types)
    AND (filter_actor_type IS NULL OR (filter_actor_type=evidence->>'actor_type' AND filter_actor_id::text=evidence->>'actor_id'))
    AND (filter_agent_id IS NULL OR filter_agent_id=p_agent)
    AND (filter_task_id IS NULL OR filter_task_id=p_task)
    AND (p_task IS NULL OR source_task_id IS DISTINCT FROM p_task)
    AND NOT EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.id=p_task AND t.context->>'wakeup_id'=issue_wakeup.id::text)
    ORDER BY id
  LOOP
   -- One pending notification per event type. Preserve the first key for
   -- duplicate suppression and the latest source reference for state reads.
   BEGIN
    INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,coalesce_key)
     VALUES(gen_random_uuid(),w.id,w.revision,p_key,p_type,
      evidence || jsonb_build_object('coalesced_count',1,'first_occurred_at',evidence->'occurred_at'),p_type)
    ON CONFLICT(wakeup_id,revision,coalesce_key) WHERE processed_at IS NULL AND coalesce_key IS NOT NULL
    -- Rotate the receipt identity on merge. An older dispatcher that read
    -- without FOR UPDATE can only consume its observed version, leaving the
    -- replacement pending instead of silently consuming unseen evidence.
    -- The receipt keeps the latest payload by event time, so a delayed older
   -- event never replaces newer evidence, while the count and the earliest time
   -- merge whatever the arrival order.
   DO UPDATE SET id=EXCLUDED.id,payload=CASE
     WHEN (EXCLUDED.payload->>'occurred_at')::timestamptz >= COALESCE((issue_wakeup_receipt.payload->>'occurred_at')::timestamptz,'-infinity')
     THEN EXCLUDED.payload ELSE issue_wakeup_receipt.payload END || jsonb_build_object(
     'coalesced_count',COALESCE((issue_wakeup_receipt.payload->>'coalesced_count')::bigint,1)+1,
     'first_occurred_at',LEAST((issue_wakeup_receipt.payload->>'first_occurred_at')::timestamptz,(EXCLUDED.payload->>'occurred_at')::timestamptz,(issue_wakeup_receipt.payload->>'occurred_at')::timestamptz))
    WHERE issue_wakeup_receipt.event_key<>p_key AND issue_wakeup_receipt.payload->>'event_id' IS DISTINCT FROM p_key;
   EXCEPTION WHEN unique_violation THEN
    GET STACKED DIAGNOSTICS violated_constraint = CONSTRAINT_NAME;
    -- A retained receipt already handled this exact source fact. This also
    -- fences the locked registration snapshot against terminal-task capture.
    IF violated_constraint <> 'issue_wakeup_receipt_key_idx' THEN RAISE; END IF;
   END;
   -- Live capture served this default instance: the drain must not count it again.
   IF w.default_rule_key IS NOT NULL THEN delivered := delivered || (w.id::text||':'||w.revision::text); END IF;
  END LOOP;
 END IF;
 -- References only: the source payload and identities, never a body or URL.
 IF scoped THEN
  chain := wakeup_scoped_event_chain(owner_workspace,owner_project,p_issue,p_type);
  INSERT INTO wakeup_scoped_event(workspace_id,issue_id,project_id,event_type,event_key,agent_id,source_task_id,actor_type,actor_id,payload,delivered,chain,captured_at)
   VALUES(owner_workspace,p_issue,owner_project,p_type,p_key,p_agent,p_task,evidence->>'actor_type',evidence->>'actor_id',p_payload,delivered,chain,clock_timestamp());
 END IF;
END $$;
