package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// CHE-455: agent actors — mat_ task-scoped tokens, or the in-process
// X-Agent-ID/X-Task-ID fallback resolveActor also trusts — must never
// persist a change to a governed instruction field, even when the agent's
// owning human holds an admin/owner workspace role. This file exercises
// every confirmed write site with the same actor-simulation convention as
// agent_env_permission_test.go: set X-Agent-ID + X-Task-ID directly on a
// request authenticated as the agent's owning human.

// asAgentActor decorates a human-authored request so it resolves as an
// agent actor via resolveActor's in-process fallback path.
func asAgentActor(req *http.Request, hostAgentID, hostTaskID string) *http.Request {
	req.Header.Set("X-Agent-ID", hostAgentID)
	req.Header.Set("X-Task-ID", hostTaskID)
	return req
}

// governedInstructionActorFixture creates a plain member and a task-bound
// agent owned by that member, giving tests both a human identity to
// authenticate as and an agent identity to project onto the request.
func governedInstructionActorFixture(t *testing.T, email, hostAgentName string) (ownerUserID, hostAgentID, hostTaskID string) {
	t.Helper()
	ownerUserID = createPlainMember(t, email)
	hostAgentID = createHandlerTestAgent(t, hostAgentName, nil)
	hostTaskID = createHandlerTestTaskForAgent(t, hostAgentID)
	return ownerUserID, hostAgentID, hostTaskID
}

// ── UpdateAgent (site 1) ─────────────────────────────────────────────────

func TestUpdateAgent_AgentActorForbiddenFromInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che455-update-agent-host@multica.test", "che455-update-agent-host")

	targetAgentID := createHandlerTestAgent(t, "che455-update-agent-target", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, ownerUserID, targetAgentID)
	// Owning human is also a workspace owner/admin in this fixture set (testUserID
	// promoted separately below is not needed — createPlainMember is role=member,
	// and agent ownership alone is enough to pass canManageAgent).

	req := withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+targetAgentID, map[string]any{
		"instructions": "do something ungoverned",
	}), "id", targetAgentID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("UpdateAgent as agent actor with instructions: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, targetAgentID).Scan(&stored)
	if stored != "" {
		t.Errorf("instructions must remain untouched, got %q", stored)
	}
}

// TestUpdateAgent_AgentActorCanWriteOtherFields proves the guard is
// field-scoped, not route-scoped: an agent actor can still update
// max_concurrent_tasks on the same endpoint without instructions present.
func TestUpdateAgent_AgentActorCanWriteOtherFields(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che455-update-agent-other-host@multica.test", "che455-update-agent-other-host")

	targetAgentID := createHandlerTestAgent(t, "che455-update-agent-other-target", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, ownerUserID, targetAgentID)

	req := withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+targetAgentID, map[string]any{
		"max_concurrent_tasks": 3,
	}), "id", targetAgentID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent as agent actor without instructions: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateAgent_HumanActorCanWriteInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID := createPlainMember(t, "che455-update-agent-human@multica.test")
	targetAgentID := createHandlerTestAgent(t, "che455-update-agent-human-target", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, ownerUserID, targetAgentID)

	req := withURLParam(newRequestAs(ownerUserID, http.MethodPut, "/api/agents/"+targetAgentID, map[string]any{
		"instructions": "approved instructions",
	}), "id", targetAgentID)

	w := httptest.NewRecorder()
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent as human owner: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, targetAgentID).Scan(&stored)
	if stored != "approved instructions" {
		t.Errorf("expected instructions to persist for human actor, got %q", stored)
	}
}

// ── CreateAgent (site 2) ─────────────────────────────────────────────────

func TestCreateAgent_AgentActorForbiddenFromInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che455-create-agent-host@multica.test", "che455-create-agent-host")

	req := newRequestAs(ownerUserID, http.MethodPost, "/api/agents", map[string]any{
		"name":         "che455-create-agent-blocked",
		"runtime_id":   handlerTestRuntimeID(t),
		"instructions": "smuggled instructions",
	})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("CreateAgent as agent actor with instructions: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent WHERE name = 'che455-create-agent-blocked'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected no agent to be created, found %d", count)
	}
}

// TestCreateAgent_AgentActorCanCreateWithoutInstructions authenticates as
// testUserID (the workspace owner, who owns the seeded test runtime) so the
// positive control isn't confounded by canUseRuntimeForAgent's unrelated
// private-runtime-ownership gate; only the agent-actor/instructions
// interaction under test varies.
func TestCreateAgent_AgentActorCanCreateWithoutInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-create-agent-ok-host", nil)
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, testUserID, hostAgentID)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	req := newRequestAs(testUserID, http.MethodPost, "/api/agents", map[string]any{
		"name":       "che455-create-agent-allowed",
		"runtime_id": handlerTestRuntimeID(t),
	})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAgent as agent actor without instructions: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE name = 'che455-create-agent-allowed'`)
	})
}

func TestCreateAgent_HumanActorCanCreateWithInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	req := newRequestAs(testUserID, http.MethodPost, "/api/agents", map[string]any{
		"name":         "che455-create-agent-human-ok",
		"runtime_id":   handlerTestRuntimeID(t),
		"instructions": "approved at create time",
	})

	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateAgent as human actor with instructions: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE name = 'che455-create-agent-human-ok'`)
	})

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM agent WHERE name = 'che455-create-agent-human-ok'`).Scan(&stored)
	if stored != "approved at create time" {
		t.Errorf("expected instructions to persist for human actor, got %q", stored)
	}
}

// ── UpdateSquad (site 3) ─────────────────────────────────────────────────

func squadReqWithParams(userID, method, path string, body any, params map[string]string) *http.Request {
	req := newRequestAs(userID, method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workspaceId", testWorkspaceID)
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestUpdateSquad_AgentActorForbiddenFromInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che455-update-squad-host@multica.test", "che455-update-squad-host-agent")
	leaderID := createHandlerTestAgent(t, "che455-update-squad-leader", nil)
	squad := createSquadAs(t, ownerUserID, "CHE-455 Squad Instructions", leaderID)

	req := squadReqWithParams(ownerUserID, "PATCH", "/api/squads", map[string]any{
		"instructions": "smuggled squad instructions",
	}, map[string]string{"id": squad.ID})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("UpdateSquad as agent actor with instructions: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateSquad_AgentActorCanRename(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID, hostAgentID, hostTaskID := governedInstructionActorFixture(t, "che455-update-squad-rename-host@multica.test", "che455-update-squad-rename-host-agent")
	leaderID := createHandlerTestAgent(t, "che455-update-squad-rename-leader", nil)
	squad := createSquadAs(t, ownerUserID, "CHE-455 Squad Rename", leaderID)

	req := squadReqWithParams(ownerUserID, "PATCH", "/api/squads", map[string]any{
		"name": "CHE-455 Squad Renamed",
	}, map[string]string{"id": squad.ID})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateSquad as agent actor without instructions: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateSquad_HumanActorCanWriteInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerUserID := createPlainMember(t, "che455-update-squad-human@multica.test")
	leaderID := createHandlerTestAgent(t, "che455-update-squad-human-leader", nil)
	squad := createSquadAs(t, ownerUserID, "CHE-455 Squad Human", leaderID)

	req := squadReqWithParams(ownerUserID, "PATCH", "/api/squads", map[string]any{
		"instructions": "approved squad instructions",
	}, map[string]string{"id": squad.ID})

	w := httptest.NewRecorder()
	testHandler.UpdateSquad(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateSquad as human actor with instructions: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var stored string
	dbfx.QueryRow(t, `SELECT instructions FROM squad WHERE id = $1`, squad.ID).Scan(&stored)
	if stored != "approved squad instructions" {
		t.Errorf("expected instructions to persist for human actor, got %q", stored)
	}
}

// ── UpdateWorkspace (site 5) ─────────────────────────────────────────────

func TestUpdateWorkspace_AgentActorForbiddenFromContext(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-update-workspace-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)
	// testUserID is the workspace owner already — resolveActor only cares
	// about the agent/task headers, not the caller's workspace role.

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"context": "smuggled workspace context",
	}), "id", testWorkspaceID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("UpdateWorkspace as agent actor with context: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateWorkspace_AgentActorCanWriteOtherFields(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-update-workspace-other-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	var previousDescription *string
	dbfx.QueryRow(t, `SELECT description FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previousDescription)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET description = $1 WHERE id = $2`, previousDescription, testWorkspaceID)
	})

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"description": "che-455 agent-writable description",
	}), "id", testWorkspaceID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateWorkspace as agent actor without context: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateWorkspace_HumanActorCanWriteContext(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var previousContext *string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previousContext)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET context = $1 WHERE id = $2`, previousContext, testWorkspaceID)
	})

	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"context": "approved workspace context",
	}), "id", testWorkspaceID)

	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateWorkspace as human owner with context: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var stored *string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored)
	if stored == nil || *stored != "approved workspace context" {
		t.Errorf("expected context to persist for human actor, got %v", stored)
	}
}

// ── UpdateProject (site 7) ────────────────────────────────────────────────

func TestUpdateProject_AgentActorForbiddenFromDescription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-update-project-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	projectID := dbfx.Project(t, "CHE-455 Update Project Instructions")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	req := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, map[string]any{
		"description": "smuggled project description",
	}), "id", projectID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateProject(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("UpdateProject as agent actor with description: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateProject_AgentActorCanWriteOtherFields(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-update-project-other-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	projectID := dbfx.Project(t, "CHE-455 Update Project Other Fields")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	req := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, map[string]any{
		"status": "in_progress",
	}), "id", projectID)
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.UpdateProject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateProject as agent actor without description: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateProject_HumanActorCanWriteDescription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	projectID := dbfx.Project(t, "CHE-455 Update Project Human")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	req := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, map[string]any{
		"description": "approved project description",
	}), "id", projectID)

	w := httptest.NewRecorder()
	testHandler.UpdateProject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateProject as human actor with description: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var stored *string
	dbfx.QueryRow(t, `SELECT description FROM project WHERE id = $1`, projectID).Scan(&stored)
	if stored == nil || *stored != "approved project description" {
		t.Errorf("expected description to persist for human actor, got %v", stored)
	}
}

// ── CreateProject (site 8) ────────────────────────────────────────────────

func TestCreateProject_AgentActorForbiddenFromDescription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-create-project-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	req := newRequest(http.MethodPost, "/api/projects", map[string]any{
		"title":       "CHE-455 Create Project Blocked",
		"description": "smuggled description",
	})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("CreateProject as agent actor with description: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM project WHERE title = 'CHE-455 Create Project Blocked'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected no project to be created, found %d", count)
	}
}

func TestCreateProject_AgentActorCanCreateWithoutDescription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	hostAgentID := createHandlerTestAgent(t, "che455-create-project-ok-host", nil)
	hostTaskID := createHandlerTestTaskForAgent(t, hostAgentID)

	req := newRequest(http.MethodPost, "/api/projects", map[string]any{
		"title": "CHE-455 Create Project Allowed",
	})
	req = asAgentActor(req, hostAgentID, hostTaskID)

	w := httptest.NewRecorder()
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateProject as agent actor without description: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE title = 'CHE-455 Create Project Allowed'`)
	})
}

func TestCreateProject_HumanActorCanCreateWithDescription(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	req := newRequest(http.MethodPost, "/api/projects", map[string]any{
		"title":       "CHE-455 Create Project Human",
		"description": "approved at create time",
	})

	w := httptest.NewRecorder()
	testHandler.CreateProject(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateProject as human actor with description: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE title = 'CHE-455 Create Project Human'`)
	})

	var stored *string
	dbfx.QueryRow(t, `SELECT description FROM project WHERE title = 'CHE-455 Create Project Human'`).Scan(&stored)
	if stored == nil || *stored != "approved at create time" {
		t.Errorf("expected description to persist for human actor, got %v", stored)
	}
}
