-- CHE-552: real protocol-omission base rate. CHE-529 (protocollint package,
-- wired into daemon.go's runProtocolLint on every CompleteTask) already
-- computes []protocollint.Violation for every completing agent turn, but
-- logProtocolLintViolations only slog.Error's them — nothing persists a
-- denominator (total turns checked) or a queryable violation record. This
-- table is that denominator: one row per protocol-lint check, cheap, no
-- second workflow-tracking model, no duplication of agent_task_queue.
--
-- No foreign keys per repo convention (see AGENTS.md) — task_id and issue_id
-- are validated in application code; the writer (runProtocolLint) already
-- holds both from a just-loaded db.AgentTaskQueue row.
--
-- issue_id is nullable: chat/non-issue tasks still run the evidence-URL
-- assertion (see runProtocolLint's early-return branch in daemon.go) and
-- still need a denominator row, but have no issue to attach to.
--
-- violation_codes is TEXT[] (not JSONB) — the values are always
-- protocollint.Violation.Code, a small closed set of stable identifiers, so a
-- plain array is enough to unnest and GROUP BY for the code-breakdown report;
-- no nested structure is ever needed. Empty array (not NULL) for a clean
-- completion, matching protocollint.Check's nil-slice-on-no-violations
-- convention but stored as '{}' so array_length / unnest behave the same for
-- every row rather than needing NULL-handling in every query.
CREATE TABLE IF NOT EXISTS protocol_lint_run (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          UUID NOT NULL,
    issue_id         UUID,
    checked_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    violation_count  INTEGER NOT NULL DEFAULT 0 CHECK (violation_count >= 0),
    violation_codes  TEXT[] NOT NULL DEFAULT '{}'
);
