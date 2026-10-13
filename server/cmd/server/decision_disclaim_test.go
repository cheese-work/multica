package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionHumanDisclaimRouter(test *testing.T) {
	for _, state := range []string{"live", "unprompted", "archived", "cancelled", "repeated", "other_human", "task_token", "wrong_workspace"} {
		test.Run(state, func(test *testing.T) {
			fixture := workspaceFixture(test)
			runtimeID := fixture.Runtime(test, "Disclaim runtime")
			agentID := fixture.Agent(test, "Disclaim agent", runtimeID, testutil.Cols{"visibility": "workspace"})
			issueID := fixture.Issue(test, "Disclaim quote")
			sessionID := fixture.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "Decisions"})
			taskID := fixture.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "status": "running", "runtime_id": runtimeID})
			origin, status, reason, lane := "asked", "answered", any(nil), "active"
			if state == "unprompted" {
				origin = "unprompted"
			}
			if state == "cancelled" {
				status, reason = "cancelled", "issue_closed"
			}
			if state == "archived" {
				lane = "archived"
			}
			fixture.Exec(test, `UPDATE chat_session SET status = $1 WHERE id = $2`, lane, sessionID)
			humanID := testUserID
			if state == "other_human" {
				humanID = fixture.User(test, "Other human", "disclaim-other@example.test")
			}
			requestID := fixture.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": humanID, "chat_session_id": sessionID, "escalation_class": "deploy", "origin": origin, "status": status, "cancel_reason": reason})
			messageID := fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Verbatim decision", "sent_via": "interactive_jwt"})
			answerID := fixture.Insert(test, "decision_answer", testutil.Cols{"decision_request_id": requestID, "answered_by_task_id": taskID, "source_chat_session_id": sessionID, "chat_message_ids": []string{messageID}, "content_snapshot": "Verbatim decision", "effective_at": testutil.Raw("clock_timestamp()")})
			quoteID := fixture.Comment(test, issueID, "immutable quote", testutil.Cols{"author_type": "agent", "author_id": agentID, "decision_answer_id": answerID, "decision_comment_kind": "quote"})
			fixture.Exec(test, `UPDATE decision_request SET issue_comment_id = $1 WHERE id = $2`, quoteID, requestID)
			fixture.Exec(test, `UPDATE decision_answer SET issue_comment_id = $1 WHERE id = $2`, quoteID, answerID)
			call := func() int {
				token, workspaceID := testToken, testWorkspaceID
				if state == "task_token" {
					token = mintAgentTaskToken(test, agentID, taskID, testUserID)
				}
				if state == "wrong_workspace" {
					workspaceID = "11111111-1111-4111-8111-111111111111"
				}
				request, _ := http.NewRequest(http.MethodPost, testServer.URL+"/api/decision-answers/"+answerID+"/disclaim", nil)
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("X-Workspace-ID", workspaceID)
				response, err := testServer.Client().Do(request)
				if err != nil {
					test.Fatal(err)
				}
				defer response.Body.Close()
				return response.StatusCode
			}
			code := call()
			if state == "other_human" || state == "task_token" || state == "wrong_workspace" {
				wanted := http.StatusForbidden
				if state == "wrong_workspace" {
					wanted = http.StatusNotFound
				}
				if code != wanted {
					test.Fatalf("unauthorized disclaim status=%d", code)
				}
				return
			}
			if code != http.StatusOK {
				test.Fatalf("disclaim status=%d", code)
			}
			if state == "repeated" && call() != http.StatusOK {
				test.Fatal("retry is not idempotent")
			}
			var gotStatus, gotReason, snapshot, content string
			var cancelled, disclaimed bool
			err := testPool.QueryRow(context.Background(), `SELECT request.status, COALESCE(request.cancel_reason, ''), request.cancel_notice_posted_at IS NOT NULL, answer.disclaimed_at IS NOT NULL AND answer.disclaim_comment_posted_at IS NOT NULL AND answer.withdrawal_comment_id IS NOT NULL, answer.content_snapshot, quote.content FROM decision_request request JOIN decision_answer answer ON answer.decision_request_id = request.id JOIN comment quote ON quote.id = answer.issue_comment_id WHERE answer.id = $1`, answerID).Scan(&gotStatus, &gotReason, &cancelled, &disclaimed, &snapshot, &content)
			if err != nil {
				test.Fatal(err)
			}
			wantedStatus, wantedReason := "open", ""
			if state == "unprompted" {
				wantedStatus, wantedReason = "cancelled", "record_disclaimed"
			} else if state == "archived" {
				wantedStatus, wantedReason = "cancelled", "lane_archived"
			} else if state == "cancelled" {
				wantedStatus, wantedReason = "cancelled", "issue_closed"
			}
			if gotStatus != wantedStatus || gotReason != wantedReason || !disclaimed || snapshot != "Verbatim decision" || content != "immutable quote" || (state == "unprompted" || state == "archived") && !cancelled {
				test.Fatalf("withdrawal state=%s reason=%s cancel=%t withdrawn=%t snapshot=%q quote=%q", gotStatus, gotReason, cancelled, disclaimed, snapshot, content)
			}
			if count := fixture.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, answerID); count != 1 {
				test.Fatalf("disclaim count=%d", count)
			}
			wantedCancel := 0
			if state == "archived" {
				wantedCancel = 1
			}
			if count := fixture.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, requestID); count != wantedCancel {
				test.Fatalf("cancel count=%d want=%d", count, wantedCancel)
			}
			if fixture.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID) != 1 || fixture.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID) != 1 || fixture.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID) != 0 {
				test.Fatal("withdrawal synchronously created a replacement DM, ping or agent run")
			}
		})
	}
}
