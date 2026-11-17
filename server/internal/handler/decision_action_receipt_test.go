package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// reloadAction is the action row as the card reads it.
func (action pendingAction) reloadAction(test *testing.T) db.DecisionAction {
	test.Helper()
	row, err := testHandler.Queries.GetDecisionAction(context.Background(), db.GetDecisionActionParams{ID: parseUUID(action.actionID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	return row
}

// refuseAs adds a refusal of the action's issue in its own window, linked to the action.
func (action pendingAction) refuseAs(test *testing.T, window, state, reason string, linked bool) string {
	test.Helper()
	id := insertTestRefusal(test, action.issueID, window)
	dbfx.Exec(test, `UPDATE run_budget_refusal SET state = $2, state_reason = $3 WHERE id = $1`, id, state, reason)
	if linked {
		dbfx.Exec(test, `UPDATE run_budget_refusal SET action_id = $2 WHERE id = $1`, id, action.actionID)
	}
	return id
}

// A queued task of the issue; started says whether it has begun.
func (action pendingAction) queuedTask(test *testing.T, refusalID string, started bool) {
	test.Helper()
	task := dbfx.Task(test, action.agentID, testutil.Cols{"issue_id": action.issueID, "runtime_id": handlerTestRuntimeID(test)})
	if started {
		dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = now() WHERE id = $1`, task)
	}
	dbfx.Exec(test, `UPDATE run_budget_refusal SET task_id = $2 WHERE id = $1`, refusalID, task)
}

func int64p(v int64) *int64 { return &v }

// Every derivation row of the receipt (CHE-6947, plan CHE-6943 r3 D2).
func TestRunBudgetReceiptDerivesTheOutcomeFromTheLinkedRefusal(test *testing.T) {
	cases := []struct {
		name  string
		setup func(test *testing.T, action pendingAction)
		want  string
	}{
		{"no refusal and no resume", func(*testing.T, pendingAction) {}, "cap_applied"},
		{"unknown_origin recorded at approval", func(test *testing.T, action pendingAction) {
			dbfx.Exec(test, `UPDATE decision_action SET result = result || '{"resume":"superseded:unknown_origin"}'::jsonb WHERE id = $1`, action.actionID)
		}, "superseded:unknown_origin"},
		{"open", func(test *testing.T, a pendingAction) { a.refuseAs(test, "w-open", "open", "", true) }, "cap_applied"},
		{"resume_pending", func(test *testing.T, a pendingAction) { a.refuseAs(test, "w-pending", "resume_pending", "", true) }, "cap_applied"},
		{"run_queued", func(test *testing.T, a pendingAction) {
			a.queuedTask(test, a.refuseAs(test, "w-queued", "run_queued", "", true), false)
		}, "run_queued"},
		{"run_queued and the task started", func(test *testing.T, a pendingAction) {
			a.queuedTask(test, a.refuseAs(test, "w-started", "run_queued", "", true), true)
		}, "run_started"},
		{"status_restored", func(test *testing.T, a pendingAction) { a.refuseAs(test, "w-restored", "status_restored", "", true) }, "status_restored"},
		{"still_blocked", func(test *testing.T, a pendingAction) { a.refuseAs(test, "w-blocked", "still_blocked", "runs", true) }, "still_blocked:runs"},
		{"superseded", func(test *testing.T, a pendingAction) {
			a.refuseAs(test, "w-superseded", "superseded", "later_window", true)
		}, "superseded:later_window"},
		{"the last of several refusals", func(test *testing.T, a pendingAction) {
			a.refuseAs(test, "w-first", "still_blocked", "runs", true)
			a.refuseAs(test, "w-second", "status_restored", "", true)
		}, "status_restored"},
		{"a refusal that is not linked", func(test *testing.T, a pendingAction) {
			a.refuseAs(test, "w-other", "superseded", "later_window", false)
		}, "cap_applied"},
	}
	for _, c := range cases {
		test.Run(c.name, func(test *testing.T) {
			action := newPendingRaise(test, 30).pendingAction
			action.approveAccepted(test)
			c.setup(test, action)
			receipt := runBudgetReceipt(context.Background(), testHandler.Queries, action.reloadAction(test))
			if receipt == nil || receipt.Outcome != c.want {
				test.Fatalf("receipt = %+v; want outcome %s", receipt, c.want)
			}
			if receipt.AppliedAt == "" {
				test.Fatalf("receipt has no applied_at: %+v", receipt)
			}
		})
	}
}

func TestRunBudgetReceiptCarriesTheSnapshotNumbers(test *testing.T) {
	test.Run("raise", func(test *testing.T) {
		raise := newPendingRaise(test, 30).pendingAction
		spendRuns(test, raise, 2)
		used := int64(runsUsedNow(test, raise.issueID))
		raise.approveAccepted(test)
		got := runBudgetReceipt(context.Background(), testHandler.Queries, raise.reloadAction(test))
		if got == nil || got.RunsUsed == nil || *got.RunsUsed != used || got.RunsLimit == nil || *got.RunsLimit != baseCap || got.RunsLimitAfter == nil || *got.RunsLimitAfter != 30 || got.NewWindow != nil {
			test.Fatalf("raise receipt = %+v; want used %d, limit %d -> 30, no new_window", got, used, baseCap)
		}
	})
	test.Run("top-up", func(test *testing.T) {
		topUp, _ := newBudgetedTopUp(test)
		topUp.approveAccepted(test)
		got := runBudgetReceipt(context.Background(), testHandler.Queries, topUp.reloadAction(test))
		if got == nil || got.RunsUsed == nil || got.NewWindow == nil || !*got.NewWindow || got.RunsLimit == nil || *got.RunsLimit != baseCap {
			test.Fatalf("top-up receipt = %+v; want the snapshot and new_window", got)
		}
	})
}

// An action executed before the snapshot existed has no numbers of its own: a
// raise maps from/to, a top-up has none, and runs_used is never invented.
func TestRunBudgetReceiptOfAnActionExecutedBeforeTheSnapshot(test *testing.T) {
	test.Run("raise", func(test *testing.T) {
		raise := newPendingRaise(test, 30).pendingAction
		raise.approveAccepted(test)
		dbfx.Exec(test, `UPDATE decision_action SET result = '{"from":10,"to":30,"outcome":"cap_applied"}'::jsonb WHERE id = $1`, raise.actionID)
		got := runBudgetReceipt(context.Background(), testHandler.Queries, raise.reloadAction(test))
		if got == nil {
			test.Fatal("pre-snapshot raise has no receipt")
		}
		want := &RunBudgetReceipt{Outcome: "cap_applied", AppliedAt: got.AppliedAt, RunsLimit: int64p(10), RunsLimitAfter: int64p(30)}
		if !reflect.DeepEqual(got, want) {
			test.Fatalf("pre-snapshot raise receipt = %+v; want %+v", got, want)
		}
	})
	test.Run("top-up", func(test *testing.T) {
		topUp, _ := newBudgetedTopUp(test)
		topUp.approveAccepted(test)
		dbfx.Exec(test, `UPDATE decision_action SET result = jsonb_build_object('issue_id', $2::text, 'window_id', 'w') WHERE id = $1`, topUp.actionID, topUp.issueID)
		got := runBudgetReceipt(context.Background(), testHandler.Queries, topUp.reloadAction(test))
		if got == nil {
			test.Fatal("pre-snapshot top-up has no receipt")
		}
		if want := (&RunBudgetReceipt{Outcome: "cap_applied", AppliedAt: got.AppliedAt}); !reflect.DeepEqual(got, want) {
			test.Fatalf("pre-snapshot top-up receipt = %+v; want %+v", got, want)
		}
	})
}

func TestRunBudgetReceiptIsOnlyForAnExecutedBudgetAction(test *testing.T) {
	cases := []struct {
		name   string
		change string
	}{
		{"pending", ""},
		{"another action type", `UPDATE decision_action SET action_type = 'issue.noop' WHERE id = $1`},
		{"failed", `UPDATE decision_action SET state = 'failed' WHERE id = $1`},
	}
	for _, c := range cases {
		test.Run(c.name, func(test *testing.T) {
			action := newPendingRaise(test, 30).pendingAction
			if c.name != "pending" {
				action.approveAccepted(test)
				dbfx.Exec(test, c.change, action.actionID)
			}
			if got := runBudgetReceipt(context.Background(), testHandler.Queries, action.reloadAction(test)); got != nil {
				test.Fatalf("receipt = %+v; want none", got)
			}
		})
	}
}

func decodeReceipt(test *testing.T, body string) *RunBudgetReceipt {
	test.Helper()
	var out decisionActionOutcome
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		test.Fatal(err)
	}
	return out.Receipt
}

// One receipt on every read of a decided action: the approve response (the
// resume worker has already run), the outcome endpoint and the card.
func TestRunBudgetReceiptIsServedOnEveryReadOfTheAction(test *testing.T) {
	action := pendingBudgetAction(test, raiseAction, func(issueID, windowID string) string {
		id := insertTestRefusal(test, issueID, windowID)
		// The hook records the status the issue was in when it refused the run.
		dbfx.Exec(test, `UPDATE run_budget_refusal SET status = (SELECT status FROM issue WHERE id = $2) WHERE id = $1`, id, issueID)
		return id
	})
	handler := withVerifier(acceptingVerifier)
	approved := decodeReceipt(test, action.approve(test, handler, action.claim(test, map[string]string{}), http.StatusOK))
	if approved == nil || approved.Outcome != "run_queued" {
		test.Fatalf("approve receipt = %+v; want the worker's outcome run_queued", approved)
	}
	if read := decodeReceipt(test, action.outcome(test, handler, http.StatusOK)); !reflect.DeepEqual(read, approved) {
		test.Fatalf("outcome receipt = %+v; approve receipt = %+v", read, approved)
	}
	if card := action.cardAction(test); card == nil || !reflect.DeepEqual(card.Receipt, approved) {
		test.Fatalf("card receipt = %+v; approve receipt = %+v", card, approved)
	}
	if listed := action.issueCardReceipt(test); !reflect.DeepEqual(listed, approved) {
		test.Fatalf("issue-cards receipt = %+v; approve receipt = %+v", listed, approved)
	}
}

// issueCardReceipt is the receipt the issue page reads for the action's card.
func (action pendingAction) issueCardReceipt(test *testing.T) *RunBudgetReceipt {
	test.Helper()
	request := newRequest(http.MethodGet, "/api/decisions/issue-cards?issue="+action.issueID, nil)
	request.Header.Set("X-User-ID", action.humanID)
	var cards []DecisionRequestCard
	testutil.Call(test, testHandler.ListIssueDecisionCards, request).Want(http.StatusOK).JSON(&cards)
	for _, card := range cards {
		if card.Action != nil && card.Action.ID == action.actionID {
			return card.Action.Receipt
		}
	}
	test.Fatalf("issue-cards has no card for action %s: %+v", action.actionID, cards)
	return nil
}

func TestPendingActionCardHasNoReceipt(test *testing.T) {
	action := newPendingRaise(test, 30).pendingAction
	if card := action.cardAction(test); card == nil || card.Receipt != nil {
		test.Fatalf("pending card = %+v; want no receipt", card)
	}
}

func TestBatchApproveItemsCarryTheReceipt(test *testing.T) {
	setWorkspacePRMergeStatusForTest(test, "none") // the batch fixture's workflow has a pr.merged edge
	run := newTopUpBatchRun(test, 2)
	var out struct {
		Items []decisionActionOutcome `json:"items"`
	}
	decodeJSON(test, run.approve(test, withVerifier(acceptingVerifier), run.claim(test), http.StatusOK), &out)
	if len(out.Items) != 2 {
		test.Fatalf("items = %+v; want 2", out.Items)
	}
	for index, item := range out.Items {
		if item.Receipt == nil || item.Receipt.Outcome != "cap_applied" || item.Receipt.NewWindow == nil || !*item.Receipt.NewWindow {
			test.Fatalf("item %d receipt = %+v; want a top-up receipt", index, item.Receipt)
		}
	}
}
