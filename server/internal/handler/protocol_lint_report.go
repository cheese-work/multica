package handler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProtocolLintOmissionReport is the real protocol-omission base rate over a
// time window (CHE-552): how many completing turns were checked by
// protocollint.Check (server/internal/handler/daemon.go's runProtocolLint),
// how many came back completely clean, and a breakdown of which
// protocollint.Violation.Code fired and how often. Built from
// protocol_lint_run, the durable record recordProtocolLintRun writes for
// every check.
type ProtocolLintOmissionReport struct {
	// Since and Until are the half-open window [Since, Until) the report
	// covers.
	Since time.Time
	Until time.Time

	// TotalChecked is the denominator: every protocol_lint_run row in the
	// window, i.e. every completing turn that was actually checked.
	TotalChecked int64

	// ZeroViolationCount is how many of those turns had violation_count = 0.
	ZeroViolationCount int64

	// ZeroViolationPercent is ZeroViolationCount / TotalChecked * 100,
	// computed here (not in SQL) to sidestep a division-by-zero /
	// rounding footgun when TotalChecked is 0 — an empty window reports 0,
	// not NaN or a SQL error.
	ZeroViolationPercent float64

	// ViolationCodeCounts breaks down every violation occurrence in the
	// window by protocollint.Violation.Code, ordered most-frequent first
	// (ties broken alphabetically by code, matching the underlying query's
	// ORDER BY). A turn with two different violation codes contributes one
	// count to each code, not one count split between them.
	ViolationCodeCounts []ProtocolLintCodeCount
}

// ProtocolLintCodeCount is one row of a ProtocolLintOmissionReport's
// violation-code breakdown.
type ProtocolLintCodeCount struct {
	Code        string
	Occurrences int64
}

// BuildProtocolLintOmissionReport computes the CHE-552 omission-rate report
// for the half-open window [since, until) by querying protocol_lint_run.
// Read-only; safe to call from a test, a one-off script, or a future CLI/API
// surface without touching any request-serving hot path.
func BuildProtocolLintOmissionReport(ctx context.Context, q *db.Queries, since, until time.Time) (ProtocolLintOmissionReport, error) {
	sinceArg := pgtype.Timestamptz{Time: since, Valid: true}
	untilArg := pgtype.Timestamptz{Time: until, Valid: true}

	rate, err := q.ReportProtocolLintOmissionRate(ctx, db.ReportProtocolLintOmissionRateParams{
		Since: sinceArg,
		Until: untilArg,
	})
	if err != nil {
		return ProtocolLintOmissionReport{}, err
	}

	breakdown, err := q.ReportProtocolLintViolationBreakdown(ctx, db.ReportProtocolLintViolationBreakdownParams{
		Since: sinceArg,
		Until: untilArg,
	})
	if err != nil {
		return ProtocolLintOmissionReport{}, err
	}

	report := ProtocolLintOmissionReport{
		Since:              since,
		Until:              until,
		TotalChecked:       rate.TotalChecked,
		ZeroViolationCount: rate.ZeroViolationCount,
	}
	if rate.TotalChecked > 0 {
		report.ZeroViolationPercent = float64(rate.ZeroViolationCount) / float64(rate.TotalChecked) * 100
	}
	for _, row := range breakdown {
		report.ViolationCodeCounts = append(report.ViolationCodeCounts, ProtocolLintCodeCount{
			Code:        row.Code,
			Occurrences: row.Occurrences,
		})
	}

	return report, nil
}
