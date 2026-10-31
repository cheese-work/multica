package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// The pilot setting and its stop are an owner decision. An agent task token
// carries the owner's user id, so only the route guard can tell it apart.
func TestAgentDecisionSettingRejectsMachineCredentials(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	var previous []byte
	if err := testPool.QueryRow(ctx, `SELECT coalesce(settings,'{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	stopped := `UPDATE workspace SET settings=jsonb_set(jsonb_set(coalesce(settings,'{}'::jsonb),'{agent_decisions_enabled}','false'),'{agent_decisions_stopped}','{"reason":"excluded category","at":"2026-10-31T00:00:00Z"}') WHERE id=$1`
	if _, err := testPool.Exec(ctx, stopped, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	state := func() (enabled, stop bool) {
		if err := testPool.QueryRow(ctx, `SELECT coalesce(settings->>'agent_decisions_enabled','')='true', settings ? 'agent_decisions_stopped' FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&enabled, &stop); err != nil {
			t.Fatal(err)
		}
		return
	}

	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id=$1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	matToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)
	put := func(bearer, body string) int {
		req, err := http.NewRequest(http.MethodPut, testServer.URL+"/api/agent-decisions/setting", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		// A forged stamp must lose to the server's own.
		req.Header.Set("X-Actor-Source", "member")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		if code := put(matToken, body); code != http.StatusForbidden {
			t.Fatalf("task token %s status=%d, want 403", body, code)
		}
		if enabled, stop := state(); enabled || !stop {
			t.Fatalf("a refused task-token request changed the setting (enabled=%v stop kept=%v)", enabled, stop)
		}
	}
	if code := put(testToken, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("human owner status=%d, want 200", code)
	}
	if enabled, stop := state(); !enabled || stop {
		t.Fatalf("human owner did not enable and clear the stop (enabled=%v stop=%v)", enabled, stop)
	}
}
