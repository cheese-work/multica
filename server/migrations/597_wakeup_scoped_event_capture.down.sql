-- Restore the pre-L9 capture functions (migrations 532 and 520).
DROP FUNCTION IF EXISTS capture_issue_wakeup(uuid, text, text, uuid, uuid, jsonb, uuid[]);
CREATE OR REPLACE FUNCTION capture_issue_wakeup(p_issue uuid, p_type text, p_key text, p_agent uuid, p_task uuid, p_payload jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE w issue_wakeup; owner_workspace uuid; evidence jsonb; violated_constraint text;
BEGIN
 IF NOT EXISTS (SELECT 1 FROM issue_wakeup WHERE issue_id=p_issue AND enabled AND kind='event' AND p_type=ANY(event_types)) THEN RETURN; END IF;
 IF p_agent IS NULL AND current_setting('multica.actor_type',true)='agent' THEN
  p_agent := NULLIF(current_setting('multica.actor_id',true),'')::uuid;
 END IF;
 SELECT i.workspace_id INTO owner_workspace FROM issue i
 WHERE i.id=p_issue AND i.status NOT IN ('done','cancelled') AND NOT EXISTS
 (SELECT 1 FROM issue_status s WHERE s.workspace_id=i.workspace_id AND s.key=i.status AND s.category IN ('done','closed'));
 IF owner_workspace IS NULL THEN RETURN; END IF;
 evidence := jsonb_build_object('event_id',p_key,'event_type',p_type,'version',1,
  'occurred_at',clock_timestamp(),'workspace_id',owner_workspace,'issue_id',p_issue,
  'source_task_id',p_task,'agent_id',p_agent,
  'actor_type',COALESCE(NULLIF(current_setting('multica.actor_type',true),''),CASE WHEN p_agent IS NOT NULL THEN 'agent' ELSE 'system' END),
  'actor_id',COALESCE(NULLIF(current_setting('multica.actor_id',true),''),p_agent::text)) || p_payload;
 FOR w IN SELECT * FROM issue_wakeup WHERE issue_id=p_issue AND workspace_id=owner_workspace AND enabled AND kind='event'
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
   DO UPDATE SET id=EXCLUDED.id,payload=EXCLUDED.payload || jsonb_build_object(
    'coalesced_count',COALESCE((issue_wakeup_receipt.payload->>'coalesced_count')::bigint,1)+1,
    'first_occurred_at',issue_wakeup_receipt.payload->'first_occurred_at')
   WHERE issue_wakeup_receipt.event_key<>p_key AND issue_wakeup_receipt.payload->>'event_id' IS DISTINCT FROM p_key;
  EXCEPTION WHEN unique_violation THEN
   GET STACKED DIAGNOSTICS violated_constraint = CONSTRAINT_NAME;
   -- A retained receipt already handled this exact source fact. This also
   -- fences the locked registration snapshot against terminal-task capture.
   IF violated_constraint <> 'issue_wakeup_receipt_key_idx' THEN RAISE; END IF;
  END;

 END LOOP;
END $$;

CREATE OR REPLACE FUNCTION capture_comment_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c comment; event_type text; source_id uuid; source_agent uuid; payload jsonb; root_id uuid;
BEGIN
 c := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
 IF NOT EXISTS (SELECT 1 FROM issue_wakeup WHERE issue_id=c.issue_id AND enabled AND kind='event') THEN RETURN c; END IF;
 source_id := NULLIF(current_setting('multica.source_task_id',true),'')::uuid;
 IF TG_OP='INSERT' THEN
  event_type := 'comment.created';
  source_id := COALESCE(source_id,c.source_task_id);
  source_agent := CASE WHEN c.author_type='agent' THEN c.author_id END;
 ELSIF TG_OP='DELETE' THEN
  -- Pruning an existing tombstone is storage cleanup, not another deletion.
  IF OLD.deleted_at IS NOT NULL THEN RETURN OLD; END IF;
  event_type := 'comment.deleted';
 ELSIF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
  event_type := 'comment.deleted';
 ELSIF NEW.deleted_at IS NOT NULL THEN RETURN NEW;
 ELSIF OLD.resolved_at IS NULL AND NEW.resolved_at IS NOT NULL THEN event_type := 'comment.resolved';
 ELSIF OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS NULL THEN event_type := 'comment.unresolved';
 ELSIF NEW.content IS DISTINCT FROM OLD.content THEN event_type := 'comment.updated';
 ELSE RETURN NEW;
 END IF;
 IF source_id IS NOT NULL THEN SELECT agent_id INTO source_agent FROM agent_task_queue WHERE id=source_id; END IF;
 -- Keep author and actor distinct: an admin editing an agent's comment is
 -- not an event produced by that agent's original run.
 IF TG_OP<>'INSERT' AND source_id IS NULL THEN
  source_agent := CASE WHEN current_setting('multica.actor_type',true)='agent' THEN NULLIF(current_setting('multica.actor_id',true),'')::uuid END;
 END IF;
 root_id := c.id;
 IF c.parent_id IS NOT NULL THEN
  WITH RECURSIVE ancestors AS (
   SELECT id,parent_id,1 AS depth FROM comment WHERE id=c.parent_id AND issue_id=c.issue_id AND workspace_id=c.workspace_id
   UNION ALL SELECT p.id,p.parent_id,a.depth+1 FROM comment p JOIN ancestors a ON p.id=a.parent_id
    WHERE p.issue_id=c.issue_id AND p.workspace_id=c.workspace_id AND a.depth<256
  ) SELECT id INTO root_id FROM ancestors ORDER BY depth DESC LIMIT 1;
 END IF;
 payload := jsonb_build_object('comment_id',c.id,'parent_comment_id',c.parent_id,'thread_id',COALESCE(root_id,c.id),
  'author_type',c.author_type,'author_id',c.author_id);
 IF TG_OP='INSERT' THEN payload := payload || jsonb_build_object('actor_type',c.author_type,'actor_id',c.author_id); END IF;
 PERFORM capture_issue_wakeup(c.issue_id,event_type,gen_random_uuid()::text,source_agent,source_id,payload);
 RETURN c;
END $$;

CREATE OR REPLACE FUNCTION capture_issue_collaboration_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source_id uuid; source_agent uuid; fields jsonb; field_name text; event_type text;
 before_row jsonb; after_row jsonb; event_key text;
BEGIN
 -- Avoid serializing potentially large issue documents on the ordinary path.
 IF NOT EXISTS (SELECT 1 FROM issue_wakeup WHERE issue_id=NEW.id AND enabled AND kind='event') THEN RETURN NEW; END IF;
 before_row := to_jsonb(OLD); after_row := to_jsonb(NEW); event_key := gen_random_uuid()::text;
 SELECT jsonb_agg(k ORDER BY k) INTO fields FROM unnest(ARRAY[
  'title','description','status','priority','assignee_type','assignee_id','parent_issue_id','project_id',
  'due_date','start_date','stage','properties','metadata','acceptance_criteria','context_refs','triage_state']) k
 WHERE before_row->k IS DISTINCT FROM after_row->k;
 IF fields IS NULL THEN RETURN NEW; END IF;
 source_id := NULLIF(current_setting('multica.source_task_id',true),'')::uuid;
 SELECT agent_id INTO source_agent FROM agent_task_queue WHERE id=source_id;
 IF source_agent IS NULL AND current_setting('multica.actor_type',true)='agent' THEN
  source_agent := NULLIF(current_setting('multica.actor_id',true),'')::uuid;
 END IF;
 PERFORM capture_issue_wakeup(NEW.id,'issue.updated',event_key||':updated',source_agent,source_id,jsonb_build_object('changed_fields',fields));
 FOR field_name,event_type IN SELECT * FROM (VALUES
  ('status','issue.status_changed'),('assignee_id','issue.assignee_changed'),
  ('parent_issue_id','issue.parent_changed'),('project_id','issue.project_changed'),
  ('properties','issue.properties_changed'),('metadata','issue.metadata_changed')) v(f,e)
 LOOP
  IF fields ? field_name OR (field_name='assignee_id' AND fields ? 'assignee_type') THEN
   PERFORM capture_issue_wakeup(NEW.id,event_type,event_key||':'||field_name,source_agent,source_id,
    CASE WHEN field_name IN ('metadata','properties') THEN
     jsonb_build_object('changed_fields',(SELECT jsonb_agg(k ORDER BY k) FROM
      (SELECT jsonb_object_keys(before_row->field_name) k UNION SELECT jsonb_object_keys(after_row->field_name)) keys
      WHERE (before_row->field_name)->k IS DISTINCT FROM (after_row->field_name)->k))
    WHEN field_name='status' THEN jsonb_build_object('previous_status',OLD.status,'status',NEW.status,
     'previous_category',(SELECT category FROM issue_status WHERE workspace_id=OLD.workspace_id AND key=OLD.status),
     'category',(SELECT category FROM issue_status WHERE workspace_id=NEW.workspace_id AND key=NEW.status))
    WHEN field_name='assignee_id' THEN jsonb_build_object('previous_assignee_id',OLD.assignee_id,'assignee_id',NEW.assignee_id,
     'previous_assignee_type',OLD.assignee_type,'assignee_type',NEW.assignee_type)
    ELSE jsonb_build_object('previous_'||field_name,before_row->field_name,field_name,after_row->field_name) END);
  END IF;
 END LOOP;
 RETURN NEW;
END $$;
DROP FUNCTION IF EXISTS wakeup_scoped_event_wanted(uuid, text);
DROP FUNCTION IF EXISTS wakeup_scoped_event_chain(uuid, uuid, uuid, text);
DROP FUNCTION IF EXISTS wakeup_scoped_event_probe(uuid, text, uuid, text);
