-- =====================
-- Protocol Lint Run
-- =====================
--
-- CHE-552: persists one row per protocollint.Check invocation
-- (server/internal/handler/daemon.go's runProtocolLint, CHE-529) so the real
-- protocol-omission base rate can be measured instead of only logged. See
-- 500_protocol_lint_run.up.sql for the schema rationale.

-- name: CreateProtocolLintRun :one
-- Best-effort write from runProtocolLint's logProtocolLintViolations, called
-- after CompleteTask's own transaction has already committed — see that
-- function's doc for why this insert must never block or fail the request
-- (log a warning on error, do not propagate). One row per Check call, so
-- both runProtocolLint call sites (chat/non-issue early-return and the
-- normal issue path) produce exactly one row each.
INSERT INTO protocol_lint_run (
    task_id, issue_id, violation_count, violation_codes
) VALUES (
    sqlc.arg('task_id'), sqlc.narg('issue_id'), sqlc.arg('violation_count'), sqlc.arg('violation_codes')
)
RETURNING *;

-- name: ReportProtocolLintOmissionRate :one
-- Base-rate summary over [since, until): total turns checked, how many had
-- zero violations, and the zero-violation percentage. Percentage is computed
-- in Go from the two counts (avoids a division-by-zero / rounding footgun in
-- SQL for the zero-rows case) — see BuildProtocolLintOmissionReport in
-- server/internal/handler/protocol_lint_report.go.
SELECT
    count(*)::bigint AS total_checked,
    count(*) FILTER (WHERE violation_count = 0)::bigint AS zero_violation_count
FROM protocol_lint_run
WHERE checked_at >= sqlc.arg('since') AND checked_at < sqlc.arg('until');

-- name: ReportProtocolLintViolationBreakdown :many
-- Violation-code breakdown over the same window: how many times each
-- protocollint.Violation.Code fired, across every checked run. unnest over
-- violation_codes so a single run with two different violation codes counts
-- once per code, not once per run.
SELECT
    code::text AS code,
    count(*)::bigint AS occurrences
FROM protocol_lint_run, unnest(violation_codes) AS code
WHERE checked_at >= sqlc.arg('since') AND checked_at < sqlc.arg('until')
GROUP BY code
ORDER BY occurrences DESC, code ASC;
