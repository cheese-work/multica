-- CHE-5121 (slice B of CHE-5046): state the merge_eligibility condition reads
-- without an agent. merge_pause_state is the last repository-variable read
-- (is_set NULL = unknown after a failed read); merge_veto_deadline persists
-- last PASS + veto window per PR.
CREATE TABLE merge_pause_state (
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    variable_name text NOT NULL,
    is_set boolean,
    fetched_at timestamptz NOT NULL,
    error text,
    PRIMARY KEY (repo_owner, repo_name, variable_name)
);
CREATE TABLE merge_veto_deadline (
    workspace_id uuid NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    last_pass_at timestamptz NOT NULL,
    due_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repo_owner, repo_name, pr_number)
);
