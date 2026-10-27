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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
	for _, want := range []string{"Ask number 1?", d.requestID, d.askCommentID, d.issueID, recordCommand(d), "Do not record"} {
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
	for _, banned := range []string{"earliest"} {
		if strings.Contains(block, banned) {
			t.Errorf("chat_decision_context binds a reply by age (%q):\n%s", banned, block)
		}
	}
	if !strings.Contains(block, "Do not guess") {
		t.Errorf("chat_decision_context must tell the run to disambiguate:\n%s", block)
	}
}

// The carrier always holds the turn's message ids, so an answered-only DM is no
// longer an empty block. It must still expose no ask and no binding instruction.
func TestClaimChatOmitsAnsweredDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	block, _ := claimDecisionContext(t, "answered")
	for _, banned := range []string{"Ask number 1?", "--parent", "[request ", "does not answer a card", "Do not guess"} {
		if strings.Contains(block, banned) {
			t.Fatalf("answered request reached the carrier (%q):\n%s", banned, block)
		}
	}
}

// A claim re-anchors queued input after the newest assistant row, so a ping
// newer than the reply cannot be staged through a claim. Build the carrier from
// a reply that predates one ping, and check eligibility follows the cutoff,
// status and kind, not the number of rows.
func TestChatDecisionContextCountsOnlyEligibleAsks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "decision context eligibility")
	eligibleID, _, _ := seedDMDecision(t, agentID, sessionID, "Eligible ask?", "open", "2 hours")
	seedDMDecision(t, agentID, sessionID, "Answered ask?", "answered", "3 hours")
	seedDMDecision(t, agentID, sessionID, "Future ask?", "open", "-1 hour")
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "assistant", "content": "Decision request cancelled: lane_archived.", "message_kind": "decision_reference", "sent_via": "task_token", "created_at": testutil.Raw("now() - interval '1 hour'")})

	block := carrierFor(t, sessionID, time.Now())
	for _, want := range []string{"Eligible ask?", eligibleID, "Do not record", "lane_archived"} {
		if !strings.Contains(block, want) {
			t.Errorf("carrier missing %q:\n%s", want, block)
		}
	}
	for _, banned := range []string{"Answered ask?", "Future ask?", "Do not guess", "Several"} {
		if strings.Contains(block, banned) {
			t.Errorf("carrier holds %q although it is not an eligible ask:\n%s", banned, block)
		}
	}
	if strings.Count(block, "[request ") != 1 {
		t.Errorf("want exactly one request row:\n%s", block)
	}
}

// carrierFor builds the carrier for a session as if its first unanswered user
// message was written at replyAt.
func carrierFor(t *testing.T, sessionID string, replyAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	session, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	input := []db.ChatMessage{{ID: parseUUID(dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "A", "created_at": replyAt.UTC().Format(time.RFC3339Nano)})), ChatSessionID: session.ID, Role: "user", Content: "A", CreatedAt: pgtype.Timestamptz{Time: replyAt, Valid: true}}}
	block, err := testHandler.decisionContextBlock(ctx, session, pgtype.UUID{}, input)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

// An unprompted decision has no open request, so the carrier must still hand the
// run the ids --decision-record needs: the reply itself and the options message
// it chose between (CHE-1958). Each id appears once, with and without open asks.
func TestClaimChatCarriesDecisionRecordIDs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, withAsk := range map[string]bool{"no open ask": false, "one open ask": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "decision ids chat")
			if withAsk {
				seedDMDecision(t, agentID, sessionID, "Ship or hold?", "open", "5 hours")
			}
			optionsID := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "assistant", "content": "1) Ship\n2) Hold", "created_at": testutil.Raw("now() - interval '3 hours'")})
			sendDirectChat(t, ctx, agentID, sessionID, "1")
			var replyID string
			if err := testPool.QueryRow(ctx, `SELECT id::text FROM chat_message WHERE chat_session_id = $1 AND role = 'user'`, sessionID).Scan(&replyID); err != nil {
				t.Fatal(err)
			}

			claimed := claimForkTask(t, runtimeID, daemonID)
			carrier := claimed.ChatDecisionContext
			for _, id := range []string{replyID, optionsID} {
				if strings.Count(carrier, id) != 1 {
					t.Errorf("id %s appears %d times, want once:\n%s", id, strings.Count(carrier, id), carrier)
				}
			}
			for _, want := range []string{"--decision-record", "--context-message"} {
				if !strings.Contains(carrier, want) {
					t.Errorf("carrier lacks %q:\n%s", want, carrier)
				}
			}
			if claimed.ChatMessage != "1" {
				t.Errorf("the message must stay the user's text, got %q", claimed.ChatMessage)
			}
		})
	}
}
