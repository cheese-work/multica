-- CHE-488: durable stage-completion-wake identity. See migration 475/476 for
-- the full rationale (server/migrations/475_stage_completion_wake.up.sql).

-- name: LockStageCompletion :exec
-- Stage-scoped advisory xact lock, held for the whole read-then-write window
-- of a stage-completion decision: reading the sibling set, evaluating the
-- stage barrier, reading stage_generation, and claiming
-- stage_completion_wake. A reopen (BumpStageGeneration) takes the same key
-- before it writes. Mirrors LockCommentThread (pkg/db/queries/comment.sql)
-- exactly — same rationale: a single READ COMMITTED transaction gives no
-- consistent snapshot across separate statements, so without a lock a reopen
-- could commit its generation bump between "read siblings, barrier closed at
-- generation N" and "claim the wake", letting a stale N-generation claim
-- through for a stage that is no longer actually complete (CHE-482: "a stale
-- event must not advance a now-incomplete stage"). Taking this lock before
-- either side's read-then-write orders the two operations relative to each
-- other: whichever acquires the lock first fully completes (commits) before
-- the other proceeds. pg_advisory_xact_lock (not pg_try_) so the rarer
-- concurrent reopen queues briefly behind an in-flight completion, or vice
-- versa, instead of failing outright.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(parent_issue_id)::uuid::text || ':stage_completion:' || sqlc.arg(stage)::int::text, 0));

-- name: BumpStageGeneration :one
-- Records a reopen (terminal -> non-terminal transition) of a staged child
-- under (parent_issue_id, stage): invalidates any wake already recorded for
-- the prior generation, so the legitimate re-closure after a real correction
-- wakes once more instead of being silently absorbed as a duplicate. The
-- first bump for a (parent, stage) pair inserts generation 1; each further
-- reopen increments it. Caller must hold LockStageCompletion for
-- (parent_issue_id, stage) first — see that query for why.
INSERT INTO stage_generation (workspace_id, parent_issue_id, stage, generation, updated_at)
VALUES ($1, $2, $3, 1, now())
ON CONFLICT (parent_issue_id, stage)
DO UPDATE SET generation = stage_generation.generation + 1, updated_at = now()
RETURNING generation;

-- name: CurrentStageGeneration :one
-- Reads the current generation for (parent_issue_id, stage) without bumping
-- it. Absent row means generation 0 (never reopened). Caller must hold
-- LockStageCompletion for (parent_issue_id, stage) first — see that query.
SELECT COALESCE(
    (SELECT generation FROM stage_generation WHERE parent_issue_id = $1 AND stage = $2),
    0
)::int AS generation;

-- name: RecordStageCompletionWake :one
-- Claims the wake for (parent_issue_id, stage, generation), BEFORE the system
-- comment is created (wake_comment_id is filled in afterwards by
-- SetStageCompletionWakeComment). Caller must hold LockStageCompletion for
-- (parent_issue_id, stage) first, so generation is guaranteed current as of
-- this insert (see that query for why this matters). The unique index
-- idx_one_wake_per_parent_stage_generation is still the enforcement backstop
-- against any caller that claims without the lock: a unique-violation (23505
-- on that index) means "already woken for this generation" — see
-- duplicateStageWakeErr in issue_child_done.go, mirroring
-- duplicateRerunLineageErr (CHE-485) and idx_one_live_rerun_per_source_task_actor.
INSERT INTO stage_completion_wake (id, workspace_id, parent_issue_id, stage, generation, created_at)
VALUES ($1, $2, $3, $4, $5, now())
RETURNING id;

-- name: SetStageCompletionWakeComment :exec
-- Best-effort backfill of the system comment id onto an already-claimed wake
-- row, for observability. Never gates the notification: the claim in
-- RecordStageCompletionWake already committed before this runs.
UPDATE stage_completion_wake SET wake_comment_id = $2 WHERE id = $1;
