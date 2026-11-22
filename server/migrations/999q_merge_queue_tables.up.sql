SET LOCAL lock_timeout = '2s';

-- CHE-7408 (B1 of CHE-7148): storage for the merge-queue dequeue-cause record.
-- Nothing reads or writes these tables until B2 and later steps land, and no
-- deployed version can set merge_queue_dequeue_routing to block before B11.
--
-- No foreign keys or cascades. repo_owner and repo_name are stored lowercased.
-- Text state columns carry CHECK constraints. Every unique and lookup index is
-- built CONCURRENTLY in 999r-999x (repo rule: one concurrent index per file,
-- none inside the table migration). Times named *_at that come from GitHub are
-- GitHub's, never webhook receipt time (plan 4.5).

-- One row per removal from the merge queue. removal_node_id is the removal's
-- timeline node id; it is NULL until the worker binds the webhook delivery to
-- it. delivery is set once, from pending, by a conditional update in the
-- mergequeue package (plan 4.3); routing is the value at the intake commit.
CREATE TABLE IF NOT EXISTS merge_queue_intake (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    removal_node_id text,
    delivery_id text,
    routing text NOT NULL DEFAULT 'warn' CHECK (routing IN ('off', 'warn', 'block')),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processed', 'stale', 'inconclusive')),
    delivery text NOT NULL DEFAULT 'pending' CHECK (delivery IN ('pending', 'held', 'run_sent')),
    delivery_reason text,
    held_issue_ids uuid[] NOT NULL DEFAULT '{}',
    reason text,
    before_commit_oid text,
    head_oid text,
    build_run_id bigint,
    removed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Cause epochs. Rows are immutable history once resolved or split; the only
-- writer of state is the transition table in the mergequeue package.
CREATE TABLE IF NOT EXISTS merge_queue_epoch (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    cause_key text NOT NULL,
    epoch_no integer NOT NULL DEFAULT 1 CHECK (epoch_no >= 1),
    kind text NOT NULL CHECK (kind IN ('shared', 'suspected')),
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'split', 'owned', 'unowned', 'verifying', 'resolved', 'stalled')),
    owner_issue_id uuid,
    lease_expires_at timestamptz,
    last_probe_at timestamptz,
    opened_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A PR's association to a cause key. It ends only on a split of its suspected
-- epoch or a PR close (plan 4.1); a queue entry by any actor ends nothing.
-- linked_issue_ids is the snapshot of issues linked to the PR at the start.
CREATE TABLE IF NOT EXISTS merge_queue_association (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    cause_key text NOT NULL,
    epoch_id uuid,
    linked_issue_ids uuid[] NOT NULL DEFAULT '{}',
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    end_reason text CHECK (end_reason IN ('split', 'pr_closed')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A wait (block) or would_wait (warn) per (issue, PR), met by the
-- merge_queue_hold predicate. One unmet row per (issue, PR) (index 999u).
CREATE TABLE IF NOT EXISTS merge_queue_wait (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    issue_id uuid NOT NULL,
    intake_id uuid,
    wakeup_id uuid,
    kind text NOT NULL CHECK (kind IN ('wait', 'would_wait')),
    state text NOT NULL DEFAULT 'unmet' CHECK (state IN ('unmet', 'met', 'timed_out')),
    recovered boolean NOT NULL DEFAULT false,
    expires_at timestamptz,
    met_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One row per closed delivery, unique on (PR, closed_at) so a redelivery under
-- a new delivery id finds the row (plan 4.5). B13 adds eligibility and retry.
CREATE TABLE IF NOT EXISTS merge_queue_pr_close (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    base_branch text,
    merged boolean NOT NULL DEFAULT false,
    merge_commit_sha text,
    closed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One row per AddedToMergeQueueEvent node, keyed by GitHub's node id. actor_*
-- come from the node's actor and __typename, never enqueuer (plan 4.5).
CREATE TABLE IF NOT EXISTS merge_queue_enqueue_event (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    node_id text NOT NULL,
    node_created_at timestamptz NOT NULL,
    actor_login text,
    actor_type text,
    actor_class text NOT NULL DEFAULT 'unknown' CHECK (actor_class IN ('agent', 'person', 'unknown')),
    head_oid text,
    head_state text NOT NULL DEFAULT 'head_unknown' CHECK (head_state IN ('known', 'head_unknown')),
    tie boolean NOT NULL DEFAULT false,
    label text CHECK (label IN ('first', 'requeue-while-open', 'requeue-after-close', 'second-requeue-same-head', 'new-head', 'requeue-order-unknown', 'requeue-head-unknown')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The PR's current queue state, updated under the repo lock.
CREATE TABLE IF NOT EXISTS merge_queue_state (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    state text NOT NULL CHECK (state IN ('enqueued', 'dequeued', 'closed')),
    changed_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only evidence: a queue build's failing tests, a main-evidence run, a
-- probe. failing_tests holds {package, name} pairs; checks holds the checks
-- that did not finish for a timeout.
CREATE TABLE IF NOT EXISTS merge_queue_evidence (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    kind text NOT NULL CHECK (kind IN ('queue_build', 'main_run', 'probe')),
    intake_id uuid,
    epoch_id uuid,
    run_id bigint,
    run_attempt integer,
    signature text,
    failing_tests jsonb NOT NULL DEFAULT '[]',
    checks jsonb NOT NULL DEFAULT '[]',
    run_created_at timestamptz,
    run_completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
