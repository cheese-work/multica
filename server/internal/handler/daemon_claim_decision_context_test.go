package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A decision ping is written by the server, not by an agent run, so no resumed
// agent session contains it. A bare reply ("A") to it reached the agent with no
// subject (CHE-1868). The claim must therefore carry the open request the
// reply is answering — and, when several are open, must not pick one.

type claimDecisionContextResponse struct {
	Task *struct {
		ChatMessage         string `json:"chat_message"`
		ChatDecisionContext string `json:"chat_decision_context"`
	} `json:"task"`
}

type seededDecision struct{ requestID, askCommentID, issueID string }

// seedDecisionRequest writes one asked request with its ping, issue and ask
// comment. pingAt orders the pings within the DM.
func seedDecisionRequest(t *testing.T, agentID, sessionID, ask, status, pingAt string) seededDecision {
	t.Helper()
	issueID := dbfx.Issue(t, "decision context issue")
	pingID := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "assistant", "content": ask, "message_kind": "decision_request", "sent_via": "task_token", "created_at": pingAt})
	requestID := dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "ping_message_id": pingID, "ask": ask, "escalation_class": "reversible", "origin": "asked", "status": status})
	askCommentID := dbfx.Insert(t, "comment", testutil.Cols{"issue_id": issueID, "workspace_id": testWorkspaceID, "author_type": "agent", "author_id": agentID, "content": ask, "decision_request_id": requestID, "decision_comment_kind": "ask"})
	dbfx.Exec(t, `UPDATE decision_request SET issue_comment_id = $2 WHERE id = $1`, requestID, askCommentID)
	return seededDecision{requestID, askCommentID, issueID}
}

// claimDecisionContext seeds one request per status in a fresh DM, oldest ping
// first, then claims the human's bare reply "A".
func claimDecisionContext(t *testing.T, statuses ...string) (string, []seededDecision) {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "decision context chat")
	seeded := make([]seededDecision, len(statuses))
	for i, status := range statuses {
		seeded[i] = seedDecisionRequest(t, agentID, sessionID, fmt.Sprintf("Ask number %d?", i+1), status, fmt.Sprintf("2020-01-01T00:00:0%dZ", i))
	}
	sendDirectChat(t, ctx, agentID, sessionID, "A")

	var resp claimDecisionContextResponse
	claimForRuntimeExpecting(t, testHandler, runtimeID, daemonID, http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ChatMessage != "A" {
		t.Fatalf("expected the bare reply to be claimed: %+v", resp.Task)
	}
	return resp.Task.ChatDecisionContext, seeded
}

func recordCommand(d seededDecision) string {
	return "comment add " + d.issueID + " --decision-record --message <id> --parent " + d.askCommentID
}

func TestClaimChatBindsBareReplyToTheOnlyOpenDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	block, seeded := claimDecisionContext(t, "open")
	d := seeded[0]
	for _, want := range []string{"Ask number 1?", d.requestID, d.askCommentID, d.issueID, recordCommand(d), "answers it"} {
		if !strings.Contains(block, want) {
			t.Errorf("chat_decision_context missing %q:\n%s", want, block)
		}
	}
}

// Two issues can each hold an open request in one DM. Age says nothing about
// which one a bare "A" answers, so the claim lists both and binds neither
// (OCR-1).
func TestClaimChatDoesNotGuessBetweenTwoOpenDecisionRequests(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	block, seeded := claimDecisionContext(t, "open", "open")
	for i, d := range seeded {
		for _, want := range []string{fmt.Sprintf("Ask number %d?", i+1), d.requestID, recordCommand(d)} {
			if !strings.Contains(block, want) {
				t.Errorf("chat_decision_context missing %q:\n%s", want, block)
			}
		}
	}
	for _, banned := range []string{"answers it", "earliest"} {
		if strings.Contains(block, banned) {
			t.Errorf("chat_decision_context binds a reply by age (%q):\n%s", banned, block)
		}
	}
	if !strings.Contains(block, "Do not guess") {
		t.Errorf("chat_decision_context must tell the run to disambiguate:\n%s", block)
	}
}

func TestClaimChatOmitsAnsweredDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	if block, _ := claimDecisionContext(t, "answered"); block != "" {
		t.Fatalf("unexpected chat_decision_context:\n%s", block)
	}
}

// A claim re-anchors queued input after the newest assistant row, so a ping
// newer than the reply cannot be staged through a claim. The filter that keeps
// such a ping out is the same one the recorder enforces, so test it directly.
func TestChatDecisionContextOmitsPingNewerThanReply(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "decision context future ping")
	seedDecisionRequest(t, agentID, sessionID, "Future ask?", "open", "2099-01-01T00:00:00Z")
	reply := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	block, err := chatDecisionContext(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(sessionID), reply)
	if err != nil || block != "" {
		t.Fatalf("ping newer than the reply was offered as context: %q, %v", block, err)
	}
}
