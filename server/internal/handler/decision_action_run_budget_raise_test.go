package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const raiseAction = "issue.run_budget.raise"

// raiseFixture is a pending run-budget raise on an issue whose workflow caps
// it at baseCap runs.
type raiseFixture struct {
	pendingAction
	windowID string
	newCap   int
}

const baseCap = 10

// budgetedRequestFixture gives the fixture's issue a workflow cap of baseCap
// and returns the window id the snapshot exposes.
func budgetedRequestFixture(test *testing.T) (singleCallFixture, string) {
	test.Helper()
	fixture := newActionFixture(test)
	var workflowID string
	dbfx.QueryRow(test, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps, max_runs_per_issue, run_budget_since) VALUES ($1, 'raise wf', 'todo', '[]'::jsonb, $2, now() - interval '1 hour') RETURNING id`, testWorkspaceID, baseCap).Scan(&workflowID)
	dbfx.Exec(test, `UPDATE issue SET workflow_id = $1 WHERE id = $2`, workflowID, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	dbfx.Cleanup(test, `DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM activity_log WHERE issue_id = $1`, fixture.issueID)
	return fixture, snapshotWindowID(test, fixture.issueID)
}

func snapshotWindowID(test *testing.T, issueID string) string {
	test.Helper()
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/run-budget", nil), "id", issueID)
	var snapshot struct {
		WindowID string `json:"window_id"`
	}
	testutil.Call(test, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&snapshot)
	return snapshot.WindowID
}

func (fixture singleCallFixture) raiseParams(windowID string, newCap int) map[string]any {
	return map[string]any{"issue_id": fixture.issueID, "new_cap": newCap, "window_id": windowID}
}

func (fixture singleCallFixture) askRaise(test *testing.T, windowID string, newCap int) pendingAction {
	test.Helper()
	body := fixture.withAction(map[string]any{"type": raiseAction, "version": 1, "params": fixture.raiseParams(windowID, newCap)})
	// An ask with the same text is the same request, so each cap gets its own.
	ask := fixture.details
	ask.Decision = fmt.Sprintf("Raise the run cap to %d?", newCap)
	body["ask"] = ask
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return pendingAction{fixture, created.ID, created.Action.ID}
}

func newPendingRaise(test *testing.T, newCap int) raiseFixture {
	test.Helper()
	fixture, windowID := budgetedRequestFixture(test)
	return raiseFixture{fixture.askRaise(test, windowID, newCap), windowID, newCap}
}

func (action pendingAction) approveAccepted(test *testing.T) decisionActionOutcome {
	test.Helper()
	var out decisionActionOutcome
	decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusOK), &out)
	return out
}

func (action pendingAction) maxRunsOverride(test *testing.T) (cap int, set bool) {
	test.Helper()
	var value *int
	dbfx.QueryRow(test, `SELECT max_runs_override FROM issue WHERE id = $1`, action.issueID).Scan(&value)
	if value == nil {
		return 0, false
	}
	return *value, true
}

func (action pendingAction) raiseEntries(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id = $1 AND kind = 'raise'`, action.issueID)
}

func (action pendingAction) raisedActivities(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_raised'`, action.issueID)
}

func (action pendingAction) assertNothingRaised(test *testing.T) {
	test.Helper()
	if _, set := action.maxRunsOverride(test); set || action.raiseEntries(test) != 0 || action.raisedActivities(test) != 0 {
		test.Fatal("a refused raise left a cap, a ledger entry or an activity row")
	}
}

func TestRunBudgetRaiseActionRaisesCapActivityAndLedgerTogether(test *testing.T) {
	action := newPendingRaise(test, 30)
	out := action.approveAccepted(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v; want executed", out)
	}
	if cap, set := action.maxRunsOverride(test); !set || cap != 30 {
		test.Fatalf("cap = %d (set %v); want 30", cap, set)
	}
	var details string
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_raised' AND actor_type = 'member' AND actor_id = $2`, action.issueID, action.humanID).Scan(&details)
	var recorded struct {
		From     int    `json:"from"`
		To       int    `json:"to"`
		ActionID string `json:"action_id"`
	}
	if err := json.Unmarshal([]byte(details), &recorded); err != nil || recorded.From != baseCap || recorded.To != 30 || recorded.ActionID != action.actionID {
		test.Fatalf("activity details %s (%v); want from %d to 30 action %s", details, err, baseCap, action.actionID)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id = $1 AND kind = 'raise' AND event_key = $2 AND cap_delta = $3`, action.issueID, "raise:action:"+action.actionID, 30-baseCap); n != 1 {
		test.Fatalf("ledger entries named raise:action:<id> = %d; want 1", n)
	}
}

func TestRunBudgetRaiseActionDuplicateApprovalChangesNothingTwice(test *testing.T) {
	action := newPendingRaise(test, 30)
	action.approveAccepted(test)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
	if !strings.Contains(body, "not_pending") {
		test.Fatalf("body %q; want not_pending", body)
	}
	if action.raiseEntries(test) != 1 || action.raisedActivities(test) != 1 {
		test.Fatalf("entries %d, activities %d; want 1 and 1", action.raiseEntries(test), action.raisedActivities(test))
	}
}

func TestRunBudgetRaiseActionNeverLowersANewerCap(test *testing.T) {
	fixture, windowID := budgetedRequestFixture(test)
	high := fixture.askRaise(test, windowID, 40)
	low := fixture.askRaise(test, windowID, 30)
	if out := high.approveAccepted(test); out.State != "executed" {
		test.Fatalf("higher raise = %+v", out)
	}
	out := low.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "stale_cap" {
		test.Fatalf("lower raise = %+v; want failed/stale_cap", out)
	}
	if cap, _ := low.maxRunsOverride(test); cap != 40 {
		test.Fatalf("cap = %d; want 40", cap)
	}
	if low.raiseEntries(test) != 1 || low.raisedActivities(test) != 1 {
		test.Fatal("the stale raise left a ledger entry or an activity row")
	}
}

func TestRunBudgetRaiseActionRefusesACapRaisedAfterTheRequest(test *testing.T) {
	action := newPendingRaise(test, 30)
	dbfx.Exec(test, `UPDATE issue SET max_runs_override = 50 WHERE id = $1`, action.issueID)
	out := action.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "stale_cap" {
		test.Fatalf("outcome = %+v; want failed/stale_cap", out)
	}
	if cap, _ := action.maxRunsOverride(test); cap != 50 {
		test.Fatalf("cap = %d; want the newer 50", cap)
	}
	if action.raiseEntries(test) != 0 || action.raisedActivities(test) != 0 {
		test.Fatal("the stale raise left a ledger entry or an activity row")
	}
}

func TestRunBudgetRaiseActionRefusesAChangedWindow(test *testing.T) {
	action := newPendingRaise(test, 30)
	// A member's top-up after the request starts a new window.
	dbfx.Exec(test, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details) VALUES ($1, $2, 'member', $3, 'run_budget_top_up', '{}'::jsonb)`, testWorkspaceID, action.issueID, action.humanID)
	out := action.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "stale_window" {
		test.Fatalf("outcome = %+v; want failed/stale_window", out)
	}
	action.assertNothingRaised(test)
}

func TestRunBudgetRaiseActionRefusesAnIssueThatIsNoLongerBudgeted(test *testing.T) {
	action := newPendingRaise(test, 30)
	dbfx.Exec(test, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, action.issueID)
	out := action.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "not_budgeted" {
		test.Fatalf("outcome = %+v; want failed/not_budgeted", out)
	}
	action.assertNothingRaised(test)
}

func TestRunBudgetRaiseActionLedgerFailureUndoesTheWholeApproval(test *testing.T) {
	action := newPendingRaise(test, 30)
	dbfx.Exec(test, `CREATE OR REPLACE FUNCTION fail_raise_entry() RETURNS trigger AS $$ BEGIN IF NEW.kind = 'raise' AND NEW.issue_id = '`+action.issueID+`' THEN RAISE EXCEPTION 'ledger down'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
	dbfx.Exec(test, `CREATE TRIGGER fail_raise_entry BEFORE INSERT ON issue_run_budget_entry FOR EACH ROW EXECUTE FUNCTION fail_raise_entry()`)
	test.Cleanup(func() {
		dbfx.Exec(test, `DROP TRIGGER IF EXISTS fail_raise_entry ON issue_run_budget_entry`)
		dbfx.Exec(test, `DROP FUNCTION IF EXISTS fail_raise_entry()`)
	})
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusInternalServerError)
	action.assertUntouched(test)
	action.assertNothingRaised(test)
}

// The response is lost after the commit: the client reads the outcome and does
// not approve again.
func TestRunBudgetRaiseActionLostResponseReadsBackTheOutcome(test *testing.T) {
	action := newPendingRaise(test, 30)
	tracker := &lockTracker{commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("connection reset during commit")
	}}
	handler := handlerWith(tracker)
	handler.ApprovalVerifier = acceptingVerifier
	var approved decisionActionOutcome
	decodeJSON(test, action.approve(test, handler, action.claim(test, map[string]string{}), http.StatusOK), &approved)
	var read decisionActionOutcome
	decodeJSON(test, action.outcome(test, testHandler, http.StatusOK), &read)
	if approved.State != "executed" || read.State != "executed" || !strings.Contains(string(read.Result), `"to":30`) {
		test.Fatalf("approve %+v, readback %+v; want executed with the new cap", approved, read)
	}
	if action.raiseEntries(test) != 1 || action.raisedActivities(test) != 1 {
		test.Fatal("the raise was applied more than once")
	}
}

// Two owners approve different caps at once: whatever the order, the larger
// cap wins, the smaller is never applied over it, and the ledger agrees.
func TestRunBudgetRaiseActionTwoConcurrentCapsKeepTheLarger(test *testing.T) {
	fixture, windowID := budgetedRequestFixture(test)
	actions := []pendingAction{fixture.askRaise(test, windowID, 30), fixture.askRaise(test, windowID, 40)}
	handler := withVerifier(acceptingVerifier)
	outcomes := make([]decisionActionOutcome, len(actions))
	var wait sync.WaitGroup
	for index, action := range actions {
		wait.Add(1)
		go func() {
			defer wait.Done()
			outcomes[index] = approveRetryingWhileBusy(test, handler, action)
		}()
	}
	wait.Wait()
	if outcomes[1].State != "executed" {
		test.Fatalf("the larger cap = %+v; want executed", outcomes[1])
	}
	if outcomes[0].State != "executed" && outcomes[0].ErrorCode != "stale_cap" {
		test.Fatalf("the smaller cap = %+v; want executed first or stale_cap", outcomes[0])
	}
	if cap, _ := actions[0].maxRunsOverride(test); cap != 40 {
		test.Fatalf("cap = %d; want 40", cap)
	}
	var held int
	dbfx.QueryRow(test, `SELECT COALESCE(sum(cap_delta), 0) FROM issue_run_budget_entry WHERE issue_id = $1 AND window_id = $2`, fixture.issueID, windowID).Scan(&held)
	if held != 40 {
		test.Fatalf("ledger cap for the window = %d; want 40", held)
	}
}

func approveRetryingWhileBusy(test *testing.T, handler *Handler, action pendingAction) decisionActionOutcome {
	test.Helper()
	raw, _ := json.Marshal(action.claim(test, map[string]string{}))
	for range 50 {
		request := action.decide("approve", action.humanID, "interactive_jwt")
		request.Body = readerCloser{bytes.NewReader(raw)}
		recorder := httptest.NewRecorder()
		handler.ApproveDecisionAction(recorder, request)
		if recorder.Code == http.StatusConflict && strings.Contains(recorder.Body.String(), "busy_retry") {
			continue
		}
		if recorder.Code != http.StatusOK {
			test.Errorf("approve = %d %s", recorder.Code, recorder.Body.String())
			return decisionActionOutcome{}
		}
		var out decisionActionOutcome
		if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
			test.Error(err)
		}
		return out
	}
	test.Error("approval stayed busy")
	return decisionActionOutcome{}
}
