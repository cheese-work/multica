package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5505 (S2b): a task token that widens an issue's run budget with
// `issue update --workflow` opens one pending issue.workflow.set@1 card instead
// of failing with budget_rebinding_requires_interactive.

type workflowSetOpened struct {
	RequestID  string `json:"request_id"`
	ActionID   string `json:"action_id"`
	ApproveURL string `json:"approve_url"`
}

func taskTokenSetWorkflow(t *testing.T, f rebindFixture, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+f.c.issueID+"/workflow", body), "task_token"), "id", f.c.issueID)
	return testutil.Call(t, testHandler.SetIssueWorkflow, req)
}

func pendingWorkflowSetActions(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id
		WHERE a.action_type = 'issue.workflow.set' AND a.state = 'pending' AND r.issue_id = $1`, issueID)
}

func TestTaskTokenWideningWorkflowSetOpensOnePendingCard(t *testing.T) {
	f := newRebindFixture(t, 5505100)
	var opened workflowSetOpened
	taskTokenSetWorkflow(t, f, map[string]any{"workflow_id": f.wide}).Want(http.StatusAccepted).JSON(&opened)
	if opened.ActionID == "" || opened.ApproveURL == "" {
		t.Fatalf("response %+v lacks the action id or the approve URL", opened)
	}
	if got := pendingWorkflowSetActions(t, f.c.issueID); got != 1 {
		t.Fatalf("pending issue.workflow.set actions = %d, want 1", got)
	}
	var override string
	dbfx.QueryRow(t, `SELECT COALESCE(workflow_id::text, '-') FROM issue WHERE id = $1`, f.c.issueID).Scan(&override)
	if override != "-" {
		t.Fatalf("opening the card wrote override %s", override)
	}

	// A repeat ask returns the open card.
	var again workflowSetOpened
	taskTokenSetWorkflow(t, f, map[string]any{"workflow_id": f.wide}).Want(http.StatusAccepted).JSON(&again)
	if again.ActionID != opened.ActionID {
		t.Fatalf("repeat ask opened action %s, want the open one %s", again.ActionID, opened.ActionID)
	}
	if got := pendingWorkflowSetActions(t, f.c.issueID); got != 1 {
		t.Fatalf("pending issue.workflow.set actions after a repeat ask = %d, want 1", got)
	}
}

// openedWorkflowSetCard is a pending card and what it takes to approve it.
type openedWorkflowSetCard struct {
	workflowSetOpened
	humanID string
	hash    string
}

func openWorkflowSetCard(t *testing.T, f rebindFixture) openedWorkflowSetCard {
	t.Helper()
	var opened workflowSetOpened
	taskTokenSetWorkflow(t, f, map[string]any{"workflow_id": f.wide}).Want(http.StatusAccepted).JSON(&opened)
	card := openedWorkflowSetCard{workflowSetOpened: opened}
	dbfx.QueryRow(t, `SELECT r.human_user_id::text, a.payload_sha256 FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id WHERE a.id = $1`, opened.ActionID).Scan(&card.humanID, &card.hash)
	dbfx.Cleanup(t, `DELETE FROM decision_action WHERE id = $1`, opened.ActionID)
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id = $1`, opened.RequestID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE id = $1`, opened.RequestID)
	return card
}

func (c openedWorkflowSetCard) approve(t *testing.T) decisionActionOutcome {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"payload_sha256": c.hash, "assertion": map[string]any{"id": "fake"}, "secrets": map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequest(http.MethodPost, "/api/decision-actions/"+c.ActionID+"/approve", nil), "id", c.ActionID)
	req.Header.Set("X-User-ID", c.humanID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Body = readerCloser{bytes.NewReader(raw)}
	var out decisionActionOutcome
	testutil.Call(t, withVerifier(acceptingVerifier).ApproveDecisionAction, req).Want(http.StatusOK).JSON(&out)
	return out
}

func TestApproveWorkflowSetExecutesWhenNothingMoved(t *testing.T) {
	f := newRebindFixture(t, 5505200)
	card := openWorkflowSetCard(t, f)
	out := card.approve(t)
	if out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	if got := issueOverride(t, f.c.issueID); got != f.wide {
		t.Fatalf("override = %s, want the wide workflow %s", got, f.wide)
	}
}

// Each change the card did not seal fails the approval with stale_base and
// writes nothing.
func TestApproveWorkflowSetFailsStaleBase(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f rebindFixture)
	}{
		{"target cap edited", func(t *testing.T, f rebindFixture) {
			dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 60 WHERE id = $1`, f.wide)
		}},
		{"target budget start changed", func(t *testing.T, f rebindFixture) {
			dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = now() WHERE id = $1`, f.wide)
		}},
		{"project workflow switched under an inheriting issue", func(t *testing.T, f rebindFixture) {
			setProjectWorkflow(t, f.narrowProj, map[string]any{"workflow_id": f.wide}).Want(http.StatusOK)
		}},
		{"issue moved to another project", func(t *testing.T, f rebindFixture) {
			dbfx.Exec(t, `UPDATE issue SET project_id = $2 WHERE id = $1`, f.c.issueID, f.wideProj)
		}},
		{"target deleted", func(t *testing.T, f rebindFixture) {
			dbfx.Exec(t, `DELETE FROM issue_workflow WHERE id = $1`, f.wide)
		}},
		{"target moved to another workspace", func(t *testing.T, f rebindFixture) {
			dbfx.Exec(t, `UPDATE issue_workflow SET workspace_id = gen_random_uuid() WHERE id = $1`, f.wide)
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebindFixture(t, int64(5505300+i))
			card := openWorkflowSetCard(t, f)
			c.mutate(t, f)
			out := card.approve(t)
			if out.State != "failed" || out.ErrorCode != staleBaseCode {
				t.Fatalf("outcome = %+v, want failed/%s", out, staleBaseCode)
			}
			if got := issueOverride(t, f.c.issueID); got != "" {
				t.Fatalf("a stale approval wrote override %s", got)
			}
		})
	}
}

// A workflow in another workspace is refused when the card is proposed.
func TestWorkflowSetRefusesCrossWorkspaceTargetAtPropose(t *testing.T) {
	f := newRebindFixture(t, 5505400)
	dbfx.Exec(t, `UPDATE issue_workflow SET workspace_id = gen_random_uuid() WHERE id = $1`, f.wide)
	taskTokenSetWorkflow(t, f, map[string]any{"workflow_id": f.wide}).Want(http.StatusBadRequest)
	if got := pendingWorkflowSetActions(t, f.c.issueID); got != 0 {
		t.Fatalf("a refused ask opened %d cards", got)
	}
}
