package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6887: LatestMemberStatusChange, LatestMemberStatusMove and
// NewestIssueStatusChange walk an issue's activity_log newest-first and test
// each row. With the issue's status rows older than its other activity they read
// all of it before they find, or fail to find, a match
// (idx_activity_log_status_moves is the partial index that fixes this).
//
// The oracle explains the production SQL (captured through sqlSpy) and bounds
// the rows the executor examined and the buffers it touched, derived from the
// fixture below rather than from a timer.
//
// Fixture, one issue, oldest to newest:
//
//	case b only: 1 old interactive member move (LatestMemberStatusChange accepts)
//	20 member moves through a PAT after the run-budget cutover
//	   (LatestMemberStatusChange rejects, LatestMemberStatusMove accepts)
//	20 member moves with cause workflow_remap (both reject)
//	50 status_changed by the system
//	300 status_changed by agents
//	10,000 non-status rows, all newer than every status row
//
// CHE-6887 asked for 5,000 non-status rows and a member bound of 450. With the
// index in place the planner reads LatestMemberStatusChange through a bitmap
// scan: each status row is counted once in the bitmap index scan and once in the
// heap recheck, so the honest bound is 2 x the status rows, about 800. The
// control must stay 10 x above that bound, which 5,000 rows cannot do; 10,000
// can (about 10,400 without the index).
const (
	oracleNonStatusRows = 10000
	oracleAgentMoves    = 300
	oracleSystemMoves   = 50
	oracleRemapMoves    = 20
	oraclePATMoves      = 20
	// A member query reads at most every status row of the issue (390, or 391
	// with the case b interactive row), and a bitmap plan counts each twice. The
	// bound is 2 x the status rows plus 20 for the limit, sort and cutover nodes
	// (see memberRows in the test).
	oracleMemberRowSlack = 20
	// NewestIssueStatusChange stops at the first index entry: the limit node, the
	// index scan row and a margin for one top-up row.
	oracleNewestRowsBound = 10
	// Without the partial index a query reads the 5,000 non-status rows too, so
	// a predicate the index cannot serve must exceed 10 x the bound.
	oracleControlFactor = 10
)

type statusMoveFixture struct {
	issueID string
	// statusRows is how many status_changed rows the issue holds.
	statusRows int
	// statusHeapPages is the number of distinct heap pages that hold this
	// issue's status rows: the pages a status-only index scan must visit.
	statusHeapPages int
}

func seedStatusMoveIssue(t *testing.T, withInteractive bool) statusMoveFixture {
	t.Helper()
	issueID := dbfx.Issue(t, "status-move oracle")
	var cutover time.Time
	dbfx.QueryRow(t, `SELECT cutover_at FROM run_budget_window_cutover`).Scan(&cutover)
	// Every row sits after the cutover, so only the trigger decides which
	// queries accept a member move; seconds apart keeps created_at distinct.
	base := cutover.Add(time.Hour)
	next := 0
	seed := func(n int, actor, action, details string) {
		t.Helper()
		if n == 0 {
			return
		}
		dbfx.Exec(t, `
			INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
			SELECT $1, $2::uuid, $3::text, CASE WHEN $3::text = 'member' THEN $4::uuid END, $5::text, $6::jsonb,
			       $7::timestamptz + (($8::int + g) * interval '1 second')
			FROM generate_series(1, $9::int) g`,
			testWorkspaceID, issueID, actor, testUserID, action, details, base, next, n)
		next += n
	}
	if withInteractive {
		seed(1, "member", "status_changed",
			`{"trigger":{"auth_source":"interactive_jwt","cause":{"kind":"ui_or_api"}}}`)
	}
	seed(oraclePATMoves, "member", "status_changed",
		`{"trigger":{"auth_source":"pat","cause":{"kind":"ui_or_api"}}}`)
	seed(oracleRemapMoves, "member", "status_changed",
		`{"trigger":{"auth_source":"interactive_jwt","cause":{"kind":"workflow_remap"}}}`)
	seed(oracleSystemMoves, "system", "status_changed", `{}`)
	seed(oracleAgentMoves, "agent", "status_changed", `{}`)
	seed(oracleNonStatusRows, "agent", "comment_created", `{}`)
	dbfx.Exec(t, `ANALYZE activity_log`)

	var pages, rows int
	dbfx.QueryRow(t, `
		SELECT count(DISTINCT (ctid::text::point)[0]), count(*) FROM activity_log
		WHERE issue_id = $1::uuid AND action IN ('status_changed', 'run_budget_top_up')`, issueID).Scan(&pages, &rows)
	return statusMoveFixture{issueID: issueID, statusRows: rows, statusHeapPages: pages}
}

// spySQL returns the statement q sends for call.
func spySQL(t *testing.T, call func(q *db.Queries)) string {
	t.Helper()
	spy := &sqlSpy{DBTX: testPool}
	call(db.New(spy))
	if spy.sql == "" {
		t.Fatal("the query sent no statement")
	}
	return spy.sql
}

// unindexable makes the predicate the partial index serves unusable, so the
// planner falls back to idx_activity_log_issue_keyset: the pre-index behaviour,
// measured on the same fixture and the same database.
func unindexable(t *testing.T, sql string) string {
	t.Helper()
	out := strings.NewReplacer(
		"action IN ('status_changed', 'run_budget_top_up')", "action || '' IN ('status_changed', 'run_budget_top_up')",
		"action = 'status_changed'", "action || '' = 'status_changed'",
	).Replace(sql)
	if out == sql {
		t.Fatalf("no indexable predicate found to disable in:\n%s", sql)
	}
	return out
}

func TestActivityLogStatusMovesOracle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		withInteractive bool
	}{
		{"a_no_interactive_member_move", false},
		{"b_old_interactive_member_move", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedStatusMoveIssue(t, tc.withInteractive)
			id := parseUUID(fx.issueID)

			type query struct {
				name       string
				rowsBound  float64
				bufBound   float64
				call       func(q *db.Queries) error
				wantResult func(t *testing.T, err error)
			}
			// Buffers: the status heap pages, plus 20 for the index descent,
			// its leaf pages and the cutover lookup.
			memberBuf := float64(fx.statusHeapPages + 20)
			memberRows := float64(2*fx.statusRows + oracleMemberRowSlack)
			queries := []query{
				{
					name: "LatestMemberStatusChange", rowsBound: memberRows, bufBound: memberBuf,
					call: func(q *db.Queries) error { _, err := q.LatestMemberStatusChange(ctx, id); return err },
					wantResult: func(t *testing.T, err error) {
						if tc.withInteractive && err != nil {
							t.Fatalf("interactive move not found: %v", err)
						}
						if !tc.withInteractive && !errors.Is(err, pgx.ErrNoRows) {
							t.Fatalf("want no row (PAT and remap moves are rejected), got %v", err)
						}
					},
				},
				{
					name: "LatestMemberStatusMove", rowsBound: memberRows, bufBound: memberBuf,
					call: func(q *db.Queries) error { _, err := q.LatestMemberStatusMove(ctx, id); return err },
					wantResult: func(t *testing.T, err error) {
						if err != nil {
							t.Fatalf("PAT move not found: %v", err)
						}
					},
				},
				{
					name: "NewestIssueStatusChange", rowsBound: oracleNewestRowsBound, bufBound: 10,
					call: func(q *db.Queries) error { _, err := q.NewestIssueStatusChange(ctx, id); return err },
					wantResult: func(t *testing.T, err error) {
						if err != nil {
							t.Fatalf("newest status change not found: %v", err)
						}
					},
				},
			}
			for _, qc := range queries {
				t.Run(qc.name, func(t *testing.T) {
					sql := spySQL(t, func(q *db.Queries) { _ = qc.call(q) })
					qc.wantResult(t, qc.call(db.New(testPool)))

					rows, buffers := explainWork(t, sql, id)
					t.Logf("rows examined %.0f (bound %.0f), buffers %.0f (bound %.0f)", rows, qc.rowsBound, buffers, qc.bufBound)
					if rows > qc.rowsBound {
						t.Errorf("rows examined = %.0f, want <= %.0f", rows, qc.rowsBound)
					}
					if buffers > qc.bufBound {
						t.Errorf("buffers = %.0f, want <= %.0f", buffers, qc.bufBound)
					}
					t.Logf("plan:\n%s", explainText(t, sql, id))

					// Negative control: the same fixture with a predicate the
					// index cannot serve must read far more, so the bound above
					// can fail.
					controlRows, _ := explainWork(t, unindexable(t, sql), id)
					t.Logf("control rows examined %.0f (must exceed %.0f)", controlRows, oracleControlFactor*qc.rowsBound)
					if controlRows <= oracleControlFactor*qc.rowsBound {
						t.Errorf("control rows examined = %.0f, want > %.0f: the fixture no longer separates the indexed plan",
							controlRows, oracleControlFactor*qc.rowsBound)
					}
				})
			}
		})
	}
}
