-- Append-only record of admin mutations (CHE-2774, L5a). Not activity_log:
-- that table cascades on issue delete and takes best-effort after-commit
-- writes. Rows here are written in the mutation's own transaction.
-- No foreign keys: workspace deletion removes rows through
-- audit_workspace_delete below.
CREATE TABLE workspace_audit_event (
    id                 UUID PRIMARY KEY,
    workspace_id       UUID NOT NULL,
    at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_type         TEXT NOT NULL CHECK (actor_type IN ('member', 'agent', 'system')),
    actor_id           UUID,
    task_id            UUID,
    actor_source       TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL,
    target_type        TEXT NOT NULL,
    target_id          UUID,
    "before"           JSONB NOT NULL DEFAULT '{}'::jsonb,
    "after"            JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Kinds of steps outside the database that the mutation requested. The
    -- row records the request, never the outcome.
    external_requested TEXT[] NOT NULL DEFAULT '{}'
);

-- Guards against bugs and accidents. A session that can set the flag or drop
-- the trigger is not stopped: the application connects as the Postgres
-- superuser in the stock docker-compose deployment.
CREATE FUNCTION workspace_audit_event_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('multica.audit_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'workspace_audit_event is append-only (% refused)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workspace_audit_event_no_update_delete
    BEFORE UPDATE OR DELETE ON workspace_audit_event
    FOR EACH ROW EXECUTE FUNCTION workspace_audit_event_guard();

CREATE TRIGGER workspace_audit_event_no_truncate
    BEFORE TRUNCATE ON workspace_audit_event
    FOR EACH STATEMENT EXECUTE FUNCTION workspace_audit_event_guard();

-- Retention purge. The floor lives here, not in the caller: a cutoff newer
-- than now() - floor is rejected. At most 10000 rows go per call; the caller
-- repeats until it returns 0. One 'system' row records the deleted count.
CREATE FUNCTION audit_retention_purge(p_workspace_id UUID, p_cutoff TIMESTAMPTZ)
RETURNS BIGINT AS $$
DECLARE
    v_floor CONSTANT INTERVAL := INTERVAL '365 days';
    v_batch CONSTANT INT := 10000;
    v_deleted BIGINT;
BEGIN
    IF p_cutoff > now() - v_floor THEN
        RAISE EXCEPTION 'audit purge cutoff % is inside the % retention floor', p_cutoff, v_floor
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    PERFORM set_config('multica.audit_purge', 'on', true);
    WITH doomed AS (
        SELECT id FROM workspace_audit_event
        WHERE workspace_id = p_workspace_id AND at < p_cutoff
        ORDER BY at, id
        LIMIT v_batch
        FOR UPDATE
    )
    DELETE FROM workspace_audit_event e USING doomed d WHERE e.id = d.id;
    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    PERFORM set_config('multica.audit_purge', 'off', true);
    INSERT INTO workspace_audit_event (id, workspace_id, actor_type, kind, target_type, target_id, after)
    VALUES (gen_random_uuid(), p_workspace_id, 'system', 'audit.retention_purge', 'workspace', p_workspace_id,
            jsonb_build_object('deleted', v_deleted, 'cutoff', p_cutoff));
    RETURN v_deleted;
END;
$$ LANGUAGE plpgsql;

-- Workspace deletion removes the workspace's rows in the teardown transaction.
CREATE FUNCTION audit_workspace_delete(p_workspace_id UUID) RETURNS BIGINT AS $$
DECLARE
    v_deleted BIGINT;
BEGIN
    PERFORM set_config('multica.audit_purge', 'on', true);
    DELETE FROM workspace_audit_event WHERE workspace_id = p_workspace_id;
    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    PERFORM set_config('multica.audit_purge', 'off', true);
    RETURN v_deleted;
END;
$$ LANGUAGE plpgsql;
