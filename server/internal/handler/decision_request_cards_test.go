package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionRequestCardsFollowStateAndPreserveAsk(test *testing.T) {
	for _, state := range []string{"open", "answered", "withdrawn", "legacy", "foreign_source"} {
		test.Run(state, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			wantedState := "open"
			var recordCommentID string
			dbfx.Exec(test, `UPDATE decision_request SET source_comment_id = $1 WHERE id = $2`, fixture.tagID, fixture.decision.ID)
			switch state {
			case "answered":
				result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
				if err != nil {
					test.Fatal(err)
				}
				recordCommentID = uuidToString(result.Comment.ID)
				wantedState = "answered"
			case "withdrawn":
				if _, err := testHandler.withdrawDecisionRequestTx(fixture.request(), fixture.decision, "Superseded by the new scope."); err != nil {
					test.Fatal(err)
				}
				wantedState = "withdrawn"
			case "legacy":
				dbfx.Exec(test, `UPDATE decision_request SET source_comment_id = NULL WHERE id = $1`, fixture.decision.ID)
			case "foreign_source":
				foreignID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": dbfx.Issue(test, "Unrelated issue"), "author_type": "member", "author_id": fixture.humanID, "content": "Private unrelated source"})
				dbfx.Exec(test, `UPDATE decision_request SET source_comment_id = $1 WHERE id = $2`, foreignID, fixture.decision.ID)
			}
			request := withURLParam(httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+fixture.sessionID+"/messages/page", nil), "sessionId", fixture.sessionID)
			request.Header.Set("X-User-ID", fixture.humanID)
			request = withChatTestWorkspaceCtx(test, request)
			var page struct {
				Messages []map[string]any `json:"messages"`
			}
			testutil.Call(test, testHandler.ListChatMessagesPage, request).Want(http.StatusOK).JSON(&page)
			if state == "withdrawn" {
				found := false
				for _, message := range page.Messages {
					if message["message_kind"] == "decision_cancel_notice" {
						found = true
						cards, ok := message["decision_requests"].([]any)
						if !ok || len(cards) != 1 || cards[0].(map[string]any)["ask"] != fixture.decision.Ask {
							test.Fatalf("cancellation page lacks its original ask card: %v", message)
						}
					}
				}
				if !found {
					test.Fatal("cancellation notice is not distinguishable from a reply")
				}
			}
			for _, message := range page.Messages {
				if message["id"] != uuidToString(fixture.decision.PingMessageID) {
					continue
				}
				card, ok := message["decision_request"].(map[string]any)
				if !ok {
					test.Fatalf("original ask lacks decision request card: %v", message)
				}
				if card["state"] != wantedState || card["ask"] != fixture.decision.Ask || card["issue_id"] != fixture.issueID || card["issue_identifier"] == "" {
					test.Fatalf("incorrect request card: %v", card)
				}
				if state == "legacy" || state == "foreign_source" {
					if card["source_comment_id"] != nil {
						test.Fatalf("invalid source link exposed: %v", card)
					}
				} else if card["source_comment_id"] != fixture.tagID {
					test.Fatalf("original source link missing: %v", card)
				}
				if state == "answered" {
					if card["record_comment_id"] != recordCommentID {
						test.Fatalf("answered card does not link its record comment: %v", card)
					}
				} else if card["record_comment_id"] != nil {
					test.Fatalf("unanswered card exposes a record comment: %v", card)
				}
				if state == "withdrawn" && (card["reason"] != "Superseded by the new scope." || card["withdrawn_at"] == nil) {
					test.Fatalf("withdrawal lost reason or time: %v", card)
				}
				return
			}
			test.Fatal("original ask missing")
		})
	}
}

func TestDecisionRequestCardsStayWithinWorkspace(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	message, err := testHandler.Queries.GetChatMessage(context.Background(), fixture.decision.PingMessageID)
	if err != nil {
		test.Fatal(err)
	}
	foreignID := dbfx.Workspace(test, "Other request workspace", "request-cards-"+fixture.issueID)
	response := testHandler.chatMessageResponses(context.Background(), foreignID, []db.ChatMessage{message})
	encoded := mustDecisionJSON(test, response)
	if strings.Contains(encoded, `"decision_request":`) {
		test.Fatalf("request metadata crossed workspaces: %s", encoded)
	}
}

func TestDecisionRequestCardsOnCancellationOnlyPage(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	withdrawn, err := testHandler.withdrawDecisionRequestTx(fixture.request(), fixture.decision, "Scope changed.")
	if err != nil {
		test.Fatal(err)
	}
	response := testHandler.chatMessageResponses(context.Background(), testWorkspaceID, []db.ChatMessage{withdrawn.Message})
	if len(response) != 1 || len(response[0].DecisionRequests) != 1 || response[0].DecisionRequests[0].Ask != fixture.decision.Ask || response[0].DecisionRequests[0].State != "withdrawn" {
		test.Fatalf("cancellation-only page lost the original ask: %+v", response)
	}
}

func TestDecisionRequestCardsGroupClosedAsks(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	addOpenRequest(test, fixture.requestCoreFixture, "spend")
	closeAndCancel(test, fixture.requestCoreFixture)
	messages, err := testHandler.Queries.ListChatMessages(context.Background(), parseUUID(fixture.sessionID))
	if err != nil {
		test.Fatal(err)
	}
	for _, message := range messages {
		if message.MessageKind != "decision_cancel_notice" {
			continue
		}
		response := testHandler.chatMessageResponses(context.Background(), testWorkspaceID, []db.ChatMessage{message})
		if len(response[0].DecisionRequests) != 2 {
			test.Fatalf("grouped close lost an ask: %+v", response)
		}
		for _, card := range response[0].DecisionRequests {
			if card.State != "withdrawn" || card.Ask == "" || card.Reason != "issue closed" || card.WithdrawnAt == nil {
				test.Fatalf("grouped close lost its card state: %+v", card)
			}
		}
		return
	}
	test.Fatal("grouped cancellation notice missing")
}
