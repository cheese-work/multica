package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// L5e (CHE-2591 design §4.6, §7 option E2, §9.11-9.12). Every test drives the
// worker pass, so it checks what the worker does, not one internal method.

func (f episodeFixture) reminderCounts(t *testing.T) (inbox, lane int) {
	t.Helper()
	inbox = dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND details->>'kind' = 'decision_wait_reminder' AND issue_id = $3`,
		testWorkspaceID, f.humanID, f.issueID)
	lane = dbfx.Count(t, `SELECT count(*) FROM chat_message m JOIN decision_request r ON r.chat_session_id = m.chat_session_id
		WHERE r.id = $1 AND m.message_kind = 'decision_reminder'`, f.requestID)
	return inbox, lane
}

func (f episodeFixture) wantReminders(t *testing.T, inbox, lane int) {
	t.Helper()
	if gotInbox, gotLane := f.reminderCounts(t); gotInbox != inbox || gotLane != lane {
		t.Fatalf("reminders: inbox=%d lane=%d, want inbox=%d lane=%d", gotInbox, gotLane, inbox, lane)
	}
}

func (f episodeFixture) dueNow(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE decision_wait SET reminder_due_at = now() - interval '1 minute' WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
}

func TestDecisionEpisodeReminderIsSentOnceWhenDue(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	dbfx.Exec(t, `UPDATE decision_wait SET reminder_due_at = now() + interval '1 hour' WHERE issue_id = $1`, f.issueID)
	decisionPass(t, runner)
	f.wantReminders(t, 0, 0)

	f.dueNow(t)
	for pass := 0; pass < 3; pass++ {
		decisionPass(t, runner)
	}
	f.wantReminders(t, 1, 1)
	if dbfx.Count(t, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND reminder_delivered_at IS NOT NULL`, f.issueID) != 1 {
		t.Fatal("reminder_delivered_at is not stamped")
	}
	// The card stays answerable and nothing else was created.
	if dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, f.requestID) != 1 {
		t.Fatal("reminder changed the request")
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, f.issueID); got != 1 {
		t.Fatalf("requests = %d, want the original only", got)
	}
}

func TestDecisionEpisodeReminderSkipsWhatCannotBeAnswered(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f episodeFixture)
	}{
		{"closed episode", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_wait SET closed_at = now(), close_reason = 'answered' WHERE issue_id = $1`, f.issueID)
		}},
		{"suspended episode", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_wait SET phase = 'suspended', suspend_reason = 'status_moved', suspended_at = now() WHERE issue_id = $1`, f.issueID)
		}},
		{"no open bound request", func(t *testing.T, f episodeFixture) {
			f.answerRequest(t, f.requestID, time.Hour, false)
		}},
		{"person left the workspace", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, f.humanID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner := newDeliveryRunner(t)
			f := newEpisodeFixture(t, false)
			f.enter(t)
			c.setup(t, f)
			dbfx.Exec(t, `UPDATE decision_wait SET reminder_due_at = now() - interval '1 minute' WHERE issue_id = $1`, f.issueID)
			decisionPass(t, runner)
			f.wantReminders(t, 0, 0)
			if dbfx.Count(t, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND reminder_delivered_at IS NOT NULL`, f.issueID) != 0 {
				t.Fatal("a skipped reminder was stamped as delivered")
			}
		})
	}
}

func TestDecisionEpisodeReminderWithoutActiveLaneStillReachesTheInbox(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = (SELECT chat_session_id FROM decision_request WHERE id = $1)`, f.requestID)
	f.dueNow(t)
	decisionPass(t, runner)
	f.wantReminders(t, 1, 0)
}

func TestDecisionEpisodeReminderIsDueAtEntryForAnExpiringAction(t *testing.T) {
	f := newEpisodeFixture(t, false)
	dbfx.Insert(t, "decision_action", testutil.Cols{"workspace_id": testWorkspaceID, "decision_request_id": f.requestID, "action_type": "agent.env_patch",
		"action_version": 1, "params": "{}", "payload_sha256": strings.Repeat("a", 64), "expires_at": time.Now().Add(90 * time.Minute),
		"requested_by_agent_id": f.agentID})
	f.enter(t)
	var early bool
	dbfx.QueryRow(t, `SELECT reminder_due_at <= entered_at + interval '1 minute' FROM decision_wait WHERE issue_id = $1`, f.issueID).Scan(&early)
	if !early {
		t.Fatal("reminder not clamped to action expiry minus two hours (never before entry)")
	}
	runner := newDeliveryRunner(t)
	decisionPass(t, runner)
	f.wantReminders(t, 1, 1)
}

func (f episodeFixture) ageRequest(t *testing.T, hours int) {
	t.Helper()
	dbfx.Exec(t, `UPDATE decision_request SET created_at = now() - make_interval(hours => $2) WHERE id = $1`, f.requestID, hours)
}

func TestDecisionEpisodeE2BoundRequestDoesNotAgeOutWhileWaiting(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.ageRequest(t, 200)
	for pass := 0; pass < 2; pass++ {
		decisionPass(t, runner)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, f.requestID) != 1 {
		t.Fatal("a bound request older than 168h was cancelled while the episode is waiting")
	}
	if len(f.episodes(t)) != 1 || f.continuations(t) != 0 {
		t.Fatalf("episodes=%d continuations=%d, want the episode still waiting", len(f.episodes(t)), f.continuations(t))
	}
}

func TestDecisionEpisodeE2UnboundRequestStillExpires(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.ageRequest(t, 200)
	// Not bound any more: the exemption follows the episode, not the age.
	dbfx.Exec(t, `UPDATE decision_wait SET closed_at = now(), close_reason = 'answered' WHERE issue_id = $1`, f.issueID)
	decisionPass(t, runner)
	if dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'expired'`, f.requestID) != 1 {
		t.Fatal("an unbound request older than 168h did not expire")
	}
}

func TestDecisionEpisodeE2SuspendedRequestExpiresAndClosesTheEpisode(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.ageRequest(t, 200)
	dbfx.Exec(t, `UPDATE decision_wait SET phase = 'suspended', suspend_reason = 'status_moved', suspended_at = now(), restore_on_resume = false WHERE issue_id = $1`, f.issueID)
	decisionPass(t, runner)
	task := f.wantOneContinuation(t, "expired")
	for _, want := range []string{"Expiry is not an answer", "Re-resolve the human in charge, or move the issue to blocked with this reason"} {
		if !strings.Contains(task.HandoffNote.String, want) {
			t.Fatalf("handoff %q does not say %q", task.HandoffNote.String, want)
		}
	}
	// Design v8 section 7: only a privileged-action expiry orders blocked.
	if strings.Contains(task.HandoffNote.String, "with the expiry stated as the reason") {
		t.Fatalf("ordinary expiry orders blocked: %q", task.HandoffNote.String)
	}
}

func TestDecisionEpisodeE2ActionExpiryClosesActionExpired(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	actionID := dbfx.Insert(t, "decision_action", testutil.Cols{"workspace_id": testWorkspaceID, "decision_request_id": f.requestID, "action_type": "agent.env_patch",
		"action_version": 1, "params": "{}", "payload_sha256": strings.Repeat("b", 64), "expires_at": time.Now().Add(time.Hour),
		"requested_by_agent_id": f.agentID})
	f.enter(t)
	f.ageRequest(t, 200)
	decisionPass(t, runner)
	if len(f.episodes(t)) != 1 {
		t.Fatal("the 168h clause closed an episode that is waiting")
	}

	dbfx.Exec(t, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, actionID)
	decisionPass(t, runner)
	decisionPass(t, runner)
	task := f.wantOneContinuation(t, "action_expired")
	for _, want := range []string{"prepared action expired before approval", "Expiry is not an answer", "Move the issue to blocked with the expiry stated as the reason"} {
		if !strings.Contains(task.HandoffNote.String, want) {
			t.Fatalf("handoff %q does not say %q", task.HandoffNote.String, want)
		}
	}
	var state string
	dbfx.QueryRow(t, `SELECT state FROM decision_action WHERE id = $1`, actionID).Scan(&state)
	if state != "expired" {
		t.Fatalf("action state = %s, want expired", state)
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "in_progress" {
		t.Fatalf("issue status = %s, want the restored in_progress", status)
	}
}

func TestDecisionEpisodeReminderTwoWorkersSendOne(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.dueNow(t)
	wait := f.onlyEpisode(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- testHandler.deliverDecisionReminder(context.Background(), parseUUID(testWorkspaceID), parseUUID(wait.id))
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	f.wantReminders(t, 1, 1)
}
