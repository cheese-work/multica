package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/runtimeapps"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
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

	move, err := testHandler.moveIssueOnEdgeTx(ctx, tq, issue, "in_review", "ci_green_advance", nil, true, nil)
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
	if _, err := testHandler.moveIssueOnEdgeTx(ctx, tq, issue, "in_review", "ci_green_advance", nil, true, nil); err != nil {
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
	_, err = testHandler.moveIssueOnEdgeTx(ctx, testHandler.Queries.WithTx(tx), issue, "in_review", "ci_green_advance", nil, true, nil)
	var notConfirmed *service.StopNotConfirmedError
	if !errors.As(err, &notConfirmed) {
		t.Fatalf("moveIssueOnEdgeTx = %v, want *StopNotConfirmedError", err)
	}
}

// The wrapper rolls the move back before it records the refusal: the activity
// row needs a key-share lock on the issue row the move holds.
func TestTryMoveIssueOnEdgeRecordsStopNotConfirmedWithoutHanging(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := newHandoffEdgeCase(t, 2853004)
	supersededRun(t, agentID, handlerTestRuntimeID(t), c.issueID, "in_review", testutil.Cols{})
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		moved bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		moved, _, err := testHandler.tryMoveIssueOnEdge(context.Background(), issue, "in_review", "ci_green_advance", nil, true)
		done <- result{moved, err}
	}()
	select {
	case r := <-done:
		var notConfirmed *service.StopNotConfirmedError
		if r.moved || !errors.As(r.err, &notConfirmed) {
			t.Fatalf("tryMoveIssueOnEdge = moved %v, %v; want refused with *StopNotConfirmedError", r.moved, r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("tryMoveIssueOnEdge did not return: the refusal waits on the move's own lock")
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status %q, want in_progress", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='stop_not_confirmed'`, c.issueID); n != 1 {
		t.Fatalf("%d stop_not_confirmed rows, want 1", n)
	}
}

// A squad's leader run is admitted and inserted in the caller's transaction, so
// it commits or rolls back with the move. No workflow step hands off to a squad
// today, so the helper is driven directly.
func TestEnqueueSquadLeaderRunInTxRollsBackWithTheCaller(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := newHandoffEdgeCase(t, 2853005)
	sq := newSquadCommentTriggerFixture(t)
	setIssueAssigneeDirect(t, c.issueID, "squad", sq.SquadID)
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
	if _, _, err := testHandler.onQueries(tq).enqueueSquadLeaderRunInTx(ctx, tq, issue); err != nil {
		t.Fatalf("enqueueSquadLeaderRunInTx: %v", err)
	}
	if _, runs := edgeTxRows(t, tx, c.issueID); runs != 1 {
		t.Fatalf("%d runs inside the transaction, want the leader's 1", runs)
	}
	if _, runs := edgeTxRows(t, testPool, c.issueID); runs != 0 {
		t.Fatal("the leader run is visible before the caller commits")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, runs := edgeTxRows(t, testPool, c.issueID); runs != 0 {
		t.Fatalf("%d runs after rollback, want 0", runs)
	}
}

// poolReadingComposio stands for the Composio service, whose store reads
// through the pool.
type poolReadingComposio struct{ pool *pgxpool.Pool }

func (c poolReadingComposio) BuildTaskOverlay(ctx context.Context, _ pgtype.UUID, _ db.Agent) (runtimeapps.MCPOverlayResult, error) {
	var one int
	return runtimeapps.MCPOverlayResult{}, c.pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
}

// The move reads only through the caller's transaction, with the Composio
// integration enabled. On a pool of one connection, held by that transaction,
// any read through the pool waits for a connection that never frees; enough
// such moves at once exhaust the real pool the same way.
func TestMoveIssueOnEdgeTxReadsOnlyThroughTheCallersTransaction(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	c, _ := newHandoffEdgeCase(t, 2853005)
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testPool.Config()
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	h := New(db.New(small), small, realtime.NewHub(), events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{})
	t.Cleanup(giveBackRunBudgetHooks)
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.ComposioMCPApps, featureflag.Rule{Default: true})
	h.TaskService.FeatureFlags = featureflag.NewService(provider)
	h.TaskService.Composio = poolReadingComposio{pool: small}

	tx, err := small.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	moveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	move, err := h.moveIssueOnEdgeTx(moveCtx, h.Queries.WithTx(tx), issue, "in_review", "ci_green_advance", nil, true, nil)
	if err != nil {
		t.Fatalf("moveIssueOnEdgeTx on a pool of one held connection: %v", err)
	}
	if move.RunRefused != nil {
		t.Fatalf("run refused: %v", move.RunRefused)
	}
	if _, runs := edgeTxRows(t, tx, c.issueID); runs != 1 {
		t.Fatalf("%d runs inside the transaction, want 1", runs)
	}
}

// overlayComposio hands every run the same overlay and counts the builds.
type overlayComposio struct {
	overlay string
	builds  int
}

func (c *overlayComposio) BuildTaskOverlay(context.Context, pgtype.UUID, db.Agent) (runtimeapps.MCPOverlayResult, error) {
	c.builds++
	return runtimeapps.MCPOverlayResult{MCPOverlay: []byte(c.overlay)}, nil
}

// The run an edge move inserts carries the Composio overlay, built before the
// move's transaction opens.
func TestTryMoveIssueOnEdgeKeepsTheComposioOverlayOnTheRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	c, _ := newHandoffEdgeCase(t, 2853006)
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	h := New(db.New(testPool), testPool, realtime.NewHub(), events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{})
	t.Cleanup(giveBackRunBudgetHooks)
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.ComposioMCPApps, featureflag.Rule{Default: true})
	h.TaskService.FeatureFlags = featureflag.NewService(provider)
	composio := &overlayComposio{overlay: `{"mcpServers":{"composio":{"url":"https://example.test/mcp"}}}`}
	h.TaskService.Composio = composio

	moved, ranHandler, err := h.tryMoveIssueOnEdge(ctx, issue, "in_review", "ci_green_advance", nil, true)
	if err != nil || !moved || !ranHandler {
		t.Fatalf("tryMoveIssueOnEdge = moved %v, ran %v, err %v; want a move that ran the handler", moved, ranHandler, err)
	}
	var stored []byte
	if err := testPool.QueryRow(ctx, `SELECT runtime_mcp_overlay FROM agent_task_queue WHERE issue_id=$1`, c.issueID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == "" || composio.builds != 1 {
		t.Fatalf("run overlay %q after %d builds, want the built overlay from 1 build", stored, composio.builds)
	}
}
