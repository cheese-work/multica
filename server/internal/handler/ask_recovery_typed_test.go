package handler

import (
	"net/http"
	"strings"
	"testing"
)

// A4 (CHE-7292): a comment with --ask <member> or a move with --waiting-on
// member:<id> is a typed ask. The addressed member gets one ask_recovery notice
// through the leased recovery pass once the run ends, whatever the S4 and S5
// modes say about untyped asks. Delivery is idempotent per comment and per move.

const typedAskText = "Cheese, please pick option A or B so the work can continue."

func typedAskFixture(t *testing.T) askGuardCommentFixture {
	t.Helper()
	f := newWaitingMoveFixture(t)
	dbfx.Cleanup(t, `DELETE FROM ask_recovery_notice WHERE issue_id = $1`, f.issueID)
	return f
}

func (f askGuardCommentFixture) endRun(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID)
}

func (f askGuardCommentFixture) notices(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice WHERE issue_id = $1 AND member_id = $2 AND state = 'delivered'`, f.issueID, f.humanID)
}

func TestTypedAskCommentSendsOneNotice(t *testing.T) {
	setWaitingAudienceMode(t, "warn")
	f := typedAskFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": typedAskText, "ask": f.humanID}).Want(http.StatusCreated)
	f.endRun(t)
	deliverRecoveries(t)
	deliverRecoveries(t)

	if n := f.notices(t); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	if got := noticeMessages(t, f.humanID); got != 1 {
		t.Fatalf("DM messages = %d, want 1", got)
	}
	if got := noticeInbox(t, f.humanID, f.issueID); got != 1 {
		t.Fatalf("inbox items = %d, want 1", got)
	}
	var content string
	dbfx.QueryRow(t, `SELECT m.content FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.creator_id = $1 AND m.message_kind = 'ask_recovery'`, f.humanID).Scan(&content)
	if !strings.Contains(content, typedAskText) {
		t.Fatalf("notice does not quote the ask:\n%s", content)
	}
}

func TestTypedAskNoticePerComment(t *testing.T) {
	setWaitingAudienceMode(t, "warn")
	f := typedAskFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": typedAskText, "ask": f.humanID}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": typedAskText + " Second question.", "ask": f.humanID}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": "FYI only", "no_ask": true}).Want(http.StatusCreated)
	f.endRun(t)
	deliverRecoveries(t)
	deliverRecoveries(t)

	if n := f.notices(t); n != 2 {
		t.Fatalf("notices = %d, want 2 (one per --ask comment, none for --no-ask)", n)
	}
}

func TestTypedAskNoticeWaitsForTheRunToEnd(t *testing.T) {
	setWaitingAudienceMode(t, "warn")
	f := typedAskFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": typedAskText, "ask": f.humanID}).Want(http.StatusCreated)
	deliverRecoveries(t)
	if n := f.notices(t); n != 0 {
		t.Fatalf("notices before the run ended = %d, want 0", n)
	}
	f.endRun(t)
	deliverRecoveries(t)
	if n := f.notices(t); n != 1 {
		t.Fatalf("notices after the run ended = %d, want 1", n)
	}
}

func TestTypedAskNoticeOffWhenSwitchOff(t *testing.T) {
	f := typedAskFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": typedAskText, "ask": f.humanID}).Want(http.StatusCreated)
	f.endRun(t)
	deliverRecoveries(t)
	if n := f.notices(t); n != 0 {
		t.Fatalf("notices with waiting_audience_guard off = %d, want 0", n)
	}
}

func TestTypedWaitingMoveSendsOneNotice(t *testing.T) {
	setWaitingOnMode(t, "warn")
	f := typedAskFixture(t)
	f.move(t, "in_review", map[string]any{"waiting_on": "member:" + f.humanID}).Want(http.StatusOK)
	// The same request again is a no-op move and must not add a notice.
	f.move(t, "in_review", map[string]any{"waiting_on": "member:" + f.humanID}).Want(http.StatusOK)
	f.endRun(t)
	deliverRecoveries(t)
	deliverRecoveries(t)

	if n := f.notices(t); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	var content string
	dbfx.QueryRow(t, `SELECT m.content FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id WHERE s.creator_id = $1 AND m.message_kind = 'ask_recovery'`, f.humanID).Scan(&content)
	if !strings.Contains(content, "in_review") {
		t.Fatalf("notice does not name the status:\n%s", content)
	}
}

func TestTypedWaitingMoveNoneSendsNothing(t *testing.T) {
	setWaitingOnMode(t, "warn")
	f := typedAskFixture(t)
	f.move(t, "in_review", map[string]any{"waiting_on": "none"}).Want(http.StatusOK)
	f.endRun(t)
	deliverRecoveries(t)
	if n := f.notices(t); n != 0 {
		t.Fatalf("notices for --waiting-on none = %d, want 0", n)
	}
}
