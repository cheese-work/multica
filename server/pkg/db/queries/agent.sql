-- name: ListAgents :many
SELECT * FROM agent
WHERE workspace_id = $1 AND archived_at IS NULL AND kind = 'user'
ORDER BY created_at ASC;

-- name: ListAllAgents :many
SELECT * FROM agent
WHERE workspace_id = $1 AND kind = 'user'
ORDER BY created_at ASC;

-- name: ListAllAgentsAnyKind :many
-- Every agent row in the workspace, INCLUDING `kind = 'system'` execution
-- carriers (agent builder sessions) that ListAllAgents deliberately hides.
--
-- Only for surfaces that aggregate over raw `agent_task_queue` / task_usage
-- rows, which carry no kind filter of their own: a system carrier runs real
-- tasks and books real usage, so a per-agent rollup returns it whether or not
-- any list endpoint will ever name it. Those surfaces need the full population
-- to decide what to hide (MUL-5409). Do NOT use this to build a user-facing
-- agent list — `kind = 'system'` must stay out of every picker and assignee
-- surface.
SELECT * FROM agent
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetAgent :one
SELECT * FROM agent
WHERE id = $1;

-- name: GetAgentForUpdate :one
-- Serializes read-modify-write updates to disabled_runtime_skills so two
-- concurrent per-skill toggles cannot overwrite each other.
SELECT * FROM agent
WHERE id = $1
FOR UPDATE;

-- name: GetAgentInWorkspace :one
SELECT * FROM agent
WHERE id = $1 AND workspace_id = $2 AND kind = 'user';

-- name: LockAgentForAutopilotAssignment :one
-- Serializes creating, retargeting, or resuming an active Autopilot with
-- Runtime teardown. Teardown takes FOR UPDATE on this same Agent row before it
-- clears runtime_id and pauses matching Autopilots. FOR SHARE also stabilizes
-- runtime_id and archived_at against concurrent ordinary Agent updates. The
-- shared lock makes the
-- two outcomes exhaustive:
--   * assignment commits first, then teardown sees and pauses it; or
--   * teardown commits first, then assignment re-reads runtime_id=NULL and
--     rejects the active Autopilot.
SELECT * FROM agent
WHERE id = $1 AND workspace_id = $2 AND kind = 'user'
FOR SHARE;

-- name: CreateAgent :one
INSERT INTO agent (
    workspace_id, name, description, avatar_url, runtime_mode,
    runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id,
    instructions, custom_env, custom_args, mcp_config, model, thinking_level,
    service_tier, conversation_starters,
    composio_toolkit_allowlist, permission_mode
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12, $13, $14, $15, $16,
    $17, COALESCE(sqlc.narg('conversation_starters')::jsonb, '[]'::jsonb),
    sqlc.narg('composio_toolkit_allowlist')::text[],
    COALESCE(sqlc.narg('permission_mode'), 'private')
)
RETURNING *;

-- name: CreateAgentBuilder :one
-- One hidden builder agent per creation session. Keeping the execution carrier
-- session-scoped freezes its model/runtime configuration when multiple builder
-- flows are open concurrently, while `kind = 'system'` keeps it out of normal
-- agent lists and assignment surfaces.
INSERT INTO agent (
    workspace_id, name, description, runtime_mode, runtime_config, runtime_id,
    visibility, permission_mode, max_concurrent_tasks, owner_id, instructions,
    custom_env, custom_args, model, kind, system_key
) VALUES (
    @workspace_id, @name, '', @runtime_mode, '{}'::jsonb, @runtime_id,
    'private', 'private', 1, @owner_id, @instructions,
    '{}'::jsonb, '[]'::jsonb, sqlc.narg('model'), 'system', @system_key
)
RETURNING *;

-- name: DeleteSystemAgentByID :exec
-- Builder sessions own their hidden execution agent. Deleting the session
-- removes that carrier and its task rows; the kind guard prevents this cleanup
-- path from ever deleting a user-authored agent.
DELETE FROM agent
WHERE id = $1 AND kind = 'system' AND system_key LIKE 'agent_builder:%';

-- name: RebindAgentBuilderRuntime :one
-- Re-points a builder carrier at another runtime mid-conversation. The carrier
-- is what SendDirectChatMessage reads to stamp a chat task's runtime_id, so this
-- UPDATE is the only thing that actually moves subsequent replies; the live-draft
-- picker alone never did. model is reset wholesale because model ids are
-- per-runtime — the new runtime resolves its own default instead of inheriting an
-- id it may not serve. The kind/system_key guard mirrors DeleteSystemAgentByID so
-- this path can never touch a user-authored agent.
--
-- Callers MUST hold LockChatSessionForRuntimeBind on the owning chat_session for
-- the whole transaction, otherwise a concurrent send can stamp a task with the
-- pre-switch runtime after the pending-task check has already passed (MUL-5163).
--
-- chat_session.runtime_id is deliberately left untouched: the daemon only resumes
-- a stored provider session when chat_session.runtime_id matches the claiming
-- task's runtime (see the chat claim path), so leaving the old pointer in place is
-- exactly what makes the new runtime start a fresh provider session.
UPDATE agent
SET runtime_id = @runtime_id,
    runtime_mode = @runtime_mode,
    model = sqlc.narg('model'),
    updated_at = now()
WHERE id = @id AND kind = 'system' AND system_key LIKE 'agent_builder:%'
RETURNING *;

-- name: UpdateAgent :one
-- composio_toolkit_allowlist is set wholesale: the API layer is responsible
-- for normalising the request payload to either (a) the new slug list — sent
-- here verbatim — or (b) an empty array to explicitly disable Composio.
-- Distinguish "field omitted" (preserve) from "explicit clear" via
-- ClearAgentComposioToolkitAllowlist below, mirroring the
-- thinking_level / mcp_config two-query pattern: COALESCE can't restore NULL.
UPDATE agent SET
    name = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description),
    avatar_url = COALESCE(sqlc.narg('avatar_url'), avatar_url),
    runtime_config = COALESCE(sqlc.narg('runtime_config'), runtime_config),
    runtime_mode = COALESCE(sqlc.narg('runtime_mode'), runtime_mode),
    runtime_id = COALESCE(sqlc.narg('runtime_id'), runtime_id),
    visibility = COALESCE(sqlc.narg('visibility'), visibility),
    permission_mode = COALESCE(sqlc.narg('permission_mode'), permission_mode),
    status = COALESCE(sqlc.narg('status'), status),
    max_concurrent_tasks = COALESCE(sqlc.narg('max_concurrent_tasks'), max_concurrent_tasks),
    instructions = COALESCE(sqlc.narg('instructions'), instructions),
    custom_env = COALESCE(sqlc.narg('custom_env'), custom_env),
    custom_env_version = custom_env_version + CASE WHEN sqlc.narg('custom_env')::jsonb IS NULL THEN 0 ELSE 1 END,
    custom_args = COALESCE(sqlc.narg('custom_args'), custom_args),
    mcp_config = COALESCE(sqlc.narg('mcp_config'), mcp_config),
    model = COALESCE(sqlc.narg('model'), model),
    thinking_level = COALESCE(sqlc.narg('thinking_level'), thinking_level),
    service_tier = COALESCE(sqlc.narg('service_tier'), service_tier),
    conversation_starters = COALESCE(sqlc.narg('conversation_starters'), conversation_starters),
    composio_toolkit_allowlist = COALESCE(sqlc.narg('composio_toolkit_allowlist')::text[], composio_toolkit_allowlist),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ClearAgentComposioToolkitAllowlist :one
-- Explicit NULL-clear for composio_toolkit_allowlist. The COALESCE-based
-- UpdateAgent cannot set the column back to NULL — sending an empty array
-- through there would persist `{}` (still a non-NULL, equivalent to "no
-- toolkits" but distinct from "field never configured"). The API uses this
-- dedicated query when the agent owner removes every toolkit; subsequent
-- dispatch decisions treat NULL identically to `{}` (both -> no overlay).
UPDATE agent SET composio_toolkit_allowlist = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ClearAgentThinkingLevel :one
-- Explicit NULL-clear for thinking_level. COALESCE-based UpdateAgent cannot
-- set the column back to NULL, so the API layer routes "user picked Default"
-- through this dedicated query.
UPDATE agent SET thinking_level = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ClearAgentServiceTier :one
-- Explicit NULL-clear for service_tier. COALESCE-based UpdateAgent cannot
-- set the column back to NULL, so the API routes "Runtime default" here.
UPDATE agent SET service_tier = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ClearAgentMcpConfig :one
UPDATE agent SET mcp_config = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateAgentCustomEnv :one
-- Replaces an agent's custom_env map wholesale. Used by the dedicated
-- env-management endpoint (POST/PUT /api/agents/{id}/env), which is the
-- only post-creation write path for env. UpdateAgent has been stripped
-- of custom_env handling so all env mutations flow through here and the
-- handler's audit-log + **** sentinel guard.
UPDATE agent
SET custom_env = $2, custom_env_version = custom_env_version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateAgentDisabledRuntimeSkills :one
UPDATE agent
SET disabled_runtime_skills = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ArchiveAgent :one
UPDATE agent SET archived_at = now(), archived_by = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ArchiveAgentsByRuntime :many
-- Bulk-archives every active agent bound to any runtime in the given set.
-- Used when revoking a leaving member's runtimes so agents pinned to those
-- runtimes can no longer be assigned new work. Returns the affected rows so
-- the caller can broadcast agent:archived per agent.
--
-- System agents are exempt: they belong to the workspace rather than to the
-- member who happened to create them, and the workspace's entry point runs
-- through one. Archiving Mika because a colleague left would take the default
-- agent away from everyone. Its runtime does go offline with the departure, so
-- it needs rebinding — but it stays visible and recoverable instead of
-- vanishing.
UPDATE agent
SET archived_at = now(), archived_by = @archived_by, updated_at = now()
WHERE runtime_id = ANY(@runtime_ids::uuid[]) AND archived_at IS NULL
  AND (system_key IS NULL OR system_key = '')
RETURNING *;

-- name: ArchiveAgentsByIDs :many
-- Narrow archive that only touches the explicit ID list. Used by the
-- cascade-delete endpoint so the user's expected_active_agent_ids list
-- is the authoritative bound on what gets archived: any agent that
-- appeared on the runtime after the user opened the dialog is filtered
-- out here so it can't be silently archived even in the (vanishingly
-- rare) case where a row-level race slips past the runtime FOR UPDATE
-- lock. Returns the affected rows so the caller can broadcast
-- agent:archived per agent.
UPDATE agent
SET archived_at = now(), archived_by = @archived_by, updated_at = now()
WHERE id = ANY(@agent_ids::uuid[]) AND archived_at IS NULL
RETURNING *;

-- name: ListActiveAgentsByRuntime :many
-- Returns every non-archived agent bound to a runtime. Backs the cascade
-- delete dialog: when DELETE /api/runtimes/:id refuses with
-- runtime_has_active_agents, the response carries this list so the front-end
-- can render exactly the agents that will be unbound if the user confirms,
-- and so the cascade endpoint's expected_active_agent_ids check has a stable
-- snapshot to compare against. Ordered by name for a deterministic display.
SELECT * FROM agent
WHERE runtime_id = $1 AND archived_at IS NULL AND kind = 'user'
ORDER BY name ASC;

-- name: ListActiveAgentsByRuntimeForUpdate :many
-- FOR UPDATE variant used inside the cascade-delete transaction. Locks
-- each currently-active agent row so a concurrent archive/move of one
-- of those rows blocks until our transaction commits. Pair with
-- LockAgentRuntime, which holds the runtime row exclusively to also
-- block FK-validated INSERTs / runtime_id updates that would otherwise
-- add a new agent to the runtime mid-cascade. Together they guarantee
-- that the set we compared against expected_active_agent_ids is stable.
SELECT * FROM agent
WHERE runtime_id = $1 AND archived_at IS NULL AND kind = 'user'
ORDER BY name ASC
FOR UPDATE;

-- name: ListUserAgentIDsByRuntime :many
-- Non-locking companion to ListUserAgentsByRuntimeForUpdate, for callers that
-- must reason about retention GC without taking the teardown's locks.
--
-- Archived rows are included deliberately, and that is the whole point: an
-- archived agent can still own a non-terminal task, and gcRuntime counts those
-- before it will delete a runtime. A read that filtered them would report a
-- runtime as reclaimable when the sweeper is going to skip it.
SELECT id FROM agent
WHERE runtime_id = $1 AND kind = 'user'
ORDER BY id;

-- name: ListUserAgentsByRuntimeForUpdate :many
-- Locks active AND archived user agents before a runtime teardown. Locking only
-- the active snapshot leaves a restore race: an archived row can become active
-- after confirmation and then be silently unbound. Runtime/profile delete
-- locks the parent runtime first (blocking new FK references), then calls this
-- in stable ID order (blocking restore/archive/move on existing rows).
SELECT * FROM agent
WHERE runtime_id = $1 AND kind = 'user'
ORDER BY id
FOR UPDATE;

-- name: ListNonOwnerAgentsByRuntime :many
-- Returns bound agents whose owners lose access under the desired shared_with
-- list, including hidden Builder execution carriers. Runtime-owner agents keep
-- their binding.
SELECT * FROM agent
WHERE runtime_id = @runtime_id
  AND owner_id IS DISTINCT FROM @runtime_owner_id
  AND NOT COALESCE(owner_id = ANY(@shared_with::uuid[]), false)
ORDER BY name ASC;

-- name: ListNonOwnerAgentsByRuntimeForUpdate :many
-- Runtime access-change locking serializes new bindings. NOWAIT avoids waiting
-- behind a writer that already holds an agent and needs the runtime row.
SELECT * FROM agent
WHERE runtime_id = @runtime_id
  AND owner_id IS DISTINCT FROM @runtime_owner_id
  AND NOT COALESCE(owner_id = ANY(@shared_with::uuid[]), false)
ORDER BY id
FOR UPDATE NOWAIT;

-- name: RestoreAgent :one
UPDATE agent SET archived_at = NULL, archived_by = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListAgentTasks :many
SELECT * FROM agent_task_queue
WHERE agent_id = @agent_id
  -- Apply visibility before LIMIT so hidden fallbacks cannot end a page early.
  -- Keep this predicate in sync with handler.visibleTaskHistory.
  AND NOT (escalation_for_task_id IS NOT NULL AND started_at IS NULL
           AND status IN ('deferred', 'cancelled'))
  AND (created_at, id) < (@before_created_at::timestamptz, @before_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @page_limit;

-- name: CreateAgentTask :one
-- Fenced against workspace teardown: lock_task_owner_rows (migration 284)
-- locks the owners' workspace rows in the writer's own transaction and returns
-- false once they are gone, so this statement writes no row instead of stranding
-- a task in a workspace that has just been deleted (MUL-5999).
-- head_sha stamps the commit under review into the task's context JSONB so the
-- reviewer-loop dedup (HasPendingTaskForIssueAndAgent) can tell a pending run
-- against an OLD head apart from a fresh request against a NEW head (TEN-356).
-- Empty/absent head_sha leaves context NULL, preserving pre-TEN-356 behavior for
-- issues with no linked PR. Issue-linked tasks never hit quick-create context
-- parsing (parseQuickCreateContext short-circuits on IssueID.Valid), so this
-- key rides harmlessly alongside.
-- id is minted by the application as a UUIDv7 (pkg/dbid) so consecutive
-- enqueues cluster in a narrow contiguous primary-key range instead of
-- scattering across the B-tree. On a table with existing v4 ids, that range
-- is not necessarily the tree's right edge.
-- COALESCE keeps the column's gen_random_uuid() default reachable, so a caller
-- that passes no id still inserts — it just gets a random v4, exactly as before.
-- The same pattern is used by every INSERT listed in pkg/dbid's write table.
-- gate_head/gate_required carry the run budget's review gate (CHE-2226). A run
-- admitted as the issue's reviewer records the head in issue_review_admission in
-- this same statement, so the claim and the task row commit or roll back
-- together. When the budget is spent (gate_required) only the statement that
-- inserted the claim writes a task; a concurrent loser writes none and the
-- caller sees no row.
WITH review_claim AS (
    INSERT INTO issue_review_admission (workspace_id, issue_id, head_sha, agent_id)
    SELECT i.workspace_id, i.id, sqlc.narg('gate_head')::text, $1
    FROM issue i
    WHERE i.id = $3
      AND COALESCE(sqlc.narg('gate_head')::text, '') <> ''
      AND lock_task_owner_rows($1, $3, $2)
    ON CONFLICT (issue_id, head_sha) DO NOTHING
    RETURNING issue_id
)
INSERT INTO agent_task_queue (
    agent_id, runtime_id, issue_id, status, priority, trigger_comment_id,
    coalesced_comment_ids, trigger_summary, force_fresh_session, is_leader_task, handoff_note,
    squad_id, context, originator_user_id, accountable_user_id, runtime_mcp_overlay, runtime_connected_apps,
    originator_source, delegated_from_task_id, rule_version_id, rerun_of_task_id, trigger_evidence_kind, trigger_evidence_ref_id,
    workflow_step, pull_request_id, taint_ids, id
)
SELECT
    $1, $2, $3, 'queued', $4, sqlc.narg(trigger_comment_id),
    COALESCE(sqlc.narg(coalesced_comment_ids)::uuid[], '{}'),
    sqlc.narg(trigger_summary),
    COALESCE(sqlc.narg('force_fresh_session')::boolean, FALSE),
    COALESCE(sqlc.narg('is_leader_task')::boolean, FALSE),
    sqlc.narg(handoff_note),
    sqlc.narg(squad_id),
    CASE
        WHEN COALESCE(sqlc.narg('head_sha')::text, '') <> ''
        THEN jsonb_build_object('head_sha', sqlc.narg('head_sha')::text)
        ELSE NULL
    END,
    sqlc.narg(originator_user_id),
    sqlc.narg(accountable_user_id),
    sqlc.narg(runtime_mcp_overlay),
    sqlc.narg(runtime_connected_apps),
    sqlc.narg(originator_source),
    sqlc.narg(delegated_from_task_id),
    sqlc.narg(rule_version_id),
    sqlc.narg(rerun_of_task_id),
    sqlc.narg(trigger_evidence_kind),
    sqlc.narg(trigger_evidence_ref_id),
    -- The issue's status as this task is queued: the workflow step it works on (MUL-7420).
    (SELECT i.status FROM issue i WHERE i.id = $3),
    -- The PR whose head the review is stamped with, so a later head can find
    -- and supersede this run (CHE-2307). A replacement passes the exact PR
    -- (pull_request_id): rebuilding it from the head would pick the newest of
    -- two PRs that share one. Otherwise only a run queued while the issue is in
    -- a review step is bound: an author's run is queued at another step, and its
    -- step advancing to in_review later does not make it a review.
    COALESCE(sqlc.narg('pull_request_id')::uuid, (SELECT pr.id FROM github_pull_request pr
       JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
      WHERE ipr.issue_id = $3
        AND (SELECT i.status FROM issue i WHERE i.id = $3) IN ('in_review', 'agent_review')
        AND COALESCE(sqlc.narg('head_sha')::text, '') <> ''
        AND pr.head_sha = sqlc.narg('head_sha')::text
      ORDER BY pr.pr_updated_at DESC LIMIT 1)),
    -- The assigning task's taint (migration 816): the trigger unions in every
    -- other source, but only the request knows which task is acting.
    COALESCE(sqlc.narg('taint_ids')::text[], '{}'),
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid())
WHERE lock_task_owner_rows($1, $3, $2)
  AND (NOT COALESCE(sqlc.narg('gate_required')::boolean, FALSE) OR EXISTS (SELECT 1 FROM review_claim))
RETURNING *;

-- name: CreateDeferredChannelIssueTask :one
-- Fenced against workspace teardown: lock_task_owner_rows (migration 284)
-- locks the owners' workspace rows in the writer's own transaction and returns
-- false once they are gone, so this statement writes no row instead of stranding
-- a task in a workspace that has just been deleted (MUL-5999).
-- Channel /issue media resolves after issue creation. Persist the assigned
-- issue task up front for crash safety, but keep it inert until attachment
-- binding settles or the fire_at fallback is promoted by the normal sweeper.
INSERT INTO agent_task_queue (
    agent_id, runtime_id, issue_id, status, priority, trigger_comment_id,
    coalesced_comment_ids, trigger_summary, force_fresh_session, is_leader_task, handoff_note,
    squad_id, context, originator_user_id, accountable_user_id, runtime_mcp_overlay, runtime_connected_apps,
    originator_source, delegated_from_task_id, rule_version_id, rerun_of_task_id,
    trigger_evidence_kind, trigger_evidence_ref_id, fire_at,
    workflow_step, id
)
SELECT
    $1, $2, $3, 'deferred', $4, sqlc.narg(trigger_comment_id),
    COALESCE(sqlc.narg(coalesced_comment_ids)::uuid[], '{}'),
    sqlc.narg(trigger_summary),
    COALESCE(sqlc.narg('force_fresh_session')::boolean, FALSE),
    COALESCE(sqlc.narg('is_leader_task')::boolean, FALSE),
    sqlc.narg(handoff_note),
    sqlc.narg(squad_id),
    jsonb_strip_nulls(jsonb_build_object(
        'head_sha', NULLIF(COALESCE(sqlc.narg('head_sha')::text, ''), ''),
        'channel_issue_media_pending', TRUE
    )),
    sqlc.narg(originator_user_id),
    sqlc.narg(accountable_user_id),
    sqlc.narg(runtime_mcp_overlay),
    sqlc.narg(runtime_connected_apps),
    sqlc.narg(originator_source),
    sqlc.narg(delegated_from_task_id),
    sqlc.narg(rule_version_id),
    sqlc.narg(rerun_of_task_id),
    sqlc.narg(trigger_evidence_kind),
    sqlc.narg(trigger_evidence_ref_id),
    @fire_at,
    -- The issue's status as this task is queued: the workflow step it works on (MUL-7420).
    (SELECT i.status FROM issue i WHERE i.id = $3),
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid())
WHERE lock_task_owner_rows($1, $3, $2)
RETURNING *;

-- name: PromoteDeferredChannelIssueTask :one
-- Early promotion is idempotent at the service layer: a task already promoted
-- by the fire_at sweeper no longer matches and is treated as settled.
UPDATE agent_task_queue
SET status = 'queued', fire_at = NULL
WHERE id = $1 AND issue_id IS NOT NULL AND status = 'deferred'
RETURNING *;

-- name: SetDeferredChannelIssueTaskRuntimeOverlay :execrows
-- The issue and its inert media-gated task commit atomically before the optional
-- external overlay is built. Only hydrate the untouched original plan: a comment
-- merge may already have replaced the trigger, attribution, and matching overlay.
UPDATE agent_task_queue
SET runtime_mcp_overlay = sqlc.narg(runtime_mcp_overlay),
    runtime_connected_apps = sqlc.narg(runtime_connected_apps)
WHERE id = @id
  AND status = 'deferred'
  AND context->>'channel_issue_media_pending' = 'true'
  AND trigger_comment_id IS NULL
  AND originator_user_id IS NOT DISTINCT FROM sqlc.narg(expected_originator_user_id)::uuid;

-- name: CreateQuickCreateTask :one
-- Fenced against workspace teardown: lock_task_owner_rows (migration 284)
-- locks the owners' workspace rows in the writer's own transaction and returns
-- false once they are gone, so this statement writes no row instead of stranding
-- a task in a workspace that has just been deleted (MUL-5999).
-- Quick-create tasks have no issue / chat / autopilot link; the entire job
-- description (prompt, requester, workspace) lives in context JSONB. The
-- daemon detects this variant via context.type == "quick_create".
-- The requester who opened the quick-create modal is a direct_human originator
-- and accountable; attribution provenance is stamped so this path is not a
-- NULL-source enqueue bypass (MUL-4302 §2).
INSERT INTO agent_task_queue (
    agent_id, runtime_id, issue_id, status, priority, context, originator_user_id,
    accountable_user_id, runtime_mcp_overlay, runtime_connected_apps,
    originator_source, trigger_evidence_kind, trigger_evidence_ref_id,
    id
)
SELECT
    $1, $2, NULL, 'queued', $3, $4,
    sqlc.narg(originator_user_id),
    sqlc.narg(accountable_user_id),
    sqlc.narg(runtime_mcp_overlay),
    sqlc.narg(runtime_connected_apps),
    sqlc.narg(originator_source),
    sqlc.narg(trigger_evidence_kind),
    sqlc.narg(trigger_evidence_ref_id),
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid())
WHERE lock_task_owner_rows($1, NULL, $2)
  AND lock_active_project(NULLIF($4::jsonb->>'project_id','')::uuid, (SELECT a.workspace_id FROM agent a WHERE a.id=$1))
RETURNING *;

-- name: LinkTaskToIssue :exec
-- Fenced against workspace teardown: lock_task_owner_rows (migration 284)
-- locks the owners' workspace rows in the writer's own transaction and returns
-- false once they are gone, so this statement writes no row instead of stranding
-- a task in a workspace that has just been deleted (MUL-5999).
-- Attaches the issue a quick-create task produced back to the task row, once
-- the agent has finished and the issue exists. Guarded by `issue_id IS NULL`
-- so this never overwrites an issue id that was set at task creation (only
-- quick-create tasks land here unset). Fixes the activity row staying on
-- "Creating issue" forever after completion.
UPDATE agent_task_queue
SET issue_id = $2
WHERE id = $1 AND issue_id IS NULL
  AND lock_task_owner_rows(NULL, $2, NULL);

-- name: CreateRetryTask :one
-- Clone an attempt with inherited attribution, planned comments, and input-batch
-- identity. The child earns its own delivered-comment receipt at claim time.
-- Preserve a NULL chat_input_task_id for legacy/channel parents rather than
-- promoting untagged history to the task-owned input path.
--
-- Keep work_dir for every retry, but clear resume context for unsafe sessions.
-- Keep failure predicates aligned with resumeUnsafeFailureReason and resume lookups.
-- Composio uses the agent owner's connection; originator remains audit provenance.
--
-- Owner-row locks fence workspace teardown. Slot conflicts yield no row instead
-- of aborting the caller's failure transaction; pgx.ErrNoRows means commit the
-- parent transition without a retry. Pending human reruns retain their slot.
--
-- Chat retry priority keeps older input ahead of newer turns. A non-NULL fire_at
-- defers dispatch until promotion. max_attempts optionally persists the effective
-- ceiling; it does not re-enable tasks whose retry budget was disabled.
INSERT INTO agent_task_queue (
    agent_id, runtime_id, issue_id, chat_session_id, autopilot_run_id,
    status, priority, trigger_comment_id, coalesced_comment_ids, trigger_summary, context,
    session_id, work_dir,
    attempt, max_attempts, parent_task_id, force_fresh_session, is_leader_task,
    squad_id, originator_user_id, accountable_user_id, runtime_mcp_overlay, runtime_connected_apps,
    originator_source, delegated_from_task_id, rule_version_id,
    trigger_evidence_kind, trigger_evidence_ref_id, retry_of_task_id,
    chat_input_task_id, fire_at,
    channel_context_revision, handoff_note, workflow_step, id
)
SELECT
    p.agent_id, p.runtime_id, p.issue_id, p.chat_session_id, p.autopilot_run_id,
    CASE WHEN sqlc.narg(fire_at)::timestamptz IS NOT NULL THEN 'deferred' ELSE 'queued' END,
    CASE WHEN p.chat_session_id IS NOT NULL THEN GREATEST(p.priority, 3) ELSE p.priority END,
    p.trigger_comment_id, p.coalesced_comment_ids, p.trigger_summary, p.context,
    CASE WHEN p.failure_reason IS NOT DISTINCT FROM 'codex_semantic_inactivity' THEN NULL ELSE p.session_id END,
    p.work_dir,
    p.attempt + 1, COALESCE(sqlc.narg(max_attempts)::int, p.max_attempts), p.id,
    p.failure_reason IS NOT DISTINCT FROM 'codex_semantic_inactivity',
    p.is_leader_task,
    p.squad_id,
    p.originator_user_id,
    p.accountable_user_id,
    sqlc.narg(runtime_mcp_overlay),
    sqlc.narg(runtime_connected_apps),
    p.originator_source, p.delegated_from_task_id, p.rule_version_id,
    p.trigger_evidence_kind, p.trigger_evidence_ref_id, p.id,
    p.chat_input_task_id, sqlc.narg(fire_at),
    p.channel_context_revision,
    -- Carried for a wakeup's prompt and a workflow handoff's brief (MUL-7420);
    -- rows from before workflow_step existed may hold a retired assignment note.
    CASE WHEN p.context->>'wakeup_id' IS NOT NULL OR p.workflow_step IS NOT NULL THEN p.handoff_note END,
    -- A retry continues its parent's work, so it keeps the parent's step (MUL-7420).
    p.workflow_step,
    -- Named new_task_id, not id: $1 above is the PARENT task's id.
    COALESCE(sqlc.narg('new_task_id')::uuid, gen_random_uuid())
FROM agent_task_queue p
WHERE p.id = $1
  AND lock_task_owner_rows(p.agent_id, p.issue_id, p.runtime_id)
  AND lock_active_project(CASE WHEN p.issue_id IS NULL THEN NULLIF(p.context->>'project_id','')::uuid END, (SELECT a.workspace_id FROM agent a WHERE a.id=p.agent_id))
ON CONFLICT (issue_id, agent_id, (COALESCE(comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid))) WHERE status IN ('queued', 'dispatched')
       OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
DO NOTHING
RETURNING *;

-- name: SetTaskWorkflowStep :exec
-- Moves a run's workflow step along with a status change the run itself made
-- (MUL-7420). See migration 557.
UPDATE agent_task_queue SET workflow_step = @workflow_step WHERE id = @id;

-- name: CreateManualQuickCreateRetryTask :one
-- A human retry of an issue-less quick-create is a new direct_human run, not
-- an automatic retry. It preserves the immutable quick-create context JSON
-- (including source_context_id), but deliberately starts a fresh session and
-- records rerun_of_task_id so attribution/reporting can distinguish the human
-- action from CreateRetryTask's retry_of_task_id lineage.
--
-- The caller locks and transfers the pending issue_source_context in the same
-- transaction. lock_task_owner_rows provides the usual workspace-teardown
-- fence; a deleted workspace therefore yields pgx.ErrNoRows and no task.
INSERT INTO agent_task_queue (
    agent_id, runtime_id, status, priority, context,
    force_fresh_session, is_leader_task, squad_id,
    originator_user_id, accountable_user_id,
    runtime_mcp_overlay, runtime_connected_apps,
    originator_source, rerun_of_task_id, id
)
SELECT
    p.agent_id, p.runtime_id, 'queued', p.priority, p.context,
    TRUE, p.is_leader_task, p.squad_id,
    sqlc.arg(actor_user_id), sqlc.arg(actor_user_id),
    sqlc.narg(runtime_mcp_overlay), sqlc.narg(runtime_connected_apps),
    'direct_human', p.id, sqlc.arg(new_task_id)
FROM agent_task_queue p
WHERE p.id = sqlc.arg(source_task_id)
  AND p.status = 'failed'
  AND p.issue_id IS NULL
  AND p.chat_session_id IS NULL
  AND p.autopilot_run_id IS NULL
  AND lock_task_owner_rows(p.agent_id, p.issue_id, p.runtime_id)
  AND lock_active_project(NULLIF(p.context->>'project_id','')::uuid, (SELECT a.workspace_id FROM agent a WHERE a.id=p.agent_id))
RETURNING *;

-- name: DeleteUnstartedQuickCreateRetryTask :execrows
-- FailTask creates an automatic retry in the same transaction as the parent
-- terminal write. An issue-less source-context retry has no pending-slot
-- unique key, so a competing retry or an already-attached context can make
-- the subsequent attach-authority transfer lose after this row was inserted.
-- Remove only that still-uncommitted child and let the parent's failure commit.
DELETE FROM agent_task_queue
WHERE id = sqlc.arg(task_id)
  AND status IN ('queued', 'deferred')
  AND issue_id IS NULL
  AND chat_session_id IS NULL
  AND autopilot_run_id IS NULL;

-- name: CancelAgentTasksByIssue :many
-- Cancels every active task on the issue and returns the affected rows so the
-- caller can reconcile each agent's status and broadcast task:cancelled events
-- (#1587). Prior :exec form silently dropped that info, leaving agents stuck at
-- status="working" with no self-correction. Only issue-deletion cleanup calls
-- this now; a status flip to cancelled/done no longer does (MUL-4465).
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE issue_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelPendingTasksByIssueAndAgent :many
-- Cancels the not-yet-started tasks for a single (issue, agent) pair, so the
-- manual rerun flow can enqueue a replacement without colliding with
-- idx_one_pending_task_per_issue_agent_v2 and without dropping the rerun's own
-- attribution onto a row somebody else created.
--
-- 'running' and 'waiting_local_directory' are deliberately NOT cancelled: an
-- agent is executing in them. Neither status appears in that unique index, so a
-- fresh queued row can be inserted alongside one, and ClaimAgentTask's
-- per-(issue, agent) serialization holds the new row until the active run
-- reaches a terminal state. Cancelling them made a manual rerun kill the pass
-- the agent was still working on; interrupting an in-flight run is CancelTask's
-- job, not rerun's.
--
-- Everything that has NOT begun executing is still cleared, including deferred
-- escalations, so rerun keeps its prior "replace the pending plan" behaviour for
-- those rows.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE issue_id = $1 AND agent_id = $2
  AND status IN ('queued', 'dispatched', 'deferred')
RETURNING *;

-- name: CancelPendingTasksByIssueAndAgentInThread :many
-- Cancel only the not-yet-started plan in the selected thread. Other threads
-- retain their queues; running tasks are stopped explicitly through CancelTask.
--
-- CHE-485: excludes a row that is already the live rerun for the caller's own
-- (source task, actor) lineage. Without this, a caller that lost the
-- idx_one_live_rerun_per_source_task_actor race on a PRIOR attempt but is
-- retrying the pending-slot reclaim would cancel the very winner it is about
-- to look up and return — the clear and the lineage check are separate
-- statements, so excluding the winner here is what makes the clear safe to
-- run before that check, instead of racing it.
--
-- The lineage columns compare with IS NOT DISTINCT FROM, not "=": an
-- original (non-rerun) task has rerun_of_task_id NULL, and "=" against a
-- NULL column yields SQL NULL rather than FALSE, which propagates through
-- the AND chain and makes the outer NOT(...) NULL too — a NULL WHERE
-- predicate drops the row from the result just like FALSE would, so the
-- ordinary pending original task was being silently excluded from its own
-- cancellation. IS NOT DISTINCT FROM treats NULL = NULL as true equality
-- instead of unknown, so a non-rerun row never matches a non-NULL protect
-- param and stays eligible for cancellation as before.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE issue_id = $1 AND agent_id = $2
  AND status IN ('queued', 'dispatched', 'deferred')
  AND comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(sqlc.narg('thread_comment_id')::uuid)
  AND NOT (
    sqlc.narg('protect_source_task_id')::uuid IS NOT NULL
    AND sqlc.narg('protect_actor_user_id')::uuid IS NOT NULL
    AND rerun_of_task_id IS NOT DISTINCT FROM sqlc.narg('protect_source_task_id')::uuid
    AND originator_user_id IS NOT DISTINCT FROM sqlc.narg('protect_actor_user_id')::uuid
  )
RETURNING *;

-- name: CancelAgentTasksByAgent :many
-- Bulk-cancel every active (queued/dispatched/running) task for an agent.
-- Returns the affected rows so callers can broadcast task:cancelled events.
-- Mirrors the shape of CancelAgentTasksByIssue / CancelPendingTasksByIssueAndAgent
-- (also :many + RETURNING + completed_at) so the three sibling cancel paths
-- behave consistently.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE agent_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelAgentTasksByTriggerComment :many
-- Cancels active tasks whose planned batch contains the edited/deleted comment.
-- The body may already have been embedded as either the primary trigger or a
-- coalesced input; cancellation prevents an agent from acting on a stale or
-- deleted version. Must run before deletion clears trigger_comment_id.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL,
    context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('comment_change_cancelled_task_id', id::text)
WHERE (trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids))
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelAgentTasksByChatSession :many
-- Cancels active tasks belonging to a chat session. Called from
-- DeleteChatSession so the daemon doesn't keep running work whose result
-- has nowhere to land. Must run BEFORE the chat_session row is deleted —
-- the FK ON DELETE SET NULL would otherwise nullify chat_session_id and we
-- could no longer reach those tasks.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE chat_session_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: GetAgentTask :one
SELECT * FROM agent_task_queue
WHERE id = $1;

-- name: HasRecentProviderServerFailure :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE agent_id = sqlc.arg('agent_id')
      AND status = 'failed'
      AND failure_reason = 'agent_error.provider_server_error'
      AND completed_at > sqlc.arg('failed_since')::timestamptz
);

-- name: GetAgentTaskStatus :one
-- Hot-path status polling needs only the task status and the owning agent's
-- workspace for authorization. Keep this independent of optional source links
-- (issue, chat session, autopilot run) so it needs no source-entity lookup.
-- The bound runtime's workspace / daemon / owner ride along (LEFT JOIN: a
-- terminal task outlives a deleted runtime) so the daemon route can enforce
-- runtime ownership without a second round trip (MUL-8154).
SELECT atq.status, a.workspace_id, atq.runtime_id,
       rt.workspace_id AS runtime_workspace_id,
       rt.daemon_id AS runtime_daemon_id,
       rt.owner_id AS runtime_owner_id
FROM agent_task_queue atq
JOIN agent a ON a.id = atq.agent_id
LEFT JOIN agent_runtime rt ON rt.id = atq.runtime_id
WHERE atq.id = $1;

-- name: GetAgentTaskForDelegatedFailureUpdate :one
-- Serializes the idempotent delegated-failure recovery signal for one failed
-- task. FailTask and the stale-task sweepers can converge on the same row; the
-- lock ensures they cannot create two recovery comments/tasks for it.
SELECT * FROM agent_task_queue
WHERE id = $1
FOR UPDATE;

-- name: HasRetryTaskForParent :one
-- Defense-in-depth for the recovery path: a delegated failure with any retry
-- child is still intermediate and must not wake its coordinator.
SELECT count(*) > 0
FROM agent_task_queue
WHERE parent_task_id = $1
  AND status <> 'cancelled';

-- name: GetAgentTaskInWorkspace :one
-- Loads a task only when its owning agent lives in the given workspace.
-- agent_id is NOT NULL on every task row (and ON DELETE CASCADE, so the agent
-- always exists), which makes this the universal tenant guard for
-- user-initiated cancellation — independent of which optional source FK
-- (issue / chat_session / autopilot_run) happens to be set. It is what lets
-- run_only autopilot tasks and quick_create tasks (whose issue does not exist
-- yet) be cancelled at all, instead of 404-ing on a missing source FK.
SELECT atq.* FROM agent_task_queue atq
JOIN agent a ON a.id = atq.agent_id
WHERE atq.id = $1 AND a.workspace_id = $2;

-- name: ClaimAgentTask :one
-- Claims the next queued task for an agent on one healthy runtime, enforcing
-- per-(issue, agent) serialization:
-- a task is only claimable when no other task for the same issue AND same agent is
-- already dispatched or running. This allows different agents to work on the same
-- issue in parallel while preventing a single agent from running duplicate tasks.
-- Chat tasks (issue_id IS NULL) use chat_session_id for serialization instead.
-- Quick-create tasks have no issue / chat / autopilot link, so they serialize on
-- "any other quick-create-shaped task" (all four FKs NULL) for the same agent —
-- otherwise a user mashing the create button could fire concurrent quick-creates
-- whose completion lookup would race over "most recent issue by this agent".
UPDATE agent_task_queue
SET status = 'dispatched',
    dispatched_at = now(),
    preset_name = NULL, preset_source = NULL, preset_skips = NULL,
    claimed_model = NULL, claimed_thinking_level = NULL,
    claimed_service_tier = NULL, claimed_context_args = NULL,
    prepare_lease_expires_at = now() + make_interval(secs => @prepare_lease_secs::double precision),
    chat_compaction_epoch = COALESCE(
        chat_compaction_epoch,
        (SELECT cs.compaction_epoch FROM chat_session cs WHERE cs.id = agent_task_queue.chat_session_id)
    )
WHERE id = (
    SELECT atq.id FROM agent_task_queue atq
    WHERE atq.agent_id = @agent_id
      AND atq.runtime_id = @runtime_id
      AND atq.status = 'queued'
      AND (atq.context->>'wakeup_id' IS NULL OR EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.id=(atq.context->>'wakeup_id')::uuid AND w.disabled_at IS NULL AND w.revision=(atq.context->>'wakeup_revision')::bigint))
      AND EXISTS (
          SELECT 1
          FROM agent a
          JOIN agent_runtime r ON r.id = atq.runtime_id
          WHERE a.id = atq.agent_id
            -- A task's persisted runtime is not authority after an agent rebind.
            AND a.runtime_id = atq.runtime_id
            -- Queued private-runtime rows are claimable so the handler can
            -- settle an owner mismatch through the existing FailTask path
            -- before daemon delivery. Public runtimes remain shareable across
            -- agent owners; dispatched reclaim keeps its owner fence below.
            AND (
                r.visibility = 'public'
                OR r.visibility = 'private'
            )
            AND r.status = 'online'
            AND COALESCE(r.last_seen_at, r.updated_at) >=
                now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
      AND NOT EXISTS (
          SELECT 1 FROM agent_task_queue active
          WHERE active.agent_id = atq.agent_id
            AND active.status IN ('dispatched', 'running', 'waiting_local_directory')
            AND (
              (atq.issue_id IS NOT NULL AND active.issue_id = atq.issue_id)
              OR (atq.chat_session_id IS NOT NULL AND active.chat_session_id = atq.chat_session_id)
              OR (
                atq.issue_id IS NULL
                AND atq.chat_session_id IS NULL
                AND atq.autopilot_run_id IS NULL
                AND active.issue_id IS NULL
                AND active.chat_session_id IS NULL
                AND active.autopilot_run_id IS NULL
              )
            )
      )
    ORDER BY atq.priority DESC, atq.created_at ASC, atq.id ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: SetTaskDeliveredCommentIDs :one
-- Replace (rather than append to) the delivery receipt for this claim. A stale
-- dispatched task may be reclaimed by a daemon with different capabilities,
-- so only the ids embedded in the newest response count as delivered. The CAS
-- keeps a stale handler from writing after execution starts, and the subset
-- guard prevents acknowledging an id outside the task's enqueue-time plan.
UPDATE agent_task_queue
SET delivered_comment_ids = @delivered_comment_ids::uuid[]
WHERE id = @task_id
  AND runtime_id = @runtime_id
  AND status = 'dispatched'
  AND started_at IS NULL
  AND dispatched_at = @dispatched_at
  AND trigger_comment_id IS NOT DISTINCT FROM sqlc.narg(expected_trigger_comment_id)::uuid
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(@delivered_comment_ids::uuid[]) AS delivered(id)
      WHERE delivered.id IS NULL
         OR (
             delivered.id IS DISTINCT FROM trigger_comment_id
             AND NOT (delivered.id = ANY(coalesced_comment_ids))
         )
  )
RETURNING delivered_comment_ids;

-- name: SetTaskIssueSnapshot :exec
-- Record the comparable issue state this claim's payload was built from, so the
-- NEXT run this agent takes on the issue can be told whether the issue itself
-- moved. Written for every issue-bound claim, not just comment-backed ones: an
-- assignment run that skips this leaves the following run with no baseline to
-- compare against, which reads as "not compared" and costs an extra issue read.
-- Same CAS as SetTaskDeliveredCommentIDs so a stale handler cannot overwrite a
-- newer reclaim's snapshot, or write one after execution has started.
UPDATE agent_task_queue
SET issue_snapshot = @issue_snapshot
WHERE id = @task_id
  AND runtime_id = @runtime_id
  AND status = 'dispatched'
  AND started_at IS NULL
  AND dispatched_at = @dispatched_at;

-- name: RequeueAgentTaskAfterClaimFailure :one
-- Claim finalization (task token + optional comment receipt) failed before any
-- response bytes were written. Return only that exact claim generation to the
-- queue so another poll can retry immediately instead of waiting for stale
-- dispatch recovery. The dispatched_at CAS prevents an old handler from
-- rolling back a newer reclaim.
UPDATE agent_task_queue
SET status = 'queued',
    dispatched_at = NULL,
    preset_name = NULL, preset_source = NULL, preset_skips = NULL,
    claimed_model = NULL, claimed_thinking_level = NULL,
    claimed_service_tier = NULL, claimed_context_args = NULL,
    prepare_lease_expires_at = NULL,
    delivered_comment_ids = '{}'
WHERE id = @task_id
  AND runtime_id = @runtime_id
  AND status = 'dispatched'
  AND started_at IS NULL
  AND dispatched_at = @dispatched_at
RETURNING *;

-- name: ReclaimStaleDispatchedTaskForRuntime :one
-- Re-delivers a task whose previous claim likely succeeded server-side but
-- whose response never reached the daemon. The task is still in `dispatched`
-- with no `started_at`, so the daemon has not acknowledged it via StartTask.
-- Refresh dispatched_at so the server-side dispatch timeout measures from the
-- recovered delivery attempt.
UPDATE agent_task_queue
SET dispatched_at = now(),
    preset_name = NULL, preset_source = NULL, preset_skips = NULL,
    claimed_model = NULL, claimed_thinking_level = NULL,
    claimed_service_tier = NULL, claimed_context_args = NULL,
    prepare_lease_expires_at = now() + make_interval(secs => @prepare_lease_secs::double precision)
WHERE id = (
    SELECT atq.id FROM agent_task_queue atq
    WHERE atq.runtime_id = $1
      AND atq.status = 'dispatched'
      AND atq.started_at IS NULL
      AND atq.dispatched_at < now() - make_interval(secs => @claim_recovery_secs::double precision)
      AND (atq.prepare_lease_expires_at IS NULL OR atq.prepare_lease_expires_at < now())
      AND EXISTS (
          -- Keep the dispatched-reclaim owner fence intentionally stricter
          -- than the queued claim carve-out below.
          SELECT 1
          FROM agent a
          JOIN agent_runtime r ON r.id = atq.runtime_id
          WHERE a.id = atq.agent_id
            AND a.runtime_id = atq.runtime_id
            AND (
                r.visibility = 'public'
                OR (
                    r.visibility = 'private'
                    AND (
                        r.owner_id IS NULL
                        OR a.owner_id IS NULL
                        OR r.owner_id = a.owner_id
                        OR a.owner_id = ANY(r.shared_with)
                    )
                )
            )
            AND r.status = 'online'
            AND COALESCE(r.last_seen_at, r.updated_at) >=
                now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
    ORDER BY atq.priority DESC, atq.dispatched_at ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ReclaimStaleDispatchedTasksForRuntimes :many
-- Batch variant of ReclaimStaleDispatchedTaskForRuntime (MUL-4257): re-delivers
-- up to @max_tasks tasks across the whole runtime set in one round trip, so a
-- machine-level batch claim recovers lost-response dispatches for every runtime
-- it hosts without one query per runtime. Same eligibility as the singular
-- query (dispatched, never started, past the recovery window, expired/absent
-- prepare lease) and the same dispatched_at refresh; only the runtime filter
-- (= ANY) and the LIMIT (max_tasks instead of 1) differ.
-- MATERIALIZED pins the candidate set so nested-loop rescans cannot exceed
-- max_tasks.
WITH candidates AS MATERIALIZED (
    SELECT atq.id FROM agent_task_queue atq
    WHERE atq.runtime_id = ANY(@runtime_ids::uuid[])
      AND atq.status = 'dispatched'
      AND atq.started_at IS NULL
      AND atq.dispatched_at < now() - make_interval(secs => @claim_recovery_secs::double precision)
      AND (atq.prepare_lease_expires_at IS NULL OR atq.prepare_lease_expires_at < now())
      AND EXISTS (
          -- Keep the dispatched-reclaim owner fence intentionally stricter
          -- than the queued claim carve-out below.
          SELECT 1
          FROM agent a
          JOIN agent_runtime r ON r.id = atq.runtime_id
          WHERE a.id = atq.agent_id
            AND a.runtime_id = atq.runtime_id
            AND (
                r.visibility = 'public'
                OR (
                    r.visibility = 'private'
                    AND (
                        r.owner_id IS NULL
                        OR a.owner_id IS NULL
                        OR r.owner_id = a.owner_id
                        OR a.owner_id = ANY(r.shared_with)
                    )
                )
            )
            AND r.status = 'online'
            AND COALESCE(r.last_seen_at, r.updated_at) >=
                now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
    ORDER BY atq.priority DESC, atq.dispatched_at ASC
    LIMIT @max_tasks::int
    FOR UPDATE SKIP LOCKED
)
UPDATE agent_task_queue
SET dispatched_at = now(),
    preset_name = NULL, preset_source = NULL, preset_skips = NULL,
    claimed_model = NULL, claimed_thinking_level = NULL,
    claimed_service_tier = NULL, claimed_context_args = NULL,
    prepare_lease_expires_at = now() + make_interval(secs => @prepare_lease_secs::double precision)
WHERE id IN (SELECT id FROM candidates)
RETURNING *;

-- name: ExtendAgentTaskPrepareLease :one
-- Keeps a dispatched task protected while the daemon resolves/cache/materializes
-- startup inputs before StartTask. Once the daemon stops extending this short
-- lease, the stale-dispatched reclaim path can recover the task without waiting
-- for a long global recovery window.
UPDATE agent_task_queue
SET prepare_lease_expires_at = now() + make_interval(secs => @lease_secs::double precision)
WHERE id = $1
  AND runtime_id = $2
  AND status IN ('dispatched', 'waiting_local_directory')
  AND started_at IS NULL
RETURNING *;

-- name: StartAgentTask :one
-- Legacy single-winner transition; generation-aware callers lock the claim first.
-- Transitions a task to running. Accepts either 'dispatched' (the normal
-- claim → run flow) or 'waiting_local_directory' (the daemon held the row in
-- a wait state while another task owned the local_directory path lock; once
-- the lock was acquired the daemon flips here). wait_reason is cleared on
-- the transition so a future read can't conflate "currently waiting" with
-- "previously waited".
UPDATE agent_task_queue
SET status = 'running',
    started_at = now(),
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
WHERE agent_task_queue.id = $1 AND agent_task_queue.status IN ('dispatched', 'waiting_local_directory')
RETURNING *;

-- name: LockAgentTaskStartClaim :one
-- Serialize start/replay with reclaim and cancellation. A stale delivery must
-- never start or acknowledge a newer claim, even on the same runtime.
SELECT * FROM agent_task_queue
WHERE id = $1 AND runtime_id = $2 AND dispatched_at = $3
  AND status IN ('dispatched', 'waiting_local_directory', 'running')
FOR UPDATE;

-- name: LockAgentTaskLegacyStart :one
SELECT * FROM agent_task_queue
WHERE id = $1 AND status IN ('dispatched', 'waiting_local_directory')
FOR UPDATE;

-- name: MarkAgentTaskWaitingLocalDirectory :one
-- Transitions a freshly-dispatched task into 'waiting_local_directory' while
-- the daemon waits for another in-flight task to release the path lock on a
-- project_resource of type local_directory. wait_reason carries a short
-- human-readable hint (typically the contested path) that the UI surfaces
-- alongside the status.
--
-- The CHECK only allows the transition from 'dispatched' so a daemon can't
-- mark an already-running or terminal task as waiting; the StartAgentTask
-- mutation handles the reverse transition once the lock is acquired.
UPDATE agent_task_queue
SET status = 'waiting_local_directory',
    wait_reason = $2,
    prepare_lease_expires_at = now() + make_interval(secs => @prepare_lease_secs::double precision)
WHERE id = $1 AND status = 'dispatched'
RETURNING *;

-- name: CompleteAgentTask :one
-- session_rollout_missing (MUL-5305): when true the daemon withheld this task's
-- Codex session because its rollout was never written to the store. Forcing
-- session_id NULL and flagging the row happen in THIS terminal transaction so an
-- auto-retry created and woken by the same commit can never observe the bad
-- pointer or a missing gap flag.
--
-- retired_session_id (GH #6066): a session this run abandoned as unresumable.
-- Recorded even on the completed path, because that is exactly the case the
-- resume lookups could not see — a fresh-session retry that SUCCEEDS still has
-- to retire the transcript it retried away from, or an older completed row
-- pointing at the same id resurrects it on the next run.
UPDATE agent_task_queue
SET status = 'completed', completed_at = now(), result = $2,
    session_id = CASE WHEN sqlc.arg('session_rollout_missing') THEN NULL ELSE $3 END,
    work_dir = $4,
    durable_work_dir = COALESCE(sqlc.narg('durable_work_dir'), durable_work_dir),
    branch_name = COALESCE(sqlc.narg('branch_name'), branch_name),
    session_rollout_missing = sqlc.arg('session_rollout_missing'),
    retired_session_id = COALESCE(sqlc.narg('retired_session_id'), retired_session_id),
    context_tokens = sqlc.narg('context_tokens'),
    context_window = sqlc.narg('context_window'),
    prepare_lease_expires_at = NULL
WHERE id = $1 AND status = 'running'
RETURNING *;

-- name: GetLastTaskSession :one
-- Find resume context for an agent/issue from completed, failed, or cancelled
-- execution tasks. Manual rerun uses its exact source task instead; its fresh
-- flag also protects older claim handlers during mixed-version deployment.
--
-- Reduce to the newest terminal row per session before applying safety filters;
-- otherwise an older healthy row could revive the same poisoned session.
-- Exclude explicitly retired sessions even when clean older rows still name them.
-- Triage sessions must never cross into execution or vice versa.
--
-- Keep failure-reason and text guards aligned with GetLastChatTaskSession and
-- resumeUnsafeFailureReason. Text guards protect legacy or unclassified rows.
-- Resume overflow can leave no session ID, so its timestamp excludes earlier
-- sessions until a later healthy terminal row restores eligibility.
WITH retired_sessions AS (
    SELECT DISTINCT r.retired_session_id AS session_id
    FROM agent_task_queue r
    WHERE r.agent_id = $1 AND r.issue_id = $2
      AND COALESCE(r.context->>'type', '') <> 'triage'
      AND r.retired_session_id IS NOT NULL
), resume_overflow_at AS (
    SELECT MAX(COALESCE(t.completed_at, t.started_at, t.dispatched_at, t.created_at)) AS at
    FROM agent_task_queue t
    WHERE t.agent_id = $1 AND t.issue_id = $2
      AND COALESCE(t.context->>'type', '') <> 'triage'
      AND t.status = 'failed'
      AND (
        COALESCE(t.failure_reason, '') = 'codex_resume_oversized'
        OR COALESCE(t.error, '') ILIKE '%response protection is unavailable%'
        OR (COALESCE(t.error, '') ILIKE '%thread/resume failed%' AND COALESCE(t.error, '') ILIKE '%token too long%')
      )
), latest_per_session AS (
    SELECT DISTINCT ON (t.session_id)
        t.session_id, t.work_dir, t.runtime_id, t.status, t.failure_reason, t.error,
        t.started_at, t.completed_at, t.issue_snapshot, t.context_tokens,
        COALESCE(t.completed_at, t.started_at, t.dispatched_at, t.created_at) AS terminal_at
    FROM agent_task_queue t
    WHERE t.agent_id = $1 AND t.issue_id = $2
      AND COALESCE(t.context->>'type', '') <> 'triage'
      AND t.session_id IS NOT NULL
      AND t.status IN ('completed', 'failed', 'cancelled')
    ORDER BY t.session_id, COALESCE(t.completed_at, t.started_at, t.dispatched_at, t.created_at) DESC
)
-- status, started_at and issue_snapshot ride along because the row this query
-- picks IS the run whose context the next turn continues, and both of a claim's
-- deltas must be measured from THAT run rather than from whichever run started
-- last (MUL-7344). status is what says the run actually delivered its prompt to
-- the provider: this query deliberately accepts failed and cancelled rows so
-- their SESSION stays resumable, but such a row may have died before the agent
-- ever ran, and its snapshot would then describe an issue the session never
-- saw. The two are not always the same row: this query skips poisoned
-- and retired sessions, so it can legitimately return an OLDER run than the
-- newest one. Measuring against the newest one would then tell an agent whose
-- resumed memory predates an edit that the issue is unchanged.
-- context_tokens is that run's last prompt size, which tells a claim whether the
-- session is too large to keep resuming (leader session compaction, CHE-1983).
-- completed_at (with started_at as the fallback) dates when the session last
-- went idle, which tells a claim whether its prompt cache has expired (CHE-2567).
SELECT session_id, work_dir, runtime_id, status, started_at, completed_at, issue_snapshot, context_tokens FROM latest_per_session
WHERE session_id NOT IN (SELECT session_id FROM retired_sessions)
  AND (
    status IN ('completed', 'cancelled')
    OR (
      status = 'failed'
      AND COALESCE(failure_reason, '') NOT IN ('iteration_limit', 'agent_fallback_message', 'api_invalid_request', 'codex_semantic_inactivity', 'agent_error.context_overflow', 'codex_resume_oversized', 'opencode_clock_skew')
      AND NOT (COALESCE(error, '') ILIKE '%400%' AND COALESCE(error, '') ILIKE '%invalid_request_error%')
      AND NOT (COALESCE(error, '') ILIKE '%response protection is unavailable%')
      AND NOT (COALESCE(error, '') ILIKE '%agent executor error%' AND COALESCE(error, '') ILIKE '%requested entity was not found%')
      AND NOT (COALESCE(error, '') ILIKE '%improperly formatted function call%')
      AND NOT (COALESCE(error, '') ILIKE '%image dimensions exceed max allowed size%' AND COALESCE(error, '') ILIKE '%image.source.base64.data%')
      -- A provider credential-resolution failure ("Could not resolve
      -- authentication method...") is deterministic on resume: the missing
      -- api_key / auth_token / header is baked into the session's provider
      -- state, so replaying it reproduces the same provider error forever. It
      -- is classified agent_error.unknown (resume-safe), so this text guard is
      -- the only thing that keeps a wedged issue from resuming the same dead
      -- session on its next trigger — there is no daemon upgrade to wait for.
      -- Keep in sync with ResumeUnsafeFailure and GetLastChatTaskSession.
      -- The phrase itself lives in taskfailure.AuthMethodUnresolved, which the
      -- daemon's in-turn fresh-session retry reads (GH #6777). This guard stays
      -- because it is the only protection for rows an older daemon wrote.
      AND NOT (COALESCE(error, '') ILIKE '%could not resolve authentication method%')
      AND NOT (COALESCE(error, '') ~* 'must not be empty|must be non-?empty|must have non-?empty|non-?empty content|cannot be empty|should not be empty'
               AND COALESCE(error, '') ~* 'role[^a-z0-9]{0,2}assistant|assistant message|message at position|messages\.[0-9]|messages\[[0-9]')
      -- MUL-8188: a runtime that names its own transcript invalid (Kiro panics
      -- with "invalid conversation history received" on every prompt) replays
      -- the same failure on every resume. Mirrors taskfailure.UnresumableHistory.
      AND NOT (COALESCE(error, '') ILIKE '%invalid conversation history%')
    )
  )
  -- MUL-5722: a resume that overflowed the reader names no session, so it can
  -- only be excluded by time, not by matching the failed row. Drop every
  -- session whose last terminal activity predates the newest such failure: one
  -- of them IS the oversized thread, and the row that would tell us which is
  -- exactly the row that could not be written. Anything that terminated after
  -- the overflow is the fresh thread that replaced it, so this un-blocks itself
  -- as soon as one succeeds.
  AND (
    (SELECT at FROM resume_overflow_at) IS NULL
    OR terminal_at > (SELECT at FROM resume_overflow_at)
  )
ORDER BY terminal_at DESC
LIMIT 1;

-- name: InheritSessionTaint :one
-- Resuming a session continues its transcript, so the claimed task takes the
-- taint of every task that ran in it (CHE-2216 plan 3.7). The trigger on
-- agent_task_queue keeps what the task already had.
UPDATE agent_task_queue t
SET taint_ids = COALESCE((
    SELECT array_agg(DISTINCT x ORDER BY x)
    FROM agent_task_queue s, unnest(s.taint_ids) AS x
    WHERE s.agent_id = t.agent_id AND s.issue_id = t.issue_id AND s.session_id = sqlc.arg(session_id)::text
), '{}')
WHERE t.id = $1
RETURNING t.taint_ids;

-- name: GetLatestTaskRolloutMissing :one
-- Reports whether the most recent terminal task for (agent_id, issue_id)
-- withheld its Codex session because the rollout was missing (MUL-5305). When
-- true, GetLastTaskSession fell back to an older session, so the next run must
-- disclose that the most recent turn's context could not be carried over. Any
-- later task that records a real session resets this to FALSE by being the new
-- most-recent row, so the disclosure fires once and then clears.
--
-- Triage runs are excluded for the same reason GetLastTaskSession excludes them
-- (MUL-7189 §5.6), and the two must agree: this query only reports whether THAT
-- one fell back. A triage run is invisible to it, so a triage run reported here
-- would tell the first execution run after accept that the previous turn's
-- context could not be carried over — about a turn it was never entitled to.
SELECT COALESCE(session_rollout_missing, FALSE) FROM agent_task_queue
WHERE agent_id = $1 AND issue_id = $2
  AND COALESCE(context->>'type', '') <> 'triage'
  AND status IN ('completed', 'failed')
  AND started_at IS NOT NULL
ORDER BY COALESCE(completed_at, started_at, dispatched_at, created_at) DESC
LIMIT 1;

-- name: GetLatestChatTaskRolloutMissing :one
-- Chat-session counterpart of GetLatestTaskRolloutMissing (MUL-5305): reports
-- whether the most recent terminal task on this chat session withheld its Codex
-- session because the rollout was missing. When true the next chat claim resumed
-- an older session (or none), so it must disclose the continuity gap.
SELECT COALESCE(session_rollout_missing, FALSE) FROM agent_task_queue
WHERE chat_session_id = sqlc.arg('chat_session_id')
  AND (
    sqlc.narg('channel_context_revision')::bigint IS NULL
    OR COALESCE(channel_context_revision, 1) = sqlc.narg('channel_context_revision')::bigint
  )
  AND status IN ('completed', 'failed')
  AND started_at IS NOT NULL
ORDER BY COALESCE(completed_at, started_at, dispatched_at, created_at) DESC
LIMIT 1;

-- name: FailAgentTask :one
-- Marks a task as failed. session_id and work_dir are merged via COALESCE so
-- if the agent already established a real session before failing (e.g. it
-- crashed mid-conversation, was cancelled, or hit a tool error) the resume
-- pointer is preserved on the task row. The next chat task can then fall
-- back to GetLastChatTaskSession and continue the conversation instead of
-- silently starting over.
--
-- failure_reason is a coarse classifier consumed by the auto-retry path;
-- 'agent_error' is the safe default when the daemon doesn't supply one.
--
-- session_rollout_missing (MUL-5305): when true the daemon withheld this task's
-- Codex session (its rollout was missing). Force session_id NULL — overriding
-- the COALESCE that would otherwise preserve a stale mid-flight pin — and flag
-- the row, in the SAME transaction that creates and wakes the auto-retry, so the
-- retry can never claim the bad pointer or miss the continuity gap.
UPDATE agent_task_queue
SET status = 'failed',
    completed_at = now(),
    error = $2,
    failure_reason = COALESCE(sqlc.narg('failure_reason'), 'agent_error'),
    session_id = CASE WHEN sqlc.arg('session_rollout_missing') THEN NULL ELSE COALESCE(sqlc.narg('session_id'), session_id) END,
    work_dir = COALESCE(sqlc.narg('work_dir'), work_dir),
    durable_work_dir = COALESCE(sqlc.narg('durable_work_dir'), durable_work_dir),
    branch_name = COALESCE(sqlc.narg('branch_name'), branch_name),
    session_rollout_missing = sqlc.arg('session_rollout_missing'),
    retired_session_id = COALESCE(sqlc.narg('retired_session_id'), retired_session_id),
    prepare_lease_expires_at = NULL
WHERE id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')
RETURNING *;

-- name: UpdateAgentTaskSession :exec
-- Pins the resume pointer mid-flight so a daemon crash leaves a usable
-- session_id/work_dir on the task row. No-op if the task is no longer
-- in dispatched/running. waiting_local_directory tasks have no session yet
-- so this query intentionally skips them.
--
-- A row the user just cancelled accepts the pin too, but only while its
-- session slot is still empty (GH #6340). The pin is asynchronous — for Codex
-- it waits for the rollout to reach the store — so a cancel landing in that
-- window used to drop the session id for good, and the cancelled turn's
-- context with it. Filling an EMPTY slot on the run's own row is exactly what
-- the mid-flight pin is for; the `session_id IS NULL` guard is what keeps this
-- from being an overwrite, and completed/failed rows stay untouchable so a
-- straggler goroutine can never contradict a terminal report.
UPDATE agent_task_queue
SET session_id = COALESCE(sqlc.narg('session_id'), session_id),
    work_dir  = COALESCE(sqlc.narg('work_dir'), work_dir)
WHERE id = $1
  AND (
    status IN ('dispatched', 'running')
    OR (status = 'cancelled' AND session_id IS NULL)
  );

-- name: RecoverOrphanedTasksForRuntime :many
-- Called by the daemon at startup. Atomically fails any dispatched/running/
-- waiting_local_directory task that the prior incarnation of this runtime
-- owned but did not finalize. Returns the failed rows so callers can hand
-- them to the auto-retry path. waiting_local_directory rows are included
-- because the daemon holding the path lock is the same process that just
-- died — without us, the row would sit waiting forever.
UPDATE agent_task_queue
SET status = 'failed',
    completed_at = now(),
    error = 'daemon restarted while task was in flight',
    failure_reason = 'runtime_recovery',
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
WHERE runtime_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory')
RETURNING *;

-- name: FailStaleTasks :many
-- Fails tasks stuck in dispatched/running beyond the given thresholds.
--
-- Each branch pairs a wall-clock deadline with a task-appropriate liveness
-- signal, so the sweeper only kills tasks whose owning daemon is no longer
-- proving it is alive:
--
--   * Dispatched: `prepare_lease_expires_at` is refreshed every 15s by the
--     daemon between claim and StartTask (see startTaskPrepareLeaseExtender).
--     A live lease excludes the row. An expired lease may fail immediately on
--     a healthy runtime, but a disconnected runtime gets the same reconnect
--     grace as a running task.
--
--   * Running: no per-task lease is renewed once StartTask fires, so we key
--     off the daemon-wide heartbeat instead — `agent_runtime.last_seen_at`,
--     which the daemon bumps every ~15s while it is up. A running task is not
--     killed until that heartbeat has been absent for the full reconnect
--     grace, even when its own wall-clock timeout elapsed earlier. This keeps
--     healthy multi-hour work alive through a network partition.
--
-- The daemon-dead case is recovered immediately when that daemon restarts via
-- RecoverOrphanedTasksForRuntime. Until then, this query and
-- FailTasksForOfflineRuntimes share the same bounded reconnect grace.
--
-- runtime_id IS NULL: a running row with no runtime is by definition not
-- proving liveness, so the wall clock is allowed to fire — same shape as
-- the legacy pure-wall-clock behavior for that (rare / historical) case.
--
-- waiting_local_directory rows are intentionally excluded: the daemon owns
-- the wait (with its own ctx-driven timeout) and a legitimate queue ahead
-- of this task can exceed the dispatch / running timeouts without being
-- "stuck". If the daemon dies, RecoverOrphanedTasksForRuntime reclaims
-- those rows at restart.
UPDATE agent_task_queue
SET status = 'failed', completed_at = now(), error = 'task timed out',
    failure_reason = 'timeout',
    prepare_lease_expires_at = NULL
WHERE (
    status = 'dispatched'
    AND dispatched_at < now() - make_interval(secs => @dispatch_timeout_secs::double precision)
    AND (prepare_lease_expires_at IS NULL OR prepare_lease_expires_at < now())
    AND (
      runtime_id IS NULL
      OR NOT EXISTS (
        SELECT 1 FROM agent_runtime r
        WHERE r.id = agent_task_queue.runtime_id
      )
      OR EXISTS (
        SELECT 1 FROM agent_runtime r
        WHERE r.id = agent_task_queue.runtime_id
          AND (
            (
              r.status = 'online'
              AND COALESCE(r.last_seen_at, r.updated_at) >=
                  now() - make_interval(secs => @runtime_stale_secs::double precision)
            )
            OR COALESCE(r.last_seen_at, r.updated_at) <
               now() - make_interval(secs => @runtime_reconnect_grace_secs::double precision)
          )
      )
    )
  )
   OR (
    status = 'running'
    AND started_at < now() - make_interval(secs => @running_timeout_secs::double precision)
    AND (
      runtime_id IS NULL
      OR NOT EXISTS (
        SELECT 1 FROM agent_runtime r
        WHERE r.id = agent_task_queue.runtime_id
          AND COALESCE(r.last_seen_at, r.updated_at) >=
              now() - make_interval(secs => @runtime_reconnect_grace_secs::double precision)
      )
    )
  )
RETURNING *;

-- name: ExpireStaleQueuedTasks :many
-- Expire queued work only when both the task's waiting age and the runtime's
-- heartbeat age exceed reconnect grace. Busy but heartbeating runtimes keep their
-- queues regardless of length. Read heartbeat age directly so a stale online
-- status cannot strand tasks. Missing runtime references are handled defensively.
--
-- runtime_offline retries have a separate reconnect expiry policy and are exempt.
-- Select bounded victims with SKIP LOCKED, then re-check status and liveness in
-- the update so concurrent claims cannot be overwritten.
WITH victims AS (
    SELECT id FROM agent_task_queue
    WHERE status = 'queued' AND context->>'wakeup_id' IS NULL
      AND created_at < now() - make_interval(secs => @reconnect_grace_secs::double precision)
      AND (
          runtime_id IS NULL
          OR NOT EXISTS (
              SELECT 1 FROM agent_runtime r WHERE r.id = agent_task_queue.runtime_id
          )
          OR EXISTS (
              SELECT 1 FROM agent_runtime r
              WHERE r.id = agent_task_queue.runtime_id
                AND COALESCE(r.last_seen_at, r.updated_at) <
                    now() - make_interval(secs => @reconnect_grace_secs::double precision)
          )
      )
      AND NOT EXISTS (
          SELECT 1 FROM agent_task_queue retry_parent
          WHERE retry_parent.id = agent_task_queue.parent_task_id
            AND retry_parent.failure_reason = 'runtime_offline'
      )
    ORDER BY created_at ASC
    LIMIT @max_per_tick::int
    FOR UPDATE SKIP LOCKED
)
UPDATE agent_task_queue t
SET status = 'failed',
    completed_at = now(),
    error = 'runtime unavailable while task was queued',
    failure_reason = 'queued_expired',
    prepare_lease_expires_at = NULL
FROM victims v
WHERE t.id = v.id
  AND t.status = 'queued'
  AND t.created_at < now() - make_interval(secs => @reconnect_grace_secs::double precision)
  AND (
      t.runtime_id IS NULL
      OR NOT EXISTS (SELECT 1 FROM agent_runtime r WHERE r.id = t.runtime_id)
      OR EXISTS (
          SELECT 1 FROM agent_runtime r
          WHERE r.id = t.runtime_id
            AND COALESCE(r.last_seen_at, r.updated_at) <
                now() - make_interval(secs => @reconnect_grace_secs::double precision)
      )
  )
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_queue retry_parent
      WHERE retry_parent.id = t.parent_task_id
        AND retry_parent.failure_reason = 'runtime_offline'
  )
RETURNING t.*;

-- name: FailExpiredRuntimeReconnectRetries :many
-- A runtime_offline retry waits in deferred until its runtime returns healthy.
-- Give that recovery state a bounded exit: after one full reconnect grace,
-- fail it with a non-retryable reason so issues, agents, and runtime GC can
-- converge. A runtime that is healthy when this query runs wins the race and
-- remains deferred for the next daemon poll to promote. The parent join is the
-- lineage discriminator; provider backoff and other deferred task types are
-- intentionally excluded.
WITH victims AS (
    SELECT retry.id
    FROM agent_task_queue retry
    JOIN agent_task_queue parent ON parent.id = retry.parent_task_id
    WHERE retry.status = 'deferred'
      AND retry.fire_at < now() - make_interval(secs => @reconnect_grace_secs::double precision)
      AND parent.failure_reason = 'runtime_offline'
      AND NOT EXISTS (
          SELECT 1 FROM agent_runtime runtime
          WHERE runtime.id = retry.runtime_id
            AND runtime.status = 'online'
            AND COALESCE(runtime.last_seen_at, runtime.updated_at) >=
                now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
    ORDER BY retry.fire_at, retry.created_at
    LIMIT @max_per_tick::int
    FOR UPDATE OF retry SKIP LOCKED
)
UPDATE agent_task_queue AS retry
SET status = 'failed',
    completed_at = now(),
    error = 'runtime did not reconnect within the configured grace period',
    failure_reason = 'runtime_reconnect_timeout',
    wait_reason = NULL,
    prepare_lease_expires_at = NULL
FROM victims
WHERE retry.id = victims.id
  AND retry.status = 'deferred'
  AND retry.fire_at < now() - make_interval(secs => @reconnect_grace_secs::double precision)
  AND EXISTS (
      SELECT 1 FROM agent_task_queue parent
      WHERE parent.id = retry.parent_task_id
        AND parent.failure_reason = 'runtime_offline'
  )
  AND NOT EXISTS (
      SELECT 1 FROM agent_runtime runtime
      WHERE runtime.id = retry.runtime_id
        AND runtime.status = 'online'
        AND COALESCE(runtime.last_seen_at, runtime.updated_at) >=
            now() - make_interval(secs => @runtime_stale_secs::double precision)
  )
RETURNING retry.*;

-- name: CancelAgentTask :one
-- Automatic cancellation without an explicit persisted failure reason. Unlike
-- CancelAgentTaskByUser, this deliberately leaves recovery inputs replayable.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelAgentTaskByUser :one
-- An explicit user cancellation is a terminal acknowledgement for any
-- delegated-failure recovery signal planned into this task. Server-initiated
-- cancellations keep using CancelAgentTask / CancelAgentTaskWithReason so a
-- stale plan, claim failure, or other automatic repair can still be replayed.
--
-- unless_assignee_* make the cancellation conditional on the issue as it stands
-- NOW: a handoff that stopped the previous assignee's run decided that for an
-- issue that had left them, and another request can hand the issue back before
-- the cancellation lands. The issue row is read FOR SHARE, so a reassignment
-- that is still uncommitted is waited for. Both null: no condition, and the
-- issue row is not touched.
WITH current_issue AS (
    SELECT i.assignee_type, i.assignee_id FROM issue i
    WHERE i.id = (SELECT t.issue_id FROM agent_task_queue t WHERE t.id = $1)
      AND sqlc.narg('unless_assignee_id')::uuid IS NOT NULL
    FOR SHARE OF i
)
UPDATE agent_task_queue AS task
SET status = 'cancelled',
    completed_at = now(),
    prepare_lease_expires_at = NULL,
    cancelled_by_type = sqlc.arg('cancelled_by_type'),
    cancelled_by_id = sqlc.narg('cancelled_by_id'),
    cancelled_by_name = sqlc.narg('cancelled_by_name'),
    delivered_comment_ids = CASE
      -- Chat and ordinary issue tasks almost never carry a delegated-failure
      -- recovery signal. Keep their high-frequency user-cancel path to a
      -- no-join update; only validate task lineage after the cheap comment
      -- shape probe finds a possible recovery signal.
      WHEN task.trigger_comment_id IS NULL
       AND COALESCE(cardinality(task.coalesced_comment_ids), 0) = 0
        THEN task.delivered_comment_ids
      WHEN NOT EXISTS (
        SELECT 1
        FROM comment recovery_signal
        WHERE (
            recovery_signal.id = task.trigger_comment_id
            OR recovery_signal.id = ANY(task.coalesced_comment_ids)
        )
          AND recovery_signal.author_type = 'system'
          AND recovery_signal.type = 'progress_update'
          AND recovery_signal.source_task_id IS NOT NULL
      ) THEN task.delivered_comment_ids
      ELSE (
        SELECT COALESCE(array_agg(DISTINCT receipt.id), '{}')::uuid[]
        FROM unnest(array_cat(
            task.delivered_comment_ids,
            ARRAY(
                SELECT recovery.id
                FROM comment recovery
                JOIN agent_task_queue failed ON failed.id = recovery.source_task_id
                JOIN agent_task_queue source ON source.id = failed.delegated_from_task_id
                WHERE (
                    recovery.id = task.trigger_comment_id
                    OR recovery.id = ANY(task.coalesced_comment_ids)
                )
                  AND recovery.author_type = 'system'
                  AND recovery.type = 'progress_update'
                  AND recovery.source_task_id IS NOT NULL
                  AND failed.status = 'failed'
                  AND failed.delegated_from_task_id IS NOT NULL
                  AND failed.autopilot_run_id IS NULL
                  AND failed.trigger_evidence_kind IS DISTINCT FROM 'delegated_failure'
                  AND source.autopilot_run_id IS NULL
                  AND source.issue_id = task.issue_id
                  AND source.agent_id = task.agent_id
                  AND recovery.issue_id = source.issue_id
            )
        )) AS receipt(id)
      )
    END
WHERE task.id = $1
  AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
  AND NOT EXISTS (
    SELECT 1 FROM current_issue
    WHERE current_issue.assignee_type = sqlc.narg('unless_assignee_type')::text
      AND current_issue.assignee_id = sqlc.narg('unless_assignee_id')::uuid
  )
RETURNING task.*;

-- name: SetAgentTaskBranchName :exec
-- Records the delivered branch on a CANCELLED task. Needed because the daemon
-- finalizes its worktree (committing whatever the agent produced) BEFORE it
-- learns the task was cancelled, so the branch exists but the cancel path has
-- no result payload to carry it. Without this the branch is real and
-- completely undiscoverable from the UI.
--
-- Never overwrites a name already recorded by a complete/fail callback, and
-- never touches any OTHER terminal state: the daemon acks on every terminal
-- status it observes, so a late or replayed ack from a stale run could
-- otherwise write its branch into a completed/failed row whose own callback —
-- the authoritative channel for those states — recorded nothing. status is
-- stable once terminal, so this CAS cannot race a legitimate write.
UPDATE agent_task_queue
SET branch_name = COALESCE(branch_name, sqlc.arg('branch_name'))
WHERE id = sqlc.arg('id') AND status = 'cancelled';

-- name: SetAgentTaskDurableWorkDir :exec
-- Records the durable replacement for a disposable worktree on a CANCELLED
-- task. The daemon only sends this after Finalize confirms the task worktree
-- is gone. As with branch_name, the status CAS rejects stale acknowledgements
-- and COALESCE makes replays idempotent.
UPDATE agent_task_queue
SET durable_work_dir = COALESCE(durable_work_dir, sqlc.arg('durable_work_dir'))
WHERE id = sqlc.arg('id') AND status = 'cancelled';

-- name: IsTaskSessionRetired :one
-- Whether any run of this agent explicitly retired session_id. The
-- manual-retry claim path resumes from its exact source task instead of
-- GetLastTaskSession / the chat pointer, so it must consult the retired set
-- itself; without this a retry of an older, resume-safe failed run revives a
-- session a later run already proved undrivable (MUL-8188). Session ids are
-- opaque runtime-issued ids, so (agent, session) identifies the transcript
-- whether the runs were issue or chat tasks.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue r
    WHERE r.agent_id = sqlc.arg('agent_id')
      AND r.retired_session_id = sqlc.arg('session_id')::text
) AS retired;

-- name: SetAgentTaskRetiredSessionIDIfCancelled :execrows
-- Cancel-ack companion to CompleteAgentTask/FailAgentTask's retired_session_id
-- (MUL-8188). A resumed run that produced no output at all before it was
-- cancelled is the signature of a session the runtime cannot drive (Kiro
-- loaded the transcript, then hung on the first prompt). Without this the
-- cancelled row kept the session resumable and every later trigger re-entered
-- the same hang. Same status CAS and never-overwrite rule as the other
-- cancel-ack writes; the row count tells the caller whether it applied.
UPDATE agent_task_queue
SET retired_session_id = COALESCE(retired_session_id, sqlc.arg('retired_session_id'))
WHERE id = sqlc.arg('id') AND status = 'cancelled';

-- name: MarkAgentTaskCancelAcked :exec
-- Records the daemon's cancel-ack as proof the runner stopped. Settles a
-- cancelled row, or a row the stop gate retained after it was rewritten
-- (stop_unconfirmed_at, set by trigger agent_task_keep_stop_unconfirmed). Any
-- other failed/completed row is a late ack from a stale run and is not
-- stamped. The first ack wins, so a replay is a no-op.
UPDATE agent_task_queue
SET cancel_acked_at = now()
WHERE id = sqlc.arg('id') AND cancel_acked_at IS NULL
  AND (status = 'cancelled' OR stop_unconfirmed_at IS NOT NULL);

-- name: ListLiveReviewRunsForPR :many
-- Live review runs of the PR stamped with a head other than the PR's current
-- one, at the step their issue is still in. Author runs carry a different
-- workflow_step and are never listed.
SELECT t.id, t.issue_id, t.agent_id FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE t.pull_request_id = sqlc.arg('pull_request_id')
  AND t.status IN ('queued', 'dispatched', 'running')
  AND t.workflow_step IN ('in_review', 'agent_review')
  AND t.workflow_step = i.status
  AND COALESCE(t.context->>'head_sha', '') <> sqlc.arg('head_sha')::text;

-- name: ListUnreleasedSupersededReviews :many
-- Superseded review runs of the issue whose single replacement has not been
-- enqueued yet, newest first. Rows of one (pull_request_id, workflow_step)
-- share one replacement. A run stays superseded after a later writer rewrites
-- its status or reason (a cleanup-unconfirmed report, /fail, a late complete):
-- the b1 trigger keeps stop_unconfirmed_at, the same identity the cancel-ack
-- settles (MarkAgentTaskCancelAcked).
SELECT t.id, t.agent_id, t.pull_request_id, t.workflow_step FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE t.issue_id = sqlc.arg('issue_id')
  AND ((t.status = 'cancelled' AND t.failure_reason = 'superseded_head') OR t.stop_unconfirmed_at IS NOT NULL)
  AND t.pull_request_id IS NOT NULL
  AND t.workflow_step IN ('in_review', 'agent_review')
  AND t.workflow_step = i.status
  AND t.replacement_released_at IS NULL
ORDER BY t.created_at DESC;

-- name: MarkReviewReplacementReleased :exec
UPDATE agent_task_queue
SET replacement_released_at = now()
WHERE issue_id = sqlc.arg('issue_id')
  AND pull_request_id = sqlc.arg('pull_request_id')
  AND workflow_step = sqlc.arg('workflow_step')
  AND ((status = 'cancelled' AND failure_reason = 'superseded_head') OR stop_unconfirmed_at IS NOT NULL)
  AND replacement_released_at IS NULL;

-- name: GetUnackedSupersededRun :one
-- The newest run of the issue cancelled by the stop gate whose runner is not yet
-- proven stopped. A superseded head holds the gate at the step the issue is in
-- now; a merged or closed PR (CHE-2298 c1) holds it whatever the step, because
-- the merge itself moves the issue on. A rewrite of such a run (for example to
-- failed/cleanup_unconfirmed) keeps the issue-wide hold via stop_hold_issue_wide;
-- any other rewritten run keeps only the step-bound hold. A run that never
-- reached a daemon (dispatched_at NULL) has no runner to wait for.
SELECT t.id FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE t.issue_id = sqlc.arg('issue_id')
  AND t.dispatched_at IS NOT NULL
  AND t.cancel_acked_at IS NULL
  AND ((t.status = 'cancelled' AND t.failure_reason IN ('pr_merged', 'pr_closed'))
       OR (t.stop_hold_issue_wide AND t.stop_unconfirmed_at IS NOT NULL)
       OR (((t.status = 'cancelled' AND t.failure_reason = 'superseded_head')
            OR t.stop_unconfirmed_at IS NOT NULL)
           AND t.workflow_step = i.status))
ORDER BY t.created_at DESC
LIMIT 1;

-- name: SetAgentTaskErrorIfEmpty :exec
-- Companion to SetAgentTaskBranchName for the cancel-ack path. A cancelled
-- worktree task whose Finalize ABORTED has no branch to deliver — the error
-- text carrying the preserved-worktree path is the only pointer to the agent's
-- work, and the cancel flow discarded the rest of the result. A user-initiated
-- cancel leaves error NULL, so filling it here never overwrites a reason
-- recorded by a fail callback or the claim gate; the status CAS keeps a late
-- ack from stamping an error onto a completed/failed row (see
-- SetAgentTaskBranchName above).
UPDATE agent_task_queue
SET error = sqlc.arg('error'),
    failure_reason = COALESCE(failure_reason, sqlc.arg('failure_reason'))
WHERE id = sqlc.arg('id') AND (error IS NULL OR error = '') AND status = 'cancelled';

-- name: CancelAgentTaskWithReason :one
-- Cancels a task AND records why, for cancellations the user did not ask for.
--
-- CancelAgentTaskByUser leaves error/failure_reason NULL because the user knows
-- why. A server-initiated refusal is the opposite: without a persisted reason
-- the run surfaces as an unexplained "cancelled", and the only trace is a 4xx
-- in a daemon log the user never sees. Retrying cannot help either (the task is
-- refused for a durable reason), so this terminal state carries its explanation.
UPDATE agent_task_queue
SET status = 'cancelled',
    completed_at = now(),
    error = sqlc.arg('error'),
    failure_reason = sqlc.arg('failure_reason'),
    prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system',
    cancelled_by_id = NULL,
    cancelled_by_name = NULL
WHERE id = sqlc.arg('id') AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
RETURNING *;

-- name: CancelQueuedAgentTask :one
-- Queue editing is a compare-and-set: never cancel a task that the daemon
-- promoted between the user's click and this statement.
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = sqlc.arg('cancelled_by_type'),
    cancelled_by_id = sqlc.narg('cancelled_by_id'),
    cancelled_by_name = sqlc.narg('cancelled_by_name')
WHERE id = sqlc.arg('id')
  AND chat_session_id = sqlc.arg('chat_session_id')
  AND status = 'queued'
RETURNING *;

-- name: CancelQueuedAgentTasksForSession :many
-- Clear only positional follow-ups. The first visible task is current even
-- when it has not been claimed yet, so an all-queued session must preserve its
-- priority/FIFO head. Keep this selector in lockstep with the visible-head
-- invariant documented above ListChatMessages in chat.sql.
WITH head AS MATERIALIZED (
  SELECT candidate.id
  FROM agent_task_queue AS candidate
  WHERE candidate.chat_session_id = $1
    AND candidate.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
    AND candidate.regenerate_quick_actions_for IS NULL
  ORDER BY
    CASE
      WHEN candidate.status IN ('dispatched', 'running', 'waiting_local_directory') THEN 0
      WHEN candidate.status = 'deferred' THEN 1
      ELSE 2
    END,
    candidate.priority DESC,
    candidate.created_at ASC,
    candidate.id ASC
  LIMIT 1
)
UPDATE agent_task_queue AS queued
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE queued.chat_session_id = $1
  AND queued.status = 'queued'
  AND queued.id IS DISTINCT FROM (SELECT id FROM head)
RETURNING queued.*;

-- name: MarkChatFinalizeDeferred :one
-- Arms the deferred chat-finalize marker for a cancelled chat task whose
-- empty-transcript judgment must wait for the daemon's flush ack (#5219).
UPDATE agent_task_queue
SET chat_finalize_deferred_at = now()
WHERE id = $1
RETURNING *;

-- name: ClaimChatFinalizeDeferred :one
-- Atomically claims the deferred marker so the daemon ack and the sweeper
-- cannot both finalize the same task (double-"Stopped." guard).
UPDATE agent_task_queue
SET chat_finalize_deferred_at = NULL
WHERE id = $1 AND chat_finalize_deferred_at IS NOT NULL
RETURNING *;

-- name: ListChatFinalizeDeferredExpired :many
-- Deferred chat finalizations whose grace period elapsed without a daemon
-- ack (dead or partitioned daemon). Batch-capped like the other sweeper
-- queries so one tick can't monopolise the DB.
SELECT * FROM agent_task_queue
WHERE chat_finalize_deferred_at IS NOT NULL
  AND chat_finalize_deferred_at < now() - make_interval(secs => @grace_secs::double precision)
ORDER BY chat_finalize_deferred_at
LIMIT @max_per_tick::int;

-- name: CountRunningTasks :one
-- waiting_local_directory remains capacity-bearing until resume admission has
-- an atomic reservation/CAS gate. Consequently a waiter-only agent is reported
-- idle by RefreshAgentStatusFromTasks but still cannot claim additional work;
-- removing it here alone could exceed max_concurrent_tasks when it resumes.
SELECT count(*) FROM agent_task_queue
WHERE agent_id = $1 AND status IN ('dispatched', 'running', 'waiting_local_directory');

-- name: GetAgentForClaimUpdate :one
SELECT * FROM agent
WHERE id = $1
FOR UPDATE;

-- name: HasTaskForIssue :one
-- Returns true if the issue has any task in any status. Webhook recovery
-- treats even a terminal task as proof that ownership moved downstream.
SELECT EXISTS (SELECT 1 FROM agent_task_queue WHERE issue_id = $1);

-- name: HasActiveTaskForIssue :one
-- Returns true if there is any queued, dispatched, waiting_local_directory,
-- or running task for the issue.
SELECT count(*) > 0 AS has_active FROM agent_task_queue
WHERE issue_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory');

-- name: HasPendingTaskForIssue :one
-- Returns true if there is a queued or dispatched (but not yet running) task for the issue.
-- Used by the coalescing queue: allow enqueue when a task is running (so
-- the agent picks up new comments on the next cycle) but skip if a pending
-- task already exists (natural dedup).
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = $1 AND status IN ('queued', 'dispatched');

-- name: HasPendingTaskForIssueAndAgent :one
-- Returns true if a specific agent already has a queued or dispatched task
-- for the given issue, or the explicitly-marked deferred task whose channel
-- media must bind before the issue agent runs. Used by @mention trigger dedup.
--
-- head_sha keys the dedup on the commit under review (TEN-356): when a caller
-- passes a non-empty head_sha, a pending task only dedups if it was stamped
-- with the SAME head_sha at enqueue time. If HEAD advanced since the pending
-- task's run began (its context head_sha differs, or predates the stamp and is
-- NULL), the dedup MISSES and a fresh review enqueues against the new HEAD.
-- When head_sha is empty/NULL (issue has no linked PR) the check falls back to
-- the pre-TEN-356 (issue_id, agent_id) key so non-PR issues keep coalescing.
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  );

-- name: HasPendingTaskForIssueAndAgentAtStep :one
-- HasPendingTaskForIssueAndAgent for a workflow handoff (MUL-7420): an
-- assignment-level run stamped for a step the issue has left does not count,
-- because its status changes would be refused as workflow_step_moved and it
-- cannot finish the step being entered. Runs tied to a comment keep counting.
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  )
  AND NOT (
    trigger_comment_id IS NULL
    AND workflow_step IS NOT NULL
    AND workflow_step <> sqlc.arg('step')::text
  );

-- name: ListStalePendingStepTasks :many
-- The runs HasPendingTaskForIssueAndAgentAtStep ignores: pending
-- assignment-level runs of one agent stamped for a step the issue has left.
-- Pending is the same set as the unique pending-task index: queued, dispatched,
-- or deferred behind channel media. A media-deferred run left out here would
-- keep the slot and make the next step's run deduplicate against it.
SELECT * FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND trigger_comment_id IS NULL
  AND workflow_step IS NOT NULL
  AND workflow_step <> sqlc.arg('step')::text;

-- name: HasQueuedRunCarryingIssueWrite :one
-- Whether a queued run of the agent can stand in for the run a write wanted
-- but lost to the pending-task unique index. A queued run has not started, so
-- it reads the issue when it is claimed. It carries the write only if it is
-- assignment-level (no wakeup, no trigger comment), keyed on the same reviewed
-- head, and stamped for the step the issue is in now. When that step hands the
-- issue off, a run without a step is not enough: the brief and the permission
-- to move the issue come from the step stamp.
SELECT count(*) > 0 AS has_queued FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
  AND context->>'wakeup_id' IS NULL
  AND trigger_comment_id IS NULL
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  )
  AND CASE
    WHEN sqlc.arg('step_hands_off')::bool THEN workflow_step = sqlc.arg('step')::text
    ELSE workflow_step IS NULL OR workflow_step = sqlc.arg('step')::text
  END;

-- name: TaintQueuedRunCarryingIssueWrite :execrows
-- HasQueuedRunCarryingIssueWrite for a write made by a tainted task: the
-- queued run that stands in for it takes the writer's taint (the task trigger
-- unions it with what the run has) and the row count says whether one exists.
UPDATE agent_task_queue SET taint_ids = sqlc.arg('taint_ids')::text[]
WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
  AND context->>'wakeup_id' IS NULL
  AND trigger_comment_id IS NULL
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  )
  AND CASE
    WHEN sqlc.arg('step_hands_off')::bool THEN workflow_step = sqlc.arg('step')::text
    ELSE workflow_step IS NULL OR workflow_step = sqlc.arg('step')::text
  END;

-- name: CancelStalePendingStepTask :one
-- Cancels one run only while it is still what ListStalePendingStepTasks
-- selected: pending, assignment-level and stamped for a step the issue has left.
-- The selection and this statement are separate, and a dispatched run the
-- daemon starts in between is work in flight that a handoff must not cancel.
-- Unlike CancelAgentTask, running is not cancellable here.
--
-- The step is also checked against the issue as it stands NOW. A request that
-- captured step B can resume after another request moved the issue to C and
-- queued the same agent's run for C; judged against B, that run looks stale.
-- The issue row is read FOR SHARE, so a move to C that is still uncommitted
-- is waited for, and the status read is the committed one.
WITH current_issue AS (
    SELECT i.status FROM issue i
    WHERE i.id = (SELECT t.issue_id FROM agent_task_queue t WHERE t.id = $1)
    FOR SHARE OF i
)
UPDATE agent_task_queue
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE agent_task_queue.id = $1
  AND EXISTS (SELECT 1 FROM current_issue WHERE current_issue.status = sqlc.arg('step')::text)
  AND context->>'wakeup_id' IS NULL
  AND trigger_comment_id IS NULL
  AND workflow_step IS NOT NULL
  AND workflow_step <> sqlc.arg('step')::text
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
RETURNING *;

-- name: HasPendingTaskForIssueAndAgentInThread :one
-- Same pending/head rules as HasPendingTaskForIssueAndAgent, scoped to one
-- root comment and all descendants. NULL selects assignment-level work.
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  )
  AND comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(sqlc.narg('thread_comment_id')::uuid);

-- name: HasPendingTaskForIssueAndAgentExcludingTriggerComment :one
-- Same as HasPendingTaskForIssueAndAgent, but ignores tasks triggered by the
-- current comment being edited. Edit preview needs this because save cancels
-- that comment's old queued/dispatched tasks before re-computing triggers.
-- Carries the same head_sha dedup key as HasPendingTaskForIssueAndAgent (TEN-356).
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = @issue_id
  AND agent_id = @agent_id
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND trigger_comment_id IS DISTINCT FROM @exclude_trigger_comment_id::uuid
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  );

-- name: HasPendingTaskForIssueAndAgentExcludingTriggerCommentInThread :one
-- Thread-scoped edit preview: ignore the comment whose old run save replaces.
SELECT count(*) > 0 AS has_pending FROM agent_task_queue
WHERE context->>'wakeup_id' IS NULL AND issue_id = @issue_id
  AND agent_id = @agent_id
  AND (
    status IN ('queued', 'dispatched')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND trigger_comment_id IS DISTINCT FROM @exclude_trigger_comment_id::uuid
  AND (
    COALESCE(sqlc.narg('head_sha')::text, '') = ''
    OR context->>'head_sha' = sqlc.narg('head_sha')::text
  )
  AND comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(sqlc.narg('thread_comment_id')::uuid);

-- name: FindLiveRerunOfTask :one
-- CHE-485: replay guard for RerunIssue. A rerun request is identified by the
-- exact source task being rerun plus the acting member — a double-click, a
-- retried HTTP request, or a duplicate webhook delivery of the SAME rerun
-- action must not cancel-and-recreate a second time. If a still-live task
-- (not yet terminal) already exists whose rerun_of_task_id points at this
-- source task and whose originator_user_id is this same actor, that row IS
-- the admitted outcome of this rerun request — return it so the caller can
-- skip straight to it instead of mutating anything again.
--
-- Scoped to non-terminal status so a rerun that already finished (or failed/
-- cancelled) is NOT treated as covering a fresh request: a failed or
-- cancelled attempt is not a fulfilled obligation, and the caller must be
-- free to rerun again for a genuinely new attempt.
SELECT * FROM agent_task_queue
WHERE rerun_of_task_id = sqlc.arg(source_task_id)
  AND originator_user_id = sqlc.arg(actor_user_id)
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY created_at DESC
LIMIT 1;

-- name: MergeCommentIntoPendingTask :one
-- MUL-4195: fold a newly-arrived comment into an existing task for (issue,
-- agent) that has NOT yet been claimed, instead of letting the
-- HasPendingTaskForIssueAndAgent dedup silently DROP it. The task's prior
-- trigger_comment_id becomes a coalesced ("also cover") comment and
-- @new_trigger_comment_id becomes the new trigger, so the injected prompt shows
-- the latest deliberate instruction while the single run is still told to
-- address every folded comment.
-- Updating trigger_comment_id takes FOR KEY SHARE on the new comment via its FK.
-- Decision delivery's preceding issue lock fences comment delete/update paths;
-- this merge changes an FK column, unlike task registration.
--
-- Target is restricted to a pre-claim task on purpose (MUL-4195 review rounds
-- 2–4). This merge is reached when HasPendingTaskForIssueAndAgent matched a
-- 'queued'/'dispatched' task, or the channel-media deferred task described
-- below. 'dispatched' is deliberately NOT a
-- target: a dispatched / waiting_local_directory / running task has already had
-- its claim response built. Folding afterward would add a planned id that is
-- absent from that response's delivered_comment_ids receipt; completion
-- reconciliation handles it instead. Ordinary 'deferred' rows remain excluded:
-- assignee-fallback escalations have their own fire_at lifecycle and may coexist
-- with a queued primary. The one exception is a task explicitly marked
-- channel_issue_media_pending. It is the issue's sole assigned-agent task and
-- has not been claimable yet, so a new comment must merge into it instead of
-- creating a competing queued task. coalesced_comment_ids remains the pre-claim
-- plan; the claim path records the actual embedded subset in
-- delivered_comment_ids.
--
-- Recompute-on-merge (MUL-4195 review must-fix #1): originator_user_id,
-- runtime_mcp_overlay and runtime_connected_apps are re-stamped to the NEW
-- trigger comment's originator (computed by the caller). Earlier this only
-- repointed the trigger and kept the old originator's overlay/attribution, so a
-- run answering user B's comment could execute under user A's connected-app
-- capabilities and audit identity. Re-stamping means the single coalescing run
-- carries the latest deliberate instruction's originator and the matching
-- overlay — no cross-user capability bleed, no stale attribution. This also
-- removes the previous originator gate + fresh-enqueue fallback, which could not
-- create a second task anyway (the idx_one_pending_task_per_issue_agent unique
-- index allows only one queued/dispatched task per (issue, agent)) and therefore
-- silently dropped the mismatched-originator comment.
--
-- Returns pgx.ErrNoRows when no queued task exists (it was claimed/started
-- between the dedup check and this call, or the only task is already
-- dispatched/running). The caller must NOT blindly enqueue a fresh task in that
-- case — a dispatched sibling would trip the unique index — it defers to
-- completion reconciliation unless no active task exists at all.
UPDATE agent_task_queue
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT e), '{}')
        FROM unnest(array_append(coalesced_comment_ids, trigger_comment_id)) AS e
        WHERE e IS NOT NULL AND e <> @new_trigger_comment_id::uuid
    ),
    trigger_comment_id = @new_trigger_comment_id::uuid,
    trigger_summary = COALESCE(sqlc.narg('new_trigger_summary'), trigger_summary),
    -- Re-attribution is ATOMIC (MUL-4302): folding a newly-arrived comment moves the
    -- WHOLE attribution snapshot to that comment's human — person columns, source
    -- label, delegation lineage, rule version, and evidence — computed by the caller
    -- as one attribution.Result. Re-stamping only the person columns would leave a
    -- run showing B accountable while still pointing at A's stale source / evidence /
    -- level. accountable comes from the resolved Result (finalizeAttribution already
    -- guaranteed originator ⟹ accountable == originator; the cross-column CHECK backs it).
    originator_user_id = sqlc.narg('new_originator_user_id')::uuid,
    accountable_user_id = sqlc.narg('new_accountable_user_id')::uuid,
    originator_source = sqlc.narg('new_originator_source'),
    delegated_from_task_id = sqlc.narg('new_delegated_from_task_id')::uuid,
    rule_version_id = sqlc.narg('new_rule_version_id')::uuid,
    trigger_evidence_kind = sqlc.narg('new_trigger_evidence_kind'),
    trigger_evidence_ref_id = sqlc.narg('new_trigger_evidence_ref_id')::uuid,
    runtime_mcp_overlay = sqlc.narg('new_runtime_mcp_overlay'),
    runtime_connected_apps = sqlc.narg('new_runtime_connected_apps')
WHERE id = (
    SELECT t.id FROM agent_task_queue t
    WHERE t.context->>'wakeup_id' IS NULL AND t.issue_id = @issue_id
      AND t.agent_id = @agent_id
      AND t.comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(@new_trigger_comment_id::uuid)
      AND (
          t.status = 'queued'
          OR (t.status = 'deferred' AND t.context->>'channel_issue_media_pending' = 'true')
      )
      -- Head-scoped (TEN-356, #5914): never fold across HEADs. The physical
      -- unique index is per (issue_id, agent_id, thread), so an insert-race loser can
      -- collide with a pending task stamped for a DIFFERENT head_sha; merging
      -- into it would give a new-HEAD comment old-HEAD review coverage. Empty/
      -- absent head_sha (no linked PR) matches any task, preserving coalescing.
      AND (
          COALESCE(sqlc.narg('head_sha')::text, '') = ''
          OR t.context->>'head_sha' = sqlc.narg('head_sha')::text
      )
    ORDER BY t.created_at DESC
    LIMIT 1
)
RETURNING id, coalesced_comment_ids;

-- name: MergeCommentIntoQueuedIssueTask :one
-- CHE-1418: one comment produces at most one run per agent. Same fold and
-- re-attribution as MergeCommentIntoPendingTask, but the target is the agent's
-- QUEUED task anywhere on the issue — another thread, or an assignment task with
-- no thread — instead of a second queued task beside it. The prompt already
-- fans a run's trigger + coalesced comments out per thread (MUL-4348).
-- Re-pointing the trigger moves the task into the new comment's thread; the
-- caller tries this only after the same-thread paths found nothing pending, so
-- the per-thread unique index is free there. A race that fills it surfaces as a
-- unique violation and the caller falls back to the per-thread path. Arrivals
-- that miss each other here are collapsed after insert (LockQueuedSiblingTask).
-- The squad role must match: a leader-role comment never folds into the same
-- agent's worker task (or the reverse), since the claim briefing differs.
UPDATE agent_task_queue
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT e), '{}')
        FROM unnest(array_append(coalesced_comment_ids, trigger_comment_id)) AS e
        WHERE e IS NOT NULL AND e <> @new_trigger_comment_id::uuid
    ),
    trigger_comment_id = @new_trigger_comment_id::uuid,
    trigger_summary = COALESCE(sqlc.narg('new_trigger_summary'), trigger_summary),
    originator_user_id = sqlc.narg('new_originator_user_id')::uuid,
    accountable_user_id = sqlc.narg('new_accountable_user_id')::uuid,
    originator_source = sqlc.narg('new_originator_source'),
    delegated_from_task_id = sqlc.narg('new_delegated_from_task_id')::uuid,
    rule_version_id = sqlc.narg('new_rule_version_id')::uuid,
    trigger_evidence_kind = sqlc.narg('new_trigger_evidence_kind'),
    trigger_evidence_ref_id = sqlc.narg('new_trigger_evidence_ref_id')::uuid,
    runtime_mcp_overlay = sqlc.narg('new_runtime_mcp_overlay'),
    runtime_connected_apps = sqlc.narg('new_runtime_connected_apps')
WHERE id = (
    SELECT t.id FROM agent_task_queue t
    WHERE t.context->>'wakeup_id' IS NULL AND t.issue_id = @issue_id
      AND t.agent_id = @agent_id
      AND t.status = 'queued'
      AND (sqlc.narg('target_task_id')::uuid IS NULL OR t.id = sqlc.narg('target_task_id')::uuid)
      AND t.is_leader_task = (sqlc.narg('squad_id')::uuid IS NOT NULL)
      AND t.squad_id IS NOT DISTINCT FROM sqlc.narg('squad_id')::uuid
      AND (
          COALESCE(sqlc.narg('head_sha')::text, '') = ''
          OR t.context->>'head_sha' = sqlc.narg('head_sha')::text
      )
    ORDER BY t.created_at DESC
    LIMIT 1
)
-- Re-checked on the row this UPDATE locks: a claim that committed while the
-- fold waited has made it 'dispatched', and its claim response already carries
-- the old trigger and attribution. Then nothing changes and the caller uses the
-- per-thread path (OCR-185-1).
  AND status = 'queued'
RETURNING id, coalesced_comment_ids;

-- name: LockQueuedSiblingTask :one
-- CHE-1418: two comments in different threads can each queue a run for the
-- same agent when neither sees the other's uncommitted task. After a fresh
-- enqueue, the caller (holding the (issue, agent) enqueue lock) looks for an
-- older queued sibling with the same squad role and reviewed head, and locks
-- it so a claim cannot take it mid-collapse. A sibling claimed first is
-- re-checked by FOR UPDATE and no longer matches.
SELECT t.id FROM agent_task_queue t
JOIN agent_task_queue m ON m.id = @task_id
WHERE t.issue_id = m.issue_id AND t.agent_id = m.agent_id AND t.id <> m.id
  AND t.status = 'queued' AND t.context->>'wakeup_id' IS NULL
  AND t.is_leader_task = m.is_leader_task
  AND t.squad_id IS NOT DISTINCT FROM m.squad_id
  AND COALESCE(t.context->>'head_sha', '') = COALESCE(m.context->>'head_sha', '')
ORDER BY t.created_at, t.id
LIMIT 1
FOR UPDATE OF t;

-- name: CancelQueuedDuplicateTask :one
-- Cancels the caller's own fresh task once a queued sibling will carry its
-- comments. Only a still-queued task: a claimed one keeps running.
UPDATE agent_task_queue
SET status = 'cancelled',
    completed_at = now(),
    cancelled_by_type = 'system',
    cancelled_by_name = 'folded into an earlier queued run'
WHERE id = @task_id AND status = 'queued'
RETURNING *;

-- name: FoldTaskIntoQueuedSibling :one
-- Moves the cancelled duplicate's trigger, covered comments and attribution
-- onto the queued sibling, the same re-attribution a comment fold applies.
UPDATE agent_task_queue t
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT e), '{}')
        FROM unnest(t.coalesced_comment_ids || ARRAY[t.trigger_comment_id] || src.coalesced_comment_ids) AS e
        WHERE e IS NOT NULL AND e IS DISTINCT FROM src.trigger_comment_id
    ),
    trigger_comment_id = src.trigger_comment_id,
    trigger_summary = src.trigger_summary,
    originator_user_id = src.originator_user_id,
    accountable_user_id = src.accountable_user_id,
    originator_source = src.originator_source,
    delegated_from_task_id = src.delegated_from_task_id,
    rule_version_id = src.rule_version_id,
    trigger_evidence_kind = src.trigger_evidence_kind,
    trigger_evidence_ref_id = src.trigger_evidence_ref_id,
    runtime_mcp_overlay = src.runtime_mcp_overlay,
    runtime_connected_apps = src.runtime_connected_apps
FROM agent_task_queue src
WHERE t.id = @sibling_id AND t.status = 'queued' AND src.id = @source_id
RETURNING t.id;

-- name: RegisterPlannedCommentForActiveTask :one
-- #5914: durably register a comment that lost an enqueue race as a PLANNED
-- (undelivered) input on the same-(issue, agent) ACTIVE task whose queued row
-- MergeCommentIntoPendingTask could no longer target (it was claimed →
-- dispatched/running). The losing comment can predate that task's created_at,
-- so completion reconciliation's `created_at > since` window cannot see it;
-- appending it to coalesced_comment_ids (the planned set) WITHOUT touching
-- delivered_comment_ids makes reconcileCommentsOnCompletion replay it as a
-- single bounded follow-up (planned-but-not-delivered ⇒ follow-up).
--
-- Head-scoped (TEN-356): only a task stamped with the SAME head_sha is a target,
-- so a new-HEAD comment is never attached to an old-HEAD run. Returns
-- pgx.ErrNoRows when no same-head active task exists (different HEAD, or the task
-- just terminated) so the caller falls back to the active-task decision.
--
-- 'queued' is DELIBERATELY EXCLUDED (#5914, Elon round 3): a not-yet-claimed
-- task has no claim receipt, so a comment merged into it WILL be recorded as
-- delivered at claim time and never earns a completion follow-up under its own
-- attribution. A queued target must therefore go through the ATOMIC
-- MergeCommentIntoPendingTask (which re-stamps trigger/originator/accountable/
-- overlay), never a bare planned append — otherwise a second member's comment
-- could execute under the first member's identity/connected-apps (MUL-4302).
-- Only claim-receipt statuses (already-built delivered set) are safe planned-id
-- targets.
-- Recheck status on the UPDATE target after a concurrent row-lock wait. The
-- subquery can see an active snapshot while completion commits; appending to
-- that completed row would be too late for its completion replay to see it.
UPDATE agent_task_queue
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT e), '{}')
        FROM unnest(array_append(coalesced_comment_ids, @comment_id::uuid)) AS e
        WHERE e IS NOT NULL
    )
WHERE id = (
    SELECT t.id FROM agent_task_queue t
    WHERE t.context->>'wakeup_id' IS NULL AND t.issue_id = @issue_id
      AND t.agent_id = @agent_id
      AND t.comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(@comment_id::uuid)
      AND t.status IN ('dispatched', 'running', 'waiting_local_directory')
      AND (sqlc.narg('target_task_id')::uuid IS NULL OR t.id = sqlc.narg('target_task_id')::uuid)
      AND (
          COALESCE(sqlc.narg('head_sha')::text, '') = ''
          OR t.context->>'head_sha' = sqlc.narg('head_sha')::text
      )
    ORDER BY t.created_at DESC
    LIMIT 1
)
AND status IN ('dispatched', 'running', 'waiting_local_directory')
RETURNING id, coalesced_comment_ids;

-- name: MergeDelegatedFailureCommentIntoPendingTask :one
-- A delegated failure is a platform-owned input, not a new human instruction:
-- fold it into the coordinator's pre-claim task without replacing that task's
-- attribution snapshot. The recovery comment is the newest input, so it becomes
-- the trigger and the prior trigger joins the coalesced plan. This preserves the
-- claim/prompt invariant that trigger_comment_id names the newest comment while
-- retaining the pending task's original human authority and connected apps.
UPDATE agent_task_queue
SET coalesced_comment_ids = (
        SELECT COALESCE(array_agg(DISTINCT e), '{}')
        FROM unnest(array_append(coalesced_comment_ids, trigger_comment_id)) AS e
        WHERE e IS NOT NULL AND e <> @comment_id::uuid
    ),
    trigger_comment_id = @comment_id::uuid,
    trigger_summary = sqlc.narg('trigger_summary')
WHERE id = (
    SELECT t.id FROM agent_task_queue t
    WHERE t.context->>'wakeup_id' IS NULL AND t.issue_id = @issue_id
      AND t.agent_id = @agent_id
      AND t.comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(@comment_id::uuid)
      AND (
          t.status = 'queued'
          OR (t.status = 'deferred' AND t.context->>'channel_issue_media_pending' = 'true')
      )
      AND t.trigger_comment_id IS DISTINCT FROM @comment_id::uuid
      AND NOT (@comment_id::uuid = ANY(t.coalesced_comment_ids))
    ORDER BY t.created_at DESC
    LIMIT 1
)
RETURNING *;

-- name: HasTaskCoveringDelegatedFailureComment :one
-- Durable idempotency check for a recovery comment. The completion reconciler
-- excludes its own just-completed task when replaying a planned-but-undelivered
-- signal. A planned comment is covered only while its task can still execute;
-- after a task becomes terminal, delivered_comment_ids is the sole durable
-- receipt. This prevents cancelled/failed pre-delivery tasks from swallowing
-- the recovery obligation while still avoiding a loop after real delivery.
SELECT count(*) > 0 AS covered
FROM agent_task_queue
WHERE issue_id = @issue_id
  AND agent_id = @agent_id
  AND (
      @comment_id::uuid = ANY(delivered_comment_ids)
      OR (
          id IS DISTINCT FROM sqlc.narg('exclude_task_id')::uuid
          AND (
              status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
              OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
          )
          AND (trigger_comment_id = @comment_id::uuid OR @comment_id::uuid = ANY(coalesced_comment_ids))
      )
  );

-- name: CountDelegatedFailureRecoveryTasks :one
-- Counts dedicated coordinator wakeups for one failed delegated task. Merged
-- recovery signals do not create a new row and therefore do not consume an
-- automatic attempt until they need a standalone successor.
SELECT count(*)
FROM agent_task_queue
WHERE trigger_evidence_kind = 'delegated_failure'
  AND trigger_evidence_ref_id = @failed_task_id;

-- name: AcknowledgeExhaustedDelegatedFailureRecovery :one
-- Once the bounded automatic attempts are exhausted, record a terminal
-- acknowledgement on the newest recovery task. The outbox treats this receipt
-- as settled, while the service writes a separate visible system comment that
-- tells the user why no further task will be generated.
UPDATE agent_task_queue AS acknowledged
SET delivered_comment_ids = (
    SELECT COALESCE(array_agg(DISTINCT receipt.id), '{}')::uuid[]
    FROM unnest(array_append(acknowledged.delivered_comment_ids, @comment_id::uuid)) AS receipt(id)
)
WHERE acknowledged.id = (
    SELECT attempt.id
    FROM agent_task_queue attempt
    WHERE attempt.trigger_evidence_kind = 'delegated_failure'
      AND attempt.trigger_evidence_ref_id = @failed_task_id
    ORDER BY attempt.created_at DESC, attempt.id DESC
    LIMIT 1
    FOR UPDATE
)
  AND (
      SELECT count(*)
      FROM agent_task_queue attempt_count
      WHERE attempt_count.trigger_evidence_kind = 'delegated_failure'
        AND attempt_count.trigger_evidence_ref_id = @failed_task_id
  ) >= @max_attempts::int
RETURNING acknowledged.*;

-- name: SettleDelegatedFailureRecoveriesForTask :execrows
-- Retire every delegated-failure recovery comment this task actually received.
--
-- Callers MUST run this inside the same transaction that makes the task
-- terminal, and only there. While a task is still dispatched its receipt is
-- REPLACED rather than appended (SetTaskDeliveredCommentIDs), because a reclaim
-- by a differently-capable daemon must be able to drop an id it will not
-- deliver. Settling at receipt-write time would freeze that legitimate
-- transient window into a permanently lost recovery. Once the task is terminal
-- the receipt is final, which is exactly the guarantee this marker records.
--
-- A comment that was only planned into the task and never delivered is absent
-- from delivered_comment_ids, so an automatic cancellation correctly leaves it
-- pending for the sweeper to replay.
--
-- The terminal-status predicate is the guarantee itself, not a caller
-- convention. Settling a dispatched or running task's receipt would freeze the
-- reclaim window into a permanently lost recovery, and this marker is
-- monotonic — there is no later pass that could undo it. A caller that passes a
-- non-terminal task therefore updates nothing rather than silently destroying
-- the obligation.
UPDATE comment recovery
SET recovery_settled_at = now()
FROM agent_task_queue task
WHERE task.id = @task_id
  AND task.status IN ('completed', 'failed', 'cancelled')
  AND recovery.id = ANY(task.delivered_comment_ids)
  AND recovery.author_type = 'system'
  AND recovery.type = 'progress_update'
  AND recovery.source_task_id IS NOT NULL
  AND recovery.recovery_settled_at IS NULL;

-- name: SettleDelegatedFailureRecoveryComment :execrows
-- Retire one recovery comment by id, for the terminal outcomes that are not a
-- task reaching a terminal status: exhaustion of the bounded automatic attempts
-- writes its receipt onto an attempt row that may still be running, and the
-- user-visible explanation comment is what closes the obligation. Callers run
-- this in the same transaction that records that outcome.
UPDATE comment
SET recovery_settled_at = now()
WHERE id = @comment_id
  AND author_type = 'system'
  AND type = 'progress_update'
  AND source_task_id IS NOT NULL
  AND recovery_settled_at IS NULL;

-- name: ListPendingDelegatedFailureRecoveries :many
-- Durable outbox scan for platform recovery comments that are not yet owned by
-- an executable task and have no terminal delivery receipt. Starting from the
-- explicit recovery signal avoids retroactively waking unrelated historical
-- delegated failures. A bounded runtime sweeper replays these comments after a
-- transient dispatch error or process restart.
--
-- recovery_settled_at is what keeps this scan from growing with history. Every
-- other exclusion below is reversible — an issue can leave 'done', a cancelled
-- retry can be superseded — so none of them can be frozen into the index
-- predicate. A delivery receipt on a terminal task cannot be taken back, so
-- that one condition is recorded as durable state instead of being re-proven
-- through four joins and two NOT EXISTS subqueries on every tick. The predicate
-- of idx_comment_delegated_failure_unsettled matches the first four conditions,
-- narrowing the scan to unsettled signals. Reversible eligibility must still
-- be checked before LIMIT so paused signals cannot starve executable ones.
SELECT recovery.*
FROM comment recovery
JOIN agent_task_queue failed ON failed.id = recovery.source_task_id
JOIN agent_task_queue source ON source.id = failed.delegated_from_task_id
JOIN issue source_issue ON source_issue.id = source.issue_id
JOIN agent source_agent ON source_agent.id = source.agent_id
LEFT JOIN issue_status source_status
  ON source_status.workspace_id = source_issue.workspace_id
 AND source_status.key = source_issue.status
WHERE recovery.author_type = 'system'
  AND recovery.type = 'progress_update'
  AND recovery.source_task_id IS NOT NULL
  AND recovery.recovery_settled_at IS NULL
  -- A deleted recovery signal is withdrawn, even when replies keep its row.
  AND recovery.deleted_at IS NULL
  AND recovery.issue_id = source_issue.id
  AND recovery.workspace_id = source_issue.workspace_id
  AND failed.status = 'failed'
  AND failed.delegated_from_task_id IS NOT NULL
  AND failed.autopilot_run_id IS NULL
  AND failed.trigger_evidence_kind IS DISTINCT FROM 'delegated_failure'
  AND source.autopilot_run_id IS NULL
  AND source.issue_id IS NOT NULL
  AND source.agent_id <> failed.agent_id
  -- Match canDispatchDelegatedFailureRecovery: lifecycle permits recovery only
  -- for open work; parking belongs exclusively to the fixed Backlog status.
  -- Built-ins resolve without catalog rows; unknown custom states stay pending.
  AND source_issue.status <> 'backlog'
  -- A coordinator waiting in Triage is the entry's proposed owner, not its
  -- owner, so a worker failure must not wake it (MUL-7189 §2.3). Triage is not
  -- a status, so it is a predicate of its own rather than a CASE arm.
  AND source_issue.triage_state IS NULL
  AND CASE
      WHEN source_issue.status IN ('backlog', 'todo') THEN 'unstarted'
      WHEN source_issue.status IN ('in_progress', 'in_review', 'blocked') THEN 'started'
      WHEN source_issue.status = 'done' THEN 'done'
      WHEN source_issue.status = 'cancelled' THEN 'closed'
      ELSE source_status.category
  END IN ('unstarted', 'started')
  AND source_agent.archived_at IS NULL
  AND source_agent.runtime_id IS NOT NULL
  AND source_agent.workspace_id = source_issue.workspace_id
  AND NOT EXISTS (
      SELECT 1
      FROM agent_task_queue retry
      WHERE retry.parent_task_id = failed.id
        AND retry.status <> 'cancelled'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM agent_task_queue covering
      WHERE covering.issue_id = source_issue.id
        AND covering.agent_id = source.agent_id
        AND (
            recovery.id = ANY(covering.delivered_comment_ids)
            OR (
                (
                    covering.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
                    OR (
                        covering.status = 'deferred'
                        AND covering.context->>'channel_issue_media_pending' = 'true'
                    )
                )
                AND (
                    covering.trigger_comment_id = recovery.id
                    OR recovery.id = ANY(covering.coalesced_comment_ids)
                )
            )
        )
  )
ORDER BY recovery.created_at ASC, recovery.id ASC
LIMIT @max_per_tick;

-- name: HasActiveTaskForIssueAndAgent :one
-- MUL-4195: true when the (issue, agent) pair has any non-terminal task in a
-- state whose completion will run completion reconciliation — queued,
-- dispatched, running, waiting_local_directory, or the explicitly-marked
-- channel-media deferred state. Used by the comment enqueue
-- path: when a merge into a pre-claim task fails (the task is already
-- dispatched/running, or a mismatched pre-claim task exists), a fresh queued
-- INSERT would collide with idx_one_pending_task_per_issue_agent AND would risk
-- a duplicate run. Instead the caller relies on that active task's completion
-- reconcile to schedule the guaranteed follow-up, and only enqueues fresh when
-- NO active task exists.
SELECT count(*) > 0 AS has_active FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  );

-- name: HasRunningTaskForIssueAndAgent :one
SELECT EXISTS (
  SELECT 1 FROM agent_task_queue
  WHERE issue_id = $1 AND agent_id = $2 AND status = 'running'
);

-- name: GetRunningTaskStartForIssueAndAgent :one
SELECT COALESCE(started_at, created_at) AS started_at
FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2 AND status = 'running'
ORDER BY COALESCE(started_at, created_at) DESC
LIMIT 1;

-- name: HasPendingIssueTaskForAgent :one
SELECT EXISTS (
  SELECT 1 FROM agent_task_queue
  WHERE issue_id = $1 AND agent_id = $2
    AND (
      status IN ('queued', 'dispatched')
      OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
    )
);

-- name: HasActiveTaskForIssueAndAgentInThread :one
-- Active execution and pending work in this comment thread only.
SELECT count(*) > 0 AS has_active FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2
  AND (
    status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
    OR (status = 'deferred' AND context->>'channel_issue_media_pending' = 'true')
  )
  AND comment_thread_id IS NOT DISTINCT FROM comment_thread_root_id(sqlc.narg('thread_comment_id')::uuid);

-- name: GetLatestTaskRoleForIssueAndAgent :one
-- Returns the role markers from the agent's most recent task on this issue.
-- Used by the squad-leader self-trigger guard to tell apart leader tasks,
-- same-squad worker tasks, and generic agent tasks such as direct mentions or
-- thread-parent replies.
SELECT is_leader_task, squad_id FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2
ORDER BY created_at DESC
LIMIT 1;

-- name: ListPendingTasksByRuntime :many
SELECT * FROM agent_task_queue
WHERE runtime_id = $1 AND status IN ('queued', 'dispatched')
ORDER BY priority DESC, created_at ASC;

-- name: ListQueuedClaimCandidatesByRuntime :many
-- Returns rows the runtime is authorized to attempt to claim. Status is restricted to
-- 'queued' (in contrast to ListPendingTasksByRuntime which also includes
-- 'dispatched') because dispatched rows are by definition already owned
-- and cannot be re-claimed — including them in the candidate list pads
-- the result with rows that always lose the per-(issue, agent) race in
-- ClaimAgentTask, wasting CPU and a SELECT every poll cycle when the
-- runtime is busy on a long-running task. Backed by the partial index
-- idx_agent_task_queue_claim_candidates so the warm path is cheap.
SELECT atq.* FROM agent_task_queue atq
WHERE atq.runtime_id = $1
  AND atq.status = 'queued'
      AND (atq.context->>'wakeup_id' IS NULL OR EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.id=(atq.context->>'wakeup_id')::uuid AND w.disabled_at IS NULL AND w.revision=(atq.context->>'wakeup_revision')::bigint))
  AND EXISTS (
      -- Keep this authorization fence in sync with ClaimAgentTask.
      SELECT 1
      FROM agent a
      JOIN agent_runtime r ON r.id = atq.runtime_id
      WHERE a.id = atq.agent_id
        AND a.runtime_id = atq.runtime_id
        AND (
            r.visibility = 'public'
            OR r.visibility = 'private'
        )
  )
ORDER BY atq.priority DESC, atq.created_at ASC;

-- name: CancelSupersededDeferredRetriesForRuntimes :many
-- Cancels deferred auto-retry rows that a newer active task has already
-- superseded, so one rerun click still means exactly one more run.
--
-- The narrow race: a manual rerun clears the pending slot, a concurrent FailTask
-- commits a deferred retry (runtime_offline / provider_network's final attempt
-- arm fire_at), and the rerun's own enqueue then succeeds because 'deferred' is
-- outside idx_one_pending_task_per_issue_agent_v2. Both rows now exist. Merely
-- refusing to promote the retry is not enough: once the rerun stops occupying the
-- slot the retry promotes and runs a SECOND time, duplicating the agent's
-- comments, side effects and cost with nothing to signal it. That contradicts
-- both "a manual rerun replaces the pending plan" and FailTask's own "a runnable
-- successor already exists, so no retry is needed".
--
-- Scope is deliberately tight:
--   * retry_of_task_id IS NOT NULL — only auto-retry clones, never a fresh run.
--   * channel-media pending rows are excluded for the same reason: that deferred
--     row is the issue's own task waiting on media, not a superseded retry.
--   * issue_id IS NOT NULL — chat / quick-create tasks have no slot semantics.
--
-- 'running' and 'waiting_local_directory' count as superseding: the duplicate
-- fires precisely when the rerun has STARTED, which is when the row would
-- otherwise no longer look blocked.
UPDATE agent_task_queue r
SET status = 'cancelled', completed_at = now(), prepare_lease_expires_at = NULL,
    cancelled_by_type = 'system', cancelled_by_id = NULL, cancelled_by_name = NULL
WHERE r.runtime_id = ANY(@runtime_ids::uuid[])
  AND r.status = 'deferred'
  AND r.issue_id IS NOT NULL
  AND r.retry_of_task_id IS NOT NULL
  AND COALESCE(r.context->>'channel_issue_media_pending', '') <> 'true'
  AND EXISTS (
    SELECT 1 FROM agent_task_queue successor
    WHERE successor.issue_id = r.issue_id
      AND successor.agent_id = r.agent_id
      AND successor.comment_thread_id IS NOT DISTINCT FROM r.comment_thread_id
      AND successor.id <> r.id
      AND successor.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
  )
RETURNING *;

-- name: PromoteDueDeferredTasksForRuntime :many
-- Promotion is fenced against the single queued/dispatched slot
-- idx_one_pending_task_per_issue_agent_thread allows per (issue, agent, thread). A deferred
-- row is NOT covered by that index, so it can legitimately coexist with a queued
-- one — a manual rerun enqueued behind a running task, plus the deferred retry
-- that task's failure armed (runtime_offline, provider_network's final attempt).
-- Flipping such a row to 'queued' unconditionally violates the index, and because
-- the claim loop promotes before it claims, that error blocked every claim on the
-- runtime — including the rerun the operator was waiting for.
--
-- Two fences: skip a row whose (issue, agent, thread) slot is already occupied, and
-- promote at most ONE row per (issue, agent, thread) so a single statement cannot collide
-- with itself. A skipped row stays deferred with fire_at in the past and is
-- promoted by a later tick once the slot frees, so nothing is lost — the human's
-- rerun simply goes first. Chat / quick-create rows (issue_id NULL) are outside
-- the index and bypass both fences.
WITH due AS (
    SELECT t.id,
           t.issue_id,
           row_number() OVER (
               PARTITION BY t.issue_id, t.agent_id, t.comment_thread_id
               ORDER BY t.priority DESC, t.created_at ASC, t.id
           ) AS rn
    FROM agent_task_queue t
    WHERE t.runtime_id = @runtime_id
      AND t.status = 'deferred'
      AND t.fire_at <= now()
      AND EXISTS (
        SELECT 1 FROM agent_runtime r
        WHERE r.id = t.runtime_id
          AND r.status = 'online'
          AND COALESCE(r.last_seen_at, r.updated_at) >=
              now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
      AND NOT EXISTS (
        SELECT 1 FROM agent_task_queue occupant
        WHERE occupant.issue_id = t.issue_id
          AND occupant.agent_id = t.agent_id
          AND occupant.comment_thread_id IS NOT DISTINCT FROM t.comment_thread_id
          AND occupant.id <> t.id
          AND (
            occupant.status IN ('queued', 'dispatched')
            OR (occupant.status = 'deferred' AND occupant.context->>'channel_issue_media_pending' = 'true')
          )
      )
)
UPDATE agent_task_queue
SET status = 'queued'
WHERE id IN (SELECT id FROM due WHERE issue_id IS NULL OR rn = 1)
RETURNING *;

-- name: ListQueuedClaimCandidatesByRuntimes :many
-- Batch variant of ListQueuedClaimCandidatesByRuntime (MUL-4257): returns
-- queued claim candidates across every runtime_id in the input set in ONE round
-- trip, so a daemon can list candidates for all of its runtimes with a single
-- query instead of one per runtime. Ordering matches the singular query
-- (priority, then FIFO) so the batch claim loop keeps the same fairness. The
-- runtime_id filter is served by the partial index
-- idx_agent_task_queue_claim_candidates; the cross-runtime ORDER BY still needs
-- a sort step (each runtime's slice is index-ordered, but merging several
-- runtimes' rows into one priority/FIFO order is not). The per-machine
-- candidate set is small, so this is cheap in practice.
SELECT atq.* FROM agent_task_queue atq
WHERE atq.runtime_id = ANY(@runtime_ids::uuid[])
  AND atq.status = 'queued'
      AND (atq.context->>'wakeup_id' IS NULL OR EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.id=(atq.context->>'wakeup_id')::uuid AND w.disabled_at IS NULL AND w.revision=(atq.context->>'wakeup_revision')::bigint))
  AND EXISTS (
      -- Keep this authorization fence in sync with ClaimAgentTask.
      SELECT 1
      FROM agent a
      JOIN agent_runtime r ON r.id = atq.runtime_id
      WHERE a.id = atq.agent_id
        AND a.runtime_id = atq.runtime_id
        AND (
            r.visibility = 'public'
            OR r.visibility = 'private'
        )
  )
ORDER BY atq.priority DESC, atq.created_at ASC;

-- name: NextDeferredTaskFireAtForRuntimes :one
-- Returns the next future deferred task for a daemon's authorized runtime set,
-- or an eligible task that crossed fire_at during this claim. Overdue tasks
-- whose runtime is offline/stale or that are blocked by an existing issue+agent
-- occupant are omitted so they cannot cause a tight poll loop. Keep both fences
-- in sync with PromoteDueDeferredTasksForRuntimes: a task that cannot be
-- promoted must not advertise an immediate follow-up claim. The response
-- converts the timestamp to a relative delay, avoiding any dependency on
-- daemon/server clock synchronization.
SELECT MIN(fire_at)::timestamptz
FROM agent_task_queue t
WHERE t.runtime_id = ANY(@runtime_ids::uuid[])
  AND t.status = 'deferred'
  AND EXISTS (
    SELECT 1 FROM agent_runtime r
    WHERE r.id = t.runtime_id
      AND r.status = 'online'
      AND COALESCE(r.last_seen_at, r.updated_at) >=
          now() - make_interval(secs => @runtime_stale_secs::double precision)
  )
  AND (
    t.fire_at > now()
    OR NOT EXISTS (
      SELECT 1 FROM agent_task_queue occupant
      WHERE occupant.issue_id = t.issue_id
        AND occupant.agent_id = t.agent_id
          AND occupant.comment_thread_id IS NOT DISTINCT FROM t.comment_thread_id
        AND occupant.id <> t.id
        AND (
          occupant.status IN ('queued', 'dispatched')
          OR (occupant.status = 'deferred' AND occupant.context->>'channel_issue_media_pending' = 'true')
        )
    )
  );

-- name: PromoteDueDeferredTasksForRuntimes :many
-- Batch variant of PromoteDueDeferredTasksForRuntime (MUL-4257): promotes all
-- due deferred tasks across the runtime set in one UPDATE. Carries the same two
-- fences as the singular query; see its comment for why.
WITH due AS (
    SELECT t.id,
           t.issue_id,
           row_number() OVER (
               PARTITION BY t.issue_id, t.agent_id, t.comment_thread_id
               ORDER BY t.priority DESC, t.created_at ASC, t.id
           ) AS rn
    FROM agent_task_queue t
    WHERE t.runtime_id = ANY(@runtime_ids::uuid[])
      AND t.status = 'deferred'
      AND t.fire_at <= now()
      AND EXISTS (
        SELECT 1 FROM agent_runtime r
        WHERE r.id = t.runtime_id
          AND r.status = 'online'
          AND COALESCE(r.last_seen_at, r.updated_at) >=
              now() - make_interval(secs => @runtime_stale_secs::double precision)
      )
      AND NOT EXISTS (
        SELECT 1 FROM agent_task_queue occupant
        WHERE occupant.issue_id = t.issue_id
          AND occupant.agent_id = t.agent_id
          AND occupant.comment_thread_id IS NOT DISTINCT FROM t.comment_thread_id
          AND occupant.id <> t.id
          AND (
            occupant.status IN ('queued', 'dispatched')
            OR (occupant.status = 'deferred' AND occupant.context->>'channel_issue_media_pending' = 'true')
          )
      )
)
UPDATE agent_task_queue
SET status = 'queued'
WHERE id IN (SELECT id FROM due WHERE issue_id IS NULL OR rn = 1)
RETURNING *;

-- name: ListActiveTasksByIssue :many
-- Backs the issue-detail "agent live" banner. Includes 'queued' so the
-- banner shows up the moment a task is enqueued — not only after a runtime
-- claims it. The queued window can be long when the runtime is offline or
-- busy on a prior task, and a silent UI during that window looks like the
-- platform never received the trigger.
SELECT * FROM agent_task_queue
WHERE issue_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY created_at DESC;

-- name: ListActiveTasksByIssueFamily :many
-- Cross-issue coordination read for parallel sub-issue work (#7768). Given a
-- family root — the target issue's parent, or the target itself when it has
-- none — return every in-flight task on the root and on all of its children,
-- so a run can see who else is already working in the family before it starts
-- overlapping work. Advisory only: nothing here gates, queues, or serialises
-- anything.
--
-- Same active set as ListActiveTasksByIssue, including 'queued': a queued
-- sibling cannot answer you yet, but it is about to touch the same code, which
-- is exactly what the caller is trying to find out. The status column tells the
-- two apart.
--
-- Issue identity is joined in because the caller renders runs from several
-- issues in one list and cannot label a row from the task alone. agent_id is
-- here for the same reason: this read spans agents, and which one is on a
-- sibling is the answer, not a detail.
--
-- Columns are named rather than embedded. This is the coordination question,
-- not the execution log: result and context are JSONB blobs, and work_dir /
-- trigger_summary / the attribution ids are all execution-log fields that a
-- caller asking "who else is here?" never reads. Selecting them would make
-- Postgres detoast and ship roughly 5x the bytes per row for nothing.
--
-- Ordered running-first so the truncation the LIMIT may impose drops the least
-- interesting rows, and bounded because a parent with hundreds of children must
-- not turn one coordination read into an unbounded scan.
SELECT
    atq.id AS task_id,
    atq.agent_id,
    atq.issue_id,
    atq.status,
    atq.created_at,
    atq.started_at,
    w.issue_prefix,
    i.number AS issue_number,
    i.title AS issue_title
FROM agent_task_queue atq
JOIN issue i ON i.id = atq.issue_id
JOIN workspace w ON w.id = i.workspace_id
WHERE i.workspace_id = @workspace_id
  AND (i.id = @root_issue_id::uuid OR i.parent_issue_id = @root_issue_id::uuid)
  AND atq.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY
    CASE atq.status
        WHEN 'running' THEN 0
        WHEN 'dispatched' THEN 1
        WHEN 'waiting_local_directory' THEN 2
        ELSE 3
    END,
    atq.created_at DESC
LIMIT @row_limit;

-- name: GetWorkspaceAgentRunCounts :many
-- Total task runs per agent over the trailing 30 days, used by the Agents
-- list RUNS column. 30-day window keeps the count meaningful (a long-dormant
-- agent shouldn't show "5,420 runs from 2 years ago") and keeps the scan
-- bounded as the workspace ages.
SELECT
    atq.agent_id,
    COUNT(*)::int AS run_count
FROM agent_task_queue atq
JOIN agent a ON a.id = atq.agent_id
WHERE a.workspace_id = $1
  AND atq.created_at > now() - INTERVAL '30 days'
GROUP BY atq.agent_id;

-- name: GetWorkspaceAgentActivity30d :many
-- Returns per-agent daily activity buckets for the last 30 days. Single
-- workspace-wide read backs both surfaces:
--   - Agents list ACTIVITY column — uses only the trailing 7 buckets
--   - Agent detail "Last 30 days" panel — uses the full 30
-- 30 days contains 7 days, so one fetch + a client-side .slice(-7) wins
-- over fetching twice. Days with no completion produce no row; the
-- front-end zero-fills.
--
-- Anchored on completed_at (not created_at) because the sparkline answers
-- "what did this agent produce?" not "what was queued at it?". A task that's
-- still in flight has no completed_at and contributes nothing here — that's
-- correct: in-flight tasks are surfaced via the live presence indicator,
-- not the historical trend.
-- Keep total activity separate from outcomes: cancelled runs belong in the
-- history, but success rate is completed / (completed + failed).
SELECT
    atq.agent_id,
    DATE_TRUNC('day', atq.completed_at)::timestamptz AS bucket,
    COUNT(*)::int AS task_count,
    COUNT(*) FILTER (WHERE atq.status = 'failed')::int AS failed_count,
    COUNT(*) FILTER (WHERE atq.status = 'completed')::int AS completed_count,
    COUNT(*) FILTER (WHERE atq.status = 'cancelled')::int AS cancelled_count,
    COALESCE(SUM(EXTRACT(EPOCH FROM (atq.completed_at - atq.started_at)) * 1000)
        FILTER (WHERE atq.completed_at > atq.started_at), 0)::float8 AS duration_ms,
    COUNT(*) FILTER (WHERE atq.completed_at > atq.started_at)::int AS duration_count
FROM agent_task_queue atq
JOIN agent a ON a.id = atq.agent_id
WHERE a.workspace_id = $1
  AND atq.completed_at IS NOT NULL
  AND atq.completed_at > now() - INTERVAL '30 days'
GROUP BY atq.agent_id, bucket
ORDER BY atq.agent_id, bucket;

-- name: ListWorkspaceAgentTaskSnapshot :many
-- Returns the tasks the front-end reads off one workspace-wide snapshot:
--   - All active tasks (queued / dispatched / running / waiting_local_directory)
--     — the current workload: working signal + counts (see derive-presence.ts).
--   - Each agent's most recent OUTCOME task (completed / failed) — NOT part of
--     presence since #1823; it is the "last activity" line the Squad hover card
--     renders (agent-live-peek-card.tsx). Kept in this response because shipped
--     desktop builds read it from here; see MUL-5436 for the plan to move it to
--     a dedicated lazy endpoint.
--
-- Cancelled tasks are excluded from the outcome half on purpose: cancel is a
-- procedural signal ("attempt aborted"), not an outcome. It tells us nothing
-- about whether the agent works, so it must NOT be allowed to mask a prior
-- failure. Concretely: if an agent fails and then the user cancels the queued
-- retry (or the parent issue closes and cascades cancels), the failure stays
-- the agent's last real outcome.
--
-- The outcome half is a per-agent LATERAL Top-1 rather than a workspace-wide
-- DISTINCT ON: with idx_agent_task_queue_agent_terminal_latest (migration 233)
-- each agent costs one ordered index scan + LIMIT 1, so the cost no longer
-- grows with how much terminal history the workspace has accumulated. The
-- (created_at, id) tie-break makes the pick deterministic when completed_at
-- ties or is NULL — plain completed_at DESC returned an arbitrary row there.
-- Deferred wakeup retries also remain visible as queued work.
--
-- Both halves JOIN / scan agent because agent_task_queue has no workspace_id.
SELECT atq.* FROM agent_task_queue atq
JOIN agent a ON a.id = atq.agent_id
WHERE a.workspace_id = $1
  AND (atq.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
    OR (atq.status='deferred' AND atq.context->>'wakeup_id' IS NOT NULL))

UNION ALL

SELECT latest.* FROM agent a
JOIN LATERAL (
  SELECT atq.*
  FROM agent_task_queue atq
  WHERE atq.agent_id = a.id
    AND atq.status IN ('completed', 'failed')
  ORDER BY atq.completed_at DESC NULLS LAST, atq.created_at DESC, atq.id DESC
  LIMIT 1
) latest ON TRUE
WHERE a.workspace_id = $1;

-- name: ListWorkspaceWorkingAgents :many
-- Workspace-level source for consumers that show currently working agents.
-- One row per visible, user-authored agent with at least one task that has
-- actually started running. work_type is optional (empty = every source);
-- source-specific reads use the same precedence as computeTaskKind:
-- chat > autopilot > issue. "issue" intentionally groups direct and
-- comment-triggered issue work. Quick-create work is present only in the
-- unfiltered projection because it has no source FK yet. mine_relation is
-- optional (empty = workspace); when set it narrows issue work to the
-- authenticated member's My Issues relation. parent_issue_id is optional
-- (NULL = workspace); when set it narrows issue work to the direct children
-- of that issue, so an issue detail's sub-issue header reads the same
-- projection as the Issues list header instead of deriving its own count.
SELECT
  a.id,
  a.name,
  a.avatar_url,
  COUNT(*)::int AS running_task_count,
  COALESCE(
    ARRAY_AGG(DISTINCT atq.issue_id ORDER BY atq.issue_id)
      FILTER (WHERE atq.issue_id IS NOT NULL),
    ARRAY[]::uuid[]
  )::uuid[] AS issue_ids
FROM agent a
JOIN agent_task_queue atq ON atq.agent_id = a.id
WHERE a.workspace_id = $1
  AND a.kind = 'user'
  AND a.archived_at IS NULL
  AND atq.status = 'running'
  AND (
    @work_type::text = ''
    OR (@work_type::text = 'chat' AND atq.chat_session_id IS NOT NULL)
    OR (
      @work_type::text = 'autopilot'
      AND atq.chat_session_id IS NULL
      AND atq.autopilot_run_id IS NOT NULL
    )
    OR (
      @work_type::text = 'issue'
      AND atq.chat_session_id IS NULL
      AND atq.autopilot_run_id IS NULL
      AND atq.issue_id IS NOT NULL
    )
  )
  AND (
    @mine_relation::text = ''
    OR EXISTS (
      SELECT 1
      FROM issue i
      WHERE i.id = atq.issue_id
        AND i.workspace_id = a.workspace_id
        AND (
          (
            @mine_relation::text IN ('assigned', 'any')
            AND i.assignee_type = 'member'
            AND i.assignee_id = @member_id::uuid
          )
          OR (
            @mine_relation::text IN ('created', 'any')
            AND i.creator_type = 'member'
            AND i.creator_id = @member_id::uuid
          )
          OR (
            @mine_relation::text IN ('involved', 'any')
            AND (
              (
                i.assignee_type = 'agent'
                AND EXISTS (
                  SELECT 1
                  FROM agent owned_agent
                  WHERE owned_agent.id = i.assignee_id
                    AND owned_agent.workspace_id = a.workspace_id
                    AND owned_agent.owner_id = @member_id::uuid
                )
              )
              OR (
                i.assignee_type = 'squad'
                AND EXISTS (
                  SELECT 1
                  FROM squad s
                  WHERE s.id = i.assignee_id
                    AND s.workspace_id = a.workspace_id
                    AND (
                      EXISTS (
                        SELECT 1
                        FROM squad_member sm
                        WHERE sm.squad_id = s.id
                          AND sm.member_type = 'member'
                          AND sm.member_id = @member_id::uuid
                      )
                      OR EXISTS (
                        SELECT 1
                        FROM agent leader
                        WHERE leader.id = s.leader_id
                          AND leader.workspace_id = a.workspace_id
                          AND leader.owner_id = @member_id::uuid
                      )
                      OR EXISTS (
                        SELECT 1
                        FROM squad_member sm
                        JOIN agent owned_member ON owned_member.id = sm.member_id
                        WHERE sm.squad_id = s.id
                          AND sm.member_type = 'agent'
                          AND owned_member.workspace_id = a.workspace_id
                          AND owned_member.owner_id = @member_id::uuid
                      )
                    )
                )
              )
            )
          )
        )
    )
  )
  AND (
    @parent_issue_id::uuid IS NULL
    OR EXISTS (
      SELECT 1
      FROM issue child
      WHERE child.id = atq.issue_id
        AND child.workspace_id = a.workspace_id
        AND child.parent_issue_id = @parent_issue_id::uuid
    )
  )
GROUP BY a.id, a.name, a.avatar_url, a.created_at
ORDER BY a.created_at ASC;

-- name: ListTasksByIssue :many
SELECT * FROM agent_task_queue
WHERE issue_id = $1
ORDER BY created_at DESC;

-- name: UpdateAgentStatus :one
UPDATE agent SET status = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: RefreshAgentStatusFromTasks :one
-- Persisted agent.status has no queued/resource-wait bucket. Keep dispatched
-- as working because the daemon is actively preparing that task, but do not
-- let a task parked on a local_directory mutex masquerade as executing work.
-- The status guard makes a correct status a no-op: :one returns no rows, so
-- callers neither rewrite updated_at nor publish a redundant status event.
WITH desired AS (
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM agent_task_queue q
        WHERE q.agent_id = $1 AND q.status IN ('dispatched', 'running')
    ) THEN 'working' ELSE 'idle' END AS status
)
UPDATE agent AS a
SET status = desired.status,
    updated_at = now()
FROM desired
WHERE a.id = $1 AND a.status IS DISTINCT FROM desired.status
RETURNING a.*;

-- name: GetAgentBySystemKey :one
-- Resolves a workspace's built-in agent by its stable system_key. This is the
-- identity lookup for system agents: their display name is owner-editable, so
-- nothing server-side may key off it.
SELECT * FROM agent
WHERE workspace_id = $1 AND system_key = $2 AND archived_at IS NULL
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: CreateSystemUserAgent :one
-- Creates a product-defined agent that members can still see, chat with, and
-- assign issues to. Deliberately kind='user': kind='system' hides the row from
-- agent lists and assignment surfaces and hard deletes it with its runtime.
-- Every product-owned field is a server constant; instructions stays empty
-- because the system half ships with the binary and is layered in at claim
-- time, leaving this column free for the workspace's own notes.
--
-- CONTRACT: call only while holding the per-workspace mika advisory lock. The
-- one-per-workspace invariant rests on a check inside that lock, not on a
-- unique index — migration 172's index keys on (workspace_id, owner_id,
-- runtime_id, system_key), so two different owners or runtimes are distinct
-- tuples and would both insert.
INSERT INTO agent (
    workspace_id, name, description, avatar_url, runtime_mode, runtime_config,
    runtime_id, model, visibility, permission_mode, max_concurrent_tasks,
    owner_id, instructions, custom_env, custom_args, kind, system_key
) VALUES (
    @workspace_id, @name, @description, @avatar_url, @runtime_mode, '{}'::jsonb,
    @runtime_id, @model, @visibility, @permission_mode, @max_concurrent_tasks,
    @owner_id, '', '{}'::jsonb, '[]'::jsonb, 'user', @system_key
)
RETURNING *;

-- name: GetCommentThreadRootID :one
SELECT comment_thread_root_id(@comment_id::uuid)::uuid AS id;

-- name: SetTaskClaimSnapshot :execrows
-- Requested and claimed run settings for one claim generation (CHE-1862
-- claimed_model, plus the agent-preset columns). The vote gate reads
-- claimed_model as the task's lab identity, so an edit of the agent row after
-- the claim cannot change what a running task is counted as. Every statement
-- that writes dispatched_at resets these columns, and this write matches
-- (id, runtime_id, dispatched_at), so a slow handler of an old claim cannot
-- write into a newer one.
UPDATE agent_task_queue
SET preset_name = @preset_name, preset_source = @preset_source, preset_skips = @preset_skips,
    claimed_model = @claimed_model, claimed_thinking_level = @claimed_thinking_level,
    claimed_service_tier = @claimed_service_tier, claimed_context_args = @claimed_context_args
WHERE id = @id
  AND runtime_id = @runtime_id
  AND dispatched_at = @dispatched_at
  AND status = 'dispatched';
