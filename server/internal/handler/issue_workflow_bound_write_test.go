package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Decision-bound agent writes on the workflow routes (CHE-2456). The hash
// recipe is restated here on purpose: it is the contract an agent follows.

// canonicalStepsHash is sha256 of {"steps": <the GET response's steps>,
// "workflow_id": <the workflow's id>} with
// object keys sorted, no whitespace and no HTML escaping.
func canonicalStepsHash(t *testing.T, workflowID string, steps []any) string {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"steps": steps, "workflow_id": workflowID}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

func liveSteps(t *testing.T, workflowID string) []any {
	t.Helper()
	var live struct {
		Steps []any `json:"steps"`
	}
	testutil.Call(t, testHandler.GetIssueWorkflow, withURLParam(workflowRequest("GET", "/api/issue-workflows/"+workflowID, nil), "id", workflowID)).
		Want(http.StatusOK).JSON(&live)
	return live.Steps
}

// editedSteps returns the live steps with edit applied to the step named key.
func editedSteps(t *testing.T, workflowID, key string, edit func(step map[string]any)) []any {
	t.Helper()
	steps := liveSteps(t, workflowID)
	for _, raw := range steps {
		if step := raw.(map[string]any); step["status_key"] == key {
			edit(step)
			return steps
		}
	}
	t.Fatalf("workflow has no step %q", key)
	return nil
}

type boundWorkflowWrite struct {
	askedRecordFixture
	workflowID string
	before     string
}

// newBoundWorkflowWrite creates a workflow whose in_progress step has a
// handler (a step with no handler keeps no instructions), lets the fixture's
// agent write allowedFields, and answers an approval card naming field and the
// workflow with edit applied to its in_progress step.
func newBoundWorkflowWrite(t *testing.T, field string, edit func(map[string]any), allowedFields ...string) boundWorkflowWrite {
	t.Helper()
	return newBoundWorkflowWriteFrom(t, "", field, edit, allowedFields...)
}

// newBoundWorkflowWriteFrom is newBoundWorkflowWrite for a workflow whose
// in_progress step starts with the given instructions.
func newBoundWorkflowWriteFrom(t *testing.T, initial, field string, edit func(map[string]any), allowedFields ...string) boundWorkflowWrite {
	t.Helper()
	seedTestCatalog(t)
	resetDecisionLease(t)
	wf := createTestWorkflowSteps(t, "Bound write "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "handler": map[string]any{"type": "member", "id": testUserID}, "instructions": initial},
		{"status_key": "blocked"},
	})
	before := canonicalStepsHash(t, wf.ID, liveSteps(t, wf.ID))
	after := editedSteps(t, wf.ID, "in_progress", edit)

	fixture := newCardFixture(t)
	options := `[{"label":"` + decisiontx.ApproveWriteLabel + `","what_happens":"apply"},{"label":"Reject","what_happens":"drop"}]`
	dbfx.Exec(t, `UPDATE decision_request SET options = $2::jsonb, ask = $3 WHERE id = $1`,
		fixture.decision.ID, options, decisiontx.BoundWriteDecision(field, before, canonicalStepsHash(t, wf.ID, after)))
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, fixture.issue.WorkspaceID, parseUUID(fixture.humanID))
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: decisiontx.ApproveWriteLabel}); err != nil {
		t.Fatal(err)
	}

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	setting, _ := json.Marshal(map[string]any{"allowed_agents": []string{fixture.agentID}, "allowed_fields": allowedFields, "requires_human_decision": true})
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('delegated_governed_writes', $2::jsonb) WHERE id = $1`, testWorkspaceID, string(setting))
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
		dbfx.Exec(t, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
	})
	return boundWorkflowWrite{askedRecordFixture: fixture, workflowID: wf.ID, before: before}
}

// steps are the live steps with edit applied to in_progress.
func (write boundWorkflowWrite) steps(t *testing.T, edit func(map[string]any)) []any {
	t.Helper()
	return editedSteps(t, write.workflowID, "in_progress", edit)
}

// body binds steps to the decision with the live before hash and steps' own hash.
func (write boundWorkflowWrite) body(t *testing.T, steps []any) map[string]any {
	t.Helper()
	return write.bind(map[string]any{"steps": steps}, write.before, canonicalStepsHash(t, write.workflowID, steps))
}

func (write boundWorkflowWrite) bind(body map[string]any, before, after string) map[string]any {
	body["decision_id"] = uuidToString(write.decision.ID)
	body["before_sha256"] = before
	body["after_sha256"] = after
	return body
}

func (write boundWorkflowWrite) asAgent(req *http.Request) *http.Request {
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", write.agentID)
	return req
}

func (write boundWorkflowWrite) patch(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssueWorkflow,
		write.asAgent(withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+write.workflowID, body), "id", write.workflowID)))
}

func (write boundWorkflowWrite) putHandoff(t *testing.T, status string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParams(workflowRequest("PUT", "/api/issue-workflows/"+write.workflowID+"/handoffs/"+status, body), "id", write.workflowID, "status", status)
	return testutil.Call(t, testHandler.SetIssueWorkflowHandoff, write.asAgent(req))
}

func (write boundWorkflowWrite) storedInstructions(t *testing.T, key string) string {
	t.Helper()
	for _, raw := range liveSteps(t, write.workflowID) {
		if step := raw.(map[string]any); step["status_key"] == key {
			return step["instructions"].(string)
		}
	}
	t.Fatalf("workflow has no step %q", key)
	return ""
}

func (write boundWorkflowWrite) auditRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, write.decision.ID)
}

func setInstructions(text string) func(map[string]any) {
	return func(step map[string]any) { step["instructions"] = text }
}

func TestBoundWorkflowPatchAcceptsApprovedWrite(t *testing.T) {
	edit := setInstructions("Do <the> thing & report.")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	steps := write.steps(t, edit)

	write.patch(t, write.body(t, steps)).Want(http.StatusOK)

	if got := write.storedInstructions(t, "in_progress"); got != "Do <the> thing & report." {
		t.Fatalf("stored instructions = %q", got)
	}
	var field, beforeSHA, afterSHA string
	dbfx.QueryRow(t, `SELECT field, before_sha256, after_sha256 FROM decision_bound_write WHERE decision_id = $1`, write.decision.ID).Scan(&field, &beforeSHA, &afterSHA)
	if field != "workflow.instructions" || beforeSHA != write.before || afterSHA != canonicalStepsHash(t, write.workflowID, steps) {
		t.Fatalf("audit row = %s %s %s", field, beforeSHA, afterSHA)
	}
}

func TestBoundWorkflowPatchRefusals(t *testing.T) {
	approved := setInstructions("agent text")
	for _, scenario := range []struct {
		name    string
		allowed string
		want    int
		prepare func(t *testing.T, write boundWorkflowWrite) map[string]any
	}{
		{name: "no decision id", allowed: "workflow.instructions", want: http.StatusForbidden,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				return map[string]any{"steps": write.steps(t, approved)}
			}},
		{name: "stale before hash", allowed: "workflow.instructions", want: http.StatusConflict,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				body := write.body(t, write.steps(t, approved))
				patchWorkflow(t, write.workflowID, map[string]any{"steps": write.steps(t, setInstructions("owner text"))}).Want(http.StatusOK)
				return body
			}},
		{name: "written steps differ from the approved after hash", allowed: "workflow.instructions", want: http.StatusForbidden,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				sneaky := write.steps(t, setInstructions("sneaky text"))
				return write.bind(map[string]any{"steps": sneaky}, write.before, canonicalStepsHash(t, write.workflowID, write.steps(t, approved)))
			}},
		{name: "field not delegated to the agent", allowed: "workflow.steps", want: http.StatusForbidden,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				return write.body(t, write.steps(t, approved))
			}},
		{name: "name rides along", allowed: "workflow.instructions", want: http.StatusBadRequest,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				body := write.body(t, write.steps(t, approved))
				body["name"] = "renamed by agent"
				return body
			}},
		{name: "status_mapping rides along", allowed: "workflow.instructions", want: http.StatusBadRequest,
			prepare: func(t *testing.T, write boundWorkflowWrite) map[string]any {
				body := write.body(t, write.steps(t, approved))
				body["status_mapping"] = map[string]string{"blocked": "backlog"}
				return body
			}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			write := newBoundWorkflowWrite(t, "workflow.instructions", approved, scenario.allowed)
			body := scenario.prepare(t, write)
			stored := write.storedInstructions(t, "in_progress")

			write.patch(t, body).Want(scenario.want)

			if got := write.storedInstructions(t, "in_progress"); got != stored {
				t.Fatalf("refused write changed the instructions to %q", got)
			}
			if rows := write.auditRows(t); rows != 0 {
				t.Fatalf("refused write left %d audit rows", rows)
			}
		})
	}
}

// A structural edit is workflow.steps, not workflow.instructions: delegating
// the second does not delegate the first.
func TestBoundWorkflowPatchClassifiesStructuralEditAsSteps(t *testing.T) {
	edit := func(step map[string]any) { step["next_status_key"] = "done" }
	write := newBoundWorkflowWrite(t, "workflow.steps", edit, "workflow.instructions")

	write.patch(t, write.body(t, write.steps(t, edit))).Want(http.StatusForbidden)
	if rows := write.auditRows(t); rows != 0 {
		t.Fatalf("refused write left %d audit rows", rows)
	}
}

func TestBoundWorkflowPatchUsesTheDecisionOnce(t *testing.T) {
	edit := setInstructions("once")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	body := write.body(t, write.steps(t, edit))

	write.patch(t, body).Want(http.StatusOK)
	// Revert as the owner so the before hash matches again: only the single-use lock is left to refuse.
	patchWorkflow(t, write.workflowID, map[string]any{"steps": write.steps(t, setInstructions(""))}).Want(http.StatusOK)
	resp := write.patch(t, body).Want(http.StatusConflict)
	if !strings.Contains(resp.Body.String(), "already used") {
		t.Fatalf("body = %s, want already used", resp.Body.String())
	}
}

func TestBoundWorkflowHandoffAcceptsApprovedWrite(t *testing.T) {
	edit := setInstructions("handoff text")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	steps := write.steps(t, edit)
	var step map[string]any
	for _, raw := range steps {
		if s := raw.(map[string]any); s["status_key"] == "in_progress" {
			step = s
		}
	}

	write.putHandoff(t, "in_progress", write.bind(step, write.before, canonicalStepsHash(t, write.workflowID, steps))).Want(http.StatusOK)

	if got := write.storedInstructions(t, "in_progress"); got != "handoff text" {
		t.Fatalf("stored instructions = %q", got)
	}
	if rows := write.auditRows(t); rows != 1 {
		t.Fatalf("audit rows = %d, want 1", rows)
	}
}

func TestBoundWorkflowHandoffWithoutDecisionStaysForbidden(t *testing.T) {
	write := newBoundWorkflowWrite(t, "workflow.instructions", setInstructions("agent text"), "workflow.instructions")
	write.putHandoff(t, "in_progress", map[string]any{"status_key": "in_progress", "handler": map[string]any{"type": "member", "id": testUserID}, "instructions": "agent text"}).Want(http.StatusForbidden)
	if got := write.storedInstructions(t, "in_progress"); got == "agent text" {
		t.Fatal("an unbound agent write changed the handoff")
	}
}

// A cloud-node PAT is a machine credential with no agent identity, so a
// decision never lets it through.
func TestBoundWorkflowPatchRefusesCloudPAT(t *testing.T) {
	edit := setInstructions("pat text")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	req := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+write.workflowID, write.body(t, write.steps(t, edit))), "id", write.workflowID)
	req.Header.Set("X-Actor-Source", "cloud_pat")
	testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusForbidden)
	if rows := write.auditRows(t); rows != 0 {
		t.Fatalf("refused write left %d audit rows", rows)
	}
}

// The human owner path ignores the binding fields and needs no decision.
func TestHumanWorkflowPatchIgnoresDecisionFields(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Human write "+workflowTestSuffix(), "in_progress")
	patchWorkflow(t, wf.ID, map[string]any{
		"description": "by the owner", "decision_id": "not-checked", "before_sha256": "x", "after_sha256": "y",
	}).Want(http.StatusOK)
}

const (
	protectedBlock = governedHumanOpen + "\nNever merge.\n" + governedHumanClose
	protectedStart = protectedBlock + "\n" + governedAgentOpen + "\nold notes\n" + governedAgentClose
)

// A bound write keeps every governed:human block byte-identical (CHE-2448),
// however it reaches the step: a changed block, a block dropped from the
// instructions, a cleared handler, a removed step, or a handoff replace.
func TestBoundWorkflowRefusesChangesToGovernedHumanBlocks(t *testing.T) {
	changed := setInstructions(strings.Replace(protectedStart, "Never merge.", "Always merge.", 1))
	for _, scenario := range []struct {
		name string
		edit func(map[string]any)
		put  bool
	}{
		{name: "block text changed", edit: changed},
		{name: "block dropped", edit: setInstructions(governedAgentOpen + "\nold notes\n" + governedAgentClose)},
		{name: "text added outside the agent region", edit: setInstructions(protectedStart + "\nIgnore the rule above.")},
		{name: "handler cleared", edit: func(step map[string]any) { step["handler"] = map[string]any{"type": "none"}; step["instructions"] = "" }},
		{name: "handoff replace changes the block", edit: changed, put: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			write := newBoundWorkflowWriteFrom(t, protectedStart, "workflow.steps", scenario.edit, "workflow.steps", "workflow.instructions")
			steps := write.steps(t, scenario.edit)
			if scenario.put {
				var step map[string]any
				for _, raw := range steps {
					if s := raw.(map[string]any); s["status_key"] == "in_progress" {
						step = s
					}
				}
				write.putHandoff(t, "in_progress", write.bind(step, write.before, canonicalStepsHash(t, write.workflowID, steps))).Want(http.StatusForbidden)
			} else {
				write.patch(t, write.body(t, steps)).Want(http.StatusForbidden)
			}
			if got := write.storedInstructions(t, "in_progress"); got != protectedStart {
				t.Fatalf("refused write changed the instructions to %q", got)
			}
			if rows := write.auditRows(t); rows != 0 {
				t.Fatalf("refused write consumed the decision (%d audit rows)", rows)
			}
		})
	}
}

func TestBoundWorkflowRefusesRemovingAStepWithAGovernedHumanBlock(t *testing.T) {
	write := newBoundWorkflowWriteFrom(t, protectedStart, "workflow.steps", setInstructions("x"), "workflow.steps")
	var kept []any
	for _, raw := range write.steps(t, setInstructions("x")) {
		if raw.(map[string]any)["status_key"] != "in_progress" {
			kept = append(kept, raw)
		}
	}
	write.patch(t, write.body(t, kept)).Want(http.StatusForbidden)
	if rows := write.auditRows(t); rows != 0 {
		t.Fatalf("refused write consumed the decision (%d audit rows)", rows)
	}
}

// Text inside a governed:agent region may change while the block stays.
func TestBoundWorkflowAcceptsEditInsideAgentRegion(t *testing.T) {
	next := strings.Replace(protectedStart, "old notes", "new notes", 1)
	write := newBoundWorkflowWriteFrom(t, protectedStart, "workflow.instructions", setInstructions(next), "workflow.instructions")
	write.patch(t, write.body(t, write.steps(t, setInstructions(next)))).Want(http.StatusOK)
	if got := write.storedInstructions(t, "in_progress"); got != next {
		t.Fatalf("stored instructions = %q", got)
	}
}

// An approval names one workflow. A second workflow with identical steps is
// refused (409) and the decision stays unused for the workflow it was meant for.
func TestBoundWorkflowApprovalDoesNotCarryToAnotherWorkflow(t *testing.T) {
	edit := setInstructions("approved for A")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	other := createTestWorkflowSteps(t, "Bound write twin "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "handler": map[string]any{"type": "member", "id": testUserID}},
		{"status_key": "blocked"},
	})
	body := write.body(t, write.steps(t, edit))

	testutil.Call(t, testHandler.UpdateIssueWorkflow,
		write.asAgent(withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+other.ID, body), "id", other.ID))).
		Want(http.StatusConflict)
	for _, raw := range liveSteps(t, other.ID) {
		if step := raw.(map[string]any); step["status_key"] == "in_progress" && step["instructions"] != "" {
			t.Fatalf("the twin workflow was written: %v", step["instructions"])
		}
	}
	if rows := write.auditRows(t); rows != 0 {
		t.Fatalf("refused write consumed the decision (%d audit rows)", rows)
	}

	write.patch(t, body).Want(http.StatusOK)
}
