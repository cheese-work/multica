package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// singleCallFixture is a running assigned task with no decision intent: the
// one-call request must open, use and seal its own window.
type singleCallFixture struct {
	intentFixture
	ask, footer string
	sourceID    string
	details     decisiontx.Ask
}

func newSingleCallFixture(test *testing.T) singleCallFixture {
	test.Helper()
	fixture := newIntentFixture(test)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, fixture.issueID)
	sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "agent", "author_id": fixture.agentID, "source_task_id": fixture.taskID, "content": "Scoped source context."})
	details := structuredDecisionAsk(fixture.issueID, sourceID, "Approve the reversible change?")
	return singleCallFixture{intentFixture: fixture, ask: details.Content(handlerTestWorkspaceSlug), footer: "Next owner: none", sourceID: sourceID, details: details}
}

func (fixture singleCallFixture) body() map[string]any {
	return map[string]any{"issue_id": fixture.issueID, "for": fixture.humanID, "ask": fixture.details, "footer": fixture.footer, "escalation_class": "reversible"}
}

func (fixture singleCallFixture) call(body map[string]any) *http.Request {
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func (fixture singleCallFixture) counts(test *testing.T) (comments, messages, pings int) {
	test.Helper()
	comments = dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND id <> $2`, fixture.issueID, fixture.sourceID)
	messages = dbfx.Count(test, `SELECT count(*) FROM chat_message message JOIN chat_session session ON session.id = message.chat_session_id WHERE session.creator_id = $1 AND session.agent_id = $2`, fixture.humanID, fixture.agentID)
	pings = dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, fixture.humanID)
	return comments, messages, pings
}

func TestDecisionSingleCallOwnerRecipientIDs(test *testing.T) {
	for _, identifier := range []string{"user", "member"} {
		for _, lane := range []string{"default", "session", "new_dm", "intent"} {
			test.Run(identifier+"/"+lane, func(test *testing.T) {
				fixture := newSingleCallFixture(test)
				var memberID string
				dbfx.QueryRow(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2 RETURNING id`, testWorkspaceID, fixture.humanID).Scan(&memberID)
				body := fixture.body()
				if identifier == "member" {
					body["for"] = memberID
				}
				if lane == "session" {
					body["session_id"] = laneNamingIssue(test, fixture.issueID, fixture.agentID, fixture.humanID, "Owner decisions")
				} else if lane == "new_dm" || lane == "intent" {
					body["new_dm"] = true
				}
				if lane == "intent" {
					openLaneIntent(test, fixture.intentFixture, fixture.mention())
				}
				var created map[string]any
				testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated).JSON(&created)
				if created["human_user_id"] != fixture.humanID {
					test.Fatalf("recipient = %v, want user ID %s", created["human_user_id"], fixture.humanID)
				}
				wantComments, wantMessages := 1, 1
				if lane == "intent" {
					wantComments++
				}
				if lane == "session" {
					wantMessages++ // the seeded message that names the issue
				}
				if comments, messages, pings := fixture.counts(test); comments != wantComments || messages != wantMessages || pings != 1 {
					test.Fatalf("comments=%d messages=%d pings=%d, want %d/%d/1", comments, messages, pings, wantComments, wantMessages)
				}
			})
		}
	}
}

func TestDecisionSingleCallRejectsForeignMemberID(test *testing.T) {
	fixture := newSingleCallFixture(test)
	workspaceID := dbfx.Workspace(test, "Foreign workspace", "foreign-"+fixture.agentID)
	body := fixture.body()
	body["for"] = dbfx.Member(test, workspaceID, fixture.humanID, "owner")
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusBadRequest)
	if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 0 || pings != 0 {
		test.Fatalf("refused request wrote comments=%d messages=%d pings=%d", comments, messages, pings)
	}
}

func TestDecisionSingleCallWritesOneCommentOneMessageOnePing(test *testing.T) {
	fixture := newSingleCallFixture(test)
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&created)
	if comments, messages, pings := fixture.counts(test); comments != 1 || messages != 1 || pings != 1 {
		test.Fatalf("comments=%d messages=%d pings=%d, want 1/1/1", comments, messages, pings)
	}
	requestID := created["id"].(string)
	var content, kind, intentID, commentRequestID string
	dbfx.QueryRow(test, `SELECT content, decision_comment_kind, decision_intent_id::text, decision_request_id::text FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`, fixture.issueID).Scan(&content, &kind, &intentID, &commentRequestID)
	if content != fixture.ask+"\n\n"+fixture.footer || kind != "ask" || commentRequestID != requestID {
		test.Fatalf("wrong ask comment: %q kind=%s request=%s", content, kind, commentRequestID)
	}
	var message, messageKind string
	dbfx.QueryRow(test, `SELECT message.content, message.message_kind FROM chat_message message JOIN decision_request request ON request.ping_message_id = message.id WHERE request.id = $1`, requestID).Scan(&message, &messageKind)
	if message != fixture.ask || messageKind != "decision_request" {
		test.Fatalf("DM must hold the ask only: %q kind=%s", message, messageKind)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_intent intent JOIN comment ask ON ask.id = intent.comment_id WHERE intent.id = $1 AND ask.decision_request_id = $2 AND intent.consumed_at IS NOT NULL AND intent.sealed_at IS NOT NULL AND intent.consumed_by_request_id = $2`, intentID, requestID) != 1 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND human_user_id = $2 AND consumed_at IS NOT NULL AND consumed_by_request_id = $3`, intentID, fixture.humanID, requestID) != 1 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND decision_intent_id = $2 AND issue_comment_id = (SELECT id FROM comment WHERE issue_id = $3 AND decision_comment_kind = 'ask')`, requestID, intentID, fixture.issueID) != 1 {
		test.Fatal("intent was not anchored on the ask comment, consumed and sealed")
	}
}

func TestDecisionSingleCallOmitsFooterSeparatorWhenFooterEmpty(test *testing.T) {
	fixture := newSingleCallFixture(test)
	body := fixture.body()
	delete(body, "footer")
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	var content string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`, fixture.issueID).Scan(&content)
	if content != fixture.ask {
		test.Fatalf("content = %q, want the bare ask", content)
	}
}

func TestDecisionSingleCallLaneSelection(test *testing.T) {
	lane := func(test *testing.T, fixture singleCallFixture, title string) string {
		return dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": title})
	}
	sessionOf := func(test *testing.T, fixture singleCallFixture, created map[string]any) (sessionID, title string) {
		dbfx.QueryRow(test, `SELECT session.id::text, session.title FROM chat_session session JOIN decision_request request ON request.chat_session_id = session.id WHERE request.id = $1`, created["id"]).Scan(&sessionID, &title)
		return sessionID, title
	}
	test.Run("new_dm_when_nothing_matches", func(test *testing.T) {
		fixture := newSingleCallFixture(test)
		lane(test, fixture, "unrelated")
		var created map[string]any
		testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&created)
		if _, title := sessionOf(test, fixture, created); title != "Decisions: intent issue" {
			test.Fatalf("title = %q, want a new Decisions DM", title)
		}
	})
	test.Run("topic_dm_reused_over_legacy_lanes", func(test *testing.T) {
		fixture := newSingleCallFixture(test)
		first := lane(test, fixture, "first")
		fixture.discussIn(test, first)
		lane(test, fixture, "newer unrelated")
		var one, two map[string]any
		testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&one)
		next := fixture.body()
		next["ask"] = structuredDecisionAsk(fixture.issueID, fixture.sourceID, "A second question?")
		testutil.Call(test, testHandler.RequestDecision, fixture.call(next)).Want(http.StatusCreated).JSON(&two)
		firstID, title := sessionOf(test, fixture, one)
		if secondID, _ := sessionOf(test, fixture, two); secondID != firstID || firstID == first || title != "Decisions: intent issue" {
			test.Fatalf("asks went to %s then %s (%q), want one topic DM apart from legacy lane %s", firstID, secondID, title, first)
		}
	})
	test.Run("explicit_new_dm_overrides_default", func(test *testing.T) {
		fixture := newSingleCallFixture(test)
		lane(test, fixture, "existing")
		body := fixture.body()
		body["new_dm"] = true
		var created map[string]any
		testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated).JSON(&created)
		if _, title := sessionOf(test, fixture, created); title != "Decisions: intent issue" {
			test.Fatalf("title = %q, want the explicit new DM", title)
		}
	})
}

func TestDecisionSingleCallParent(test *testing.T) {
	fixture := newSingleCallFixture(test)
	parent := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": testUserID, "content": "root", "type": "comment"})
	body := fixture.body()
	body["parent_id"] = parent
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	if dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask' AND parent_id = $2`, fixture.issueID, parent) != 1 {
		test.Fatal("ask did not reply under the given parent")
	}
	foreign := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": dbfx.Issue(test, "other issue"), "author_type": "member", "author_id": testUserID, "content": "elsewhere", "type": "comment"})
	body = fixture.body()
	body["parent_id"] = foreign
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusBadRequest)
	if comments, _, _ := fixture.counts(test); comments != 2 {
		test.Fatalf("comments = %d, want the root and the first ask only", comments)
	}
}

func TestDecisionSingleCallRefusalsWriteNothing(test *testing.T) {
	for _, reason := range []string{"pat", "cli_jwt", "unstamped", "invalid_task", "other_task", "other_agent", "chat_task", "other_issue", "non_assignee", "done_issue", "non_member", "private_agent", "no_target", "bad_class", "empty_ask", "foreign_session"} {
		test.Run(reason, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			body := fixture.body()
			headers := map[string]string{}
			switch reason {
			case "pat", "cli_jwt":
				headers["X-Actor-Source"] = reason
			case "unstamped":
				headers["X-Actor-Source"] = ""
			case "invalid_task":
				headers["X-Task-ID"] = "bad-task"
			case "other_task":
				headers["X-Task-ID"] = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": dbfx.Issue(test, "other task issue"), "status": "running", "runtime_id": handlerTestRuntimeID(test)})
			case "other_agent":
				headers["X-Agent-ID"] = createHandlerTestAgent(test, "wrong requester", []byte("[]"))
			case "chat_task":
				dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "chat task"}), fixture.taskID)
			case "other_issue":
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "wrong issue"), fixture.taskID)
			case "non_assignee":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, fixture.issueID)
			case "done_issue":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			case "non_member":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
			case "private_agent":
				dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fixture.agentID)
			case "no_target":
				delete(body, "for")
			case "bad_class":
				body["escalation_class"] = "unknown"
			case "empty_ask":
				body["ask"] = " \n"
			case "foreign_session":
				body["session_id"] = dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": testUserID, "title": "someone else's lane"})
			}
			request := fixture.call(body)
			for name, value := range headers {
				if value == "" {
					request.Header.Del(name)
				} else {
					request.Header.Set(name, value)
				}
			}
			if status := testutil.Call(test, testHandler.RequestDecision, request).Code; status < 400 || status >= 500 {
				test.Fatalf("status = %d, want a 4xx refusal", status)
			}
			if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 0 || pings != 0 ||
				dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID) != 0 ||
				dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1`, fixture.issueID) != 0 {
				test.Fatalf("refusal left writes: comments=%d messages=%d pings=%d", comments, messages, pings)
			}
		})
	}
}

func TestDecisionSingleCallAskCommentBindsRecords(test *testing.T) {
	fixture := newSingleCallFixture(test)
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&created)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		test.Fatal(err)
	}
	located, err := locateDecisionRecordContext(context.Background(), testHandler.Queries, issue, parseUUID(created["issue_comment_id"].(string)))
	if err != nil || uuidToString(located.ID) != created["id"] {
		test.Fatalf("record under the ask comment must bind its request: %+v %v", located, err)
	}
}

func TestDecisionLegacyFlowStillAcceptsFooterAndRejectsParent(test *testing.T) {
	test.Run("footer", func(test *testing.T) {
		fixture := newRequestCoreFixture(test, false)
		input := fixture.input()
		input.Footer = "Next owner: none"
		result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
		if err != nil {
			test.Fatal(err)
		}
		if result.Comment.Content != input.Ask+"\n\n"+input.Footer || result.Message.Content != input.Ask || result.Comment.ParentID != parseUUID(fixture.tagID) {
			test.Fatalf("legacy ask must stay under the intent comment with the footer on the comment only: %+v", result.Comment)
		}
	})
	test.Run("parent", func(test *testing.T) {
		fixture := newRequestCoreFixture(test, false)
		input := fixture.input()
		input.ParentID = parseUUID(fixture.tagID)
		_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
		requireRequestRefusal(test, err)
		fixture.assertCounts(test, 0)
	})
}

func TestDecisionSingleCallAtomicRollback(test *testing.T) {
	fixture := newSingleCallFixture(test)
	useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: CreateChatMessage", fail: &pgconn.PgError{Code: "23514", Message: "injected message insert refusal"}})
	input := decisionRequestInput{NewDM: true, HumanUserID: parseUUID(fixture.humanID), Ask: fixture.ask, EscalationClass: "reversible"}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testHandler.createDecisionRequest(fixture.call(nil), issue, input); err == nil {
		test.Fatal("injected message failure committed")
	}
	if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 0 || pings != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1`, fixture.issueID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID) != 0 {
		test.Fatal("failed single call left comment, intent, DM or ping behind")
	}
}

func TestDecisionSingleCallAuthCannotBeForged(test *testing.T) {
	fixture := newSingleCallFixture(test)
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(http.HandlerFunc(testHandler.RequestDecision))
	for source, token := range provenanceCredentials(test, fixture.agentID) {
		test.Run(source, func(test *testing.T) {
			request := fixture.call(fixture.body())
			request.Header.Set("Authorization", "Bearer "+token)
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
			if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 0 || pings != 0 {
				test.Fatalf("forged %s left writes: %d/%d/%d", source, comments, messages, pings)
			}
		})
	}
}

func (fixture singleCallFixture) assertNoWrites(test *testing.T) {
	test.Helper()
	if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 0 || pings != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1`, fixture.issueID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1 AND agent_id = $2`, fixture.humanID, fixture.agentID) != 0 {
		test.Fatalf("refusal left writes: comments=%d messages=%d pings=%d", comments, messages, pings)
	}
}

// The single-call ping is part of the request transaction: a failed ping must
// refuse the call and leave nothing behind, not return 201 for a retry later.
func TestDecisionSingleCallPingFailureRollsBackEverything(test *testing.T) {
	fixture := newSingleCallFixture(test)
	useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: MarkDecisionPingNotified", fail: &pgconn.PgError{Code: "23514", Message: "injected ping receipt failure"}})
	if status := testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Code; status < 500 || status >= 600 {
		test.Fatalf("status = %d, want a 5xx when the ping cannot be recorded", status)
	}
	fixture.assertNoWrites(test)
}

// The stored ask comment is ask + "\n\n" + footer; the limit applies to that
// composed text, not to the two fields summed.
func TestDecisionSingleCallComposedCommentLimit(test *testing.T) {
	const limit = maxCommentContentBytes
	cases := []struct {
		name        string
		ask, footer int
		want        int
	}{
		{"footer_at_limit", limit - 2 - 10, 10, http.StatusCreated},
		{"footer_one_over", limit - 2 - 10 + 1, 10, http.StatusBadRequest},
		{"fields_sum_to_limit_but_separator_overflows", limit - 10, 10, http.StatusBadRequest},
		{"no_footer_at_limit", limit, 0, http.StatusCreated},
		{"no_footer_one_over", limit + 1, 0, http.StatusBadRequest},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			body := fixture.body()
			ask := fixture.details
			ask.Decision = strings.Repeat("a", tc.ask-len(ask.Content(handlerTestWorkspaceSlug))+len(ask.Decision))
			body["ask"] = ask
			body["footer"] = strings.Repeat("f", tc.footer)
			testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(tc.want)
			if tc.want == http.StatusBadRequest {
				fixture.assertNoWrites(test)
				return
			}
			var size int
			dbfx.QueryRow(test, `SELECT octet_length(content) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`, fixture.issueID).Scan(&size)
			if size != limit {
				test.Fatalf("stored comment = %d bytes, want exactly %d", size, limit)
			}
		})
	}
}
