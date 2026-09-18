package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestBuildProtocolLintOmissionReport_Aggregation seeds a known set of
// protocol_lint_run rows — some clean, some with one or two violation codes,
// one deliberately outside the report window — and verifies
// BuildProtocolLintOmissionReport's math: total checked, zero-violation
// count/percentage, and the per-code breakdown all match hand-computed
// expectations. This is the report's only test of correctness against real
// aggregation SQL (ReportProtocolLintOmissionRate /
// ReportProtocolLintViolationBreakdown), independent of whether any
// production data exists yet.
func TestBuildProtocolLintOmissionReport_Aggregation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	queries := db.New(testPool)

	var agentID, runtimeID string
	dbfx.QueryRow(t, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID)

	issueID := dbfx.Issue(t, "che-552 report aggregation fixture")

	// Window under test: [windowStart, windowEnd), pinned to a fixed
	// far-past date rather than time.Now() so no other test in this package
	// — including TestCompleteTask_PersistsProtocolLintRun_* above, which
	// writes real protocol_lint_run rows with checked_at = now() through the
	// actual handler path — can ever land a row inside it.
	windowStart := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2020, 6, 2, 0, 0, 0, 0, time.UTC)
	insideEarly := windowStart.Add(10 * time.Minute)
	insideLate := windowEnd.Add(-10 * time.Minute)
	outsideBefore := windowStart.Add(-10 * time.Minute)

	seed := func(checkedAt time.Time, violationCount int, codes []string) string {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID,
			"issue_id":   issueID,
			"status":     "running",
		})
		return dbfx.Insert(t, "protocol_lint_run", testutil.Cols{
			"task_id":         taskID,
			"issue_id":        issueID,
			"checked_at":      checkedAt,
			"violation_count": violationCount,
			"violation_codes": codes,
		})
	}

	// In-window: 3 clean, 1 with reply_parent_mismatch, 1 with both
	// reply_parent_mismatch and unsupported_waiver.
	seed(insideEarly, 0, []string{})
	seed(insideEarly, 0, []string{})
	seed(insideLate, 0, []string{})
	seed(insideLate, 1, []string{"reply_parent_mismatch"})
	seed(insideLate, 2, []string{"reply_parent_mismatch", "unsupported_waiver"})

	// Out-of-window row must not affect the totals.
	seed(outsideBefore, 1, []string{"evidence_url_malformed"})

	report, err := BuildProtocolLintOmissionReport(ctx, queries, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("BuildProtocolLintOmissionReport: %v", err)
	}

	if report.TotalChecked != 5 {
		t.Fatalf("TotalChecked = %d, want 5", report.TotalChecked)
	}
	if report.ZeroViolationCount != 3 {
		t.Fatalf("ZeroViolationCount = %d, want 3", report.ZeroViolationCount)
	}
	wantPercent := 3.0 / 5.0 * 100
	if diff := report.ZeroViolationPercent - wantPercent; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ZeroViolationPercent = %v, want %v", report.ZeroViolationPercent, wantPercent)
	}

	byCode := map[string]int64{}
	for _, c := range report.ViolationCodeCounts {
		byCode[c.Code] = c.Occurrences
	}
	if byCode["reply_parent_mismatch"] != 2 {
		t.Fatalf("reply_parent_mismatch occurrences = %d, want 2", byCode["reply_parent_mismatch"])
	}
	if byCode["unsupported_waiver"] != 1 {
		t.Fatalf("unsupported_waiver occurrences = %d, want 1", byCode["unsupported_waiver"])
	}
	if _, ok := byCode["evidence_url_malformed"]; ok {
		t.Fatalf("evidence_url_malformed from the out-of-window row leaked into the breakdown: %v", byCode)
	}
	// Ordered most-frequent first: reply_parent_mismatch (2) before
	// unsupported_waiver (1).
	if len(report.ViolationCodeCounts) < 2 || report.ViolationCodeCounts[0].Code != "reply_parent_mismatch" {
		t.Fatalf("ViolationCodeCounts not ordered by occurrences desc: %+v", report.ViolationCodeCounts)
	}
}

// TestBuildProtocolLintOmissionReport_EmptyWindow guards the
// division-by-zero footgun ZeroViolationPercent's doc calls out: a window
// with no checked turns must report 0%, not NaN or a panic.
func TestBuildProtocolLintOmissionReport_EmptyWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	queries := db.New(testPool)

	// A window far enough in the past that no other test's seeded rows can
	// land in it.
	until := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	report, err := BuildProtocolLintOmissionReport(ctx, queries, since, until)
	if err != nil {
		t.Fatalf("BuildProtocolLintOmissionReport: %v", err)
	}
	if report.TotalChecked != 0 {
		t.Fatalf("TotalChecked = %d, want 0", report.TotalChecked)
	}
	if report.ZeroViolationPercent != 0 {
		t.Fatalf("ZeroViolationPercent = %v, want 0", report.ZeroViolationPercent)
	}
	if len(report.ViolationCodeCounts) != 0 {
		t.Fatalf("ViolationCodeCounts = %v, want empty", report.ViolationCodeCounts)
	}
}
