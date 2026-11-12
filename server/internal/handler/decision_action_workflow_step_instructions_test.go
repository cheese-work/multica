package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// These tests drive workflow.step.instructions.set (CHE-2949 D5b) through
// request and approval. The shared core rules are proven in
// decision_action_config_change_test.go; here only what this action adds.

const stepInstructionsAction = "workflow.step.instructions.set"

// allowDelegatedInstructions lets agentID propose workflow.instructions writes
// for the length of the test.
func allowDelegatedInstructions(test *testing.T, agentID string) {
	test.Helper()
	var original string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, original)
	})
	setting, err := json.Marshal(map[string]any{"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.instructions"}, "requires_human_decision": false})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb) WHERE id = $1`, testWorkspaceID, delegatedWritesKey, string(setting))
}

func stepInstructionsParams(test *testing.T, workflowID, statusKey, instructions string) map[string]any {
	test.Helper()
	return map[string]any{"workflow_id": workflowID, "status_key": statusKey, "base_sha256": liveStepsDigest(test, workflowID), "instructions": instructions}
}

// requestStepInstructions asks for the action as an agent that may (or may
// not, when delegated is false) write workflow.instructions.
func requestStepInstructions(test *testing.T, status int, delegated bool, workflowID string, params map[string]any) (configRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	if delegated {
		allowDelegatedInstructions(test, fixture.agentID)
	}
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": stepInstructionsAction, "version": 1, "params": params}), status, &created)
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

func newStepInstructionsRequest(test *testing.T) configRequest {
	test.Helper()
	workflowID, _ := stepsWorkflow(test)
	request, _ := requestStepInstructions(test, http.StatusCreated, true, workflowID, stepInstructionsParams(test, workflowID, "todo", "do A better"))
	return request
}

func storedSteps(test *testing.T, workflowID string) []issueworkflow.Step {
	test.Helper()
	var raw []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, workflowID).Scan(&raw)
	steps, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		test.Fatal(err)
	}
	return steps
}

func TestStepInstructionsSetIsRegisteredAsAPreviewedWorkflowChange(test *testing.T) {
	definition, found := decisionaction.Lookup(stepInstructionsAction, 1)
	if !found || definition.Target != decisionaction.TargetWorkflow || !definition.RequiresPreview || definition.MinClass != "spend" {
		test.Fatalf("definition %+v (found %v); want a spend-class workflow action that requires a preview", definition, found)
	}
	if _, ok := configChanges[stepInstructionsAction]; !ok {
		test.Fatal("workflow.step.instructions.set is not registered with the config-change core")
	}
}

func TestStepInstructionsSetRequestStoresTheOneStepPreview(test *testing.T) {
	request := newStepInstructionsRequest(test)
	stored := request.preconditions(test)
	shown, _ := stored["preview"].(map[string]any)
	if shown["field"] != "workflow.instructions" || shown["target_id"] != request.workflowID {
		test.Fatalf("preview %v lacks the field and target", shown)
	}
	before, _ := shown["before"].(map[string]any)
	after, _ := shown["after"].(map[string]any)
	if before["status_key"] != "todo" || before["instructions"] != "do A" || after["status_key"] != "todo" || after["instructions"] != "do A better" {
		test.Fatalf("preview before %v after %v; want the one step, before and after", before, after)
	}
	if stored["before_sha256"] != request.base || stored["preview_sha256"] == nil {
		test.Fatalf("preconditions %v lack the base digest or the preview digest", stored)
	}
}

// The approver must see which workflow and step the text lands on. Both come
// from the live row, inside the preview digest, never from the agent's ask.
func TestStepInstructionsSetPreviewNamesTheWorkflowAndStatusKey(test *testing.T) {
	request := newStepInstructionsRequest(test)
	stored := request.preconditions(test)
	shown, _ := stored["preview"].(map[string]any)
	meta, _ := shown["meta"].(map[string]any)
	var name string
	dbfx.QueryRow(test, `SELECT name FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&name)
	if meta["workflow_name"] != name || meta["status_key"] != "todo" {
		test.Fatalf("preview meta %v; want workflow_name %q and status_key \"todo\"", meta, name)
	}
}

func TestStepInstructionsSetRequestRefusals(test *testing.T) {
	cases := []struct {
		name      string
		status    int
		delegated bool
		want      string
		params    func(workflowID string) map[string]any
	}{
		{"agent may not write workflow.instructions", http.StatusForbidden, false, "not_permitted", func(id string) map[string]any {
			return stepInstructionsParams(test, id, "todo", "x")
		}},
		{"unknown status key", http.StatusBadRequest, true, "unknown_status_key", func(id string) map[string]any {
			return stepInstructionsParams(test, id, "no_such_status", "x")
		}},
		{"stale base digest", http.StatusConflict, true, "precondition_drift", func(id string) map[string]any {
			params := stepInstructionsParams(test, id, "todo", "x")
			params["base_sha256"] = strings.Repeat("0", 64)
			return params
		}},
		{"governed human block edited", http.StatusBadRequest, true, "governed_blocks_changed", func(id string) map[string]any {
			return stepInstructionsParams(test, id, "in_review", "keep <!-- governed:human -->weakened<!-- /governed:human --> tail")
		}},
		{"workflow of another workspace", http.StatusBadRequest, true, "target_workflow_mismatch", func(id string) map[string]any {
			params := stepInstructionsParams(test, id, "todo", "x")
			params["workflow_id"] = foreignWorkspaceWorkflow(test)
			return params
		}},
		{"unknown param field", http.StatusBadRequest, true, "invalid_params", func(id string) map[string]any {
			params := stepInstructionsParams(test, id, "todo", "x")
			params["name"] = "renamed"
			return params
		}},
		{"empty status key", http.StatusBadRequest, true, "invalid_params", func(id string) map[string]any {
			return stepInstructionsParams(test, id, "", "x")
		}},
		{"base digest is not hex", http.StatusBadRequest, true, "invalid_params", func(id string) map[string]any {
			params := stepInstructionsParams(test, id, "todo", "x")
			params["base_sha256"] = "nope"
			return params
		}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			workflowID, _ := stepsWorkflow(test)
			_, body := requestStepInstructions(test, tc.status, tc.delegated, workflowID, tc.params(workflowID))
			mustContain(test, body, tc.want)
		})
	}
}

func TestStepInstructionsSetApprovalWritesOnlyThatStep(test *testing.T) {
	request := newStepInstructionsRequest(test)
	before := storedSteps(test, request.workflowID)
	var name, description string
	var maxRuns int
	dbfx.QueryRow(test, `SELECT name, description, COALESCE(max_runs_per_issue, -1) FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&name, &description, &maxRuns)
	var want map[string]any
	dbfx.QueryRow(test, `SELECT preconditions FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&want)

	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)

	after := storedSteps(test, request.workflowID)
	if len(after) != len(before) {
		test.Fatalf("step count %d, want %d", len(after), len(before))
	}
	for i := range before {
		expected := before[i]
		if expected.StatusKey == "todo" {
			expected.Instructions = "do A better"
		}
		if !reflect.DeepEqual(after[i], expected) {
			test.Fatalf("step %d is %+v, want %+v", i, after[i], expected)
		}
	}
	if got := liveStepsDigest(test, request.workflowID); got != want["after_sha256"] {
		test.Fatalf("live digest %s, want the approved %v", got, want["after_sha256"])
	}
	var nameAfter, descriptionAfter string
	var maxRunsAfter int
	dbfx.QueryRow(test, `SELECT name, description, COALESCE(max_runs_per_issue, -1) FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&nameAfter, &descriptionAfter, &maxRunsAfter)
	if nameAfter != name || descriptionAfter != description || maxRunsAfter != maxRuns {
		test.Fatalf("workflow header changed: %q %q %d, want %q %q %d", nameAfter, descriptionAfter, maxRunsAfter, name, description, maxRuns)
	}
	if state := request.action.state(test); state != "executed" {
		test.Fatalf("action state %s, want executed", state)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d, want 1", n)
	}
}

func TestStepInstructionsSetEditBetweenProposeAndApproveFailsStaleBase(test *testing.T) {
	request := newStepInstructionsRequest(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET steps = replace(steps::text, 'do A', 'do A2')::jsonb WHERE id = $1`, request.workflowID)
	drifted := liveStepsDigest(test, request.workflowID)
	body := request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	mustContain(test, body, "stale_base")
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'failed' AND error_code = 'stale_base'`, request.action.actionID); n != 1 {
		test.Fatalf("action is not failed with stale_base: %s", body)
	}
	if got := liveStepsDigest(test, request.workflowID); got != drifted {
		test.Fatalf("workflow digest %s, want unchanged %s", got, drifted)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

// A person edits the protected block of the target step after the request. The
// approval must report the changed base, not blame the proposal for the block.
func TestStepInstructionsSetHumanEditOfProtectedBlockAfterRequestFailsStaleBase(test *testing.T) {
	workflowID, _ := stepsWorkflow(test)
	steps := storedSteps(test, workflowID)
	for i := range steps {
		if steps[i].StatusKey == "in_review" {
			steps[i].Instructions = governedAgentText
		}
	}
	encoded, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE issue_workflow SET steps = $2::jsonb WHERE id = $1`, workflowID, string(encoded))
	edited := strings.Replace(governedAgentText, "role: author", "role: author, reviewer", 1)
	request, _ := requestStepInstructions(test, http.StatusCreated, true, workflowID, stepInstructionsParams(test, workflowID, "in_review", edited))
	dbfx.Exec(test, `UPDATE issue_workflow SET steps = replace(steps::text, 'never merge', 'merge freely')::jsonb WHERE id = $1`, workflowID)
	drifted := liveStepsDigest(test, workflowID)
	body := request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	mustContain(test, body, "stale_base")
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'failed' AND error_code = 'stale_base'`, request.action.actionID); n != 1 {
		test.Fatalf("action is not failed with stale_base: %s", body)
	}
	if got := liveStepsDigest(test, workflowID); got != drifted {
		test.Fatalf("workflow digest %s, want unchanged %s", got, drifted)
	}
}

func TestStepInstructionsSetReplayedApprovalChangesNothing(test *testing.T) {
	request := newStepInstructionsRequest(test)
	handler := withVerifier(acceptingVerifier)
	request.approveOwner(test, handler, http.StatusOK)
	written := liveStepsDigest(test, request.workflowID)
	request.action.approve(test, handler, request.claim(test), http.StatusConflict)
	if got := liveStepsDigest(test, request.workflowID); got != written {
		test.Fatalf("a replay changed the workflow: %s, want %s", got, written)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

// savedStepEdit is a workflow save as the editor does it: the catalog lock
// first, then the workflow row, then a read-modify-write of one step.
func savedStepEdit(workflowID, statusKey, suffix string) error {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := testHandler.Queries.WithTx(tx).LockIssueStatusCatalog(ctx, parseUUID(testWorkspaceID)); err != nil {
		return err
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT steps FROM issue_workflow WHERE id = $1 FOR UPDATE`, workflowID).Scan(&raw); err != nil {
		return err
	}
	steps, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		return err
	}
	for i := range steps {
		if steps[i].StatusKey == statusKey {
			steps[i].Instructions += suffix
		}
	}
	encoded, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_workflow SET steps = $2::jsonb WHERE id = $1`, workflowID, string(encoded)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestStepInstructionsSetConcurrentSaveAndApprovalLoseNothingAndDoNotDeadlock(test *testing.T) {
	for round := range 5 {
		request := newStepInstructionsRequest(test)
		makeApproverOwner(test, request.action, "owner")
		handler := withVerifier(acceptingVerifier)
		start := make(chan struct{})
		var group sync.WaitGroup
		var saveErr error
		var status int
		var body string
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			saveErr = savedStepEdit(request.workflowID, "in_review", " saved")
		}()
		go func() {
			defer group.Done()
			<-start
			status, body = approveResult(test, handler, request)
		}()
		finished := make(chan struct{})
		go func() { group.Wait(); close(finished) }()
		close(start)
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			test.Fatalf("round %d: save and approval did not finish; deadlock", round)
		}
		if saveErr != nil {
			test.Fatalf("round %d: the save failed: %v", round, saveErr)
		}
		steps := storedSteps(test, request.workflowID)
		inReview, todo := "", ""
		for _, step := range steps {
			switch step.StatusKey {
			case "in_review":
				inReview = step.Instructions
			case "todo":
				todo = step.Instructions
			}
		}
		if !strings.HasSuffix(inReview, " saved") {
			test.Fatalf("round %d: the save was lost: %q", round, inReview)
		}
		switch {
		case status == http.StatusOK && request.action.state(test) == "executed":
			if todo != "do A better" {
				test.Fatalf("round %d: executed but step holds %q", round, todo)
			}
		default:
			// The save won, or the approval met a busy lock: nothing of the action landed.
			if todo != "do A" {
				test.Fatalf("round %d: approval answered %d %s but step holds %q", round, status, body, todo)
			}
		}
	}
}

func TestStepInstructionsSetApprovalTakesTheCatalogLockFirst(test *testing.T) {
	request := newStepInstructionsRequest(test)
	makeApproverOwner(test, request.action, "owner")
	ctx := context.Background()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = holder.Rollback(ctx) })
	if err := testHandler.Queries.WithTx(holder).LockIssueStatusCatalog(ctx, parseUUID(testWorkspaceID)); err != nil {
		test.Fatal(err)
	}
	result := make(chan int, 1)
	go func() {
		status, _ := approveResult(test, withVerifier(acceptingVerifier), request)
		result <- status
	}()
	select {
	case status := <-result:
		// The approval waits for the catalog lock until its lock timeout, then
		// refuses; it must not get as far as writing while the lock is held.
		if status == http.StatusOK && request.action.state(test) == "executed" {
			test.Fatal("the approval wrote while another transaction held the status catalog lock")
		}
	case <-time.After(20 * time.Second):
		test.Fatal("the approval neither finished nor refused while the catalog lock was held")
	}
	if got := storedSteps(test, request.workflowID)[0].Instructions; got != "do A" {
		test.Fatalf("step holds %q while the catalog lock was held, want it unchanged", got)
	}
}
