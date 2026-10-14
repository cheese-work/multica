package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func assertDecisionPingUnread(test *testing.T, humanID, sessionID string) {
	test.Helper()
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(humanID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	for _, status := range []string{"active", "all"} {
		request := newRequestAs(humanID, http.MethodGet, "/api/chat/sessions?status="+status, nil)
		request.Header.Set("X-Actor-Source", "interactive_jwt")
		request = request.WithContext(middleware.SetMemberContext(request.Context(), testWorkspaceID, member))
		var sessions []ChatSessionResponse
		testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		found := false
		for _, session := range sessions {
			if session.ID == sessionID {
				found = true
				if session.UnreadCount != 1 || !session.HasUnread {
					test.Errorf("%s list: ping unread count=%d has_unread=%v, want 1", status, session.UnreadCount, session.HasUnread)
				}
			}
		}
		if !found {
			test.Errorf("%s list: DM %s missing", status, sessionID)
		}
	}
}

func TestDecisionRequestPingCountsAsUnread(test *testing.T) {
	for _, newDM := range []bool{false, true} {
		test.Run(map[bool]string{false: "existing DM", true: "new DM"}[newDM], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			input := fixture.input()
			if newDM {
				input = decisionRequestInput{NewDM: true, HumanUserID: parseUUID(fixture.humanID), Ask: "Approve the reversible change?", EscalationClass: "reversible"}
			}
			result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
			if err != nil {
				test.Fatal(err)
			}
			if newDM != (result.CreatedSession != nil) {
				test.Fatalf("created session = %v, want new DM %v", result.CreatedSession, newDM)
			}
			assertDecisionPingUnread(test, fixture.humanID, uuidToString(result.Message.ChatSessionID))
		})
	}
}
