package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func vetoNoticeFixture(test *testing.T) (askedRecordFixture, db.DecisionAnswer) {
	test.Helper()
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	if _, err := decisiontx.NotifyPing(context.Background(), testPool, testHandler.Queries, fixture.issue.WorkspaceID, fixture.decision.ID); err != nil {
		test.Fatal(err)
	}
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), "Do not publish: approval was conditional.", true)
	if response.Code != http.StatusCreated {
		test.Fatalf("veto status=%d body=%s", response.Code, response.Body.String())
	}
	return fixture, record.Answer
}

func TestDecisionVetoNoticeDestinations(test *testing.T) {
	for _, branch := range []string{"source", "archived", "source_archived", "source_missing", "both_archived", "both_missing", "human_disclaim"} {
		test.Run(branch, func(test *testing.T) {
			fixture, answer := vetoNoticeFixture(test)
			destination := fixture.sessionID
			if branch == "source_archived" || branch == "source_missing" || branch == "both_archived" || branch == "both_missing" {
				source := dbfx.ChatSession(test, fixture.agentID, testutil.Cols{"creator_id": fixture.humanID})
				dbfx.Exec(test, `UPDATE decision_answer SET source_chat_session_id = $1 WHERE id = $2`, source, answer.ID)
				if branch == "source_missing" || branch == "both_missing" {
					dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, source)
				} else {
					dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, source)
				}
			}
			if branch == "archived" || branch == "both_archived" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
				destination = ""
			}
			if branch == "both_missing" {
				dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, fixture.sessionID)
				destination = ""
			}
			if branch == "human_disclaim" {
				dbfx.Exec(test, `UPDATE decision_answer SET disclaim_reason = 'human', disclaimed_by_agent_id = NULL, veto_reason = NULL WHERE id = $1`, answer.ID)
			}
			before := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			decisionPass(test, runner)
			if branch == "human_disclaim" {
				if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_veto'`, fixture.sessionID) != 0 || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID) != before {
					test.Fatal("human disclaim produced an agent-veto notice")
				}
				return
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND veto_note_posted_at IS NOT NULL`, answer.ID) != 1 {
				test.Fatal("veto notice was not durably acknowledged")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID); count != before+1 {
				test.Fatalf("inbox count=%d, want %d after retries", count, before+1)
			}
			var body string
			if err := testPool.QueryRow(context.Background(), `SELECT body FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2 ORDER BY created_at DESC LIMIT 1`, fixture.issueID, fixture.humanID).Scan(&body); err != nil || !strings.Contains(body, "Do not publish: approval was conditional.") {
				test.Fatalf("missing veto reason: body=%q err=%v", body, err)
			}
			if destination != "" {
				var message db.ChatMessage
				if err := testPool.QueryRow(context.Background(), `SELECT content FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_veto'`, destination).Scan(&message.Content); err != nil {
					test.Fatal(err)
				}
				var issueKey string
				dbfx.QueryRow(test, `SELECT workspace.issue_prefix || '-' || issue.number::text FROM issue JOIN workspace ON workspace.id = issue.workspace_id WHERE issue.id = $1`, fixture.issueID).Scan(&issueKey)
				workspace, err := testHandler.Queries.GetWorkspace(context.Background(), fixture.issue.WorkspaceID)
				if err != nil {
					test.Fatal(err)
				}
				wantIssueLink := "/" + workspace.Slug + "/issues/" + fixture.issueID
				if !strings.Contains(message.Content, issueKey) || !strings.Contains(message.Content, "Do not publish: approval was conditional.") || !strings.Contains(message.Content, fixture.agentID) || !strings.Contains(message.Content, "Open issue: "+wantIssueLink) {
					test.Fatalf("veto note lacks issue, actor or reason: %s", message.Content)
				}
			} else if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_veto'`, fixture.sessionID) != 0 {
				test.Fatal("notice was inserted into an archived lane")
			}
		})
	}
}

func TestDecisionVetoNoticeRollback(test *testing.T) {
	fixture, answer := vetoNoticeFixture(test)
	handler, runner := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionFailCommit{testPool}
	before := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID)
	_, _ = runner.ProcessPass(context.Background())
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND veto_note_posted_at IS NOT NULL`, answer.ID) != 0 || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_veto'`, fixture.sessionID) != 0 || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID) != before {
		test.Fatal("uncommitted veto notice escaped its transaction")
	}
	handler.TxStarter = testPool
	decisionPass(test, runner)
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND veto_note_posted_at IS NOT NULL`, answer.ID) != 1 {
		test.Fatal("veto notice did not recover after rollback")
	}
}

func TestDecisionSourceRetentionAfterEmptyTranscript(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, fixture.chatTaskID)
	dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID)
	if _, err := testHandler.CheckDecisionSource(context.Background(), fixture.issue.WorkspaceID, record.Request.ID); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	if _, err := testHandler.CancelDecisionLifecycle(context.Background(), fixture.issue.WorkspaceID, record.Request.ID); err != nil {
		test.Fatal(err)
	}
	response := testutil.Call(test, testHandler.DeleteChatSession, decisionSessionRequestForHuman(test, fixture.requestCoreFixture, "delete")).Want(http.StatusConflict)
	if !strings.Contains(response.Body.String(), "decision") {
		test.Fatalf("missing decision retention explanation: %s", response.Body.String())
	}
}

func TestDecisionVetoNoticeSessionRaces(test *testing.T) {
	for _, action := range []string{"archive", "delete"} {
		for _, first := range []string{"notice", "session"} {
			test.Run(action+"/"+first, func(test *testing.T) {
				fixture, answer := vetoNoticeFixture(test)
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'record_vetoed', cancel_notice_posted_at = now() WHERE id = $1`, fixture.decision.ID)
				original := *testHandler
				query := "-- name: CreateChatMessage"
				if first == "session" {
					query = "-- name: DecisionOpenLaneExists"
					if action == "delete" {
						query = "-- name: ListDecisionSessionReferences"
					}
				}
				paused, unblock := pauseDecisionTransaction(test, query)
				handler, runner := newDecisionRunnerFixture(test)
				if first == "notice" {
					handler.TxStarter = testHandler.TxStarter
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				notice := make(chan error, 1)
				startNotice := func() { go func() { _, err := runner.ProcessPass(ctx); notice <- err }() }
				request := decisionSessionRequestForHuman(test, fixture.requestCoreFixture, action)
				var session <-chan int
				if first == "notice" {
					startNotice()
				} else {
					session = startDecisionSessionAction(test, testHandler, request, action)
				}
				holderPID := awaitDecisionPause(test, paused)
				if first == "notice" {
					session = startDecisionSessionAction(test, &original, request, action)
				} else {
					startNotice()
				}
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					test.Fatal("competing operation bypassed the session fence")
				}
				unblock()
				if err := <-notice; err != nil {
					test.Fatal(err)
				}
				wanted := http.StatusOK
				if action == "delete" {
					wanted = http.StatusConflict
				}
				if status := awaitDecisionSessionAction(test, session); status != wanted {
					test.Fatalf("session status=%d want=%d", status, wanted)
				}
				messages := 1
				if action == "archive" && first == "session" {
					messages = 0
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_veto'`, fixture.sessionID); count != messages {
					test.Fatalf("veto notes=%d want=%d", count, messages)
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND veto_note_posted_at IS NOT NULL`, answer.ID) != 1 {
					test.Fatal("race lost the veto notice receipt")
				}
			})
		}
	}
}

// A human who left the workspace cannot receive the notice. The worker records
// the notice as handled instead of failing on the missing inbox row every pass.
func TestDecisionVetoNoticeToFormerMemberIsRecordedNotRetried(test *testing.T) {
	fixture, answer := vetoNoticeFixture(test)
	dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
	before := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID)

	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)

	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND veto_note_posted_at IS NOT NULL`, answer.ID) != 1 {
		test.Fatal("the notice for a former member stayed unhandled, so the worker retries it every pass")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID); count != before {
		test.Fatalf("inbox items for the former member = %d, want %d", count, before)
	}
}
