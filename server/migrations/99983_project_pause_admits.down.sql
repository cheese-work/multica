CREATE OR REPLACE FUNCTION lock_active_project(p_project_id uuid, p_workspace_id uuid)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE project_status text;
BEGIN
    IF p_project_id IS NULL THEN RETURN TRUE; END IF;
    PERFORM id FROM workspace WHERE id = p_workspace_id FOR KEY SHARE;
    SELECT status INTO project_status FROM project
    WHERE id = p_project_id AND workspace_id = p_workspace_id FOR SHARE;
    IF project_status IN ('paused', 'cancelled', 'completed') THEN
        RAISE EXCEPTION 'project_inactive' USING ERRCODE = 'P0001';
    END IF;
    RETURN TRUE;
END;
$$;
