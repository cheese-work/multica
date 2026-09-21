-- Durable per-issue, per-agent checkpoint store (CHE-489/CHE-593): a
-- claim-time snapshot of what a prior run already covered, so a later claim
-- can skip re-reading resolved history instead of re-scanning the whole
-- comment tree from scratch. See server/internal/checkpoint for the
-- zero-model diff/render logic this table backs.
--
-- No foreign keys per repo convention (see CLAUDE.md) — workspace_id,
-- issue_id, and agent_id are validated in application code.
--
-- Table, not agent_task_queue.context JSONB: a checkpoint's useful lifetime
-- outlives any single task row (agent_task_queue rows are per-run and get
-- reaped/rotated independently), and a checkpoint must be readable at the
-- next claim before that claim's own task row exists yet. Keying off the
-- most recent completed task's context would also require a join back
-- through task history on every claim; a dedicated table keyed on
-- (issue_id, agent_id) is a single point lookup instead.
--
-- One live checkpoint per (issue_id, agent_id): a later write overwrites the
-- earlier one (upsert), since only the most recent coverage per issue+agent
-- is ever useful — an older checkpoint bettered by a newer one has no reader.
--
-- coverage, accepted_decisions, obligations, blockers, evidence, and
-- resolved_threads store the Checkpoint content contract fields (see
-- checkpoint.Checkpoint) as JSONB rather than normalized rows: they are
-- always read/written whole (never queried by sub-field) and their shape is
-- owned entirely by the checkpoint package, not by SQL.
CREATE TABLE IF NOT EXISTS issue_checkpoint (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          UUID NOT NULL,
    issue_id              UUID NOT NULL,
    agent_id              UUID NOT NULL,
    issue_revision        BIGINT NOT NULL,
    candidate_id          TEXT NOT NULL DEFAULT '',
    coverage              JSONB NOT NULL DEFAULT '{}'::jsonb,
    accepted_decisions    JSONB NOT NULL DEFAULT '[]'::jsonb,
    obligations           JSONB NOT NULL DEFAULT '[]'::jsonb,
    blockers              JSONB NOT NULL DEFAULT '[]'::jsonb,
    next_permitted_action TEXT NOT NULL DEFAULT '',
    evidence              JSONB NOT NULL DEFAULT '[]'::jsonb,
    resolved_threads      JSONB NOT NULL DEFAULT '[]'::jsonb,
    built_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
