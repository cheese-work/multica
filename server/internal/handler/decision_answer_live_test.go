package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3555 (T1b, plan M17/M18): an answer from any view is checked against the
// live decision. Each dead-answer case is a typed refusal: a stable code prefix
// and its own status, and the refused answer leaves nothing behind.
func TestCardAnswerDeadCasesAreTypedRefusals(test *testing.T) {
	stale := int32(99)
	for name, tc := range map[string]struct {
		setup  func(test *testing.T, fixture askedRecordFixture)
		input  decisionCardAnswerInput
		member string // answering user; empty = the request's human
		status int
		code   string
	}{
		"unauthorized": {member: testUserID, status: http.StatusForbidden, code: "unauthorized_answer:", input: decisionCardAnswerInput{Option: "Option A"}},
		"stale":        {status: http.StatusConflict, code: "stale_answer:", input: decisionCardAnswerInput{Option: "Option A", AnswerRevision: &stale}},
		"expired": {status: http.StatusConflict, code: "expired_answer:", input: decisionCardAnswerInput{Option: "Option A"}, setup: func(test *testing.T, fixture askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, fixture.decision.ID)
		}},
		"superseded": {status: http.StatusConflict, code: "superseded_answer:", input: decisionCardAnswerInput{Option: "Option A"}, setup: func(test *testing.T, fixture askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', withdrawn_at = clock_timestamp(), withdraw_reason = 'replaced by a newer ask' WHERE id = $1`, fixture.decision.ID)
		}},
		"used": {status: http.StatusConflict, code: "used_answer:", input: decisionCardAnswerInput{Option: "Option B"}, setup: func(test *testing.T, fixture askedRecordFixture) {
			if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
				test.Fatal(err)
			}
		}},
	} {
		test.Run(name, func(test *testing.T) {
			if tc.member == testUserID && testUserID == "" {
				test.Skip("no second member")
			}
			fixture := newCardFixture(test)
			if tc.setup != nil {
				tc.setup(test, fixture)
			}
			if tc.member == testUserID && fixture.humanID == testUserID {
				test.Skip("fixture human is the test user")
			}
			who := fixture.humanID
			if tc.member != "" {
				who = tc.member
			}
			answersBefore := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID)
			_, err := testHandler.answerDecisionRequestTx(fixture.request(), fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(who), tc.input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != tc.status || !strings.HasPrefix(refusal.message, tc.code) {
				test.Fatalf("answer = %v, want %d refusal %q", err, tc.status, tc.code)
			}
			if after := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); after != answersBefore {
				test.Fatalf("a refused answer left rows: %d -> %d", answersBefore, after)
			}
		})
	}
}

// A card mirrored in another DM is the same decision: the answer is keyed by the
// decision id alone. With the original lane archived, an active DM of the same
// person answers it: one shared answer, history in the archived lane untouched,
// and the original view then reads it as used.
func TestAnswerThroughAMirroredViewUpdatesTheOneSharedDecision(test *testing.T) {
	fixture := newCardFixture(test)
	mirror := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Mirror DM"})
	dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
	originalMessages := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatalf("answer through the active mirror = %v, want recorded", err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, fixture.decision.ID); n != 1 {
		test.Fatalf("shared decision answered rows = %d, want 1", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND source_chat_session_id = $2`, fixture.decision.ID, mirror); n != 1 {
		test.Fatalf("answers recorded from the mirror = %d, want 1", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID); n != originalMessages {
		test.Fatalf("archived lane messages %d -> %d, want read-only history", originalMessages, n)
	}
	_, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.message, "used_answer:") {
		test.Fatalf("second view answer = %v, want used_answer", err)
	}
}

// Delivered, read and answered are three stored states on the one decision:
// reading the DM never answers, answering never rewrites delivered or read.
func TestDeliveredReadAnsweredAreSeparateStoredStates(test *testing.T) {
	fixture := newCardFixture(test)
	states := func() (delivered, read bool, status string) {
		dbfx.QueryRow(test, `SELECT ping_notified_at IS NOT NULL, read_at IS NOT NULL, status FROM decision_request WHERE id = $1`, fixture.decision.ID).Scan(&delivered, &read, &status)
		return
	}
	dbfx.Exec(test, `UPDATE decision_request SET ping_notified_at = clock_timestamp() WHERE id = $1`, fixture.decision.ID)
	if d, r, s := states(); !d || r || s != "open" {
		test.Fatalf("after delivery: delivered=%v read=%v status=%s, want true false open", d, r, s)
	}
	session, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(fixture.sessionID))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testHandler.markChatSessionRead(context.Background(), session); err != nil {
		test.Fatal(err)
	}
	if d, r, s := states(); !d || !r || s != "open" {
		test.Fatalf("after read: delivered=%v read=%v status=%s, want true true open", d, r, s)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	if d, r, s := states(); !d || !r || s != "answered" {
		test.Fatalf("after answer: delivered=%v read=%v status=%s, want true true answered", d, r, s)
	}
}
