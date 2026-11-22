package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// countingTxStarter counts every statement the first transaction it starts
// sends to the database. Later transactions (the background condition pass)
// are not counted.
type countingTxStarter struct {
	TxStarter
	statements *int
	once       *sync.Once
}

func (c countingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	counted := false
	c.once.Do(func() { counted = true })
	if !counted {
		return tx, nil
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

	for _, status := range []string{"paused", "in_progress"} {
		var statements int
		tasks := &TaskService{Queries: wakeups.Tasks.Queries, Bus: wakeups.Tasks.Bus,
			TxStarter: countingTxStarter{TxStarter: wakeups.Tasks.TxStarter, statements: &statements, once: new(sync.Once)}}
		started := time.Now()
		_, err := tasks.UpdateProject(ctx, util.MustParseUUID(fixture.WorkspaceID), db.UpdateProjectParams{
			ID: util.MustParseUUID(projectID), Status: pgtype.Text{String: status, Valid: true},
		}, false, nil)
		if err != nil {
			test.Fatalf("flip to %s: %v", status, err)
		}
		test.Logf("flip to %s: %d statements, %s", status, statements, time.Since(started))
		if statements > 50 {
			test.Errorf("flip to %s sent %d statements for %d rules; want a constant handful", status, statements, rules)
		}
	}
	// The rules settle as before: overdue slots skipped, due one-shots off, and
	// every condition rule baselined by the pass after the commit.
	var stale int
	fixture.QueryRow(test, `SELECT count(*) FROM issue_wakeup w JOIN issue i ON i.id=w.issue_id
		WHERE i.project_id=$1 AND w.enabled AND w.condition IS NULL AND w.next_fire_at<=now()`, projectID).Scan(&stale)
	if stale != 0 {
		test.Errorf("%d enabled non-condition rules are still overdue after the flips", stale)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var held int
		fixture.QueryRow(test, `SELECT count(*) FROM issue_wakeup w JOIN issue i ON i.id=w.issue_id
			WHERE i.project_id=$1 AND w.enabled AND w.condition IS NOT NULL AND w.next_fire_at>now()+interval '1 minute'`, projectID).Scan(&held)
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			test.Fatalf("%d condition rules were never baselined after the flips", held)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
