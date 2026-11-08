package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func personalBGRequest(t *testing.T, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, testServer.URL+"/api/workspaces/"+testWorkspaceID+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// CHE-4113: the personal write routes belong to every workspace member, not
// only owner/admin. Through the real router a plain member reaches the handler:
// an effective write is refused by the shipped-disabled switch (409), an
// unchanged write is accepted.
func TestPersonalBackgroundAgentsWritesReachPlainMember(t *testing.T) {
	ctx := context.Background()
	var userID, agentID string
	email := uuid.NewString() + "@multica.ai"
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Personal BG member', $1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, userID); err != nil {
		t.Fatal(err)
	}
	// A plain member can invoke only an agent they own.
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		SELECT workspace_id, 'personal-bg-router-agent', '', 'cloud', '{}'::jsonb, runtime_id, 'private', 1, $2, '', '{}'::jsonb, '[]'::jsonb
		FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1
		RETURNING id`, testWorkspaceID, userID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID) })
	token, err := generateTestJWT(userID, email, "Personal BG member")
	if err != nil {
		t.Fatal(err)
	}

	status, body := personalBGRequest(t, token, http.MethodPut, "/me/background-agents", `{"dm_title":{"agent_id":"`+agentID+`"}}`)
	if status != http.StatusConflict || body["code"] != "personal_background_agents_not_enabled" {
		t.Errorf("member PUT effective write = %d %v, want 409 personal_background_agents_not_enabled", status, body)
	}
	if status, body := personalBGRequest(t, token, http.MethodPut, "/me/background-agents", `{"dm_title":{"agent_id":null}}`); status != http.StatusOK {
		t.Errorf("member PUT unchanged write = %d %v, want 200", status, body)
	}
	if status, body := personalBGRequest(t, token, http.MethodDelete, "/me/background-agents/dm_title", ""); status != http.StatusOK {
		t.Errorf("member DELETE unset job = %d %v, want 200", status, body)
	}
}
