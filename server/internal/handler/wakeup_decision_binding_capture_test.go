package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5445: a wakeup bound to a decision request fires only on the answer to
// that request, never on another comment by the member it waits for.
func TestDecisionBoundWakeupFiresOnlyOnItsOwnAnswer(t *testing.T) {
	comment := func(t *testing.T, f askGuardWakeupFixture, authorType, authorID, parentID string) string {
		t.Helper()
		cols := testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issueID, "author_type": authorType, "author_id": authorID, "content": "text"}
		if parentID != "" {
			cols["parent_id"] = parentID
		}
		return dbfx.Insert(t, "comment", cols)
	}
	// answerTo records the answer to requestID the way delivery does: the answer
	// comment, the answer row, then the member-attributed wakeup capture.
	answerTo := func(t *testing.T, f askGuardWakeupFixture, requestID, parentID string) {
		t.Helper()
		commentID := comment(t, f, "agent", f.agentID, parentID)
		answerID := dbfx.Insert(t, "decision_answer", testutil.Cols{"decision_request_id": requestID, "content_snapshot": "Yes",
			"effective_at": time.Now().Add(-time.Minute), "issue_comment_id": commentID, "answer_source": "card",
			"answered_by_user_id": f.humanID, "chat_message_ids": "{}"})
		tx, err := dbfx.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if err := captureDecisionAnswerWakeup(t.Context(), tx, parseUUID(f.issueID), parseUUID(answerID), parseUUID(testWorkspaceID)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		act  func(t *testing.T, f askGuardWakeupFixture, bound, other string)
		want int
	}{
		{"answer to its own request", func(t *testing.T, f askGuardWakeupFixture, bound, _ string) { answerTo(t, f, bound, "") }, 1},
		{"answer to its own request as a nested reply", func(t *testing.T, f askGuardWakeupFixture, bound, _ string) {
			answerTo(t, f, bound, comment(t, f, "agent", f.agentID, ""))
		}, 1},
		{"answer to another request", func(t *testing.T, f askGuardWakeupFixture, _, other string) { answerTo(t, f, other, "") }, 0},
		{"unrelated member comment", func(t *testing.T, f askGuardWakeupFixture, _, _ string) { comment(t, f, "member", f.humanID, "") }, 0},
		{"unrelated nested member reply", func(t *testing.T, f askGuardWakeupFixture, _, _ string) {
			comment(t, f, "member", f.humanID, comment(t, f, "agent", f.agentID, ""))
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAskGuardWakeupFixture(t)
			bound := f.request(t, f.issueID, f.humanID, "open")
			other := f.request(t, f.issueID, f.humanID, "open")
			f.create(t, f.replyWait("wait for the answer", map[string]any{"decision_request_id": bound, "filter_actor_type": "member", "filter_actor_id": f.humanID})).Want(http.StatusCreated)
			c.act(t, f, bound, other)
			if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, f.issueID); got != c.want {
				t.Errorf("receipts = %d, want %d", got, c.want)
			}
		})
	}
}
