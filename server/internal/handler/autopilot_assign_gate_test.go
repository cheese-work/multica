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
