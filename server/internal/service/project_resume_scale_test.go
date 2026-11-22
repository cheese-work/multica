package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// countingTxStarter counts every statement a transaction sends to the database.
type countingTxStarter struct {
	TxStarter
	statements *int
}

func (c countingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return countingTx{Tx: tx, statements: c.statements}, nil
}

type countingTx struct {
	pgx.Tx
	statements *int
}

func (c countingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	*c.statements++
	return c.Tx.Exec(ctx, sql, args...)
}

func (c countingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	*c.statements++
	return c.Tx.Query(ctx, sql, args...)
}

func (c countingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	*c.statements++
	return c.Tx.QueryRow(ctx, sql, args...)
}

// seedProjectWakeups spreads `perIssue` clones of one enabled event rule over
// `issues` issues of the project (an issue holds at most 32 enabled rules),
// then turns the clones into a realistic mix: condition rules, overdue every
// and cron rules, and overdue one-shots.
func seedProjectWakeups(test *testing.T, fixture principalFixture, template db.IssueWakeup, projectID string, issues, perIssue int) {
	test.Helper()
	ids := make([]pgtype.UUID, issues)
	for i := range ids {
		ids[i] = parseTestUUID(test, fixture.Issue(test, fmt.Sprintf("Scale issue %d", i)))
	}
	fixture.Cleanup(test, "DELETE FROM issue_wakeup WHERE workspace_id=$1", fixture.WorkspaceID)
	fixture.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=ANY($1)", ids, projectID)
	rows, err := fixture.Pool.Query(context.Background(), "SELECT column_name FROM information_schema.columns WHERE table_name='issue_wakeup' AND table_schema='public' AND column_name NOT IN ('id','issue_id') ORDER BY ordinal_position")
	if err != nil {
		test.Fatal(err)
	}
	var columns, prefixed []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			test.Fatal(err)
		}
		columns = append(columns, `"`+column+`"`)
		prefixed = append(prefixed, `w."`+column+`"`)
	}
	rows.Close()
	fixture.Exec(test, fmt.Sprintf("INSERT INTO issue_wakeup (id,issue_id,%s) SELECT gen_random_uuid(),i,%s FROM issue_wakeup w, unnest($3::uuid[]) i, generate_series(1,$2) WHERE w.id=$1", strings.Join(columns, ","), strings.Join(prefixed, ",")), template.ID, perIssue, ids)
	// Four equal slices, picked by row order.
	fixture.Exec(test, `WITH numbered AS (SELECT id,row_number() OVER (ORDER BY id) AS n FROM issue_wakeup WHERE issue_id=ANY($1))
UPDATE issue_wakeup w SET
  condition=CASE WHEN n%4=0 THEN '{"type":"issue_field","field":"status","value":"todo"}'::jsonb END,
  kind=CASE WHEN n%4=1 THEN 'every' WHEN n%4=2 THEN 'cron' WHEN n%4=3 THEN 'at' ELSE w.kind END,
  mode=CASE WHEN n%4 IN (1,2) THEN 'continuous' ELSE w.mode END,
  interval_seconds=CASE WHEN n%4=1 THEN 60 END,
  cron_expression=CASE WHEN n%4=2 THEN '*/5 * * * *' END,
  next_fire_at=CASE WHEN n%4=0 THEN NULL ELSE now()-interval '3 days' END
FROM numbered WHERE numbered.id=w.id`, ids)
}

// Resuming a large paused project must not cost a round trip per wakeup rule.
// CHE-9000: a 5,365-issue project timed out the status change at 30 s and
// rolled back, so the project stayed paused.
func TestProjectStatusChangeCostIsIndependentOfWakeupCount(test *testing.T) {
	fixture, wakeups, issue, agent := conditionFixture(test)
	ctx := context.Background()
	projectID := fixture.Project(test, "Large project", testutil.Cols{"status": "in_progress"})
	fixture.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issue, projectID)
	template := wakeCreate(test, fixture, wakeups, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "Template"})
	const rules = 2000
	seedProjectWakeups(test, fixture, template, projectID, 100, rules/100)

	var statements int
	tasks := *wakeups.Tasks
	tasks.TxStarter = countingTxStarter{TxStarter: tasks.TxStarter, statements: &statements}
	for _, status := range []string{"paused", "in_progress"} {
		statements = 0
		_, err := tasks.UpdateProject(ctx, util.MustParseUUID(fixture.WorkspaceID), db.UpdateProjectParams{
			ID: util.MustParseUUID(projectID), Status: pgtype.Text{String: status, Valid: true},
		}, false, nil)
		if err != nil {
			test.Fatalf("flip to %s: %v", status, err)
		}
		if statements > 50 {
			test.Errorf("flip to %s sent %d statements for %d rules; want a constant handful", status, statements, rules)
		}
	}
}
