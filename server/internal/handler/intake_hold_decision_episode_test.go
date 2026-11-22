package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/intakehold"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7074 (E3, rows 3 to 5): the wait episode, a person's discussion message
// and a person's revoke dispatch nothing on a held issue. Case (d) and case (g)
// of criterion 11.

// holdEpisodeProject puts the hold entry (mode) on the episode fixture's project.
func holdEpisodeProject(test *testing.T, f episodeFixture, mode string) holdFixture {
	test.Helper()
	var projectID string
	dbfx.QueryRow(test, `SELECT project_id::text FROM issue WHERE id = $1`, f.issueID).Scan(&projectID)
	now := time.Now().UTC()
	fixture := holdFixture{projectID, map[string]any{
		"project_id": projectID, "mode": mode, "source": "CHE-6911",
		"starts_at": now.Add(-time.Hour).Format(time.RFC3339), "ends_at": now.Add(time.Hour).Format(time.RFC3339),
	}}
	return newHoldFixtureWith(test, fixture, func(map[string]any) {})
}

// enterFromBacklog is a held issue whose asking run is still in flight: a person
// moves the issue to backlog, then the asking agent moves it into the waiting
// step. The code records backlog as the restore target (design 4.4.6); the test
// writes no issue or decision_wait row.
func enterFromBacklog(test *testing.T, f episodeFixture) {
	test.Helper()
	f.memberMove(test, "backlog").Want(http.StatusOK)
	f.enter(test)
	if got := f.onlyEpisode(test).restoreStatus; got != "backlog" {
		test.Fatalf("restore_status = %q, want backlog recorded by the entry", got)
	}
}

// Row 3, restore clause: an agent enters the step from backlog (a person moved
// the issue there first), so the episode records backlog as the restore target.
// This is not case (d) in the order of criterion 11(d); the test below is.
// Mode block: the episode closes, the answer is acknowledged skipped_ineligible,
// no resume task is queued and no refusal clock starts.
func TestHeldIssueEpisodeDeliversNothing(test *testing.T) {
	runner := newDeliveryRunner(test)
	f := newEpisodeFixture(test, false)
	hold := holdEpisodeProject(test, f, "off")
	enterFromBacklog(test, f)
	newHoldFixtureWith(test, hold, func(entry map[string]any) { entry["mode"] = "block" })
	answerID, _ := f.answerRequest(test, f.requestID, -time.Minute, false)
	tasks := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
	logs := recordHoldLogs(test)

	decisionPass(test, runner)

	// The restore put the issue back in backlog, where the hold applies.
	if status, _, _ := issueStatusAndAssignee(test, f.issueID); status != "backlog" {
		test.Fatalf("status after the restore = %q, want backlog", status)
	}
	wantHold(test, f.issueID, true, true, intakehold.ReasonHeld)
	closed := f.closedEpisodes(test)
	if len(closed) != 1 || closed[0].reason != "answered" || closed[0].outcome != "skipped_ineligible" || closed[0].resumeTaskID != "" {
		test.Fatalf("closed episodes = %+v, want one closed answered with skipped_ineligible and no resume task", closed)
	}
	if woken, outcome := f.answerState(test, answerID); !woken || outcome != "skipped_ineligible" {
		test.Fatalf("answer woken=%v outcome=%q, want acknowledged skipped_ineligible", woken, outcome)
	}
	if after := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID); after != tasks {
		test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND dispatch_refused_since IS NOT NULL`, f.issueID) != 0 {
		test.Fatal("dispatch_refused_since is set")
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_episode", 1)
	settled := dbfx.Count(test, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND closed_at IS NOT NULL`, f.issueID)
	decisionPass(test, runner)
	if again := f.closedEpisodes(test); len(again) != settled || f.continuations(test) != 0 {
		test.Fatalf("a second pass changed state: %+v, %d runs", again, f.continuations(test))
	}
}

// Row 3, case (d), in the order of plan criterion 11(d): with the entry in mode
// off an agent moves the issue into the human_in_charge step, the issue is then
// moved back to backlog, and only then the entry is set to block. The issue is
// held before the answer. The test writes no issue or decision_wait row.
func TestHeldIssueEpisodeEnteredThenBackloggedThenBlocked(test *testing.T) {
	runner := newDeliveryRunner(test)
	f := newEpisodeFixture(test, false)
	hold := holdEpisodeProject(test, f, "off")
	f.enter(test)
	f.memberMove(test, "backlog").Want(http.StatusOK)
	// The person's move suspends the open episode and clears the restore flag.
	if s := f.suspension(test); s.phase != "suspended" || s.reason != "status_moved" || s.flag {
		test.Fatalf("after the move to backlog = %+v, want suspended status_moved with the flag clear", s)
	}
	newHoldFixtureWith(test, hold, func(entry map[string]any) { entry["mode"] = "block" })
	// Held before the answer.
	if status, _, _ := issueStatusAndAssignee(test, f.issueID); status != "backlog" {
		test.Fatalf("status before the answer = %q, want backlog", status)
	}
	wantHold(test, f.issueID, true, true, intakehold.ReasonHeld)
	answerID, _ := f.answerRequest(test, f.requestID, -time.Minute, false)
	tasks := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
	logs := recordHoldLogs(test)

	decisionPass(test, runner)

	if status, _, _ := issueStatusAndAssignee(test, f.issueID); status != "backlog" {
		test.Fatalf("status after pass one = %q, want backlog", status)
	}
	wantHold(test, f.issueID, true, true, intakehold.ReasonHeld)
	closed := f.closedEpisodes(test)
	if len(closed) != 1 || closed[0].reason != "answered" || closed[0].outcome != "skipped_ineligible" || closed[0].resumeTaskID != "" {
		test.Fatalf("closed episodes = %+v, want one closed answered with skipped_ineligible and no resume task", closed)
	}
	if woken, outcome := f.answerState(test, answerID); !woken || outcome != "skipped_ineligible" {
		test.Fatalf("answer woken=%v outcome=%q, want acknowledged skipped_ineligible", woken, outcome)
	}
	if after := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID); after != tasks {
		test.Fatalf("tasks %d -> %d, want none queued", tasks, after)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND dispatch_refused_since IS NOT NULL`, f.issueID) != 0 {
		test.Fatal("dispatch_refused_since is set")
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_episode", 1)
	settled := dbfx.Count(test, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND closed_at IS NOT NULL`, f.issueID)
	decisionPass(test, runner)
	if status, _, _ := issueStatusAndAssignee(test, f.issueID); status != "backlog" {
		test.Fatalf("status after pass two = %q, want backlog", status)
	}
	if again := f.closedEpisodes(test); len(again) != settled || f.continuations(test) != 0 {
		test.Fatalf("a second pass changed state: %+v, %d runs", again, f.continuations(test))
	}
}

// Mode warn dispatches as today.
func TestHeldIssueEpisodeWarnModeDispatches(test *testing.T) {
	runner := newDeliveryRunner(test)
	f := newEpisodeFixture(test, false)
	holdEpisodeProject(test, f, "warn")
	enterFromBacklog(test, f)
	f.answerRequest(test, f.requestID, -time.Minute, false)
	logs := recordHoldLogs(test)
	decisionPass(test, runner)
	f.wantOneContinuation(test, "answered")
	if len(logs.lines("intake_hold_warn")) == 0 {
		test.Fatal("no intake_hold_warn line")
	}
}

// holdActionIssue moves the issue of a decision action card into the hold.
func holdActionIssue(test *testing.T, action pendingAction) {
	test.Helper()
	hold := newHoldFixture(test, nil)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1, status = 'backlog', parent_issue_id = NULL WHERE id = $2`, hold.projectID, action.issueID)
	wantHold(test, action.issueID, true, true, intakehold.ReasonHeld)
}

// Row 4, case (g): a discussion message on an open card of a held issue.
func TestHeldIssueDiscussionStoresMessageAndDispatchesNothing(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	holdActionIssue(test, action)
	logs := recordHoldLogs(test)

	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+action.requestID+"/discuss", map[string]any{"message": "why this one?"}), "id", action.requestID)
	request.Header.Set("X-User-ID", action.humanID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	var out map[string]string
	testutil.Call(test, testHandler.DiscussDecisionRequest, request).Want(http.StatusOK).JSON(&out)

	if out["wake"] != "skipped_ineligible" || out["outcome"] != "discussion" {
		test.Fatalf("response = %v, want wake skipped_ineligible", out)
	}
	if dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND content = 'why this one?'`, out["comment_id"]) != 1 {
		test.Fatal("the message was not stored")
	}
	if n := action.runs(test); n != 0 {
		test.Fatalf("runs = %d, want none queued", n)
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_discussion", 1)
}

// Mode warn: the discussion wakes the agent as today.
func TestHeldIssueDiscussionWarnModeWakes(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_request SET proposal_fingerprint = 'p1:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, action.issueID)
	hold := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "warn" })
	dbfx.Exec(test, `UPDATE issue SET project_id = $1, status = 'backlog', parent_issue_id = NULL WHERE id = $2`, hold.projectID, action.issueID)
	action.discuss(test, "why this one?", http.StatusOK)
	if n := action.runs(test); n != 1 {
		test.Fatalf("runs = %d, want 1 in mode warn", n)
	}
}

// Row 5, case (g): a revoke of an executed action on a held issue.
func TestHeldIssueRevokeStoresRevokeAndDispatchesNothing(test *testing.T) {
	action := appliedPause(test)
	holdActionIssue(test, action)
	logs := recordHoldLogs(test)

	action.revoke(test, withVerifier(acceptingVerifier), map[string]any{"reason": "undo"}, http.StatusOK)

	if read := readRevoke(test, action.requestID); read.Revoke == nil || read.Revoke.Reason != "undo" {
		test.Fatalf("revoke not stored: %+v", read)
	}
	if dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE 'Revoked %'`, action.issueID) != 1 {
		test.Fatal("the revoke comment was not stored")
	}
	if n := action.runs(test); n != 0 {
		test.Fatalf("runs = %d, want none queued", n)
	}
	logs.wantSite(test, "intake_hold_delivery_skipped", "decision_revoke", 1)
}
