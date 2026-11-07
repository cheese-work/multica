package handler

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestEffectiveDueLiveGraphTiming times fillEffectiveDue (the per-read cost
// added to GET /api/issues/{id}) for every open issue of a workspace loaded
// into the test database. Skipped unless EFFECTIVE_DUE_BENCH_WORKSPACE is set:
//
//	EFFECTIVE_DUE_BENCH_WORKSPACE=<workspace uuid> go test ./internal/handler -run TestEffectiveDueLiveGraphTiming -count=1 -v
func TestEffectiveDueLiveGraphTiming(t *testing.T) {
	ws := os.Getenv("EFFECTIVE_DUE_BENCH_WORKSPACE")
	if ws == "" {
		t.Skip("EFFECTIVE_DUE_BENCH_WORKSPACE not set")
	}
	ctx := context.Background()
	var wsID pgtype.UUID
	if err := wsID.Scan(ws); err != nil {
		t.Fatal(err)
	}
	rows, err := testPool.Query(ctx, `SELECT id FROM issue WHERE workspace_id=$1 AND status NOT IN ('done','cancelled') ORDER BY number`, wsID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	issues := make([]db.Issue, 0, len(ids))
	for _, id := range ids {
		is, err := testHandler.Queries.GetIssue(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		issues = append(issues, is)
	}
	read := func(is db.Issue) time.Duration {
		var resp IssueResponse
		start := time.Now()
		testHandler.fillEffectiveDue(ctx, is, &resp)
		return time.Since(start)
	}
	for _, is := range issues { // warm-up pass, not recorded
		read(is)
	}
	const passes = 5
	var all []time.Duration
	withDate, withDiag := 0, 0
	for p := 0; p < passes; p++ {
		for _, is := range issues {
			var resp IssueResponse
			start := time.Now()
			testHandler.fillEffectiveDue(ctx, is, &resp)
			all = append(all, time.Since(start))
			if p == 0 {
				if resp.EffectiveDue != nil {
					withDate++
				}
				if len(resp.Diagnostics) > 0 {
					withDiag++
				}
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	q := func(f float64) time.Duration { return all[int(float64(len(all)-1)*f)] }
	t.Logf("open issues=%d reads=%d (passes=%d after 1 warm-up) with_effective_due=%d with_diagnostics=%d", len(issues), len(all), passes, withDate, withDiag)
	t.Logf("fillEffectiveDue p50=%s p95=%s p99=%s max=%s", q(0.50), q(0.95), q(0.99), all[len(all)-1])
}
