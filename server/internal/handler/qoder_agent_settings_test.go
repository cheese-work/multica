package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestQoderAgentWritesValidateEffectiveSettings(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	qoderRuntime := dbfx.Runtime(t, "qoder settings runtime", testutil.Cols{"provider": "qoder_cloud"})
	localRuntime := dbfx.Runtime(t, "local settings runtime")
	qoderAgent := dbfx.Agent(t, "qoder settings agent", qoderRuntime)
	modelAgent := dbfx.Agent(t, "local model agent", localRuntime, testutil.Cols{"model": "gpt-5"})
	var skillID string
	dbfx.QueryRow(t, `
		INSERT INTO skill (workspace_id, name, description, content, config, created_by)
		VALUES ($1, 'Qoder settings skill', '', '# Skill', '{}'::jsonb, $2)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&skillID)
	dbfx.Cleanup(t, `DELETE FROM skill WHERE id = $1`, skillID)

	put := func(handler http.HandlerFunc, agentID, path string, body map[string]any) *testutil.Response {
		return testutil.Call(t, handler, withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentID+path, body), "id", agentID))
	}

	for name, body := range map[string]map[string]any{
		"model":       {"model": "gpt-5"},
		"custom args": {"custom_args": []string{"--verbose"}},
		"mcp config":  {"mcp_config": map[string]any{"mcpServers": map[string]any{"x": map[string]any{"command": "x"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			put(testHandler.UpdateAgent, qoderAgent, "", body).Want(http.StatusBadRequest)
		})
	}
	put(testHandler.UpdateAgent, qoderAgent, "", map[string]any{"description": "still fine"}).Want(http.StatusOK)
	put(testHandler.UpdateAgent, qoderAgent, "", map[string]any{"model": ""}).Want(http.StatusOK)

	put(testHandler.UpdateAgent, modelAgent, "", map[string]any{"runtime_id": qoderRuntime, "runtime_config": map[string]any{"qoder_agent_id": "agent_1"}}).Want(http.StatusBadRequest)

	put(testHandler.UpdateAgentEnv, qoderAgent, "/env", map[string]any{"custom_env": map[string]string{"API_KEY": "x"}}).Want(http.StatusBadRequest)
	put(testHandler.SetAgentSkills, qoderAgent, "/skills", map[string]any{"skill_ids": []string{skillID}}).Want(http.StatusBadRequest)
	put(testHandler.SetAgentSkills, qoderAgent, "/skills", map[string]any{"skill_ids": []string{}}).Want(http.StatusOK)
}

// PATCH /env is a key-scoped writer that shares one merge path with the
// approved env.patch decision action. It must apply the same restriction PUT
// does to the effective merged map: a Qoder agent may not gain a custom env,
// but may still shed one it already carries.
func TestQoderAgentEnvPatchValidatesEffectiveMap(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	qoderRuntime := dbfx.Runtime(t, "qoder env patch runtime", testutil.Cols{"provider": "qoder_cloud"})
	localRuntime := dbfx.Runtime(t, "local env patch runtime")
	qoderAgent := dbfx.Agent(t, "qoder env patch agent", qoderRuntime)
	legacyAgent := dbfx.Agent(t, "qoder legacy env agent", qoderRuntime, testutil.Cols{"custom_env": testutil.Raw(`'{"OLD":"v","KEEP":"v"}'::jsonb`)})
	localAgent := dbfx.Agent(t, "local env patch agent", localRuntime)

	patch := func(agentID string, body map[string]any) *testutil.Response {
		return testutil.Call(t, testHandler.PatchAgentEnv, withURLParam(newRequest(http.MethodPatch, "/api/agents/"+agentID+"/env", body), "id", agentID))
	}

	// (a) a PATCH that leaves a nonempty env on a Qoder agent is rejected and writes nothing.
	patch(qoderAgent, map[string]any{"set": map[string]string{"EXAMPLE": "value"}}).Want(http.StatusBadRequest)
	if got := storedAgentEnv(t, qoderAgent); len(got) != 0 {
		t.Fatalf("rejected PATCH wrote env: %v", got)
	}

	// (b) cleanup path: a partial unset that still leaves keys is rejected;
	// removing the last key is allowed.
	patch(legacyAgent, map[string]any{"unset": []string{"OLD"}}).Want(http.StatusBadRequest)
	if got := storedAgentEnv(t, legacyAgent); len(got) != 2 {
		t.Fatalf("rejected PATCH changed env: %v", got)
	}
	patch(legacyAgent, map[string]any{"unset": []string{"OLD", "KEEP"}}).Want(http.StatusOK)
	if got := storedAgentEnv(t, legacyAgent); len(got) != 0 {
		t.Fatalf("cleanup PATCH left env: %v", got)
	}

	// (c) non-Qoder PATCH is unchanged.
	patch(localAgent, map[string]any{"set": map[string]string{"EXAMPLE": "value"}}).Want(http.StatusOK)
	if got := storedAgentEnv(t, localAgent); got["EXAMPLE"] != "value" {
		t.Fatalf("local PATCH did not store env: %v", got)
	}
}
