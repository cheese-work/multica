package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2394 P5: the named human's reply directly under the ask comment answers a kind=action request.

func newActionReplyFixture(test *testing.T) askedRecordFixture {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	return fixture
}

// replyAs posts a comment as userID; parentID is "" for a top-level comment.
func (fixture askedRecordFixture) replyAs(test *testing.T, userID, parentID, content string) *testutil.Response {
	test.Helper()
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/comments", body), "id", fixture.issueID)
	request.Header.Set("X-User-ID", userID)
	return testutil.Call(test, testHandler.CreateComment, request)
}

func (fixture askedRecordFixture) answerCount(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID)
}

func TestNamedHumanReplyUnderTheAskAnswersTheActionRequest(test *testing.T) {
	fixture := newActionReplyFixture(test)
	var reply struct {
		ID string `json:"id"`
	}
	fixture.replyAs(test, fixture.humanID, fixture.parentID, "done, I ran it").Want(http.StatusCreated).JSON(&reply)

	var status, source, label, snapshot, sourceComment string
	var taskless, userless bool
	dbfx.QueryRow(test, `SELECT request.status, answer.answer_source, coalesce(answer.answered_label, ''), answer.content_snapshot, answer.source_comment_id::text, answer.answered_by_task_id IS NULL, answer.answered_by_user_id IS NULL
		FROM decision_request request JOIN decision_answer answer ON answer.decision_request_id = request.id WHERE request.id = $1`, fixture.decision.ID).
		Scan(&status, &source, &label, &snapshot, &sourceComment, &taskless, &userless)
	if status != "answered" || source != "agent_record" || label != "replied" || snapshot != "done, I ran it" || sourceComment != reply.ID || !taskless || !userless {
		test.Fatalf("answer = %s/%s/%q/%q/source %s taskless=%v userless=%v", status, source, label, snapshot, sourceComment, taskless, userless)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE request_id = $1 AND type = 'answered'`, fixture.decision.ID); n != 1 {
		test.Fatalf("answered events = %d, want 1", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote' AND parent_id = $2`, fixture.issueID, fixture.parentID); n != 1 {
		test.Fatalf("quote comments under the ask = %d, want 1", n)
	}
}

func TestReplyUnderTheAskThatDoesNotBindLeavesTheRequestOpen(test *testing.T) {
	for name, post := range map[string]func(fixture askedRecordFixture) int{
		"another member": func(fixture askedRecordFixture) int {
			fixture.replyAs(test, testUserID, fixture.parentID, "done").Want(http.StatusCreated)
			return 0
		},
		"top-level comment": func(fixture askedRecordFixture) int {
			fixture.replyAs(test, fixture.humanID, "", "done").Want(http.StatusCreated)
			return 0
		},
		"another thread": func(fixture askedRecordFixture) int {
			var other struct {
				ID string `json:"id"`
			}
			fixture.replyAs(test, testUserID, "", "an unrelated thread").Want(http.StatusCreated).JSON(&other)
			fixture.replyAs(test, fixture.humanID, other.ID, "done").Want(http.StatusCreated)
			return 0
		},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newActionReplyFixture(test)
			post(fixture)
			if n := fixture.answerCount(test); n != 0 {
				test.Fatalf("a reply that does not bind left %d answers", n)
			}
			if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, fixture.decision.ID); n != 1 {
				test.Fatal("the request left open")
			}
		})
	}
}

func TestReplyUnderTheAskOfADecisionRequestStaysAPlainComment(test *testing.T) {
	fixture := newAskedRecordFixture(test) // kind=decision
	fixture.replyAs(test, fixture.humanID, fixture.parentID, "done").Want(http.StatusCreated)
	if n := fixture.answerCount(test); n != 0 {
		test.Fatalf("a decision request took a reply-under-ask answer: %d answers", n)
	}
}

func TestReplyUnderTheAskAfterExpiryIsRefusedAndPostsNothing(test *testing.T) {
	fixture := newActionReplyFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, fixture.decision.ID)
	before := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID)
	fixture.replyAs(test, fixture.humanID, fixture.parentID, "done, sorry it is late").Want(http.StatusConflict)
	if after := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); after != before {
		test.Fatalf("a refused reply left comments: %d -> %d", before, after)
	}
	if n := fixture.answerCount(test) + dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE request_id = $1`, fixture.decision.ID); n != 0 {
		test.Fatalf("a refused reply left %d answer or event rows", n)
	}
}

// The bound DM reply to an action request carries the "replied" label.
func TestChatReplyToAnActionRequestIsLabelledReplied(test *testing.T) {
	agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Action reply")
	requestID, _, _ := seedDMDecision(test, agentID, sessionID, "Run the command", "open", "1 minute")
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, requestID)
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "ran it", "decision_request_id": requestID})
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND answered_label = 'replied'`, requestID); n != 1 {
		test.Fatalf("replied answers = %d, want 1", n)
	}
}

// The answer wakes through its receipts: the reply itself starts no run.
func TestBoundReplyUnderTheAskStartsNoRunFromTheComment(test *testing.T) {
	fixture := newActionReplyFixture(test)
	before := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'dispatched')`, fixture.issueID)
	fixture.replyAs(test, fixture.humanID, fixture.parentID, "done").Want(http.StatusCreated)
	if after := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'dispatched')`, fixture.issueID); after != before {
		test.Fatalf("queued runs went %d -> %d", before, after)
	}
}
