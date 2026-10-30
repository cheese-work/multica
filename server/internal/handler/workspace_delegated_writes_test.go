package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Delegated governed writes (CHE-2454): the setting lives in workspace.settings,
// defaults to no agents and no fields, and only a human owner/admin changes it.

func delegatedWritesFixture(t *testing.T, slug string) (wsID, agentID string) {
	t.Helper()
	wsID = pauseWorkspaceFixture(t, slug)
	runtime := dbfx.Runtime(t, slug, testutil.Cols{"workspace_id": wsID})
	agentID = dbfx.Agent(t, slug, runtime, testutil.Cols{"workspace_id": wsID})
	return wsID, agentID
}

func putDelegatedWrites(t *testing.T, wsID string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/workspaces/"+wsID+"/delegated-writes", body), "id", wsID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.UpdateDelegatedWrites, req)
}

func delegatedWriteCheck(t *testing.T, wsID, agentID, field string) (requiresDecision, ok bool) {
	t.Helper()
	ws, err := util.ParseUUID(wsID)
	if err != nil {
		t.Fatal(err)
	}
	return testHandler.delegatedWriteAllowed(context.Background(), ws, agentID, field)
}

func TestDelegatedWritesDefaultDeniesAll(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-default")
	for _, field := range delegatedWriteFields {
		if requires, ok := delegatedWriteCheck(t, wsID, agentID, field); ok || !requires {
			t.Fatalf("default %s: ok=%v requiresDecision=%v, want denied and requiring a decision", field, ok, requires)
		}
	}
	var cfg DelegatedWritesConfig
	testutil.Call(t, testHandler.GetDelegatedWrites,
		withURLParam(newRequest("GET", "/api/workspaces/"+wsID+"/delegated-writes", nil), "id", wsID)).
		Want(http.StatusOK).JSON(&cfg)
	if len(cfg.AllowedAgents) != 0 || len(cfg.AllowedFields) != 0 || !cfg.RequiresHumanDecision {
		t.Fatalf("default config = %+v", cfg)
	}
}

func TestDelegatedWritesAgentTokenCannotChange(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-token")
	body := map[string]any{"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.steps"}}
	for _, source := range []string{"task_token", "cloud_pat"} {
		putDelegatedWrites(t, wsID, body, func(r *http.Request) { r.Header.Set("X-Actor-Source", source) }).Want(http.StatusForbidden)
	}
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); ok {
		t.Fatal("a refused write changed the setting")
	}
}

func TestDelegatedWritesGenericSettingsSaveCannotChange(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-generic")
	// PATCH /api/workspaces/{id} carries an admin's whole settings blob; an
	// agent holding that route must not be able to grant itself the setting.
	saveWorkspaceSettings(t, wsID, map[string]any{
		delegatedWritesKey: map[string]any{"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.steps"}, "requires_human_decision": false},
	})
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); ok {
		t.Fatal("generic settings save granted a delegated write")
	}
	// A later unrelated save keeps a human-set value.
	putDelegatedWrites(t, wsID, map[string]any{"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.steps"}}, nil).Want(http.StatusOK)
	saveWorkspaceSettings(t, wsID, map[string]any{"github_pr_sidebar_enabled": false})
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); !ok {
		t.Fatal("unrelated settings save dropped the setting")
	}
	saveWorkspaceSettings(t, wsID, map[string]any{delegatedWritesKey: map[string]any{}})
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); !ok {
		t.Fatal("generic settings save cleared the setting")
	}
}

func TestDelegatedWritesRequiresOwnerOrAdmin(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-member")
	dbfx.Exec(t, `UPDATE member SET role='member' WHERE workspace_id=$1 AND user_id=$2`, wsID, testUserID)
	putDelegatedWrites(t, wsID, map[string]any{"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.steps"}}, nil).Want(http.StatusForbidden)
}

func TestDelegatedWritesListedAgentAndFieldOnly(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-allow")
	runtime := dbfx.Runtime(t, "dw-allow-other", testutil.Cols{"workspace_id": wsID})
	otherAgent := dbfx.Agent(t, "dw-allow-other", runtime, testutil.Cols{"workspace_id": wsID})
	putDelegatedWrites(t, wsID, map[string]any{
		"allowed_agents": []string{agentID},
		"allowed_fields": []string{"workflow.steps", "agent.instructions"},
	}, nil).Want(http.StatusOK)

	if requires, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); !ok || !requires {
		t.Fatalf("listed agent+field: ok=%v requiresDecision=%v, want allowed and requiring a decision", ok, requires)
	}
	if _, ok := delegatedWriteCheck(t, wsID, otherAgent, "workflow.steps"); ok {
		t.Fatal("non-listed agent allowed")
	}
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workspace.context"); ok {
		t.Fatal("non-listed field allowed")
	}
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "no.such.field"); ok {
		t.Fatal("unknown field allowed")
	}

	putDelegatedWrites(t, wsID, map[string]any{
		"allowed_agents": []string{agentID}, "allowed_fields": []string{"workflow.steps"}, "requires_human_decision": false,
	}, nil).Want(http.StatusOK)
	if requires, ok := delegatedWriteCheck(t, wsID, agentID, "workflow.steps"); !ok || requires {
		t.Fatalf("opt-out: ok=%v requiresDecision=%v, want allowed without a decision", ok, requires)
	}

	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE workspace_id=$1 AND action='delegated_writes_changed' AND actor_type='member' AND actor_id=$2`, wsID, testUserID).Scan(&n)
	if n != 2 {
		t.Fatalf("audit records = %d, want 2", n)
	}
}

func TestDelegatedWritesRejectsInvalidBodies(t *testing.T) {
	wsID, agentID := delegatedWritesFixture(t, "dw-invalid")
	for name, body := range map[string]map[string]any{
		"unknown field":  {"allowed_agents": []string{agentID}, "allowed_fields": []string{"issue.title"}},
		"not a uuid":     {"allowed_agents": []string{"nope"}, "allowed_fields": []string{"workflow.steps"}},
		"foreign agent":  {"allowed_agents": []string{"00000000-0000-0000-0000-000000000001"}, "allowed_fields": []string{"workflow.steps"}},
		"unknown key":    {"allowed_agents": []string{}, "allowed_fields": []string{}, "extra": true},
		"null agents":    {"allowed_agents": nil, "allowed_fields": []string{}},
		"missing fields": {"allowed_agents": []string{}},
	} {
		t.Run(name, func(t *testing.T) { putDelegatedWrites(t, wsID, body, nil).Want(http.StatusBadRequest) })
	}
	var stored map[string]json.RawMessage
	var raw []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, wsID).Scan(&raw)
	_ = json.Unmarshal(raw, &stored)
	if _, set := stored[delegatedWritesKey]; set {
		t.Fatalf("rejected bodies stored a setting: %s", raw)
	}
}
