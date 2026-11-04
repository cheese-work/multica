package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// These tests drive the real workflow.steps.set action through request and
// approval; the shared core rules are proven with a stand-in action in
// decision_action_config_change_test.go.

const stepsAction = "workflow.steps.set"

// stepEdit changes the steps an agent read before it sends them back.
type stepEdit func(steps []IssueWorkflowStepPayload)

func editInstructions(statusKey, text string) stepEdit {
	return func(steps []IssueWorkflowStepPayload) {
		for i := range steps {
			if steps[i].StatusKey == statusKey {
				steps[i].Instructions = text
			}
		}
	}
}

// stepsParams reads the workflow as GET returns it, applies edit and returns
// the params a correct agent would send, with both digests.
func stepsParams(test *testing.T, workflowID string, edit stepEdit) map[string]any {
	test.Helper()
	var raw []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, workflowID).Scan(&raw)
	live, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		test.Fatal(err)
	}
	payload := make([]IssueWorkflowStepPayload, len(live))
	for i, step := range live {
		payload[i] = stepToPayload(step)
	}
	edit(payload)
	sent, err := json.Marshal(payload)
	if err != nil {
		test.Fatal(err)
	}
	workflow, err := testHandler.Queries.GetIssueWorkflow(context.Background(), db.GetIssueWorkflowParams{ID: parseUUID(workflowID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	after, err := workflowStepsAfter(context.Background(), testHandler.Queries, workflow, sent)
	if err != nil {
		test.Fatalf("steps the test sends are not valid: %v", err)
	}
	return map[string]any{
		"workflow_id": workflowID, "steps": json.RawMessage(sent),
		"base_sha256": liveStepsDigest(test, workflowID), "after_sha256": workflowStepsHash(workflow.ID, after),
	}
}

// stepsParamsRaw is params whose steps are sent as written, for edits the
// server must refuse: no after digest can be computed for them.
func stepsParamsRaw(test *testing.T, workflowID, steps string) map[string]any {
	test.Helper()
	return map[string]any{
		"workflow_id": workflowID, "steps": json.RawMessage(steps),
		"base_sha256": liveStepsDigest(test, workflowID), "after_sha256": strings.Repeat("0", 64),
	}
}

// stepsWorkflow is configWorkflow with a unique name and the steps a workflow
// save would store, so an edit to one instruction is a workflow.instructions
// change and two workflows can live in one workspace.
func stepsWorkflow(test *testing.T) (id, digest string) {
	test.Helper()
	id, _ = configWorkflow(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET name = 'steps wf ' || id::text WHERE id = $1`, id)
	var raw []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, id).Scan(&raw)
	live, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		test.Fatal(err)
	}
	payload := make([]IssueWorkflowStepPayload, len(live))
	for i, step := range live {
		payload[i] = stepToPayload(step)
	}
	sent, err := json.Marshal(payload)
	if err != nil {
		test.Fatal(err)
	}
	workflow, err := testHandler.Queries.GetIssueWorkflow(context.Background(), db.GetIssueWorkflowParams{ID: parseUUID(id), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	normalized, err := workflowStepsAfter(context.Background(), testHandler.Queries, workflow, sent)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := issueworkflow.EncodeSteps(normalized)
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE issue_workflow SET steps = $2::jsonb WHERE id = $1`, id, string(encoded))
	return id, liveStepsDigest(test, id)
}

// requestSteps asks for workflow.steps.set with params and expects status.
func requestSteps(test *testing.T, status int, workflowID string, params map[string]any) (configRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": stepsAction, "version": 1, "params": params}), status, &created)
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

func newStepsRequest(test *testing.T) configRequest {
	test.Helper()
	workflowID, _ := stepsWorkflow(test)
	request, _ := requestSteps(test, http.StatusCreated, workflowID, stepsParams(test, workflowID, editInstructions("todo", "do A better")))
	return request
}

func TestWorkflowStepsSetIsRegisteredAsAPreviewedWorkflowChange(test *testing.T) {
	definition, found := decisionaction.Lookup(stepsAction, 1)
	if !found || definition.Target != decisionaction.TargetWorkflow || !definition.RequiresPreview {
		test.Fatalf("definition %+v (found %v); want a workflow-target action that requires a preview", definition, found)
	}
	if _, ok := configChanges[stepsAction]; !ok {
		test.Fatal("workflow.steps.set is not registered with the config-change core")
	}
}

func TestWorkflowStepsSetRequestStoresThePreview(test *testing.T) {
	request := newStepsRequest(test)
	stored := request.preconditions(test)
	shown, _ := stored["preview"].(map[string]any)
	if shown["field"] != "workflow.instructions" || shown["target_id"] != request.workflowID {
		test.Fatalf("preview %v lacks the field and target", shown)
	}
	if stored["before_sha256"] != request.base || stored["preview_sha256"] == nil {
		test.Fatalf("preconditions %v lack the base digest or the preview digest", stored)
	}
	if diff, _ := shown["diff"].([]any); len(diff) == 0 {
		test.Fatalf("preview %v has no diff", shown)
	}
}

func TestWorkflowStepsSetNamesTheFieldItWrites(test *testing.T) {
	workflowID, _ := stepsWorkflow(test)
	request, _ := requestSteps(test, http.StatusCreated, workflowID, stepsParams(test, workflowID, func(steps []IssueWorkflowStepPayload) { steps[0].NextStatusKey = "done" }))
	shown, _ := request.preconditions(test)["preview"].(map[string]any)
	if shown["field"] != "workflow.steps" {
		test.Fatalf("field %v; a change beyond instructions must be workflow.steps", shown["field"])
	}
}

func TestWorkflowStepsSetOwnerApprovalWritesAuditsAndReadsBack(test *testing.T) {
	request := newStepsRequest(test)
	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	var want map[string]any
	dbfx.QueryRow(test, `SELECT preconditions FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&want)
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	if got := liveStepsDigest(test, request.workflowID); got != want["after_sha256"] {
		test.Fatalf("live digest %s, want the approved %v", got, want["after_sha256"])
	}
	var stored []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&stored)
	if !strings.Contains(string(stored), "do A better") {
		test.Fatalf("stored steps %s lack the approved instructions", stored)
	}
	var raw string
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
	var details map[string]any
	decodeJSON(test, raw, &details)
	for key, want := range map[string]string{"field": "workflow.instructions", "target_id": request.workflowID, "decision_action_id": request.action.actionID, "credential_id": keyID} {
		if details[key] != want {
			test.Fatalf("audit %s = %v, want %s", key, details[key], want)
		}
	}
	if state := request.action.state(test); state != "executed" {
		test.Fatalf("action state %s, want executed", state)
	}
}

func TestWorkflowStepsSetRequestRefusals(test *testing.T) {
	const governedWeakened = "keep <!-- governed:human -->weakened<!-- /governed:human --> tail"
	cases := []struct {
		name   string
		status int
		want   string
		params func(workflowID string) map[string]any
	}{
		{"agent after digest is wrong", http.StatusBadRequest, "after_digest_mismatch", func(id string) map[string]any {
			params := stepsParams(test, id, editInstructions("todo", "x"))
			params["after_sha256"] = strings.Repeat("0", 64)
			return params
		}},
		{"agent base digest is stale", http.StatusConflict, "precondition_drift", func(id string) map[string]any {
			params := stepsParams(test, id, editInstructions("todo", "x"))
			params["base_sha256"] = strings.Repeat("0", 64)
			return params
		}},
		{"workflow of another workspace", http.StatusBadRequest, "target_workflow_mismatch", func(id string) map[string]any {
			params := stepsParams(test, id, editInstructions("todo", "x"))
			params["workflow_id"] = foreignWorkspaceWorkflow(test)
			return params
		}},
		{"governed human block edited", http.StatusBadRequest, "governed_blocks_changed", func(id string) map[string]any {
			return stepsParams(test, id, editInstructions("in_review", governedWeakened))
		}},
		{"unknown status key", http.StatusBadRequest, "invalid_workflow", func(id string) map[string]any {
			return stepsParamsRaw(test, id, `[{"status_key":"no_such_status","handler":{"type":"none"}}]`)
		}},
		{"status removed", http.StatusBadRequest, "status_removed", func(id string) map[string]any {
			return stepsParamsRaw(test, id, `[{"status_key":"todo","handler":{"type":"none"}}]`)
		}},
		{"new status creation", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			return stepsParamsRaw(test, id, `[{"new_status":{"name":"Brand new"}}]`)
		}},
		{"unknown param field", http.StatusBadRequest, "invalid_params", func(id string) map[string]any {
			params := stepsParams(test, id, editInstructions("todo", "x"))
			params["name"] = "renamed"
			return params
		}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			workflowID, _ := stepsWorkflow(test)
			_, body := requestSteps(test, tc.status, workflowID, tc.params(workflowID))
			mustContain(test, body, tc.want)
		})
	}
}

func TestWorkflowStepsSetApprovalRefusals(test *testing.T) {
	test.Run("approver is not an owner", func(test *testing.T) {
		request := newStepsRequest(test)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test, request.base)
	})
	test.Run("no assertion", func(test *testing.T) {
		request := newStepsRequest(test)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = nil
		request.action.approve(test, passkeyHandler(test), claim, http.StatusForbidden)
		request.assertNotApplied(test, request.base)
	})
	test.Run("invalid assertion", func(test *testing.T) {
		request := newStepsRequest(test)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = map[string]any{"challenge_id": "00000000-0000-0000-0000-000000000000", "credential": map[string]any{"id": "forged"}}
		request.action.approve(test, passkeyHandler(test), claim, http.StatusForbidden)
		request.assertNotApplied(test, request.base)
	})
	test.Run("assertion over another payload", func(test *testing.T) {
		request := newStepsRequest(test)
		other := newStepsRequest(test)
		request.action.enrollPasskey(test)
		otherDevice, _ := other.action.enrollPasskey(test)
		handler := passkeyHandler(test)
		makeApproverOwner(test, request.action, "owner")
		makeApproverOwner(test, other.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = other.action.assertion(test, otherDevice, other.action.issue(test, handler), passkeyStart+1)
		request.action.approve(test, handler, claim, http.StatusForbidden)
		request.assertNotApplied(test, request.base)
	})
	test.Run("params point at a workflow of another workspace", func(test *testing.T) {
		request := newStepsRequest(test)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{workflow_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceWorkflow(test), request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "target_workflow_mismatch")
		request.assertNotApplied(test, request.base)
	})
	test.Run("payload hash altered", func(test *testing.T) {
		request := newStepsRequest(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["payload_sha256"] = strings.Repeat("0", 64)
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict), "payload_mismatch")
		request.assertNotApplied(test, request.base)
	})
	test.Run("preview digest altered or absent", func(test *testing.T) {
		for _, digest := range []any{strings.Repeat("0", 64), nil} {
			request := newStepsRequest(test)
			makeApproverOwner(test, request.action, "owner")
			claim := request.claim(test)
			claim["preview_sha256"] = digest
			mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict), "preview_required")
			request.assertNotApplied(test, request.base)
		}
	})
	test.Run("workflow changed after the request", func(test *testing.T) {
		request := newStepsRequest(test)
		dbfx.Exec(test, `UPDATE issue_workflow SET steps = replace(steps::text, 'do A', 'do A2')::jsonb WHERE id = $1`, request.workflowID)
		drifted := liveStepsDigest(test, request.workflowID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test, drifted)
	})
	test.Run("expired", func(test *testing.T) {
		request := newStepsRequest(test)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = now() - interval '1 minute' WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "expired")
		request.assertNotApplied(test, request.base)
	})
	test.Run("owner role revoked after the request", func(test *testing.T) {
		request := newStepsRequest(test)
		makeApproverOwner(test, request.action, "member")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test, request.base)
	})
	test.Run("governed human block changed behind the request", func(test *testing.T) {
		request := newStepsRequest(test)
		dbfx.Exec(test, `UPDATE issue_workflow SET steps = replace(steps::text, 'human rule', 'weakened rule')::jsonb WHERE id = $1`, request.workflowID)
		drifted := liveStepsDigest(test, request.workflowID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "governed_blocks_changed")
		request.assertNotApplied(test, drifted)
	})
}

func TestWorkflowStepsSetSecondApprovalIsNotPending(test *testing.T) {
	request := newStepsRequest(test)
	handler := withVerifier(acceptingVerifier)
	request.approveOwner(test, handler, http.StatusOK)
	request.action.approve(test, handler, request.claim(test), http.StatusConflict)
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

func TestWorkflowStepsSetConcurrentApprovalsOnTheSameBaseApplyOnce(test *testing.T) {
	workflowID, _ := stepsWorkflow(test)
	first, _ := requestSteps(test, http.StatusCreated, workflowID, stepsParams(test, workflowID, editInstructions("todo", "do A better")))
	second, _ := requestSteps(test, http.StatusCreated, workflowID, stepsParams(test, workflowID, editInstructions("todo", "do A differently")))
	makeApproverOwner(test, first.action, "owner")
	handler := withVerifier(acceptingVerifier)
	statuses := make([]int, 2)
	var group sync.WaitGroup
	for i, request := range []configRequest{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i] = approveStatus(test, handler, request)
		}()
	}
	group.Wait()
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 || (statuses[0] != http.StatusOK && statuses[1] != http.StatusOK) {
		test.Fatalf("applied %d times (statuses %v), want exactly 1 and one 200", applied, statuses)
	}
	for i, request := range []configRequest{first, second} {
		if statuses[i] != http.StatusOK {
			request.action.assertUntouched(test)
		}
	}
}
