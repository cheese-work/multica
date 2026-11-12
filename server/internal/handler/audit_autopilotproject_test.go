package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2774 L5a: autopilot, trigger, project, project workflow and project
// resource admin writes record an audit row in the mutation's transaction.

func apjAssertNoLeak(t *testing.T, row auditRow, secrets ...string) {
	t.Helper()
	encoded, _ := json.Marshal(row)
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("%q reached the audit row: %s", secret, encoded)
		}
	}
}

func apjAssertMemberRow(t *testing.T, rows []auditRow, kind, targetType string) auditRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s audit rows = %d; want 1", kind, len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != targetType || row.Kind != kind {
		t.Errorf("row = %+v; want %s by member %s on target %s", row, kind, testUserID, targetType)
	}
	return row
}

// ---- autopilot ------------------------------------------------------------

func apjCreateAutopilotViaHandler(t *testing.T, agentID, title string) (int, AutopilotResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.CreateAutopilot(w, newRequest(http.MethodPost, "/api/autopilots", map[string]any{
		"title":          title,
		"description":    "secret prompt body\nsecond line",
		"assignee_id":    agentID,
		"execution_mode": "create_issue",
	}))
	var resp AutopilotResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID != "" {
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, resp.ID) })
	}
	return w.Code, resp
}

func TestCreateAutopilot_WritesAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-create-agent")
	code, resp := apjCreateAutopilotViaHandler(t, agentID, "che2774 ap create")
	if code != http.StatusCreated {
		t.Fatalf("CreateAutopilot: %d", code)
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.create", resp.ID), "autopilot.create", "autopilot")
	if len(row.Before) != 0 {
		t.Errorf("create before = %v; want empty", row.Before)
	}
	if row.After["title"] != "che2774 ap create" || row.After["status"] != "active" || row.After["assignee_id"] != agentID {
		t.Errorf("after = %v; want title, status active, assignee", row.After)
	}
	if d, _ := row.After["description"].(map[string]any); d == nil || d["lines"] != float64(2) {
		t.Errorf("description must be a 2-line digest, got %v", row.After["description"])
	}
	apjAssertNoLeak(t, row, "secret prompt body")
}

func TestCreateAutopilot_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-create-fail-agent")
	failAuditInsertForKind(t, "autopilot.create", testWorkspaceID)
	code, _ := apjCreateAutopilotViaHandler(t, agentID, "che2774 ap create must vanish")
	if code != http.StatusInternalServerError {
		t.Fatalf("CreateAutopilot with failing audit insert: %d; want 500", code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM autopilot WHERE title = 'che2774 ap create must vanish'`); n != 0 {
		t.Fatalf("autopilot rows = %d after failed audit; the create must roll back", n)
	}
}

func apjPatchAutopilot(id string, body map[string]any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler.UpdateAutopilot(w, withURLParam(newRequest(http.MethodPatch, "/api/autopilots/"+id, body), "id", id))
	return w
}

func TestUpdateAutopilot_WritesAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-update-agent")
	id := createWebhookTestAutopilot(t, agentID, "active", "run_only")

	w := apjPatchAutopilot(id, map[string]any{"title": "che2774 renamed", "status": "paused", "description": "secret prompt body"})
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAutopilot: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.update", id), "autopilot.update", "autopilot")
	if row.Before["status"] != "active" || row.After["status"] != "paused" || row.After["title"] != "che2774 renamed" {
		t.Errorf("diff = %v -> %v; want status active->paused and the new title", row.Before, row.After)
	}
	if _, ok := row.After["execution_mode"]; ok {
		t.Errorf("unchanged execution_mode must not appear: %v", row.After)
	}
	apjAssertNoLeak(t, row, "secret prompt body")
}

func TestUpdateAutopilot_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-update-fail-agent")
	id := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	failAuditInsertFor(t, id)

	if w := apjPatchAutopilot(id, map[string]any{"status": "paused"}); w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateAutopilot with failing audit insert: %d; want 500", w.Code)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM autopilot WHERE id = $1`, id).Scan(&status)
	if status != "active" {
		t.Fatalf("autopilot status = %q after failed audit; the update must roll back", status)
	}
}

func apjDeleteAutopilot(id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler.DeleteAutopilot(w, withURLParam(newRequest(http.MethodDelete, "/api/autopilots/"+id, nil), "id", id))
	return w
}

func TestDeleteAutopilot_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-delete-agent")
	id := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	if w := apjDeleteAutopilot(id); w.Code != http.StatusNoContent {
		t.Fatalf("DeleteAutopilot: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.delete", id), "autopilot.delete", "autopilot")
	if row.Before["status"] != "active" || len(row.After) != 0 {
		t.Errorf("diff = %v -> %v; want the pre-archive snapshot and no after", row.Before, row.After)
	}
}

func TestDeleteAutopilot_AuditInsertFailureKeepsAutopilot(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-ap-delete-fail-agent")
	id := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	failAuditInsertFor(t, id)
	if w := apjDeleteAutopilot(id); w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteAutopilot with failing audit insert: %d; want 500", w.Code)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM autopilot WHERE id = $1`, id).Scan(&status)
	if status != "active" {
		t.Fatalf("autopilot status = %q after failed audit; the archive must roll back", status)
	}
}

// ---- autopilot triggers ---------------------------------------------------

func apjCreateTriggerViaHandler(autopilotID string, body map[string]any) (int, AutopilotTriggerResponse) {
	w := httptest.NewRecorder()
	testHandler.CreateAutopilotTrigger(w, withURLParam(newRequest(http.MethodPost, "/api/autopilots/"+autopilotID+"/triggers", body), "id", autopilotID))
	var resp AutopilotTriggerResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func TestCreateAutopilotTrigger_WritesAuditRowWithoutSecrets(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-create-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")

	code, hook := apjCreateTriggerViaHandler(apID, map[string]any{"kind": "webhook", "label": "che2774 hook"})
	if code != http.StatusCreated || hook.WebhookToken == nil || *hook.WebhookToken == "" {
		t.Fatalf("webhook trigger create: %d token=%v", code, hook.WebhookToken)
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.trigger.create", hook.ID), "autopilot.trigger.create", "autopilot_trigger")
	if row.After["kind"] != "webhook" || row.After["autopilot_id"] != apID {
		t.Errorf("after = %v; want kind webhook on autopilot %s", row.After, apID)
	}
	apjAssertNoLeak(t, row, *hook.WebhookToken)

	code, sched := apjCreateTriggerViaHandler(apID, map[string]any{"kind": "schedule", "cron_expression": "0 0 * * *"})
	if code != http.StatusCreated {
		t.Fatalf("schedule trigger create: %d", code)
	}
	row = apjAssertMemberRow(t, auditRowsFor(t, "autopilot.trigger.create", sched.ID), "autopilot.trigger.create", "autopilot_trigger")
	if row.After["kind"] != "schedule" || row.After["cron_expression"] != "0 0 * * *" {
		t.Errorf("after = %v; want schedule 0 0 * * *", row.After)
	}
}

func TestCreateAutopilotTrigger_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-create-fail-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	failAuditInsertForKind(t, "autopilot.trigger.create", testWorkspaceID)

	for _, body := range []map[string]any{
		{"kind": "webhook"},
		{"kind": "schedule", "cron_expression": "0 0 * * *"},
	} {
		if code, _ := apjCreateTriggerViaHandler(apID, body); code != http.StatusInternalServerError {
			t.Fatalf("%v trigger create with failing audit insert: %d; want 500", body["kind"], code)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM autopilot_trigger WHERE autopilot_id = $1`, apID); n != 0 {
		t.Fatalf("trigger rows = %d after failed audit; creates must roll back", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM autopilot_rule_version WHERE autopilot_id = $1`, apID); n != 0 {
		t.Fatalf("rule versions = %d after failed audit; the republish must roll back too", n)
	}
}

func apjScheduleTriggerFixture(t *testing.T, autopilotID string) string {
	t.Helper()
	return dbfx.Insert(t, "autopilot_trigger", testutil.Cols{
		"autopilot_id": autopilotID, "kind": "schedule", "enabled": true,
		"cron_expression": "0 0 * * *", "timezone": "UTC", "next_run_at": "2099-01-01T00:00:00Z",
	})
}

func apjPatchTrigger(apID, triggerID string, body map[string]any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := withURLParams(newRequest(http.MethodPatch, "/api/autopilots/"+apID+"/triggers/"+triggerID, body), "id", apID, "triggerId", triggerID)
	testHandler.UpdateAutopilotTrigger(w, req)
	return w
}

func TestUpdateAutopilotTrigger_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-update-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	triggerID := apjScheduleTriggerFixture(t, apID)

	if w := apjPatchTrigger(apID, triggerID, map[string]any{"enabled": false, "cron_expression": "5 4 * * *"}); w.Code != http.StatusOK {
		t.Fatalf("UpdateAutopilotTrigger: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.trigger.update", triggerID), "autopilot.trigger.update", "autopilot_trigger")
	if row.Before["enabled"] != true || row.After["enabled"] != false || row.After["cron_expression"] != "5 4 * * *" {
		t.Errorf("diff = %v -> %v; want enabled true->false and the new cron", row.Before, row.After)
	}
}

func TestUpdateAutopilotTrigger_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-update-fail-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	triggerID := apjScheduleTriggerFixture(t, apID)
	failAuditInsertFor(t, triggerID)

	if w := apjPatchTrigger(apID, triggerID, map[string]any{"enabled": false}); w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateAutopilotTrigger with failing audit insert: %d; want 500", w.Code)
	}
	var enabled bool
	dbfx.QueryRow(t, `SELECT enabled FROM autopilot_trigger WHERE id = $1`, triggerID).Scan(&enabled)
	if !enabled {
		t.Fatal("trigger disabled though the audit insert failed")
	}
}

func apjWebhookTriggerFixture(t *testing.T, autopilotID string) string {
	t.Helper()
	return dbfx.Insert(t, "autopilot_trigger", testutil.Cols{
		"autopilot_id": autopilotID, "kind": "webhook", "enabled": true,
		"webhook_token": "che2774-hook-token-" + uuid.NewString(), "signing_secret": "che2774-whsec-signing",
	})
}

func apjDeleteTrigger(apID, triggerID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := withURLParams(newRequest(http.MethodDelete, "/api/autopilots/"+apID+"/triggers/"+triggerID, nil), "id", apID, "triggerId", triggerID)
	testHandler.DeleteAutopilotTrigger(w, req)
	return w
}

func TestDeleteAutopilotTrigger_WritesAuditRowWithoutSecrets(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-delete-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	triggerID := apjWebhookTriggerFixture(t, apID)
	var token string
	dbfx.QueryRow(t, `SELECT webhook_token FROM autopilot_trigger WHERE id = $1`, triggerID).Scan(&token)

	if w := apjDeleteTrigger(apID, triggerID); w.Code != http.StatusNoContent {
		t.Fatalf("DeleteAutopilotTrigger: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "autopilot.trigger.delete", triggerID), "autopilot.trigger.delete", "autopilot_trigger")
	if row.Before["kind"] != "webhook" || row.Before["signing_secret"] != "changed" || row.Before["webhook_token"] != "changed" {
		t.Errorf("before = %v; want kind webhook with secrets reduced to the changed marker", row.Before)
	}
	apjAssertNoLeak(t, row, token, "che2774-whsec-signing")
}

func TestDeleteAutopilotTrigger_AuditInsertFailureKeepsTrigger(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createWebhookTestAgent(t, "che2774-trigger-delete-fail-agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	triggerID := apjScheduleTriggerFixture(t, apID)
	failAuditInsertFor(t, triggerID)

	if w := apjDeleteTrigger(apID, triggerID); w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteAutopilotTrigger with failing audit insert: %d; want 500", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM autopilot_trigger WHERE id = $1`, triggerID); n != 1 {
		t.Fatalf("trigger rows = %d after failed audit; the delete must roll back", n)
	}
}

// ---- projects -------------------------------------------------------------

func apjCreateProjectViaHandler(t *testing.T, title string, resources ...map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"title": title, "description": "secret project plan\nline two"}
	if len(resources) > 0 {
		body["resources"] = resources
	}
	w := httptest.NewRecorder()
	testHandler.CreateProject(w, newRequest(http.MethodPost, "/api/projects", body))
	var resp ProjectResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID != "" {
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM project_resource WHERE project_id = $1`, resp.ID)
			testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, resp.ID)
		})
	}
	return w.Code, resp.ID
}

func TestCreateProject_WritesAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, resources := range map[string][]map[string]any{
		"plain":   nil,
		"bundled": {{"resource_type": "github_repo", "resource_ref": map[string]any{"url": "https://github.com/che2774/bundled"}}},
	} {
		code, id := apjCreateProjectViaHandler(t, "che2774 project "+name, resources...)
		if code != http.StatusCreated {
			t.Fatalf("%s CreateProject: %d", name, code)
		}
		row := apjAssertMemberRow(t, auditRowsFor(t, "project.create", id), "project.create", "project")
		if row.After["title"] != "che2774 project "+name || len(row.Before) != 0 {
			t.Errorf("%s diff = %v -> %v", name, row.Before, row.After)
		}
		apjAssertNoLeak(t, row, "secret project plan", "che2774/bundled")
	}
}

func TestCreateProject_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	failAuditInsertForKind(t, "project.create", testWorkspaceID)
	for name, resources := range map[string][]map[string]any{
		"plain":   nil,
		"bundled": {{"resource_type": "github_repo", "resource_ref": map[string]any{"url": "https://github.com/che2774/bundled-fail"}}},
	} {
		title := "che2774 project vanish " + name
		if code, _ := apjCreateProjectViaHandler(t, title, resources...); code != http.StatusInternalServerError {
			t.Fatalf("%s CreateProject with failing audit insert: %d; want 500", name, code)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM project WHERE title = $1`, title); n != 0 {
			t.Fatalf("%s project rows = %d after failed audit; the create must roll back", name, n)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM project_resource WHERE resource_ref->>'url' = 'https://github.com/che2774/bundled-fail'`); n != 0 {
		t.Fatalf("project_resource rows = %d after failed audit", n)
	}
}

func apjDeleteProject(id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler.DeleteProject(w, withURLParam(newRequest(http.MethodDelete, "/api/projects/"+id, nil), "id", id))
	return w
}

func TestDeleteProject_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	id := dbfx.Project(t, "che2774 project delete", testutil.Cols{"description": "secret project plan"})
	if w := apjDeleteProject(id); w.Code != http.StatusNoContent {
		t.Fatalf("DeleteProject: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.delete", id), "project.delete", "project")
	if row.Before["title"] != "che2774 project delete" || len(row.After) != 0 {
		t.Errorf("diff = %v -> %v; want the deleted snapshot and no after", row.Before, row.After)
	}
	apjAssertNoLeak(t, row, "secret project plan")
}

func TestDeleteProject_AuditInsertFailureKeepsProject(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	id := dbfx.Project(t, "che2774 project delete fail")
	failAuditInsertFor(t, id)
	if w := apjDeleteProject(id); w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteProject with failing audit insert: %d; want 500", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM project WHERE id = $1`, id); n != 1 {
		t.Fatalf("project rows = %d after failed audit; the delete must roll back", n)
	}
}

func apjUpdateProject(id string, body map[string]any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler.UpdateProject(w, withURLParam(newRequest(http.MethodPut, "/api/projects/"+id, body), "id", id))
	return w
}

func TestUpdateProject_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	id := dbfx.Project(t, "che2774 project update", testutil.Cols{"description": "secret project plan"})
	if w := apjUpdateProject(id, map[string]any{"title": "che2774 project renamed", "description": "secret revised plan"}); w.Code != http.StatusOK {
		t.Fatalf("UpdateProject: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.update", id), "project.update", "project")
	if row.Before["title"] != "che2774 project update" || row.After["title"] != "che2774 project renamed" {
		t.Errorf("diff = %v -> %v; want the title change", row.Before, row.After)
	}
	apjAssertNoLeak(t, row, "secret project plan")
	apjAssertNoLeak(t, row, "secret revised plan")
}

func TestUpdateProject_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	id := dbfx.Project(t, "che2774 project update fail")
	failAuditInsertFor(t, id)
	w := apjUpdateProject(id, map[string]any{"title": "che2774 should not stick"})
	if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") == "" {
		t.Fatalf("UpdateProject with failing audit insert: %d %s; want retryable 500", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM project WHERE id = $1 AND title = 'che2774 project update fail'`, id); n != 1 {
		t.Fatalf("project title changed despite failed audit; the update must roll back")
	}
}

// ---- project workflow -----------------------------------------------------

func apjSwitchProjectWorkflow(projectID string, workflowID any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := withURLParam(workflowRequest(http.MethodPost, "/api/projects/"+projectID+"/workflow", map[string]any{"workflow_id": workflowID}), "id", projectID)
	testHandler.SetProjectWorkflow(w, req)
	return w
}

func TestSetProjectWorkflow_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflow(t, "che2774 wf "+workflowTestSuffix())
	projectID := createWorkflowTestProject(t, "che2774 workflow project")

	if w := apjSwitchProjectWorkflow(projectID, wf.ID); w.Code != http.StatusOK {
		t.Fatalf("SetProjectWorkflow: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.workflow.update", projectID), "project.workflow.update", "project")
	if row.After["workflow_id"] != wf.ID {
		t.Errorf("after = %v; want workflow_id %s", row.After, wf.ID)
	}
}

func TestSetProjectWorkflow_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wf := createTestWorkflow(t, "che2774 wf fail "+workflowTestSuffix())
	projectID := createWorkflowTestProject(t, "che2774 workflow project fail")
	failAuditInsertFor(t, projectID)

	if w := apjSwitchProjectWorkflow(projectID, wf.ID); w.Code != http.StatusInternalServerError {
		t.Fatalf("SetProjectWorkflow with failing audit insert: %d; want 500", w.Code)
	}
	var workflowID *string
	dbfx.QueryRow(t, `SELECT workflow_id::text FROM project WHERE id = $1`, projectID).Scan(&workflowID)
	if workflowID != nil {
		t.Fatalf("project workflow_id = %s after failed audit; the switch must roll back", *workflowID)
	}
}

// ---- project resources ----------------------------------------------------

func apjCreateResource(projectID, url string) (int, ProjectResourceResponse) {
	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodPost, "/api/projects/"+projectID+"/resources", map[string]any{
		"resource_type": "github_repo", "resource_ref": map[string]any{"url": url}, "label": "che2774 label",
	}), "id", projectID)
	testHandler.CreateProjectResource(w, req)
	var resp ProjectResourceResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func apjResourceRequest(method, projectID, resourceID string, body any) *http.Request {
	req := newRequest(method, "/api/projects/"+projectID+"/resources/"+resourceID, body)
	return withURLParams(req, "id", projectID, "resourceId", resourceID)
}

func apjResourceFixture(t *testing.T, projectID, url string) string {
	t.Helper()
	return dbfx.Insert(t, "project_resource", testutil.Cols{
		"project_id": projectID, "workspace_id": testWorkspaceID, "resource_type": "github_repo",
		"resource_ref": `{"url":"` + url + `"}`, "label": "che2774 label",
	})
}

func TestCreateProjectResource_WritesAuditRowWithoutRef(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource create")
	code, created := apjCreateResource(projectID, "https://github.com/che2774/created")
	if code != http.StatusCreated {
		t.Fatalf("CreateProjectResource: %d", code)
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.resource.create", created.ID), "project.resource.create", "project_resource")
	if row.After["resource_type"] != "github_repo" || row.After["project_id"] != projectID {
		t.Errorf("after = %v; want github_repo on project %s", row.After, projectID)
	}
	apjAssertNoLeak(t, row, "che2774/created")
}

func TestCreateProjectResource_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource create fail")
	failAuditInsertForKind(t, "project.resource.create", testWorkspaceID)
	if code, _ := apjCreateResource(projectID, "https://github.com/che2774/create-fail"); code != http.StatusInternalServerError {
		t.Fatalf("CreateProjectResource with failing audit insert: %d; want 500", code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM project_resource WHERE project_id = $1`, projectID); n != 0 {
		t.Fatalf("project_resource rows = %d after failed audit; the create must roll back", n)
	}
}

func TestUpdateProjectResource_WritesAuditRowWithoutRef(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource update")
	resourceID := apjResourceFixture(t, projectID, "https://github.com/che2774/before")

	w := httptest.NewRecorder()
	testHandler.UpdateProjectResource(w, apjResourceRequest(http.MethodPut, projectID, resourceID, map[string]any{
		"resource_ref": map[string]any{"url": "https://github.com/che2774/after"}, "label": "che2774 renamed", "position": 3,
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateProjectResource: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.resource.update", resourceID), "project.resource.update", "project_resource")
	if row.After["label"] != "che2774 renamed" || row.After["position"] != float64(3) || row.After["resource_ref"] != "changed" {
		t.Errorf("after = %v; want label, position and a changed resource_ref marker", row.After)
	}
	apjAssertNoLeak(t, row, "che2774/before", "che2774/after")
}

func TestUpdateProjectResource_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource update fail")
	resourceID := apjResourceFixture(t, projectID, "https://github.com/che2774/keep")
	failAuditInsertFor(t, resourceID)

	w := httptest.NewRecorder()
	testHandler.UpdateProjectResource(w, apjResourceRequest(http.MethodPut, projectID, resourceID, map[string]any{"label": "must-not-stick"}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdateProjectResource with failing audit insert: %d; want 500", w.Code)
	}
	var label string
	dbfx.QueryRow(t, `SELECT label FROM project_resource WHERE id = $1`, resourceID).Scan(&label)
	if label != "che2774 label" {
		t.Fatalf("label = %q after failed audit; the update must roll back", label)
	}
}

func TestDeleteProjectResource_WritesAuditRowWithoutRef(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource delete")
	resourceID := apjResourceFixture(t, projectID, "https://github.com/che2774/deleted")

	w := httptest.NewRecorder()
	testHandler.DeleteProjectResource(w, apjResourceRequest(http.MethodDelete, projectID, resourceID, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteProjectResource: %d %s", w.Code, w.Body.String())
	}
	row := apjAssertMemberRow(t, auditRowsFor(t, "project.resource.delete", resourceID), "project.resource.delete", "project_resource")
	if row.Before["resource_type"] != "github_repo" || len(row.After) != 0 {
		t.Errorf("diff = %v -> %v; want the deleted snapshot and no after", row.Before, row.After)
	}
	apjAssertNoLeak(t, row, "che2774/deleted")
}

func TestDeleteProjectResource_AuditInsertFailureKeepsResource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "che2774 resource delete fail")
	resourceID := apjResourceFixture(t, projectID, "https://github.com/che2774/delete-fail")
	failAuditInsertFor(t, resourceID)

	w := httptest.NewRecorder()
	testHandler.DeleteProjectResource(w, apjResourceRequest(http.MethodDelete, projectID, resourceID, nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteProjectResource with failing audit insert: %d; want 500", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM project_resource WHERE id = $1`, resourceID); n != 1 {
		t.Fatalf("project_resource rows = %d after failed audit; the delete must roll back", n)
	}
}
