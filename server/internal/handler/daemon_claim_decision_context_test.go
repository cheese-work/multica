package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A decision ping is written by the server, not by an agent run, so no resumed
// agent session contains it. A bare reply ("A") to it reached the agent with no
// subject (CHE-1868). The claim must therefore carry the open request the
// reply is answering.

type claimDecisionContextResponse struct {
	Task *struct {
		ChatMessage         string `json:"chat_message"`
		ChatDecisionContext string `json:"chat_decision_context"`
	} `json:"task"`
}

// claimDecisionContext seeds one decision request in a fresh DM with the given
// status, then claims the human's bare reply "A".
func claimDecisionContext(t *testing.T, status string) (ctxBlock, requestID, askCommentID, issueID string) {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "decision context chat")
	issueID = dbfx.Issue(t, "decision context issue")
	pingID := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "assistant", "content": "Pick A or B?", "message_kind": "decision_request", "sent_via": "task_token", "created_at": "2020-01-01T00:00:00Z"})
	requestID = dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "ping_message_id": pingID, "ask": "Pick A or B?", "escalation_class": "reversible", "origin": "asked", "status": status})
	askCommentID = dbfx.Insert(t, "comment", testutil.Cols{"issue_id": issueID, "workspace_id": testWorkspaceID, "author_type": "agent", "author_id": agentID, "content": "Pick A or B?", "decision_request_id": requestID, "decision_comment_kind": "ask"})
	dbfx.Exec(t, `UPDATE decision_request SET issue_comment_id = $2 WHERE id = $1`, requestID, askCommentID)
	sendDirectChat(t, ctx, agentID, sessionID, "A")

	var resp claimDecisionContextResponse
	claimForRuntimeExpecting(t, testHandler, runtimeID, daemonID, http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ChatMessage != "A" {
		t.Fatalf("expected the bare reply to be claimed: %+v", resp.Task)
	}
	return resp.Task.ChatDecisionContext, requestID, askCommentID, issueID
}

func TestClaimChatCarriesOpenDecisionRequestBeforeReply(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	block, requestID, askCommentID, issueID := claimDecisionContext(t, "open")
	for _, want := range []string{"Pick A or B?", requestID, askCommentID, issueID, "comment add " + issueID + " --decision-record --message <id> --parent " + askCommentID} {
		if !strings.Contains(block, want) {
			t.Errorf("chat_decision_context missing %q:\n%s", want, block)
		}
	}
}

func TestClaimChatOmitsAnsweredDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	if block, _, _, _ := claimDecisionContext(t, "answered"); block != "" {
		t.Fatalf("unexpected chat_decision_context:\n%s", block)
	}
}
