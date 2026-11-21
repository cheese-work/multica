package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// owedAction writes the ledger row of an action the watchdog took on the
// issue's current status occurrence, and no notice row: the state after a crash
// between the action and its notice.
func owedAction(t *testing.T, issueID, action string) {
	t.Helper()
	dbfx.Cleanup(t, `DELETE FROM issue_step_watchdog_action WHERE issue_id = $1`, issueID)
	var key string
	dbfx.QueryRow(t, `SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' ORDER BY created_at DESC, id DESC LIMIT 1`, issueID).Scan(&key)
	dbfx.Insert(t, "issue_step_watchdog_action", testutil.Cols{
		"id": uuid.NewString(), "workspace_id": testWorkspaceID, "issue_id": issueID, "occurrence_key": key, "action": action,
		"details": `{"case":"` + action + `","status":"approved","in_step_since":"` + time.Now().Add(-30*time.Hour).UTC().Format(time.RFC3339) + `"}`,
	})
}

func noticeRows(t *testing.T, issueID, outcome string) int {
	return dbfx.Count(t, `SELECT count(*) FROM issue_step_watchdog_action WHERE issue_id = $1 AND action = 'notify' AND outcome = $2`, issueID, outcome)
}

func watchdogInbox(t *testing.T, member, issueID string) int {
	return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND issue_id = $2 AND details ? 'step_watchdog_action_id'`, member, issueID)
}

func passOwedNotices(t *testing.T) {
	t.Helper()
	if err := testHandler.deliverOwedStepNotices(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
}

// Every owed action produces one durable notice to requested_by, a second pass
// sends nothing, and a status change after the action does not cancel it.
func TestStepWatchdogDeliversOneOwedNoticePerAction(t *testing.T) {
	f := newWatchdogFixture(t)
	for _, action := range []string{"block_budget", "redispatch", "notify_only"} {
		for _, moved := range []bool{false, true} {
			name := action
			if moved {
				name += "/issue moved on"
			}
			t.Run(name, func(t *testing.T) {
				issueID := f.stalledIssue(t, 30*time.Hour, 0)
				dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, issueID, testUserID)
				owedAction(t, issueID, action)
				if moved {
					dbfx.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, issueID)
				}
				passOwedNotices(t)
				passOwedNotices(t)

				if n := noticeRows(t, issueID, "delivered"); n != 1 {
					t.Fatalf("delivered notify rows = %d, want 1", n)
				}
				if n := watchdogInbox(t, testUserID, issueID); n != 1 {
					t.Fatalf("inbox items = %d, want 1", n)
				}
				var content string
				dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%Step watchdog%'`, issueID).Scan(&content)
				for _, want := range []string{"mention://member/" + testUserID, action} {
					if !strings.Contains(content, want) {
						t.Fatalf("notice lacks %q:\n%s", want, content)
					}
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%Step watchdog%'`, issueID); n != 1 {
					t.Fatalf("notice comments = %d, want 1", n)
				}
			})
		}
	}
}

// A requester the watchdog cannot reach is recorded, not retried and not guessed.
func TestStepWatchdogRecordsUnreachableRequester(t *testing.T) {
	f := newWatchdogFixture(t)
	outsider := dbfx.User(t, "Not a member", "outsider-"+uuid.NewString()+"@test.invalid")
	for name, requester := range map[string]any{"no requester": nil, "not a member": outsider} {
		t.Run(name, func(t *testing.T) {
			issueID := f.stalledIssue(t, 30*time.Hour, 0)
			dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, issueID, requester)
			owedAction(t, issueID, "redispatch")
			passOwedNotices(t)
			passOwedNotices(t)

			if n := noticeRows(t, issueID, "notice_unreachable"); n != 1 {
				t.Fatalf("notice_unreachable rows = %d, want 1", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%Step watchdog%'`, issueID); n != 0 {
				t.Fatalf("an unreachable requester got %d comments", n)
			}
		})
	}
}

// The on-mode sweep that blocks a budget-refused issue also delivers its notice.
func TestStepWatchdogSweepDeliversTheBlockNotice(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")
	issueID := f.stalledIssue(t, 25*time.Hour, 36)
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, issueID, testUserID)
	dbfx.Cleanup(t, `DELETE FROM issue_step_watchdog_action WHERE issue_id = $1`, issueID)
	for i := 0; i < 2; i++ {
		if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
	}
	if n := noticeRows(t, issueID, "delivered"); n != 1 {
		t.Fatalf("delivered notify rows = %d, want 1", n)
	}
	if n := watchdogInbox(t, testUserID, issueID); n != 1 {
		t.Fatalf("inbox items = %d, want 1", n)
	}
}
