-- CHE-552: the omission-rate report (ReportProtocolLintOmissionRate /
-- ReportProtocolLintViolationBreakdown, server/pkg/db/queries/protocol_lint.sql)
-- always filters protocol_lint_run by a checked_at date range. Built
-- CONCURRENTLY and in its own migration file per repo convention (see
-- AGENTS.md) since it is not part of the CREATE TABLE statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_protocol_lint_run_checked_at
    ON protocol_lint_run (checked_at);
