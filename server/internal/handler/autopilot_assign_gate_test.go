package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-5947 (retro W14): an issue to edit an autopilot was assigned to an agent
// whose run could not write that autopilot, and sat blocked for 16 h. The
// assign-time gate refuses it at the API, judged by the same acting human and
// the same predicate as the autopilot write gate.

func autopilotAssignFixture(t *testing.T, email string) (apID, memberID, agentID string) {
	t.Helper()
	apID = createAutopilotAs(t, "", "che5947-ap") // created by the workspace owner
	memberID = createPlainMember(t, email)        // neither creator nor admin: can_write=false
	agentID = createHandlerTestAgent(t, "che5947-assignee", nil)
	return apID, memberID, agentID
}

func setAutopilotAssignGate(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	var prev []byte
	if err := testPool.QueryRow(ctx, `SELECT COALESCE(settings,'{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings,'{}'::jsonb) || jsonb_build_object('autopilot_assign_gate', $2::text) WHERE id=$1`, testWorkspaceID, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(prev))
	})
}

func createIssueAs(userID string, body map[string]any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	path := "/api/issues?workspace_id=" + testWorkspaceID
	if userID == "" {
		testHandler.CreateIssue(w, newRequest("POST", path, body))
	} else {
		testHandler.CreateIssue(w, newRequestAs(userID, "POST", path, body))
	}
	if w.Code == http.StatusCreated {
		var is IssueResponse
		json.NewDecoder(strings.NewReader(w.Body.String())).Decode(&is)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, is.ID)
	}
	return w
}

func TestCreateIssue_AutopilotEditAssignedWithoutWriteIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-create@multica.test")
	body := map[string]any{
		"title": "plan(setup): edit the retro autopilot", "status": "todo",
		"description":   "Edit the description of autopilot `" + apID + "`.",
		"assignee_type": "agent", "assignee_id": agentID,
	}

	w := createIssueAs(memberID, body)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "autopilot_assign_no_write") || !strings.Contains(w.Body.String(), apID) {
		t.Fatalf("member without write: got %d %s, want 403 autopilot_assign_no_write naming the autopilot", w.Code, w.Body.String())
	}
	if w := createIssueAs("", body); w.Code != http.StatusCreated {
		t.Fatalf("owner (can_write): got %d %s, want 201", w.Code, w.Body.String())
	}
}

func TestCreateIssue_AutopilotAssignGateLeavesOtherCallsAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-other@multica.test")
	cases := map[string]map[string]any{
		"no assignee":        {"title": "t", "description": "autopilot `" + apID + "`"},
		"no autopilot word":  {"title": "t", "description": "see `" + apID + "`", "assignee_type": "agent", "assignee_id": agentID},
		"id is no autopilot": {"title": "t", "description": "autopilot 11111111-2222-4333-8444-555555555555", "assignee_type": "agent", "assignee_id": agentID},
	}
	for name, body := range cases {
		body["status"] = "todo"
		if w := createIssueAs(memberID, body); w.Code != http.StatusCreated {
			t.Errorf("%s: got %d %s, want 201", name, w.Code, w.Body.String())
		}
	}
}

// Revert path: the workspace setting turns the gate off, or to warn.
func TestCreateIssue_AutopilotAssignGateModes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-mode@multica.test")
	body := map[string]any{"title": "t", "status": "todo", "description": "autopilot `" + apID + "`",
		"assignee_type": "agent", "assignee_id": agentID}
	for mode, want := range map[string]int{"off": http.StatusCreated, "warn": http.StatusCreated, "block": http.StatusForbidden} {
		setAutopilotAssignGate(t, mode)
		if w := createIssueAs(memberID, body); w.Code != want {
			t.Errorf("mode %s: got %d %s, want %d", mode, w.Code, w.Body.String(), want)
		}
	}
}

func TestUpdateIssue_AutopilotEditReassignedWithoutWriteIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-update@multica.test")
	// An unassigned issue that names the autopilot, made by the member.
	w := httptest.NewRecorder()
	testHandler.CreateIssue(w, newRequestAs(memberID, "POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "edit autopilot", "status": "todo", "description": "Edit autopilot `" + apID + "`."}))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
	}
	var is IssueResponse
	json.NewDecoder(w.Body).Decode(&is)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, is.ID)
	})

	update := func(userID string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := newRequestAs(userID, "PUT", "/api/issues/"+is.ID+"?workspace_id="+testWorkspaceID,
			map[string]any{"assignee_type": "agent", "assignee_id": agentID})
		testHandler.UpdateIssue(w, withURLParam(req, "id", is.ID))
		return w
	}
	if w := update(memberID); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "autopilot_assign_no_write") {
		t.Fatalf("member without write: got %d %s, want 403 autopilot_assign_no_write", w.Code, w.Body.String())
	}
	if w := update(testUserID); w.Code != http.StatusOK {
		t.Fatalf("owner: got %d %s, want 200", w.Code, w.Body.String())
	}
}

func TestValidateAutopilotAssignGateSetting(t *testing.T) {
	for _, ok := range []any{map[string]any{}, map[string]any{"autopilot_assign_gate": "off"}, map[string]any{"autopilot_assign_gate": "warn"}, map[string]any{"autopilot_assign_gate": "block"}} {
		if err := validateAutopilotAssignGateSetting(ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range []any{map[string]any{"autopilot_assign_gate": "on"}, map[string]any{"autopilot_assign_gate": nil}, map[string]any{"autopilot_assign_gate": 1}} {
		if err := validateAutopilotAssignGateSetting(bad); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}

// F1: a project column that hands off on create picks the assignee after the
// request's own assignee is read. The gate judges that resolved assignee.
func TestCreateIssue_AutopilotEditHandedOffByWorkflowWithoutWriteIsRefused(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-handoff@multica.test")
	projectID := createWorkflowTestProject(t, "Autopilot gate handoff project")
	setColumnHandoff(t, projectID, "todo", map[string]any{
		"handler": map[string]any{"type": "agent", "id": agentID}, "instructions": "Edit it.",
	}).Want(http.StatusOK)
	body := map[string]any{
		"title": "edit the retro autopilot", "project_id": projectID,
		"description": "Edit the description of autopilot `" + apID + "`.",
	}

	w := createIssueAs(memberID, body)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "autopilot_assign_no_write") {
		t.Fatalf("member without write, workflow hand-off: got %d %s, want 403 autopilot_assign_no_write", w.Code, w.Body.String())
	}
	if w := createIssueAs("", body); w.Code != http.StatusCreated {
		t.Fatalf("owner (can_write): got %d %s, want 201", w.Code, w.Body.String())
	}
}

// F2: the batch path skips an issue the gate refuses, like its other guards.
func TestBatchUpdateIssues_AutopilotEditReassignedWithoutWriteIsSkipped(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	apID, memberID, agentID := autopilotAssignFixture(t, "che5947-batch@multica.test")
	newIssue := func(title string) string {
		w := httptest.NewRecorder()
		testHandler.CreateIssue(w, newRequestAs(memberID, "POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title": title, "status": "todo", "description": "Edit autopilot `" + apID + "`."}))
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
		}
		var is IssueResponse
		json.NewDecoder(w.Body).Decode(&is)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, is.ID)
			testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id=$1`, is.ID)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, is.ID)
		})
		return is.ID
	}
	assignee := func(id string) *string {
		var a *string
		testPool.QueryRow(context.Background(), `SELECT assignee_id::text FROM issue WHERE id=$1`, id).Scan(&a)
		return a
	}
	batch := func(userID string, ids ...string) {
		w := httptest.NewRecorder()
		testHandler.BatchUpdateIssues(w, newRequestAs(userID, "POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID, map[string]any{
			"issue_ids": ids, "updates": map[string]any{"assignee_type": "agent", "assignee_id": agentID}}))
		if w.Code != http.StatusOK {
			t.Fatalf("BatchUpdateIssues: %d %s", w.Code, w.Body.String())
		}
	}

	refused, plain := newIssue("batch edit autopilot one"), newIssue("batch edit autopilot two")
	testPool.Exec(context.Background(), `UPDATE issue SET description='nothing here' WHERE id=$1`, plain)
	batch(memberID, refused, plain)
	if a := assignee(refused); a != nil {
		t.Fatalf("batch assigned an autopilot edit to an agent that cannot write it: %s", *a)
	}
	if a := assignee(plain); a == nil || *a != agentID {
		t.Fatalf("batch skipped an issue the gate does not cover: assignee = %v, want %s", a, agentID)
	}
	owned := newIssue("batch edit autopilot three")
	batch(testUserID, owned)
	if a := assignee(owned); a == nil || *a != agentID {
		t.Fatalf("owner (can_write): assignee = %v, want %s", a, agentID)
	}
}

// Round 2: the gate judges the human the RUN will act for, not the assigner. A
// member assigning acts for themselves; an agent assigning starts a run whose
// human comes from the issue (its creator chain), not from the assigner's task.

func autopilotIssueBy(t *testing.T, creatorUserID, title, apID string) string {
	t.Helper()
	w := httptest.NewRecorder()
	body := map[string]any{"title": title, "status": "todo", "description": "Edit autopilot `" + apID + "`."}
	testHandler.CreateIssue(w, newRequestAs(creatorUserID, "POST", "/api/issues?workspace_id="+testWorkspaceID, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", w.Code, w.Body.String())
	}
	var is IssueResponse
	json.NewDecoder(w.Body).Decode(&is)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id=$1`, is.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, is.ID)
	})
	return is.ID
}

// assignAsAgent assigns the issue to targetAgent from inside a running task of
// assigner (an agent), the way the CLI does.
func assignAsAgent(t *testing.T, batch bool, issueID, assignerAgent, assignerTask, targetAgent string) *httptest.ResponseRecorder {
	t.Helper()
	return assignAsAgentWithHandoff(t, batch, issueID, assignerAgent, assignerTask, targetAgent, "")
}

// assignAsAgentWithHandoff is assignAsAgent for a single update that names a
// handoff comment: the run it starts is attributed to the moving task's human.
func assignAsAgentWithHandoff(t *testing.T, batch bool, issueID, assignerAgent, assignerTask, targetAgent, handoffComment string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	assignee := map[string]any{"assignee_type": "agent", "assignee_id": targetAgent}
	if handoffComment != "" {
		assignee["handoff_comment_id"] = handoffComment
	}
	var r *http.Request
	if batch {
		r = newRequestAs(testUserID, "POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID,
			map[string]any{"issue_ids": []string{issueID}, "updates": assignee})
	} else {
		r = withURLParam(newRequestAs(testUserID, "PUT", "/api/issues/"+issueID+"?workspace_id="+testWorkspaceID, assignee), "id", issueID)
	}
	r.Header.Set("X-Actor-Source", "task_token")
	r.Header.Set("X-Agent-ID", assignerAgent)
	r.Header.Set("X-Task-ID", assignerTask)
	if batch {
		testHandler.BatchUpdateIssues(w, r)
	} else {
		testHandler.UpdateIssue(w, r)
	}
	return w
}

func TestAutopilotAssignGate_JudgesTheRunsHumanNotTheAssigner(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	apID, memberID, targetAgent := autopilotAssignFixture(t, "che5947-run-human@multica.test")
	assigner := createHandlerTestAgent(t, "che5947-assigner", nil)
	ownerTask := callerTask(t, assigner, testUserID) // assigner's task acts for the owner (can_write)
	memberTask := callerTask(t, assigner, memberID)  // assigner's task acts for a member without write
	assignee := func(id string) *string {
		var a *string
		testPool.QueryRow(context.Background(), `SELECT assignee_id::text FROM issue WHERE id=$1`, id).Scan(&a)
		return a
	}

	for _, batch := range []bool{false, true} {
		name := map[bool]string{false: "update", true: "batch"}[batch]

		// The assigner's human can write, but the run would act for the issue's
		// creator, a member without write: refuse (batch: skip).
		byMember := autopilotIssueBy(t, memberID, name+" authorized assigner, creator without write", apID)
		w := assignAsAgent(t, batch, byMember, assigner, ownerTask, targetAgent)
		if batch {
			if w.Code != http.StatusOK || assignee(byMember) != nil {
				t.Errorf("%s: authorized assigner, run acts for a member without write: got %d, assigned, want skipped", name, w.Code)
			}
		} else if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "autopilot_assign_no_write") {
			t.Errorf("%s: authorized assigner, run acts for a member without write: got %d %s, want 403 autopilot_assign_no_write", name, w.Code, w.Body.String())
		}

		// The assigner's human cannot write, but the run acts for the issue's
		// creator, the owner: allow.
		byOwner := autopilotIssueBy(t, testUserID, name+" unauthorized assigner, creator owner", apID)
		w = assignAsAgent(t, batch, byOwner, assigner, memberTask, targetAgent)
		if a := assignee(byOwner); w.Code != http.StatusOK || a == nil || *a != targetAgent {
			t.Errorf("%s: unauthorized assigner, run acts for the owner: got %d %s, assignee %v, want 200 and assigned", name, w.Code, w.Body.String(), a)
		}
	}
}

// A status handoff that names a comment starts its run for the moving task's
// human (statusHandoffAttribution), not the issue's. The gate must judge that
// same human, in both directions.
func TestAutopilotAssignGate_HandoffJudgesTheMovingTasksHuman(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	apID, memberID, targetAgent := autopilotAssignFixture(t, "che5947-handoff-human@multica.test")
	mover := createHandlerTestAgent(t, "che5947-mover", nil)
	ownerTask := callerTask(t, mover, testUserID) // moving task acts for the owner (can_write)
	memberTask := callerTask(t, mover, memberID)  // moving task acts for a member without write
	assignee := func(id string) *string {
		var a *string
		testPool.QueryRow(context.Background(), `SELECT assignee_id::text FROM issue WHERE id=$1`, id).Scan(&a)
		return a
	}

	// The issue came from a member without write, the moving task acts for the
	// owner: the run acts for the owner, so allow.
	byMember := autopilotIssueBy(t, memberID, "handoff moving task owner, issue by member", apID)
	w := assignAsAgentWithHandoff(t, false, byMember, mover, ownerTask, targetAgent, dbfx.Comment(t, byMember, "handoff"))
	if a := assignee(byMember); w.Code != http.StatusOK || a == nil || *a != targetAgent {
		t.Errorf("moving task acts for the owner: got %d %s, assignee %v, want 200 and assigned", w.Code, w.Body.String(), a)
	}

	// The issue came from the owner, the moving task acts for a member without
	// write: the run acts for that member, so refuse.
	byOwner := autopilotIssueBy(t, testUserID, "handoff moving task member, issue by owner", apID)
	w = assignAsAgentWithHandoff(t, false, byOwner, mover, memberTask, targetAgent, dbfx.Comment(t, byOwner, "handoff"))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "autopilot_assign_no_write") {
		t.Errorf("moving task acts for a member without write: got %d %s, want 403 autopilot_assign_no_write", w.Code, w.Body.String())
	}
}
