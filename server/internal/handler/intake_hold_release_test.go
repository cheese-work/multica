package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7072 (child A3 of CHE-6911): a person's release is stored as the key
// details.intake_release on the person's own status row, under the issue lock.

// releasedBy is the entry source the fixture stores; the key carries it.
const releasedBy = "CHE-6911"

// personWriter is one of the three writes that store a status row.
type personWriter struct {
	name string
	// move is a person's move of the issue to status, through handler.
	move func(test *testing.T, handler *Handler, issueID, status string) *testutil.Response
}

func asPerson(request *http.Request) *http.Request {
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	return request
}

var personWriters = []personWriter{
	{"single update", func(test *testing.T, handler *Handler, issueID, status string) *testutil.Response {
		return testutil.Call(test, handler.UpdateIssue, asPerson(withURLParam(newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": status}), "id", issueID)))
	}},
	{"batch update", func(test *testing.T, handler *Handler, issueID, status string) *testutil.Response {
		return testutil.Call(test, handler.BatchUpdateIssues, asPerson(newRequest("POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID, map[string]any{
			"issue_ids": []string{issueID}, "updates": map[string]any{"status": status},
		})))
	}},
	{"issue route", func(test *testing.T, handler *Handler, issueID, status string) *testutil.Response {
		return testutil.Call(test, handler.RouteIssue, asPerson(withURLParam(newRequest("POST", "/api/issues/"+issueID+"/route", map[string]any{
			"status": status, "reason": "person", "confidence": 1.0,
		}), "id", issueID)))
	}},
}

// statusRowOf returns the stored status row of one move. found is false when
// the issue has no such row; key is its intake_release, "" when it has none.
func statusRowOf(test *testing.T, issueID, from, to string) (key string, createdAt time.Time, found bool) {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT COALESCE(details ->> 'intake_release', ''), created_at FROM activity_log
		WHERE issue_id = $1 AND action = 'status_changed' AND details ->> 'from' = $2 AND details ->> 'to' = $3`, issueID, from, to)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if found {
			test.Fatalf("two status rows from %s to %s", from, to)
		}
		if err := rows.Scan(&key, &createdAt); err != nil {
			test.Fatal(err)
		}
		found = true
	}
	return
}

func wantReleaseKey(test *testing.T, issueID, from, to string) {
	test.Helper()
	key, _, found := statusRowOf(test, issueID, from, to)
	if !found {
		test.Fatalf("no status row from %s to %s", from, to)
	}
	if key != releasedBy {
		test.Errorf("row %s to %s: intake_release = %q, want %q", from, to, key, releasedBy)
	}
}

// wantNoKey asserts that no status row of the issue carries the key.
func wantNoKey(test *testing.T, issueID string) {
	test.Helper()
	if keyed := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' AND details ? 'intake_release'`, issueID); keyed != 0 {
		test.Fatalf("%d status rows of the issue carry intake_release, want none", keyed)
	}
}

func queuedHandlerRuns(test *testing.T, issueID, agentID string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, issueID, agentID)
}

// releaseCard is a held issue in a project whose workflow has the steps a
// request parks to. The in_progress step hands to the same agent from backlog
// and from need_decision.
type releaseCard struct {
	core    requestCoreFixture
	hold    holdFixture
	issueID string
	handler string
}

// newUnparkedCard is newHeldCard up to the request: the issue is held in
// backlog and no request has parked it.
func newUnparkedCard(test *testing.T) releaseCard {
	test.Helper()
	resetDecisionLease(test)
	readinessID := ensureParkCatalog(test)
	hold := newHoldFixture(test, nil)
	flow := dbfx.Insert(test, "issue_workflow", testutil.Cols{"workspace_id": testWorkspaceID, "name": "hold " + hold.projectID, "initial_status_key": "backlog", "steps": `[{"status_key":"backlog"},{"status_key":"in_progress"},{"status_key":"need_decision"},{"status_key":"blocked"}]`})
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, flow, hold.projectID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE project SET workflow_id = NULL WHERE id = $1`, hold.projectID)
		dbfx.Exec(test, `DELETE FROM issue_workflow WHERE id = $1`, flow)
	})
	core := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'backlog', project_id = $1, properties = jsonb_build_object($2::text, $3::text) WHERE id = $4`, hold.projectID, readinessID, heldReadiness, core.issueID)
	var err error
	if core.issue, err = testHandler.Queries.GetIssue(context.Background(), parseUUID(core.issueID)); err != nil {
		test.Fatal(err)
	}
	agentID := seededReadyAgentID(test)
	setColumnHandoff(test, hold.projectID, "in_progress", handToAgent(agentID, "Go.")).Want(http.StatusOK)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, core.issueID)
	})
	return releaseCard{core, hold, core.issueID, agentID}
}

// park sends the request through the real path, which parks the issue.
func (card releaseCard) park(test *testing.T) error {
	input := card.core.input()
	input.EscalationClass = "reversible"
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(card.issueID))
	if err != nil {
		return err
	}
	result, err := testHandler.createDecisionRequest(card.core.request(), issue, input)
	if err != nil {
		return err
	}
	if result.ParkedActivity == nil {
		return errors.New("the request did not park the issue")
	}
	return nil
}

func newReleaseCard(test *testing.T) releaseCard {
	test.Helper()
	card := newUnparkedCard(test)
	if err := card.park(test); err != nil {
		test.Fatal(err)
	}
	return card
}

func (card releaseCard) parkRows(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, card.issueID)
}

// wantStillReleasedAfterReturn: the person's move out of the parked state left
// the issue released, so an agent's move back to backlog passes and the hold
// does not return, while the park row is still stored.
func (card releaseCard) wantStillReleasedAfterReturn(test *testing.T) {
	test.Helper()
	holdAgentPut(test, card.issueID, "backlog").Want(http.StatusOK)
	if got := card.parkRows(test); got != 1 {
		test.Fatalf("park rows = %d, want the park row still stored", got)
	}
	wantHold(test, card.issueID, false, false, "released_by_member")
	holdAgentPut(test, card.issueID, "in_progress").Want(http.StatusOK)
}

// 8 (b), sequence: park, then a person's move, through each writer.
func TestIntakeReleaseKeyAfterPark(test *testing.T) {
	seedTestCatalog(test)
	for _, writer := range personWriters {
		test.Run(writer.name, func(test *testing.T) {
			card := newReleaseCard(test)
			wantIntakeRefusal(test, holdAgentPut(test, card.issueID, "in_progress"))

			writer.move(test, testHandler, card.issueID, "in_progress").Want(http.StatusOK)
			wantReleaseKey(test, card.issueID, "need_decision", "in_progress")
			if queued := queuedHandlerRuns(test, card.issueID, card.handler); queued != 1 {
				test.Errorf("queued handler runs = %d, want 1", queued)
			}
			if got := card.parkRows(test); got != 1 {
				test.Fatalf("park rows = %d, want the park row still stored", got)
			}
			wantHold(test, card.issueID, false, false, "left_backlog")
			card.wantStillReleasedAfterReturn(test)
		})
	}
}

// releaseGapStarter wraps the handler's transaction starter. Its transactions run
// hook once, before the status write locks the issue: the person's transaction
// has begun, and the park commits before the person's write takes the lock.
type releaseGapStarter struct {
	txStarter
	hook func()
}

func (starter *releaseGapStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return releaseGapTx{tx, starter}, nil
}

type releaseGapTx struct {
	pgx.Tx
	starter *releaseGapStarter
}

func (tx releaseGapTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if hook := tx.starter.hook; hook != nil && strings.Contains(sql, "name: LockIssueForDescriptionUpdate") {
		tx.starter.hook = nil // one shot: the hook's own writes pass through
		hook()
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// 8 (b), transaction barrier: the means is a wrapper of the handler's
// transaction starter that parks the issue, through the real request path,
// once, after the person's transaction began and before its issue lock.
func TestIntakeReleaseKeyTransactionBarrier(test *testing.T) {
	seedTestCatalog(test)
	for _, writer := range personWriters {
		test.Run(writer.name, func(test *testing.T) {
			card := newUnparkedCard(test)
			wantHold(test, card.issueID, true, true, "held")

			barrier := &releaseGapStarter{txStarter: testHandler.TxStarter}
			parked := false
			barrier.hook = func() {
				if err := card.park(test); err != nil {
					test.Errorf("the request in the gap did not park the issue: %v", err)
					return
				}
				parked = true
			}
			handler := *testHandler
			handler.TxStarter = barrier

			writer.move(test, &handler, card.issueID, "in_progress").Want(http.StatusOK)
			if !parked {
				test.Fatal("the park did not happen in the gap")
			}
			// The order the test built: the person's transaction began before the
			// park's, so the person's row sorts before the park's row.
			_, personAt, found := statusRowOf(test, card.issueID, "need_decision", "in_progress")
			if !found {
				test.Fatal("no person row from need_decision to in_progress")
			}
			_, parkAt, found := statusRowOf(test, card.issueID, "backlog", "need_decision")
			if !found {
				test.Fatal("no park row from backlog to need_decision")
			}
			if !personAt.Before(parkAt) {
				test.Fatalf("person row created_at %s is not before the park row %s", personAt, parkAt)
			}

			wantReleaseKey(test, card.issueID, "need_decision", "in_progress")
			if queued := queuedHandlerRuns(test, card.issueID, card.handler); queued != 1 {
				test.Errorf("queued handler runs = %d, want 1", queued)
			}
			if got := card.parkRows(test); got != 1 {
				test.Fatalf("park rows = %d, want the park row still stored", got)
			}
			wantHold(test, card.issueID, false, false, "left_backlog")
			card.wantStillReleasedAfterReturn(test)
		})
	}
}

// 8 (g): the rows that get no key.
func TestIntakeReleaseNoKey(test *testing.T) {
	seedTestCatalog(test)
	asCloudPAT := asSource("cloud_pat")

	test.Run("warn mode: a machine move passes and writes no key", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "warn" })
		fromBacklog := fixture.issue(test, "feat(x): warn backlog")
		putIssue(test, fromBacklog, map[string]any{"status": "todo"}, asCloudPAT).Want(http.StatusOK)
		parked := fixture.issue(test, "feat(x): warn parked")
		holdPark(test, parked)
		putIssue(test, parked, map[string]any{"status": "in_progress"}, asCloudPAT).Want(http.StatusOK)
		byAgent := fixture.issue(test, "feat(x): warn agent")
		holdAgentPut(test, byAgent, "todo").Want(http.StatusOK)
		for _, issueID := range []string{fromBacklog, parked, byAgent} {
			wantNoKey(test, issueID)
		}

		// With the entry in block, the issue that returns to backlog is held.
		newHoldFixtureWith(test, fixture, func(entry map[string]any) { entry["mode"] = "block" })
		holdAgentPut(test, fromBacklog, "backlog").Want(http.StatusOK)
		wantHold(test, fromBacklog, true, true, "held")
		wantIntakeRefusal(test, holdAgentPut(test, fromBacklog, "in_progress"))
	})

	person := func(test *testing.T, issueID, status string) {
		test.Helper()
		putIssue(test, issueID, map[string]any{"status": status}).Want(http.StatusOK)
	}
	test.Run("an issue with a parent", func(test *testing.T) {
		fixture := newHoldFixture(test, nil)
		parent := fixture.issue(test, "feat(x): parent", testutil.Cols{"status": "in_progress"})
		child := fixture.issue(test, "feat(x): child", testutil.Cols{"parent_issue_id": parent})
		person(test, child, "todo")
		wantNoKey(test, child)
	})
	test.Run("an issue created before starts_at", func(test *testing.T) {
		fixture := newHoldFixture(test, nil)
		issueID := fixture.issue(test, "feat(x): old", testutil.Cols{"created_at": time.Now().Add(-3 * time.Hour)})
		person(test, issueID, "todo")
		wantNoKey(test, issueID)
	})
	test.Run("a project with no entry", func(test *testing.T) {
		newHoldFixture(test, nil)
		issueID := dbfx.Issue(test, "feat(x): free", testutil.Cols{"status": "backlog", "project_id": dbfx.Project(test, "no entry")})
		person(test, issueID, "todo")
		wantNoKey(test, issueID)
	})
	test.Run("mode off", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "off" })
		issueID := fixture.issue(test, "feat(x): off")
		person(test, issueID, "todo")
		wantNoKey(test, issueID)
	})
	test.Run("at or after ends_at", func(test *testing.T) {
		fixture := newHoldFixture(test, func(entry map[string]any) {
			entry["ends_at"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		})
		issueID := fixture.issue(test, "feat(x): late")
		person(test, issueID, "todo")
		wantNoKey(test, issueID)
	})
	test.Run("a person's move from the parked state to backlog", func(test *testing.T) {
		fixture := newHoldFixture(test, nil)
		issueID := fixture.issue(test, "feat(x): back")
		holdPark(test, issueID)
		person(test, issueID, "backlog")
		wantNoKey(test, issueID)
		wantHold(test, issueID, true, true, "held")
	})
	test.Run("a person's move from in_progress to in_review", func(test *testing.T) {
		fixture := newHoldFixture(test, nil)
		issueID := fixture.issue(test, "feat(x): review", testutil.Cols{"status": "in_progress"})
		person(test, issueID, "in_review")
		wantNoKey(test, issueID)
	})
	test.Run("the park's row and the unpark's row", func(test *testing.T) {
		fixture := newHoldFixture(test, nil)
		issueID := fixture.issue(test, "feat(x): card")
		holdPark(test, issueID)
		issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
		if err != nil {
			test.Fatal(err)
		}
		if back, _, err := unparkIssueAfterDecision(context.Background(), testHandler.Queries, issue, nil); err != nil || back.Status != "backlog" {
			test.Fatalf("unpark: status %q, err %v", back.Status, err)
		}
		if rows := holdStatusRows(test, issueID); rows != 2 {
			test.Fatalf("status rows = %d, want the park row and the unpark row", rows)
		}
		wantNoKey(test, issueID)
	})
}

// 8 (h): a person's row from the parked state with no key, as a server before
// this change wrote it, does not release; one more move by the person does.
func TestIntakeReleaseHistoryRowWithoutKey(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	issueID := fixture.issue(test, "feat(x): history")
	statusRow(test, issueID, "member", "ui_or_api", "interactive_jwt", "need_decision", "in_progress", nil)
	wantHold(test, issueID, true, true, "held")
	wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))

	putIssue(test, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	holdAgentPut(test, issueID, "backlog").Want(http.StatusOK)
	wantHold(test, issueID, false, false, "released_by_member")
	holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
}
