package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// moveIssueOnEdgeTx writes the status, its history row and the next step's run
// in the caller's transaction (CHE-2853).

var handoffEdgeSteps = func(agentID string) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": agentID}, "instructions": "Review it."},
		{"status_key": "blocked"},
	}
}

func newHandoffEdgeCase(t *testing.T, installationID int64) (edgeCase, string) {
	t.Helper()
	agentID := createHandlerTestAgent(t, "edge tx handler", nil)
	c := newEdgeCase(t, installationID, handoffEdgeSteps(agentID))
	c.enter(t, "in_progress")
	return c, agentID
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func edgeTxRows(t *testing.T, db rowQuerier, issueID string) (history, runs int) {
	t.Helper()
	ctx := context.Background()
	if err := db.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='status_changed'`, issueID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	return history, runs
}

func TestMoveIssueOnEdgeTxWritesStatusHistoryAndRunTogether(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := newHandoffEdgeCase(t, 2853001)
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tq := testHandler.Queries.WithTx(tx)

	move, err := testHandler.moveIssueOnEdgeTx(ctx, tq, issue, "in_review", "ci_green_advance", nil, true)
	if err != nil {
		t.Fatalf("moveIssueOnEdgeTx: %v", err)
	}
	if move.RunRefused != nil {
		t.Fatalf("run refused: %v", move.RunRefused)
	}
	history, runs := edgeTxRows(t, tx, c.issueID)
	if history != 1 || runs != 1 {
		t.Fatalf("inside the transaction: %d history rows, %d runs, want 1 and 1", history, runs)
	}
	if got, _, _ := issueStatusAndAssignee(t, c.issueID); got == "in_review" {
		t.Fatal("the move is visible before the caller commits")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ran := move.AfterCommit(ctx); !ran {
		t.Fatal("AfterCommit reported no run for the handler")
	}
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, c.issueID)
	if status != "in_review" || assigneeType != "agent" || assigneeID != agentID {
		t.Fatalf("after commit: status %q assignee %s/%s, want in_review agent/%s", status, assigneeType, assigneeID, agentID)
	}
	if h, r := edgeTxRows(t, testPool, c.issueID); h != 1 || r != 1 {
		t.Fatalf("after commit: %d history rows, %d runs, want 1 and 1", h, r)
	}
}

// A failure after the status write takes the status, the history row and the
// run back together.
func TestMoveIssueOnEdgeTxRollsBackWithTheCaller(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := newHandoffEdgeCase(t, 2853002)
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tq := testHandler.Queries.WithTx(tx)
	if _, err := testHandler.moveIssueOnEdgeTx(ctx, tq, issue, "in_review", "ci_green_advance", nil, true); err != nil {
		t.Fatalf("moveIssueOnEdgeTx: %v", err)
	}
	if h, r := edgeTxRows(t, tx, c.issueID); h != 1 || r != 1 {
		t.Fatalf("inside the transaction: %d history rows, %d runs, want 1 and 1", h, r)
	}
	// the caller's step after the write fails
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status %q after rollback, want in_progress", got)
	}
	if h, r := edgeTxRows(t, testPool, c.issueID); h != 0 || r != 0 {
		t.Fatalf("after rollback: %d history rows, %d runs, want 0 and 0", h, r)
	}
}

// A run the stop gate holds back refuses the move instead of committing it
// without the run.
func TestMoveIssueOnEdgeTxRefusesWhenStopNotConfirmed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := newHandoffEdgeCase(t, 2853003)
	runtimeID := handlerTestRuntimeID(t)
	supersededRun(t, agentID, runtimeID, c.issueID, "in_review", testutil.Cols{})
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = testHandler.moveIssueOnEdgeTx(ctx, testHandler.Queries.WithTx(tx), issue, "in_review", "ci_green_advance", nil, true)
	var notConfirmed *service.StopNotConfirmedError
	if !errors.As(err, &notConfirmed) {
		t.Fatalf("moveIssueOnEdgeTx = %v, want *StopNotConfirmedError", err)
	}
}
