package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func decisionGuardRequest(test *testing.T, sessionID, action string) *http.Request {
	test.Helper()
	body := map[string]any{"archived": true}
	method := http.MethodPut
	if action == "continue" {
		method, body = http.MethodPost, map[string]any{"summary": "Preserve the context."}
	} else if action == "delete" {
		method, body = http.MethodDelete, nil
	}
	return withChatTestWorkspaceCtx(test, withURLParam(newRequest(method, "/api/chat/sessions/"+sessionID, body), "sessionId", sessionID))
}

func TestDecisionSessionRetention(test *testing.T) {
	for _, action := range []string{"archive", "continue", "delete"} {
		for _, status := range []string{"open", "answered", "cancelled"} {
			test.Run(action+"/"+status, func(test *testing.T) {
				agentID := createHandlerTestAgent(test, "retention", []byte("[]"))
				sessionID := dbfx.ChatSession(test, agentID)
				issueID := dbfx.Issue(test, "retained decision")
				requestID := dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "escalation_class": "reversible", "origin": "asked", "status": status})
				test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM decision_request WHERE id = $1`, requestID) })
				before := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID)
				wanted := http.StatusOK
				if action == "continue" {
					wanted = http.StatusCreated
				} else if action == "delete" {
					wanted = http.StatusNoContent
				}
				if status == "open" {
					wanted = http.StatusConflict
				}
				var handler http.HandlerFunc = testHandler.SetChatSessionArchived
				if action == "continue" {
					handler = testHandler.ContinueChatSession
				} else if action == "delete" {
					handler = testHandler.DeleteChatSession
				}
				testutil.Call(test, handler, decisionGuardRequest(test, sessionID, action)).Want(wanted)
				var stored string
				if err := testPool.QueryRow(context.Background(), `SELECT status FROM decision_request WHERE id = $1`, requestID).Scan(&stored); err != nil || stored != status {
					test.Fatalf("baseline changed decision row: status=%q err=%v", stored, err)
				}
				if wanted == http.StatusConflict {
					if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID); count != before {
						test.Fatal("refusal created a continuation")
					}
					if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 0 {
						test.Fatal("refusal created a message")
					}
				}
			})
		}
	}
}

func TestDecisionSessionDeleteReferences(test *testing.T) {
	for _, withdrawn := range []bool{false, true} {
		test.Run(fmt.Sprintf("withdrawn=%t", withdrawn), func(test *testing.T) {
			agentID := createHandlerTestAgent(test, "answer-retention", []byte("[]"))
			sessionID := dbfx.ChatSession(test, agentID)
			issueID := dbfx.Issue(test, "answer retention")
			requestID := dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "escalation_class": "reversible", "origin": "asked", "status": "answered"})
			test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM decision_request WHERE id = $1`, requestID) })
			messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Yes"})
			taskID := dbfx.Task(test, agentID, testutil.Cols{"status": "completed", "chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(test)})
			answerID := dbfx.Insert(test, "decision_answer", testutil.Cols{"decision_request_id": requestID, "answered_by_task_id": taskID, "source_chat_session_id": sessionID, "chat_message_ids": "{" + messageID + "}", "content_snapshot": "Yes", "effective_at": time.Now()})
			if withdrawn {
				dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, answerID)
			}
			response := testutil.Call(test, testHandler.DeleteChatSession, decisionGuardRequest(test, sessionID, "delete")).Want(http.StatusConflict)
			var issueKey string
			dbfx.QueryRow(test, `SELECT workspace.issue_prefix || '-' || issue.number::text FROM issue JOIN workspace ON workspace.id = issue.workspace_id WHERE issue.id = $1`, issueID).Scan(&issueKey)
			if !strings.Contains(response.Body.String(), issueKey) {
				test.Fatalf("retention refusal does not identify the issue: %s", response.Body.String())
			}
		})
	}
}

func TestDecisionSessionChecksDoNotLockDecisionRows(test *testing.T) {
	for _, action := range []string{"archive", "continue", "delete"} {
		test.Run(action, func(test *testing.T) {
			agentID := createHandlerTestAgent(test, "no-decision-lock", []byte("[]"))
			sessionID := dbfx.ChatSession(test, agentID)
			issueID := dbfx.Issue(test, "locked decision")
			requestID := dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "escalation_class": "reversible", "origin": "asked"})
			test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM decision_request WHERE id = $1`, requestID) })
			transaction, err := testPool.Begin(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer transaction.Rollback(context.Background())
			if _, err := transaction.Exec(context.Background(), `SELECT id FROM decision_request WHERE id = $1 FOR UPDATE`, requestID); err != nil {
				test.Fatal(err)
			}
			request := decisionGuardRequest(test, sessionID, action)
			ctx, cancel := context.WithTimeout(request.Context(), time.Second)
			defer cancel()
			var handler http.HandlerFunc = testHandler.SetChatSessionArchived
			if action == "continue" {
				handler = testHandler.ContinueChatSession
			} else if action == "delete" {
				handler = testHandler.DeleteChatSession
			}
			testutil.Call(test, handler, request.WithContext(ctx)).Want(http.StatusConflict)
		})
	}
}
