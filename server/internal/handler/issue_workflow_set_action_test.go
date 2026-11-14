package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// An older build has no issue.workflow.set@1 in its registry. A card opened by
// this build and approved there fails closed: nothing runs and nothing is
// written. The registry miss is the same for an unknown version, and approve
// answers it with action_not_executable (501).
func TestOrphanWorkflowSetCardFailsClosedOnAnUnknownType(t *testing.T) {
	f := newRebindFixture(t, 5505500)
	card := openWorkflowSetCard(t, f)
	dbfx.Exec(t, `UPDATE decision_action SET action_version = 99 WHERE id = $1`, card.ActionID)
	raw, err := json.Marshal(map[string]any{"payload_sha256": card.hash, "assertion": map[string]any{"id": "fake"}, "secrets": map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequest(http.MethodPost, "/api/decision-actions/"+card.ActionID+"/approve", nil), "id", card.ActionID)
	req.Header.Set("X-User-ID", card.humanID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Body = readerCloser{bytes.NewReader(raw)}
	res := testutil.Call(t, withVerifier(acceptingVerifier).ApproveDecisionAction, req)
	if res.Code != http.StatusNotImplemented || !strings.Contains(res.Text(), "action_not_executable") {
		t.Fatalf("approve = %d %s, want 501 action_not_executable", res.Code, res.Text())
	}
	if got := issueOverride(t, f.c.issueID); got != "" {
		t.Fatalf("an orphan card wrote override %s", got)
	}
	var state string
	dbfx.QueryRow(t, `SELECT state FROM decision_action WHERE id = $1`, card.ActionID).Scan(&state)
	if state != "pending" {
		t.Fatalf("orphan card state = %s, want pending", state)
	}
}

// approveRetrying approves the card, retrying while the approval queue is busy,
// and reports a failure with Errorf so it is safe inside a goroutine.
func (c openedWorkflowSetCard) approveRetrying(t *testing.T) decisionActionOutcome {
	raw, _ := json.Marshal(map[string]any{"payload_sha256": c.hash, "assertion": map[string]any{"id": "fake"}, "secrets": map[string]string{}})
	handler := withVerifier(acceptingVerifier)
	for range 50 {
		req := withURLParam(newRequest(http.MethodPost, "/api/decision-actions/"+c.ActionID+"/approve", nil), "id", c.ActionID)
		req.Header.Set("X-User-ID", c.humanID)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		req.Body = readerCloser{bytes.NewReader(raw)}
		rec := httptest.NewRecorder()
		handler.ApproveDecisionAction(rec, req)
		if rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "busy_retry") {
			continue
		}
		var out decisionActionOutcome
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Errorf("approve = %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	t.Error("approval stayed busy")
	return decisionActionOutcome{}
}

// The approval and a workflow switch on the issue's project run together, many
// times. They must not deadlock, and each pair ends in a valid execution or
// stale_base, never a partial write.
func TestApproveWorkflowSetRacesProjectWorkflowSwitch(t *testing.T) {
	for i := range 8 {
		f := newRebindFixture(t, int64(5505600+i))
		card := openWorkflowSetCard(t, f)
		var (
			wait    sync.WaitGroup
			out     decisionActionOutcome
			switchd int
			start   = make(chan struct{})
		)
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			out = card.approveRetrying(t)
		}()
		go func() {
			defer wait.Done()
			<-start
			req := withURLParam(workflowRequest("POST", "/api/projects/"+f.narrowProj+"/workflow", map[string]any{"workflow_id": f.wide}), "id", f.narrowProj)
			rec := httptest.NewRecorder()
			testHandler.SetProjectWorkflow(rec, req)
			switchd = rec.Code
			if rec.Code != http.StatusOK {
				t.Errorf("project switch = %d %s", rec.Code, rec.Body.String())
			}
		}()
		close(start)
		done := make(chan struct{})
		go func() { wait.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("round %d: approval and project switch did not finish (deadlock)", i)
		}
		override := issueOverride(t, f.c.issueID)
		switch {
		case out.State == "executed" && override == f.wide:
		case out.State == "failed" && out.ErrorCode == staleBaseCode && override == "":
		default:
			t.Fatalf("round %d: outcome %+v with override %q (project switch %d) is not an execution or stale_base", i, out, override, switchd)
		}
	}
}

// The card the owner sees names the issue, the workflow and cap before and
// after, and the delta, rendered by the server from the sealed bindings.
func TestWorkflowSetCardPreviewShowsBeforeAfterAndDelta(t *testing.T) {
	f := newRebindFixture(t, 5629100)
	card := openWorkflowSetCard(t, f)
	action := testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), parseUUID(card.RequestID))
	fields := previewFields(t, action)
	assertPreviewDigest(t, action.Preview)
	if got := fields["Run cap"]; got != [2]string{"2", "50"} {
		t.Fatalf("Run cap = %v; want [2 50]", got)
	}
	if got := fields["Workflow"]; got[0] == "" || got[1] == "" || got[0] == got[1] {
		t.Fatalf("Workflow = %v; want two different names", got)
	}
	if got := fields["Run budget"][1]; !strings.Contains(got, "widens by 48") {
		t.Fatalf("Run budget = %q; want it to say the move widens by 48 runs", got)
	}
	if _, found := fields["Issue"]; !found {
		t.Fatalf("preview lacks the issue: %v", fields)
	}
}
