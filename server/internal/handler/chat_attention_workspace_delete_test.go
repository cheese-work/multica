package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The attention tables have no foreign keys (repo rule), so explicit statements remove both.
func TestDeleteWorkspace_RemovesChatAttentionRows(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	const slug = "handler-tests-delete-chat-attention"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)

	var wsID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description) VALUES ($1, $2, '') RETURNING id
	`, "Handler Test Chat Attention Delete", slug).Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
	`, wsID, testUserID); err != nil {
		t.Fatalf("create owner member: %v", err)
	}

	var runtimeID, agentID, sessionID, markRoot string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at
		)
		VALUES ($1, NULL, 'Chat Attention Delete Runtime', 'cloud', 'isolated_test', 'online', '', '{}'::jsonb, now())
		RETURNING id
	`, wsID).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, 'Chat Attention Delete Agent', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3)
		RETURNING id
	`, wsID, runtimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status)
		VALUES ($1, $2, $3, 'Chat Attention Delete Session', 'active')
		RETURNING id
	`, wsID, agentID, testUserID).Scan(&sessionID); err != nil {
		t.Fatalf("create chat session: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_attention_item (workspace_id, chain_root_session_id, chat_session_id, user_id, kind)
		VALUES ($1, $2, $2, $3, 'unseen')
	`, wsID, sessionID, testUserID); err != nil {
		t.Fatalf("seed attention item: %v", err)
	}
	// Marks have no workspace_id (merged table, no writer yet), so they are removed through a live
	// session of the workspace; this one stays alive while the other root is hard-deleted below.
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status)
		VALUES ($1, $2, $3, 'Chat Attention Delete Mark Session', 'active')
		RETURNING id
	`, wsID, agentID, testUserID).Scan(&markRoot); err != nil {
		t.Fatalf("create mark session: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_attention_mark (user_id, subject_key, chain_root_session_id, acked_at)
		VALUES ($1, 'turn:00000000-0000-0000-0000-000000000001', $2, now())
	`, testUserID, markRoot); err != nil {
		t.Fatalf("seed attention mark: %v", err)
	}

	// CHE-4149: the per-turn outcome row carries workspace_id, so it is removed even when its root
	// session was hard-deleted before the workspace.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_turn_outcome (task_id, workspace_id, chat_session_id, chain_root_session_id, user_id, kind)
		VALUES (gen_random_uuid(), $1, $2, $2, $3, 'done')
	`, wsID, sessionID, testUserID); err != nil {
		t.Fatalf("seed turn outcome: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("hard-delete root session: %v", err)
	}

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+wsID, nil), "id", wsID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	var items, marks int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_attention_item WHERE workspace_id = $1`, wsID).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_attention_mark WHERE chain_root_session_id = $1`, markRoot).Scan(&marks); err != nil {
		t.Fatalf("count marks: %v", err)
	}
	var outcomes int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM chat_turn_outcome WHERE workspace_id = $1`, wsID).Scan(&outcomes); err != nil {
		t.Fatalf("count outcomes: %v", err)
	}
	if items != 0 || marks != 0 || outcomes != 0 {
		t.Errorf("workspace delete left attention rows: items=%d marks=%d outcomes=%d", items, marks, outcomes)
	}
}
