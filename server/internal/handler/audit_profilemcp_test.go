package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2774 L5a: runtime profiles, workspace MCP servers, agent create, runtime
// rename and Qoder stop/disconnect write one audit row in the mutation's tx.

// pmFailAuditKind makes every audit insert of one kind in one workspace
// fail with a real database error. Creates have no target id to key on before
// the insert, so the trigger matches on kind + workspace instead.
func pmFailAuditKind(t *testing.T, kind, workspaceID string) {
	t.Helper()
	name := "audit_fail_kind_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := auditExec(t, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind = '%[2]s' AND NEW.workspace_id = '%[3]s' THEN RAISE EXCEPTION 'forced audit insert failure'; END IF;
			RETURN NEW;
		END $$`, name, kind, workspaceID)); err != nil {
		t.Fatalf("create failing function: %v", err)
	}
	if err := auditExec(t, fmt.Sprintf(
		`CREATE TRIGGER %[1]s BEFORE INSERT ON workspace_audit_event FOR EACH ROW EXECUTE FUNCTION %[1]s()`, name)); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON workspace_audit_event`, name))
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %[1]s()`, name))
	})
}

func pmRequireRetryable500(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%s with failing audit insert: %d %s; want retryable 500", what, w.Code, w.Body.String())
	}
}

func pmRequireNoLeak(t *testing.T, row auditRow, secrets ...string) {
	t.Helper()
	text := auditRowText(row)
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("audit row leaked %q: %s", secret, text)
		}
	}
}

// ── runtime profiles ─────────────────────────────────────────────────────

func pmProfileRequest(method, profileID string, body any) *http.Request {
	path := "/api/workspaces/" + testWorkspaceID + "/runtime-profiles"
	if profileID == "" {
		return withURLParam(newRequest(method, path, body), "id", testWorkspaceID)
	}
	return withURLParams(newRequest(method, path+"/"+profileID, body), "id", testWorkspaceID, "profileId", profileID)
}

func pmCreateProfile(t *testing.T, displayName string) string {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.CreateRuntimeProfile(w, pmProfileRequest(http.MethodPost, "", map[string]any{
		"display_name": displayName, "runtime_type": "codex", "command_name": "audit-codex",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create profile fixture: %d %s", w.Code, w.Body.String())
	}
	var resp RuntimeProfileResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM runtime_profile WHERE id = $1`, resp.ID) })
	return resp.ID
}

func TestCreateRuntimeProfile_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-profile-" + uuid.NewString()[:8]
	w := httptest.NewRecorder()
	testHandler.CreateRuntimeProfile(w, pmProfileRequest(http.MethodPost, "", map[string]any{
		"display_name": name, "runtime_type": "codex", "command_name": "audit-codex",
		"description": "internal wrapper", "fixed_args": []string{"--api-key=profile-secret-arg"},
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateRuntimeProfile: %d %s", w.Code, w.Body.String())
	}
	var resp RuntimeProfileResponse
	json.NewDecoder(w.Body).Decode(&resp)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM runtime_profile WHERE id = $1`, resp.ID) })

	rows := auditRowsFor(t, "runtime_profile.create", resp.ID)
	if len(rows) != 1 {
		t.Fatalf("runtime_profile.create audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "runtime_profile" {
		t.Errorf("row = %+v; want member %s on runtime_profile", row, testUserID)
	}
	if len(row.Before) != 0 || row.After["display_name"] != name {
		t.Errorf("before/after = %v / %v; want empty before and the name after", row.Before, row.After)
	}
	if row.After["fixed_args"] != "changed" {
		t.Errorf("fixed_args = %v; want the changed marker", row.After["fixed_args"])
	}
	pmRequireNoLeak(t, row, "profile-secret-arg", "internal wrapper")
}

func TestCreateRuntimeProfile_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-profile-fail-" + uuid.NewString()[:8]
	pmFailAuditKind(t, "runtime_profile.create", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateRuntimeProfile(w, pmProfileRequest(http.MethodPost, "", map[string]any{
		"display_name": name, "runtime_type": "codex", "command_name": "audit-codex",
	}))
	pmRequireRetryable500(t, w, "CreateRuntimeProfile")
	if got := dbfx.Count(t, `SELECT count(*) FROM runtime_profile WHERE display_name = $1`, name); got != 0 {
		t.Fatalf("profiles named %q = %d after failed audit; the create must roll back", name, got)
	}
}

func TestUpdateRuntimeProfile_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-update-"+uuid.NewString()[:8])
	w := httptest.NewRecorder()
	testHandler.UpdateRuntimeProfile(w, pmProfileRequest(http.MethodPatch, profileID, map[string]any{
		"command_name": "audit-codex-2", "enabled": false, "fixed_args": []string{"--token=update-secret-arg"},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateRuntimeProfile: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "runtime_profile.update", profileID)
	if len(rows) != 1 {
		t.Fatalf("runtime_profile.update audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.TargetType != "runtime_profile" {
		t.Errorf("row = %+v; want member on runtime_profile", row)
	}
	if row.Before["command_name"] != "audit-codex" || row.After["command_name"] != "audit-codex-2" {
		t.Errorf("command_name diff = %v -> %v", row.Before["command_name"], row.After["command_name"])
	}
	if row.Before["enabled"] != true || row.After["enabled"] != false {
		t.Errorf("enabled diff = %v -> %v", row.Before["enabled"], row.After["enabled"])
	}
	if _, unchanged := row.After["display_name"]; unchanged {
		t.Errorf("unchanged display_name must not appear: %v", row.After)
	}
	pmRequireNoLeak(t, row, "update-secret-arg")
}

func TestUpdateRuntimeProfile_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-update-fail-"+uuid.NewString()[:8])
	failAuditInsertFor(t, profileID)
	w := httptest.NewRecorder()
	testHandler.UpdateRuntimeProfile(w, pmProfileRequest(http.MethodPatch, profileID, map[string]any{"command_name": "must-not-stick"}))
	pmRequireRetryable500(t, w, "UpdateRuntimeProfile")
	var command string
	dbfx.QueryRow(t, `SELECT command_name FROM runtime_profile WHERE id = $1`, profileID).Scan(&command)
	if command != "audit-codex" {
		t.Fatalf("command_name = %q after failed audit; the update must roll back", command)
	}
}

func TestDeleteRuntimeProfile_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-delete-"+uuid.NewString()[:8])
	w := httptest.NewRecorder()
	testHandler.DeleteRuntimeProfile(w, pmProfileRequest(http.MethodDelete, profileID, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteRuntimeProfile: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "runtime_profile.delete", profileID)
	if len(rows) != 1 {
		t.Fatalf("runtime_profile.delete audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.TargetType != "runtime_profile" || row.Before["command_name"] != "audit-codex" {
		t.Errorf("row = %+v; want member delete of the audit-codex profile", row)
	}
}

func TestDeleteRuntimeProfile_AuditInsertFailureKeepsProfile(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-delete-fail-"+uuid.NewString()[:8])
	failAuditInsertFor(t, profileID)
	w := httptest.NewRecorder()
	testHandler.DeleteRuntimeProfile(w, pmProfileRequest(http.MethodDelete, profileID, nil))
	pmRequireRetryable500(t, w, "DeleteRuntimeProfile")
	if got := dbfx.Count(t, `SELECT count(*) FROM runtime_profile WHERE id = $1`, profileID); got != 1 {
		t.Fatalf("profile rows = %d after failed audit; the delete must roll back", got)
	}
}

// ── workspace MCP servers ────────────────────────────────────────────────

// pmMcpParams sets both the workspace id and the server id on a request.
func pmMcpParams(req *http.Request, serverID string) *http.Request {
	return withURLParams(req, "id", testWorkspaceID, "serverId", serverID)
}

func TestCreateWorkspaceMcpServer_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-mcp-" + uuid.NewString()[:8]
	w := httptest.NewRecorder()
	testHandler.CreateWorkspaceMcpServer(w, withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/mcp-servers", map[string]any{
		"name": name, "config": map[string]any{"url": "https://user:mcp-secret-token@mcp.example/sse"},
	}), "id", testWorkspaceID))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateWorkspaceMcpServer: %d %s", w.Code, w.Body.String())
	}
	var resp WorkspaceMcpServerResponse
	json.NewDecoder(w.Body).Decode(&resp)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace_mcp_server WHERE id = $1`, resp.ID) })

	rows := auditRowsFor(t, "mcp_server.create", resp.ID)
	if len(rows) != 1 {
		t.Fatalf("mcp_server.create audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "workspace_mcp_server" {
		t.Errorf("row = %+v; want member %s on workspace_mcp_server", row, testUserID)
	}
	if row.After["name"] != name || row.After["config"] != "changed" {
		t.Errorf("after = %v; want the name and a changed marker for config", row.After)
	}
	pmRequireNoLeak(t, row, "mcp-secret-token", "mcp.example")
}

func TestCreateWorkspaceMcpServer_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-mcp-fail-" + uuid.NewString()[:8]
	pmFailAuditKind(t, "mcp_server.create", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateWorkspaceMcpServer(w, withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/mcp-servers", map[string]any{
		"name": name, "config": map[string]any{"url": "https://mcp.example"},
	}), "id", testWorkspaceID))
	pmRequireRetryable500(t, w, "CreateWorkspaceMcpServer")
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_mcp_server WHERE name = $1`, name); got != 0 {
		t.Fatalf("mcp servers named %q = %d after failed audit; the create must roll back", name, got)
	}
}

func TestUpdateWorkspaceMcpServer_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	serverID := createWorkspaceMcpServerForTest(t, "che2774-mcp-update", `{"url":"https://old.example"}`)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspaceMcpServer(w, pmMcpParams(newRequest(http.MethodPut, "/x", map[string]any{
		"name": "che2774-mcp-renamed", "config": map[string]any{"command": "npx", "env": map[string]any{"TOKEN": "mcp-env-secret"}},
	}), serverID))
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateWorkspaceMcpServer: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "mcp_server.update", serverID)
	if len(rows) != 1 {
		t.Fatalf("mcp_server.update audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.TargetType != "workspace_mcp_server" {
		t.Errorf("row = %+v; want member on workspace_mcp_server", row)
	}
	if row.Before["name"] != "che2774-mcp-update" || row.After["name"] != "che2774-mcp-renamed" {
		t.Errorf("name diff = %v -> %v", row.Before["name"], row.After["name"])
	}
	if row.Before["transport"] != "http" || row.After["transport"] != "stdio" {
		t.Errorf("transport diff = %v -> %v", row.Before["transport"], row.After["transport"])
	}
	pmRequireNoLeak(t, row, "mcp-env-secret", "old.example", "npx")
}

func TestUpdateWorkspaceMcpServer_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	serverID := createWorkspaceMcpServerForTest(t, "che2774-mcp-update-fail", `{"url":"https://old.example"}`)
	failAuditInsertFor(t, serverID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspaceMcpServer(w, pmMcpParams(newRequest(http.MethodPut, "/x", map[string]any{
		"name": "must-not-stick",
	}), serverID))
	pmRequireRetryable500(t, w, "UpdateWorkspaceMcpServer")
	var name string
	dbfx.QueryRow(t, `SELECT name FROM workspace_mcp_server WHERE id = $1`, serverID).Scan(&name)
	if name != "che2774-mcp-update-fail" {
		t.Fatalf("name = %q after failed audit; the update must roll back", name)
	}
}

func TestDeleteWorkspaceMcpServer_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	serverID := createWorkspaceMcpServerForTest(t, "che2774-mcp-delete", `{"url":"https://gone.example"}`)
	w := httptest.NewRecorder()
	testHandler.DeleteWorkspaceMcpServer(w, pmMcpParams(newRequest(http.MethodDelete, "/x", nil), serverID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspaceMcpServer: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "mcp_server.delete", serverID)
	if len(rows) != 1 {
		t.Fatalf("mcp_server.delete audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.TargetType != "workspace_mcp_server" || row.Before["name"] != "che2774-mcp-delete" {
		t.Errorf("row = %+v; want member delete of che2774-mcp-delete", row)
	}
	pmRequireNoLeak(t, row, "gone.example")
}

func TestDeleteWorkspaceMcpServer_AuditInsertFailureKeepsServer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	serverID := createWorkspaceMcpServerForTest(t, "che2774-mcp-delete-fail", `{"url":"https://keep.example"}`)
	failAuditInsertFor(t, serverID)
	w := httptest.NewRecorder()
	testHandler.DeleteWorkspaceMcpServer(w, pmMcpParams(newRequest(http.MethodDelete, "/x", nil), serverID))
	pmRequireRetryable500(t, w, "DeleteWorkspaceMcpServer")
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_mcp_server WHERE id = $1`, serverID); got != 1 {
		t.Fatalf("mcp server rows = %d after failed audit; the delete must roll back", got)
	}
}

// ── agent create ─────────────────────────────────────────────────────────

func TestCreateAgent_WritesAuditRowWithRedaction(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-create-agent-" + uuid.NewString()[:8]
	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, newRequest(http.MethodPost, "/api/agents?workspace_id="+testWorkspaceID, map[string]any{
		"name": name, "runtime_id": handlerTestRuntimeID(t), "model": "audit-model",
		"instructions": "never leak this line\nor this one",
		"custom_env":   map[string]string{"API_KEY": "agent-env-secret"},
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAgent: %d %s", w.Code, w.Body.String())
	}
	var resp AgentResponse
	json.NewDecoder(w.Body).Decode(&resp)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, resp.ID) })

	rows := auditRowsFor(t, "agent.create", resp.ID)
	if len(rows) != 1 {
		t.Fatalf("agent.create audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "agent" {
		t.Errorf("row = %+v; want member %s on agent", row, testUserID)
	}
	if len(row.Before) != 0 || row.After["name"] != name || row.After["model"] != "audit-model" {
		t.Errorf("before/after = %v / %v; want empty before, name and model after", row.Before, row.After)
	}
	if ins, _ := row.After["instructions"].(map[string]any); ins == nil || ins["lines"] != float64(2) {
		t.Errorf("instructions must be a 2-line digest, got %v", row.After["instructions"])
	}
	pmRequireNoLeak(t, row, "never leak this line", "agent-env-secret")
}

func TestCreateAgent_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	name := "che2774-create-agent-fail-" + uuid.NewString()[:8]
	pmFailAuditKind(t, "agent.create", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, newRequest(http.MethodPost, "/api/agents?workspace_id="+testWorkspaceID, map[string]any{
		"name": name, "runtime_id": handlerTestRuntimeID(t),
	}))
	pmRequireRetryable500(t, w, "CreateAgent")
	if got := dbfx.Count(t, `SELECT count(*) FROM agent WHERE name = $1`, name); got != 0 {
		t.Fatalf("agents named %q = %d after failed audit; the create must roll back", name, got)
	}
}

// ── runtime rename ───────────────────────────────────────────────────────

func TestUpdateAgentRuntime_RenameWritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID, ownerID, _ := runtimeVisibilityFixture(t)
	w := patchRuntimeCustomName(ownerID, runtimeID, map[string]any{"custom_name": "Prod Box"})
	if w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "runtime.rename", runtimeID)
	if len(rows) != 1 {
		t.Fatalf("runtime.rename audit rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != "member" || row.ActorID != ownerID || row.TargetType != "runtime" {
		t.Errorf("row = %+v; want member %s on runtime", row, ownerID)
	}
	if row.After["custom_name"] != "Prod Box" {
		t.Errorf("after = %v; want custom_name Prod Box", row.After)
	}

	// Clearing the name records the previous value.
	w = patchRuntimeCustomName(ownerID, runtimeID, map[string]any{"custom_name": "  "})
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	rows = auditRowsFor(t, "runtime.rename", runtimeID)
	if len(rows) != 2 || rows[1].Before["custom_name"] != "Prod Box" || rows[1].After["custom_name"] != nil {
		t.Fatalf("rows after clear = %+v; want a second row Prod Box -> null", rows)
	}
}

func TestUpdateAgentRuntime_RenameAuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID, ownerID, _ := runtimeVisibilityFixture(t)
	failAuditInsertFor(t, runtimeID)
	w := patchRuntimeCustomName(ownerID, runtimeID, map[string]any{"custom_name": "must-not-stick"})
	pmRequireRetryable500(t, w, "rename")
	var name *string
	dbfx.QueryRow(t, `SELECT custom_name FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&name)
	if name != nil {
		t.Fatalf("custom_name = %q after failed audit; the rename must roll back", *name)
	}
}

func TestUpdateAgentRuntime_MachineRenameWritesAuditRowAndRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, ownerID, _ := runtimeVisibilityFixture(t)
	daemonID := "che2774-rename-daemon-" + uuid.NewString()[:8]
	makeRuntime := func(provider string) string {
		return dbfx.Runtime(t, provider+" (host)", testutil.Cols{
			"daemon_id": daemonID, "provider": provider, "runtime_mode": "local", "owner_id": ownerID,
		})
	}
	idA, idB := makeRuntime("ccn_a"), makeRuntime("ccn_b")

	failAuditInsertFor(t, idA)
	w := patchRuntimeCustomName(ownerID, idA, map[string]any{"custom_name": "Machine", "apply_to_machine": true})
	pmRequireRetryable500(t, w, "machine rename")
	for _, id := range []string{idA, idB} {
		var name *string
		dbfx.QueryRow(t, `SELECT custom_name FROM agent_runtime WHERE id = $1`, id).Scan(&name)
		if name != nil {
			t.Fatalf("runtime %s custom_name = %q after failed audit; the whole fan-out must roll back", id, *name)
		}
	}
}

func TestUpdateAgentRuntime_MachineRenameRecordsCount(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, ownerID, _ := runtimeVisibilityFixture(t)
	daemonID := "che2774-rename-daemon-" + uuid.NewString()[:8]
	makeRuntime := func(provider string) string {
		return dbfx.Runtime(t, provider+" (host)", testutil.Cols{
			"daemon_id": daemonID, "provider": provider, "runtime_mode": "local", "owner_id": ownerID,
		})
	}
	idA := makeRuntime("ccn_a")
	makeRuntime("ccn_b")

	w := patchRuntimeCustomName(ownerID, idA, map[string]any{"custom_name": "Machine", "apply_to_machine": true})
	if w.Code != http.StatusOK {
		t.Fatalf("machine rename: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "runtime.rename", idA)
	if len(rows) != 1 {
		t.Fatalf("runtime.rename audit rows = %d; want 1", len(rows))
	}
	if rows[0].After["custom_name"] != "Machine" || rows[0].After["runtimes_renamed"] != float64(2) {
		t.Errorf("after = %v; want custom_name Machine and runtimes_renamed 2", rows[0].After)
	}
}

// ── Qoder stop and disconnect ────────────────────────────────────────────

func pmConnectQoder(t *testing.T) {
	t.Helper()
	withVCSBox(t)
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1)`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1`, testWorkspaceID)
	})
	input := map[string]any{"name": "QCA", "base_url": qoderAPIURL, "environment_id": "env_test", "qoder_token": "qoder-secret-token"}
	testutil.Call(t, testHandler.SaveQoderConnection, vcsHandlerRequest("PUT", "/qoder", input, "")).Want(200)
}

func TestStopQoderConnection_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	before := len(auditRowsFor(t, "qoder.stop", testWorkspaceID))
	testutil.Call(t, testHandler.StopQoderConnection, vcsHandlerRequest("POST", "/qoder/stop", nil, "")).Want(200)
	rows := auditRowsFor(t, "qoder.stop", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("qoder.stop audit rows = %d; want %d", len(rows), before+1)
	}
	row := rows[len(rows)-1]
	if row.ActorType != "member" || row.ActorID != testUserID || row.TargetType != "qoder_connection" {
		t.Errorf("row = %+v; want member %s on qoder_connection", row, testUserID)
	}
	if row.After["status"] != "stopped" {
		t.Errorf("after = %v; want status stopped", row.After)
	}
	pmRequireNoLeak(t, row, "qoder-secret-token")
}

func TestStopQoderConnection_AuditInsertFailureKeepsConnectionRunning(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	pmFailAuditKind(t, "qoder.stop", testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.StopQoderConnection(w, vcsHandlerRequest("POST", "/qoder/stop", nil, ""))
	pmRequireRetryable500(t, w, "StopQoderConnection")
	var enabled bool
	var tokens int
	dbfx.QueryRow(t, `SELECT enabled FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&enabled)
	dbfx.QueryRow(t, `SELECT count(*) FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1)`, testWorkspaceID).Scan(&tokens)
	if !enabled || tokens != 1 {
		t.Fatalf("enabled=%v tokens=%d after failed audit; the stop and the credential revoke must roll back", enabled, tokens)
	}
}

func pmDisconnectQoderRequest() *http.Request {
	return vcsHandlerRequest("DELETE", "/qoder", map[string]any{"expected_active_agent_ids": []string{}}, "")
}

func TestDisconnectQoderConnection_WritesAuditRows(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	runtimeID := dbfx.Runtime(t, "QCA", testutil.Cols{"provider": "qoder_cloud", "runtime_mode": "cloud"})
	stopsBefore := len(auditRowsFor(t, "qoder.stop", testWorkspaceID))
	disconnectsBefore := len(auditRowsFor(t, "qoder.disconnect", testWorkspaceID))

	testutil.Call(t, testHandler.DisconnectQoderConnection, pmDisconnectQoderRequest()).Want(200)

	if got := len(auditRowsFor(t, "qoder.stop", testWorkspaceID)); got != stopsBefore+1 {
		t.Errorf("qoder.stop rows = %d; want %d (disconnect stops the connection first)", got, stopsBefore+1)
	}
	rows := auditRowsFor(t, "qoder.disconnect", testWorkspaceID)
	if len(rows) != disconnectsBefore+1 {
		t.Fatalf("qoder.disconnect audit rows = %d; want %d", len(rows), disconnectsBefore+1)
	}
	row := rows[len(rows)-1]
	if row.ActorType != "member" || row.TargetType != "qoder_connection" || row.After["runtimes_removed"] != float64(1) {
		t.Errorf("row = %+v; want member disconnect removing 1 runtime", row)
	}
	pmRequireNoLeak(t, row, "qoder-secret-token")
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_runtime WHERE id = $1`, runtimeID); got != 0 {
		t.Errorf("runtime rows = %d; want the Qoder runtime removed", got)
	}
}

func TestDisconnectQoderConnection_RecordsRuntimeGoneOutsideStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	runtimeID := dbfx.Runtime(t, "QCA", testutil.Cols{"provider": "qoder_cloud", "runtime_mode": "cloud"})
	notifier := &recordingRuntimeGoneNotifier{}
	h := *testHandler
	h.DaemonRuntimeGone = notifier
	before := len(auditRowsFor(t, "qoder.disconnect", testWorkspaceID))

	testutil.Call(t, h.DisconnectQoderConnection, pmDisconnectQoderRequest()).Want(200)

	rows := auditRowsFor(t, "qoder.disconnect", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("qoder.disconnect audit rows = %d; want %d", len(rows), before+1)
	}
	if got := rows[len(rows)-1].External; len(got) != 1 || got[0] != "runtime_gone_notice" {
		t.Errorf("external_requested = %v; want [runtime_gone_notice]", got)
	}
	if len(notifier.runtimeIDs) != 1 || notifier.runtimeIDs[0] != runtimeID {
		t.Errorf("runtime-gone notifications = %v; the outside step must still run inline", notifier.runtimeIDs)
	}
}

func TestDisconnectQoderConnection_NoRuntimesRecordsNoOutsideStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	before := len(auditRowsFor(t, "qoder.disconnect", testWorkspaceID))

	testutil.Call(t, testHandler.DisconnectQoderConnection, pmDisconnectQoderRequest()).Want(200)

	rows := auditRowsFor(t, "qoder.disconnect", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("qoder.disconnect audit rows = %d; want %d", len(rows), before+1)
	}
	if got := rows[len(rows)-1].External; len(got) != 0 {
		t.Errorf("external_requested = %v; want empty when no runtime was removed", got)
	}
}

func TestDisconnectQoderConnection_AuditInsertFailureKeepsConnection(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pmConnectQoder(t)
	runtimeID := dbfx.Runtime(t, "QCA", testutil.Cols{"provider": "qoder_cloud", "runtime_mode": "cloud"})
	pmFailAuditKind(t, "qoder.disconnect", testWorkspaceID)

	w := httptest.NewRecorder()
	testHandler.DisconnectQoderConnection(w, pmDisconnectQoderRequest())
	pmRequireRetryable500(t, w, "DisconnectQoderConnection")
	if got := dbfx.Count(t, `SELECT count(*) FROM cloud_agent_connection WHERE provider='qoder_cloud' AND workspace_id=$1`, testWorkspaceID); got != 1 {
		t.Fatalf("connection rows = %d after failed audit; the disconnect must roll back", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_runtime WHERE id = $1`, runtimeID); got != 1 {
		t.Fatalf("runtime rows = %d after failed audit; the teardown must roll back", got)
	}
}

type recordingProfileRefreshNotifier struct{ profileIDs []string }

func (n *recordingProfileRefreshNotifier) NotifyRuntimeProfilesChanged(_, profileID string) {
	n.profileIDs = append(n.profileIDs, profileID)
}

func TestDeleteRuntimeProfile_RecordsOutsideStepsAndStillRunsThem(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-ext-"+uuid.NewString()[:8])
	runtimeID := insertProfileRuntimeFixture(t, context.Background(), profileID, "che2774-profile-ext-rt", "codex")
	gone := &recordingRuntimeGoneNotifier{}
	refresh := &recordingProfileRefreshNotifier{}
	h := *testHandler
	h.DaemonRuntimeGone, h.DaemonProfileRefresh = gone, refresh

	w := httptest.NewRecorder()
	h.DeleteRuntimeProfile(w, pmProfileRequest(http.MethodDelete, profileID, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteRuntimeProfile: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "runtime_profile.delete", profileID)
	if len(rows) != 1 {
		t.Fatalf("runtime_profile.delete audit rows = %d; want 1", len(rows))
	}
	got := append([]string(nil), rows[0].External...)
	slices.Sort(got)
	if want := []string{"daemon_profile_refresh", "runtime_gone_notice"}; !slices.Equal(got, want) {
		t.Errorf("external_requested = %v; want %v", got, want)
	}
	if len(gone.runtimeIDs) != 1 || gone.runtimeIDs[0] != runtimeID || len(refresh.profileIDs) != 1 {
		t.Errorf("outside steps must still run inline: gone=%v refresh=%v", gone.runtimeIDs, refresh.profileIDs)
	}
}

func TestDeleteRuntimeProfile_AuditInsertFailureSkipsOutsideSteps(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	profileID := pmCreateProfile(t, "che2774-profile-ext-fail-"+uuid.NewString()[:8])
	insertProfileRuntimeFixture(t, context.Background(), profileID, "che2774-profile-ext-fail-rt", "codex")
	failAuditInsertFor(t, profileID)
	gone := &recordingRuntimeGoneNotifier{}
	refresh := &recordingProfileRefreshNotifier{}
	h := *testHandler
	h.DaemonRuntimeGone, h.DaemonProfileRefresh = gone, refresh

	w := httptest.NewRecorder()
	h.DeleteRuntimeProfile(w, pmProfileRequest(http.MethodDelete, profileID, nil))
	pmRequireRetryable500(t, w, "DeleteRuntimeProfile")
	if len(gone.runtimeIDs) != 0 || len(refresh.profileIDs) != 0 {
		t.Errorf("outside steps ran though nothing committed: gone=%v refresh=%v", gone.runtimeIDs, refresh.profileIDs)
	}
}

// A PATCH that changes access and renames is one mutation: if the rename's
// audit insert fails, the access change must roll back with it.
func TestUpdateAgentRuntime_CombinedPatchAuditFailureRollsBackBoth(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID, ownerID, _ := runtimeVisibilityFixture(t)
	pmFailAuditKind(t, "runtime.rename", testWorkspaceID)
	w := patchRuntimeCustomName(ownerID, runtimeID, map[string]any{"visibility": "public", "custom_name": "must-not-stick"})
	pmRequireRetryable500(t, w, "combined PATCH")
	var visibility string
	var name *string
	dbfx.QueryRow(t, `SELECT visibility, custom_name FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&visibility, &name)
	if visibility != "private" || name != nil {
		t.Fatalf("visibility=%q custom_name=%v after a failed audit; want private and unset", visibility, name)
	}
	if rows := auditRowsFor(t, "runtime.access.update", runtimeID); len(rows) != 0 {
		t.Fatalf("access audit rows = %d for a rolled-back mutation; want 0", len(rows))
	}
}
