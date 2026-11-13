package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func deliverRecoveries(t *testing.T) {
	t.Helper()
	if err := testHandler.deliverAskRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// recoveryScenario opens an intent that names A and B, serves A, and ends the
// run: B is the one unserved target. It returns A, B and B's event.
func recoveryScenario(t *testing.T) (fixture intentFixture, a, b, eventID string) {
	t.Helper()
	setAskGuardMode(t, "block")
	fixture = newIntentFixture(t)
	a, b, intentID := twoTargetIntent(t, fixture)
	dbfx.Exec(t, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, a)
	endTaskAndSeal(t, fixture, intentID)
	reconcileAskGuard(t)
	dbfx.QueryRow(t, `SELECT id::text FROM ask_guard_event WHERE intent_id = $1`, intentID).Scan(&eventID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ask_recovery_notice WHERE event_id = $1`, eventID)
	})
	return fixture, a, b, eventID
}

func noticeMessages(t *testing.T, member string) int {
	return dbfx.Count(t, `SELECT count(*) FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id
		WHERE s.creator_id = $1 AND m.message_kind = 'ask_recovery'`, member)
}

func noticeInbox(t *testing.T, member, issueID string) int {
	return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND issue_id = $2 AND details ? 'ask_recovery_notice_id'`, member, issueID)
}

// TestAskRecoveryReachesOnlyTheAddressedMember: with two members, the notice
// goes to the unserved one in one transaction, quotes the source verbatim, and
// never becomes a decision request. A repeated pass sends nothing more.
func TestAskRecoveryReachesOnlyTheAddressedMember(t *testing.T) {
	fixture, a, b, eventID := recoveryScenario(t)
	deliverRecoveries(t)
	deliverRecoveries(t)

	if n := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE event_id = $1 AND member_id = $2 AND state = 'delivered'`, eventID, b); n != 1 {
		t.Fatalf("notices for B = %d, want 1", n)
	}
	if got, other := noticeMessages(t, b), noticeMessages(t, a); got != 1 || other != 0 {
		t.Fatalf("DM messages: B=%d A=%d, want 1 and 0", got, other)
	}
	if got, other := noticeInbox(t, b, fixture.issueID), noticeInbox(t, a, fixture.issueID); got != 1 || other != 0 {
		t.Fatalf("inbox items: B=%d A=%d, want 1 and 0", got, other)
	}
	var content string
	dbfx.QueryRow(t, `SELECT m.content FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.creator_id = $1 AND m.message_kind = 'ask_recovery'`, b).Scan(&content)
	var source string
	dbfx.QueryRow(t, `SELECT c.content FROM decision_intent i JOIN comment c ON c.id = i.comment_id JOIN ask_guard_event e ON e.intent_id = i.id WHERE e.id = $1`, eventID).Scan(&source)
	for _, want := range []string{source, "automatic recovery", "mention://issue/" + fixture.issueID, "comment thread under this notice"} {
		if !strings.Contains(content, want) {
			t.Fatalf("notice lacks %q:\n%s", want, content)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'agent' AND content LIKE '%automatic recovery%'`, fixture.issueID); n != 1 {
		t.Fatalf("issue notice comments = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); n != 0 {
		t.Fatalf("a notice created %d decision requests", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE id = $1 AND cleared_at IS NOT NULL AND delivered_at IS NOT NULL`, eventID); n != 1 {
		t.Fatal("event is not delivered")
	}
}

// TestAskRecoveryIgnoresMutedPreferences: the recovery ping is mandatory, so a
// recipient who muted activity on issues they are not assigned to still gets
// the inbox item, and the event is not marked delivered without it.
func TestAskRecoveryIgnoresMutedPreferences(t *testing.T) {
	fixture, _, b, eventID := recoveryScenario(t)
	dbfx.Exec(t, `INSERT INTO notification_preference (workspace_id, user_id, preferences) VALUES ($1, $2, '{"unassigned_activity":"muted"}')`, testWorkspaceID, b)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM notification_preference WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, b)
	})
	deliverRecoveries(t)
	if noticeInbox(t, b, fixture.issueID) != 1 {
		t.Fatal("a muted recipient got no recovery inbox item")
	}
	if dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE id = $1 AND delivered_at IS NOT NULL`, eventID) != 1 {
		t.Fatal("event is not delivered")
	}
}

// TestAskRecoveryCrashAroundCommit: a crash just before the commit sends
// nothing, a crash just after leaves the full record, and the retry after
// either leaves exactly one notice.
func TestAskRecoveryCrashAroundCommit(t *testing.T) {
	for _, c := range []struct {
		point      string
		afterCrash int
	}{{"before_commit", 0}, {"after_commit", 1}} {
		t.Run(c.point, func(t *testing.T) {
			fixture, _, b, eventID := recoveryScenario(t)
			askRecoveryFault = func(point string) error {
				if point == c.point {
					return errors.New("crash at " + point)
				}
				return nil
			}
			t.Cleanup(func() { askRecoveryFault = nil })
			if err := testHandler.deliverAskRecoveries(context.Background()); err == nil {
				t.Fatal("expected the injected crash")
			}
			askRecoveryFault = nil
			if got := noticeMessages(t, b); got != c.afterCrash {
				t.Fatalf("messages after crash = %d, want %d", got, c.afterCrash)
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE event_id = $1`, eventID); got != c.afterCrash {
				t.Fatalf("notices after crash = %d, want %d", got, c.afterCrash)
			}
			deliverRecoveries(t)
			deliverRecoveries(t)
			if noticeMessages(t, b) != 1 || noticeInbox(t, b, fixture.issueID) != 1 ||
				dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE event_id = $1`, eventID) != 1 ||
				dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%automatic recovery%'`, fixture.issueID) != 1 {
				t.Fatal("retry did not leave exactly one notice, message, comment and inbox item")
			}
		})
	}
}

// TestAskRecoveryNeedsAccessSourceAndOneMember: an event that cannot be sent
// safely sends nothing and records why, for the repair slice.
func TestAskRecoveryNeedsAccessSourceAndOneMember(t *testing.T) {
	cases := []struct {
		name, reason string
		setup        func(fixture intentFixture) (member any, source any)
	}{
		{"recipient never had access", "recipient_no_access", func(f intentFixture) (any, any) {
			return dbfx.User(t, "Outsider", "outsider-"+uuid.NewString()+"@test.invalid"), "please decide"
		}},
		{"access revoked before send", "recipient_no_access", func(f intentFixture) (any, any) {
			u := dbfx.User(t, "Leaver", "leaver-"+uuid.NewString()+"@test.invalid")
			m := dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": u, "role": "member"})
			dbfx.Exec(t, `DELETE FROM member WHERE id = $1`, m)
			return u, "please decide"
		}},
		{"no stored source", "no_source", func(f intentFixture) (any, any) { return f.humanID, nil }},
		{"no single member", "member_not_single", func(f intentFixture) (any, any) { return nil, "please decide" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setAskGuardMode(t, "block")
			fixture := newIntentFixture(t)
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
			member, source := c.setup(fixture)
			eventID := dbfx.Insert(t, "ask_guard_event", testutil.Cols{"id": uuid.NewString(), "workspace_id": testWorkspaceID, "issue_id": fixture.issueID,
				"task_id": fixture.taskID, "operation_kind": "comment", "payload_sha256": uuid.NewString(), "mode": "block", "member_id": member, "source_text": source})
			deliverRecoveries(t)
			var reason *string
			var cleared bool
			dbfx.QueryRow(t, `SELECT repair_reason, cleared_at IS NOT NULL FROM ask_guard_event WHERE id = $1`, eventID).Scan(&reason, &cleared)
			if reason == nil || *reason != c.reason || cleared {
				t.Fatalf("repair_reason=%v cleared=%v, want %q and open", reason, cleared, c.reason)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE event_id = $1`, eventID) +
				dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE message_kind = 'ask_recovery' AND content LIKE '%please decide%'`) +
				dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%automatic recovery%'`, fixture.issueID); n != 0 {
				t.Fatalf("a notice, DM or excerpt was written: %d rows", n)
			}
		})
	}
}

// TestAskRecoveryAnswerAndAccessAfterDelivery: only the addressed member's
// reply in the notice thread answers it, and a member who lost access can
// neither read the notice nor answer it.
func TestAskRecoveryAnswerAndAccessAfterDelivery(t *testing.T) {
	fixture, a, b, eventID := recoveryScenario(t)
	deliverRecoveries(t)
	var commentID, sessionID string
	dbfx.QueryRow(t, `SELECT comment_id::text, chat_session_id::text FROM ask_recovery_notice WHERE event_id = $1`, eventID).Scan(&commentID, &sessionID)
	reply := func(user string) *testutil.Response {
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/comments", map[string]any{"content": "go ahead", "parent_id": commentID}), "id", fixture.issueID)
		req.Header.Set("X-User-ID", user)
		return testutil.Call(t, testHandler.CreateComment, req)
	}
	state := func() string {
		var s string
		dbfx.QueryRow(t, `SELECT state FROM ask_recovery_notice WHERE event_id = $1`, eventID).Scan(&s)
		return s
	}
	// The member context is built while the member exists: it stands for a
	// request already in flight when access is revoked.
	listRequest := func(user string) *http.Request {
		req := withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages", nil), "sessionId", sessionID)
		req.Header.Set("X-User-ID", user)
		return chatPendingCtxAs(t, req, user)
	}
	read := func(req *http.Request) *testutil.Response { return testutil.Call(t, testHandler.ListChatMessages, req) }

	reply(a).Want(http.StatusCreated) // A was served by someone else: not the addressee
	if state() != "delivered" {
		t.Fatalf("a reply by another member answered the notice: %s", state())
	}
	staleRead := listRequest(b)
	if !strings.Contains(read(listRequest(b)).Want(http.StatusOK).Text(), "automatic recovery") {
		t.Fatal("the addressed member cannot read the notice")
	}
	reply(b).Want(http.StatusCreated)
	if state() != "answered" {
		t.Fatalf("state after the addressee replied = %s, want answered", state())
	}

	dbfx.Exec(t, `UPDATE ask_recovery_notice SET state = 'delivered', answered_at = NULL WHERE event_id = $1`, eventID)
	dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, b)
	if resp := read(staleRead); resp.Code < 400 || strings.Contains(resp.Text(), "automatic recovery") {
		t.Fatalf("a member without access read the notice: %d %s", resp.Code, resp.Text())
	}
	if resp := reply(b); resp.Code < 400 {
		t.Fatalf("a member without access answered: %d", resp.Code)
	}
	if state() != "delivered" {
		t.Fatalf("a refused reply changed the state to %s", state())
	}
}
