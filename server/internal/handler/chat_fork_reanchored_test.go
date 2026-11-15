package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2895: a fork copies history with its original time. A message a retry
// claim restamped keeps the restamped time, so the copy must keep reanchored_at
// too, or a decision record could take the fork copy as a fresh answer.
func TestForkChatSessionKeepsTheReanchoredMarker(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-reanchored", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID, testutil.Cols{"title": "Reanchored"})
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "plain", "created_at": testutil.Raw("now() - interval '3 minutes'")})
	forkMessage(test, sourceID, testutil.Cols{"role": "user", "content": "restamped", "created_at": testutil.Raw("now() - interval '2 minutes'"), "reanchored_at": testutil.Raw("now() - interval '1 minute'")})

	var result ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "reanchored-op")).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)

	for content, want := range map[string]int{"restamped": 1, "plain": 0} {
		if got := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND content = $2 AND reanchored_at IS NOT NULL`, result.Session.ID, content); got != want {
			test.Fatalf("fork copy of %q with reanchored_at = %d rows, want %d", content, got, want)
		}
	}
}
