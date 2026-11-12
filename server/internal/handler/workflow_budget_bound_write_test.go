package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5228: an agent changes a workflow's run budget with a decision-bound
// PATCH, like a step edit (CHE-2456). The owner approves one card for the
// workflow.budget field; the server applies it with the card's digests.

type boundBudgetWrite struct {
	askedRecordFixture
	workflowID string
	before     string
	after      string
}

func budgetCap(n int) *int { return &n }

// newBoundBudgetWrite creates a workflow with no cap and answers an approval
// card for the cap becoming 12 (child PRs unchanged), delegating the field to
// the card's agent unless delegate is false.
func newBoundBudgetWrite(t *testing.T, delegate bool) boundBudgetWrite {
	t.Helper()
	seedTestCatalog(t)
	resetDecisionLease(t)
	wf := createTestWorkflowSteps(t, "Bound budget "+workflowTestSuffix(), statusSteps("backlog", "in_progress", "blocked"))
	before := decisionaction.WorkflowBudgetDigest(wf.ID, nil, true)
	after := decisionaction.WorkflowBudgetDigest(wf.ID, budgetCap(12), true)
	fixture := approvedBoundWriteCard(t, "workflow.budget", before, after)

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	if delegate {
		setting, _ := json.Marshal(map[string]any{"allowed_agents": []string{fixture.agentID}, "allowed_fields": []string{"workflow.budget"}, "requires_human_decision": true})
		dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('delegated_governed_writes', $2::jsonb) WHERE id = $1`, testWorkspaceID, string(setting))
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
		dbfx.Exec(t, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
	})
	return boundBudgetWrite{askedRecordFixture: fixture, workflowID: wf.ID, before: before, after: after}
}

func (write boundBudgetWrite) body(extra map[string]any) map[string]any {
	body := map[string]any{
		"max_runs_per_issue": 12,
		"decision_id":        uuidToString(write.decision.ID),
		"before_sha256":      write.before,
		"after_sha256":       write.after,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (write boundBudgetWrite) patchResponse(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+write.workflowID, body), "id", write.workflowID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", write.agentID)
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, req)
}

func (write boundBudgetWrite) patch(t *testing.T, body map[string]any) int {
	t.Helper()
	return write.patchResponse(t, body).Code
}

func (write boundBudgetWrite) storedCap(t *testing.T) *int {
	t.Helper()
	var cap *int
	dbfx.QueryRow(t, `SELECT max_runs_per_issue FROM issue_workflow WHERE id = $1`, write.workflowID).Scan(&cap)
	return cap
}

func TestBoundBudgetPatchAcceptsApprovedWrite(t *testing.T) {
	write := newBoundBudgetWrite(t, true)
	if got := write.patch(t, write.body(nil)); got != http.StatusOK {
		t.Fatalf("approved bound budget write = %d, want 200", got)
	}
	if cap := write.storedCap(t); cap == nil || *cap != 12 {
		t.Fatalf("stored cap = %v, want 12", cap)
	}
	var field string
	dbfx.QueryRow(t, `SELECT field FROM decision_bound_write WHERE decision_id = $1`, write.decision.ID).Scan(&field)
	if field != "workflow.budget" {
		t.Fatalf("audit field = %q", field)
	}
	// The decision is single-use.
	if got := write.patch(t, write.body(nil)); got != http.StatusConflict {
		t.Fatalf("replay = %d, want 409", got)
	}
}

func TestBoundBudgetPatchRefusals(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		delegate bool
		edit     func(w boundBudgetWrite) map[string]any
		want     int
	}{
		{name: "no decision", delegate: true, want: http.StatusForbidden,
			edit: func(w boundBudgetWrite) map[string]any { return map[string]any{"max_runs_per_issue": 12} }},
		{name: "agent not delegated", delegate: false, want: http.StatusForbidden,
			edit: func(w boundBudgetWrite) map[string]any { return w.body(nil) }},
		{name: "stale before digest", delegate: true, want: http.StatusConflict,
			edit: func(w boundBudgetWrite) map[string]any {
				return w.body(map[string]any{"before_sha256": decisionaction.WorkflowBudgetDigest(w.workflowID, budgetCap(3), true)})
			}},
		{name: "after digest is another budget", delegate: true, want: http.StatusForbidden,
			edit: func(w boundBudgetWrite) map[string]any { return w.body(map[string]any{"max_runs_per_issue": 99}) }},
		{name: "mixed with steps", delegate: true, want: http.StatusBadRequest,
			edit: func(w boundBudgetWrite) map[string]any {
				return w.body(map[string]any{"steps": liveSteps(t, w.workflowID)})
			}},
		{name: "mixed with a name", delegate: true, want: http.StatusBadRequest,
			edit: func(w boundBudgetWrite) map[string]any { return w.body(map[string]any{"name": "renamed"}) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			write := newBoundBudgetWrite(t, scenario.delegate)
			if got := write.patch(t, scenario.edit(write)); got != scenario.want {
				t.Fatalf("status = %d, want %d", got, scenario.want)
			}
			if cap := write.storedCap(t); cap != nil {
				t.Fatalf("refused write stored cap %d", *cap)
			}
			if rows := dbfx.Count(t, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, write.decision.ID); rows != 0 {
				t.Fatalf("refused write recorded %d audit rows", rows)
			}
		})
	}
}

// A decision for the cap is not a decision for the child-PR switch.
func TestBoundBudgetPatchCannotWidenBeyondTheCard(t *testing.T) {
	write := newBoundBudgetWrite(t, true)
	body := write.body(map[string]any{"include_child_prs": false})
	if got := write.patch(t, body); got != http.StatusForbidden {
		t.Fatalf("extra include_child_prs = %d, want 403", got)
	}
	if cap := write.storedCap(t); cap != nil {
		t.Fatalf("stored cap %d after refusal", *cap)
	}
}

// The unbound refusal keeps pointing at the decision path.
func TestBoundBudgetPatchWithoutDecisionNamesThePath(t *testing.T) {
	write := newBoundBudgetWrite(t, true)
	res := write.patchResponse(t, map[string]any{"max_runs_per_issue": 12})
	if res.Code != http.StatusForbidden || !strings.Contains(res.Text(), "workflow.budget") {
		t.Fatalf("unbound = %d %s", res.Code, res.Text())
	}
}
