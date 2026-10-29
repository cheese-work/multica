package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The web UI shows the predecessor DMs inline (CHE-2064); the agent must still
// get only the summary, never the earlier messages.
func TestContinuedChatSessionClaimSendsOnlyTheSummary(test *testing.T) {
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(test, ctx, "Continued")
	predecessorID := dbfx.Insert(test, "chat_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "Predecessor",
		"status": "archived", "explicitly_created_at": testutil.Raw("now()"),
	})
	forkMessage(test, predecessorID, testutil.Cols{"role": "user", "content": "predecessor-question-7731", "created_at": testutil.Raw("now() - interval '3 minutes'")})
	forkMessage(test, predecessorID, testutil.Cols{"role": "assistant", "content": "predecessor-answer-7731", "created_at": testutil.Raw("now() - interval '2 minutes'")})

	dbfx.Exec(test, `UPDATE chat_session SET continued_from_session_id = $2 WHERE id = $1`, sessionID, predecessorID)
	forkMessage(test, sessionID, testutil.Cols{"role": "assistant", "content": "the-summary-7731", "message_kind": "continuation_summary", "created_at": testutil.Raw("now() - interval '1 minute'")})

	sendDirectChat(test, ctx, agentID, sessionID, "Next step")
	claimed := claimForkTask(test, runtimeID, daemonID).ChatMessage

	want := "Continuation context from the previous DM:\n\nthe-summary-7731\n\nCurrent message:\n\nNext step"
	if claimed != want {
		test.Fatalf("claim input = %q, want %q", claimed, want)
	}
	if strings.Contains(claimed, "predecessor-") {
		test.Fatalf("claim input carries predecessor messages: %q", claimed)
	}
}
