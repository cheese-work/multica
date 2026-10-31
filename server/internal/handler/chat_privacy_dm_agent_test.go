package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Chat content is private to the DM's creator (MUL-8374), but the DM's own
// agent is not a bystander. Its task token carries the runtime owner as the
// user, which need not be the creator, and the agent must still read the DM it
// is answering: `multica chat history` before a decision request or a handoff,
// and `multica chat continue` to roll the DM over.
// TestDecisionLanesScopeAndNoTranscripts covers the decision-lane listing with
// the same creator/token-user split.
func TestChatPrivacyKeepsDMReadableToItsRunningAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	creatorID := dbfx.User(t, "DM creator", "dm-agent-privacy-"+uuid.NewString()+"@example.test")
	dbfx.Member(t, testWorkspaceID, creatorID, "member")
	agentID := createHandlerTestAgent(t, "dm-agent-privacy-"+uuid.NewString(), []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"creator_id": creatorID})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "private DM ask: pick lane B"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":      handlerTestRuntimeID(t),
		"chat_session_id": sessionID,
		"status":          "running",
		"started_at":      testutil.Raw("now()"),
	})
	// newRequest authenticates as testUserID; the DM belongs to creatorID.
	if creatorID == testUserID {
		t.Fatal("fixture must split the token user from the DM creator")
	}

	t.Run("chat history", func(t *testing.T) {
		withSlackHistory(t, nil)
		response := httptest.NewRecorder()
		testHandler.GetChatChannelHistory(response, withChatTestWorkspaceCtx(t, taskActorReq("/api/chat/history?limit=10", taskID)))
		if response.Code != http.StatusOK {
			t.Fatalf("history: %d %s", response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "private DM ask: pick lane B") {
			t.Fatalf("the DM's agent cannot read the DM: %s", response.Body.String())
		}
	})

	t.Run("chat continue", func(t *testing.T) {
		response := continueChatRequest(t, sessionID, "Retained goal: lane B.", taskID)
		if response.Code != http.StatusCreated {
			t.Fatalf("continue: %d %s", response.Code, response.Body.String())
		}
		var result ChatContinueResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
		dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
		if result.PreviousSessionID != sessionID || result.Session.CreatorID != creatorID {
			t.Fatalf("continuation = %+v, want it to follow %s for creator %s", result, sessionID, creatorID)
		}
	})
}
