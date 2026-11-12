package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// These tests drive workflow.budget.set (CHE-5228) through request and
// approval. The shared core rules are proven in
// decision_action_config_change_test.go; here only what this action adds.

const budgetAction = decisionaction.WorkflowBudgetSet

// liveBudget reads a workflow's stored budget fields.
func liveBudget(test *testing.T, workflowID string) (maxRuns *int, includeChildPRs bool) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT max_runs_per_issue, include_child_prs FROM issue_workflow WHERE id = $1`, workflowID).Scan(&maxRuns, &includeChildPRs)
	return maxRuns, includeChildPRs
}

func liveBudgetDigest(test *testing.T, workflowID string) string {
	test.Helper()
	maxRuns, include := liveBudget(test, workflowID)
	return decisionaction.WorkflowBudgetDigest(workflowID, maxRuns, include)
}

func budgetParams(test *testing.T, workflowID string, maxRuns int, includeChildPRs bool) map[string]any {
	test.Helper()
	return map[string]any{"workflow_id": workflowID, "base_sha256": liveBudgetDigest(test, workflowID), "max_runs_per_issue": maxRuns, "include_child_prs": includeChildPRs}
}

// requestBudget asks for the action as an agent. No delegation setting is
// needed: the owner's approval is the gate.
func requestBudget(test *testing.T, status int, workflowID string, params map[string]any) (configRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": budgetAction, "version": 1, "params": params}), status, &created)
	base, _ := params["base_sha256"].(string)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return configRequest{workflowID: workflowID, base: base}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return configRequest{action: pendingAction{fixture, created.ID, created.Action.ID}, workflowID: workflowID, base: base}, body
}

func newBudgetRequest(test *testing.T) configRequest {
	test.Helper()
	workflowID, _ := stepsWorkflow(test)
	request, _ := requestBudget(test, http.StatusCreated, workflowID, budgetParams(test, workflowID, 40, false))
	return request
}

func TestWorkflowBudgetSetIsRegisteredAsAPreviewedWorkflowChange(test *testing.T) {
	definition, found := decisionaction.Lookup(budgetAction, 1)
	if !found || definition.Target != decisionaction.TargetWorkflow || !definition.RequiresPreview || definition.MinClass != "spend" {
		test.Fatalf("definition %+v (found %v); want a spend-class workflow action that requires a preview", definition, found)
	}
	if _, ok := configChanges[budgetAction]; !ok {
		test.Fatal("workflow.budget.set is not registered with the config-change core")
	}
}

func TestWorkflowBudgetSetDigestCoversBothFieldsAndTheWorkflow(test *testing.T) {
	five, six := 5, 6
	const id = "11111111-1111-1111-1111-111111111111"
	base := decisionaction.WorkflowBudgetDigest(id, &five, false)
	for name, other := range map[string]string{
		"cap":      decisionaction.WorkflowBudgetDigest(id, &six, false),
		"unset":    decisionaction.WorkflowBudgetDigest(id, nil, false),
		"children": decisionaction.WorkflowBudgetDigest(id, &five, true),
		"workflow": decisionaction.WorkflowBudgetDigest("22222222-2222-2222-2222-222222222222", &five, false),
	} {
		if other == base || len(other) != 64 {
			test.Fatalf("digest ignores %s: %q vs %q", name, other, base)
		}
	}
}

func TestWorkflowBudgetSetRequestStoresTheBudgetPreview(test *testing.T) {
	request := newBudgetRequest(test)
	stored := request.preconditions(test)
	shown, _ := stored["preview"].(map[string]any)
	if shown["field"] != "workflow.budget" || shown["target_id"] != request.workflowID {
		test.Fatalf("preview %v lacks the field and target", shown)
	}
	before, _ := shown["before"].(map[string]any)
	after, _ := shown["after"].(map[string]any)
	if before["include_child_prs"] != true || after["include_child_prs"] != false || after["max_runs_per_issue"] != float64(40) {
		test.Fatalf("preview before %v after %v; want the cap and child-PR switch, before and after", before, after)
	}
	if stored["before_sha256"] != request.base || stored["preview_sha256"] == nil {
		test.Fatalf("preconditions %v lack the base digest or the preview digest", stored)
	}
}

func TestWorkflowBudgetSetRequestRefusals(test *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
		params func(workflowID string) map[string]any
	}{
		{"stale base digest", http.StatusConflict, "precondition_drift", func(id string) map[string]any {
			params := budgetParams(test, id, 40, true)
			params["base_sha256"] = strings.Repeat("0", 64)
			return params
		}},
		{"cap above the limit", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			return budgetParams(test, id, 1001, true)
		}},
		{"negative cap", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			return budgetParams(test, id, -1, true)
		}},
		{"neither field named", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			params := budgetParams(test, id, 40, true)
			delete(params, "max_runs_per_issue")
			delete(params, "include_child_prs")
			return params
		}},
		{"workflow of another workspace", http.StatusBadRequest, "target_workflow_mismatch", func(id string) map[string]any {
			params := budgetParams(test, id, 40, true)
			params["workflow_id"] = foreignWorkspaceWorkflow(test)
			return params
		}},
		{"unknown param field", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			params := budgetParams(test, id, 40, true)
			params["name"] = "renamed"
			return params
		}},
		{"base digest is not hex", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			params := budgetParams(test, id, 40, true)
			params["base_sha256"] = "nope"
			return params
		}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			workflowID, _ := stepsWorkflow(test)
			_, body := requestBudget(test, tc.status, workflowID, tc.params(workflowID))
			mustContain(test, body, tc.want)
		})
	}
}

func TestWorkflowBudgetSetApprovalWritesOnlyTheBudget(test *testing.T) {
	request := newBudgetRequest(test)
	stepsBefore := liveStepsDigest(test, request.workflowID)
	var name, description string
	dbfx.QueryRow(test, `SELECT name, description FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&name, &description)

	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)

	maxRuns, include := liveBudget(test, request.workflowID)
	if maxRuns == nil || *maxRuns != 40 || include {
		test.Fatalf("stored budget %v/%v, want 40/false", maxRuns, include)
	}
	if got := liveStepsDigest(test, request.workflowID); got != stepsBefore {
		test.Fatalf("steps changed: %s, want %s", got, stepsBefore)
	}
	var nameAfter, descriptionAfter string
	dbfx.QueryRow(test, `SELECT name, description FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&nameAfter, &descriptionAfter)
	if nameAfter != name || descriptionAfter != description {
		test.Fatalf("workflow header changed: %q %q, want %q %q", nameAfter, descriptionAfter, name, description)
	}
	if state := request.action.state(test); state != "executed" {
		test.Fatalf("action state %s, want executed", state)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d, want 1", n)
	}
}

// A cap of 0 clears the stored cap, as it does on a workflow save; leaving a
// field out keeps its stored value.
func TestWorkflowBudgetSetKeepsAFieldItDoesNotName(test *testing.T) {
	workflowID, _ := stepsWorkflow(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET max_runs_per_issue = 12 WHERE id = $1`, workflowID)
	params := budgetParams(test, workflowID, 0, false)
	delete(params, "max_runs_per_issue")
	request, _ := requestBudget(test, http.StatusCreated, workflowID, params)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	maxRuns, include := liveBudget(test, workflowID)
	if maxRuns == nil || *maxRuns != 12 || include {
		test.Fatalf("stored budget %v/%v, want 12/false", maxRuns, include)
	}
}

func TestWorkflowBudgetSetEditBetweenProposeAndApproveFailsStaleBase(test *testing.T) {
	request := newBudgetRequest(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET max_runs_per_issue = 77, include_child_prs = false WHERE id = $1`, request.workflowID)
	body := request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	mustContain(test, body, "stale_base")
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'failed' AND error_code = 'stale_base'`, request.action.actionID); n != 1 {
		test.Fatalf("action is not failed with stale_base: %s", body)
	}
	maxRuns, include := liveBudget(test, request.workflowID)
	if maxRuns == nil || *maxRuns != 77 || include {
		test.Fatalf("stored budget %v/%v, want the concurrent 77/false", maxRuns, include)
	}
}
