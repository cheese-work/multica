SET LOCAL lock_timeout = '2s';

-- Operation receipt of a status move that carries an operation_key (CHE-5491,
-- CHE-2299 S2b). The status transaction inserts it first, so a retry of the
-- same request finds it and returns the stored result. The request hash binds
-- the key to one request body. No foreign keys: the issue, comment, history row
-- and dispatch intent are validated in application code. Rows older than 7 days
-- are deleted by the handoff sweep.
CREATE TABLE IF NOT EXISTS issue_handoff_operation (
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    operation_key uuid NOT NULL,
    actor_type text NOT NULL,
    actor_id uuid,
    request_hash text NOT NULL,
    status_history_id uuid,
    created_comment_id uuid,
    dispatch_id uuid,
    response jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, operation_key)
);
