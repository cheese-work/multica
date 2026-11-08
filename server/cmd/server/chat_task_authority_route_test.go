package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2909: a compaction task that is still running, with a valid task token,
// loses DM access the moment the session creator can no longer invoke the
// compact agent. Nothing cancels the task here; the refusal has to come from
// the authority check on every agent-facing chat route, through the real router
// and token middleware.
func TestChatTaskAuthorityRefusesRunningCompactionTaskAfterInvocationRightsRevoked(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	fx := workspaceFixture(t)

	creator := fx.User(t, "Compaction creator", "che2909-route-creator@example.test")
	fx.Member(t, testWorkspaceID, creator, "member")
	var runtimeID string
	fx.QueryRow(t, `SELECT runtime_id FROM agent WHERE workspace_id=$1 AND name='Integration Test Agent'`, testWorkspaceID).Scan(&runtimeID)
	compactAgent := fx.Agent(t, "che2909-route-compact-agent", runtimeID, testutil.Cols{
		"visibility": "workspace", "permission_mode": "public_to",
	})
	fx.Insert(t, "agent_invocation_target", testutil.Cols{
		"agent_id": compactAgent, "target_type": "workspace", "target_id": testWorkspaceID,
	})
	dmAgent := fx.Agent(t, "che2909-route-dm-agent", runtimeID, testutil.Cols{"visibility": "workspace"})
	session := fx.ChatSession(t, dmAgent, testutil.Cols{"creator_id": creator})

	var settings []byte
	fx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, settings, testWorkspaceID)
	})
	fx.Exec(t, `UPDATE workspace SET settings = settings - 'compact_agent_id'
		|| jsonb_build_object('background_agents', jsonb_build_object('compact', jsonb_build_object('agent_id', $2::text)))
		WHERE id = $1`, testWorkspaceID, compactAgent)

	taskID := fx.Task(t, compactAgent, testutil.Cols{
		"runtime_id": runtimeID, "status": "running", "chat_session_id": session,
		"purpose": "compaction", "task_kind": "compact", "started_at": testutil.Raw("now()"),
	})
	token := "mat_che2909_" + taskID
	fx.Exec(t, `INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')`, auth.HashToken(token), taskID, compactAgent, testWorkspaceID, testUserID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM task_token WHERE task_id = $1`, taskID) })

	reads := []string{"/api/chat/history", "/api/chat/thread"}
	for _, path := range reads {
		if status, body := chatRequestWithToken(t, token, path); status != http.StatusOK {
			t.Fatalf("before revoke: GET %s = %d %s, want 200", path, status, body)
		}
	}

	// Revoke: the creator is a plain member, so private mode removes their right
	// to invoke the compact agent. The task is not touched.
	fx.Exec(t, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, compactAgent)
	var status string
	fx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	if status != "running" {
		t.Fatalf("task status = %q after revoke, want running (the route must refuse, not a cancel)", status)
	}

	for _, path := range reads {
		code, body := chatRequestWithToken(t, token, path)
		if code != http.StatusForbidden || !strings.Contains(string(body), "compaction_agent_not_current") {
			t.Fatalf("after revoke: GET %s = %d %s, want 403 compaction_agent_not_current", path, code, body)
		}
	}
	if code, body := postCompact(t, token, "summary after revoke"); code != http.StatusForbidden {
		t.Fatalf("after revoke: POST /api/chat/compact = %d %s, want 403", code, body)
	}
}
