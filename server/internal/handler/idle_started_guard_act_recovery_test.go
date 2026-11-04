package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func (fixture idleActFixture) gaveUp(test *testing.T) {
	test.Helper()
	// A workflow that lists neither target status refuses every move.
	fixture.workflow(test, bareSteps("backlog", "in_progress", "done"))
	for range 4 {
		_ = testHandler.SweepIdleStartedIssues(context.Background())
	}
}

func TestIdleStartedGuardActOpenRequestStaysWithoutNeedDecision(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.neutralComment(test)
	fixture.openRequest(test)
	fixture.workflow(test, bareSteps("backlog", "in_progress", "blocked", "done"))
	fixture.sweep(test)
	if got := fixture.status(test); got != "in_progress" {
		test.Fatalf("status = %q, an open decision request is a legitimate wait and must not be blocked", got)
	}
	if fixture.receipt(test, "state") != "" || fixture.systemComments(test) != 0 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("receipt = %q, comments = %d, tasks = %d, want no move, comment or task", fixture.receipt(test, "state"), fixture.systemComments(test), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActGiveUpFailureStaysRecoverable(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `CREATE OR REPLACE FUNCTION idle_guard_test_fail() RETURNS trigger AS $$ BEGIN
		IF NEW.content LIKE 'Idle started-issue guard: gave up%' THEN RAISE EXCEPTION 'injected comment failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
	dbfx.Exec(test, `CREATE TRIGGER idle_guard_test_fail BEFORE INSERT ON comment FOR EACH ROW EXECUTE FUNCTION idle_guard_test_fail()`)
	removed := false
	removeFault := func() {
		if !removed {
			removed = true
			dbfx.Exec(test, `DROP TRIGGER IF EXISTS idle_guard_test_fail ON comment`)
			dbfx.Exec(test, `DROP FUNCTION IF EXISTS idle_guard_test_fail()`)
		}
	}
	test.Cleanup(removeFault)
	fixture.gaveUp(test)
	if got := fixture.receipt(test, "state"); got == "gave_up" {
		test.Fatal("the give-up receipt committed without its explanation: a failed comment insert lost the visible result for good")
	}
	removeFault()
	fixture.alertDue(test)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "gave_up" || fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("state = %q, comments = %d, tasks = %d, want gave_up with one note and one alert after recovery",
			fixture.receipt(test, "state"), fixture.systemComments(test), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActGaveUpEpisodeEndsOnNewActivity(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.gaveUp(test)
	if fixture.receipt(test, "state") != "gave_up" {
		test.Fatalf("setup: state = %q, want gave_up", fixture.receipt(test, "state"))
	}
	// Nothing changed: the failed episode is not started again, nor re-announced.
	fixture.sweep(test)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "gave_up" || fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("an unchanged episode restarted: state = %q, comments = %d, tasks = %d", fixture.receipt(test, "state"), fixture.systemComments(test), fixture.leaderTasks(test))
	}
	// The workflow is repaired and a person picks the issue up again.
	dbfx.Exec(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, fixture.issue)
	dbfx.Comment(test, fixture.issue, "back at it", testutil.Cols{"author_type": "member", "author_id": testUserID})
	fixture.sweep(test)
	if got := fixture.receipt(test, "state"); got != "" {
		test.Fatalf("state = %q, new activity must end the old episode", got)
	}
	// It goes idle again: the guard reconsiders it.
	fixture.ageEverything(test)
	fixture.sweep(test)
	if got := fixture.status(test); got == "in_progress" {
		test.Fatal("the issue idled again after the repair and was never reconsidered")
	}
}

func TestIdleStartedGuardActRefusedEpisodeEndsOnNewActivity(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.workflow(test, bareSteps("backlog", "in_progress", "done"))
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "refused" || fixture.receipt(test, "attempts") != "1" {
		test.Fatalf("setup: state = %q, attempts = %q, want refused after one attempt", fixture.receipt(test, "state"), fixture.receipt(test, "attempts"))
	}
	dbfx.Comment(test, fixture.issue, "back at it", testutil.Cols{"author_type": "member", "author_id": testUserID})
	fixture.sweep(test)
	if got := fixture.receipt(test, "state"); got != "" {
		test.Fatalf("state = %q, a refused episode must end when the issue is active again", got)
	}
}

func TestIdleStartedGuardActRefusedEpisodeEndsWhenAnOpenRequestIsTheWait(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.neutralComment(test)
	fixture.workflow(test, bareSteps("backlog", "in_progress", "done"))
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "refused" {
		test.Fatalf("setup: state = %q, want refused", fixture.receipt(test, "state"))
	}
	// A person's decision request opens, on a workflow that has blocked only.
	fixture.openRequest(test)
	fixture.workflow(test, bareSteps("backlog", "in_progress", "blocked", "done"))
	fixture.sweep(test)
	if got := fixture.receipt(test, "state"); got != "" {
		test.Fatalf("state = %q, the old refused receipt must not outlive a legitimate wait", got)
	}
	if got := fixture.status(test); got != "in_progress" || fixture.systemComments(test) != 0 {
		test.Fatalf("status = %q, comments = %d, an open request must stay where it is", got, fixture.systemComments(test))
	}
}

// alertDue ends the cooldown that follows a failed alert admission.
func (fixture idleActFixture) alertDue(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `UPDATE issue SET metadata = jsonb_set(metadata, '{idle_guard,alert_at}', '"2020-01-01T00:00:00Z"') WHERE id = $1`, fixture.issue)
}

func (fixture idleActFixture) archiveLeaderSquad(test *testing.T, archived bool) {
	test.Helper()
	if archived {
		dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
		return
	}
	dbfx.Exec(test, `UPDATE squad SET archived_at = NULL WHERE id = $1`, fixture.squad)
}

func TestIdleStartedGuardActGiveUpRevalidatesUnderTheLock(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.workflow(test, bareSteps("backlog", "in_progress", "done"))
	fixture.sweep(test)
	fixture.sweep(test)
	if fixture.receipt(test, "attempts") != "2" {
		test.Fatalf("setup: attempts = %q, want 2", fixture.receipt(test, "attempts"))
	}
	// The issue resumes after the retry scan and before the give-up transaction.
	idleGuardBeforeMove = func() {
		dbfx.Comment(test, fixture.issue, "back at it", testutil.Cols{"author_type": "member", "author_id": testUserID})
	}
	test.Cleanup(func() { idleGuardBeforeMove = func() {} })
	fixture.sweep(test)
	idleGuardBeforeMove = func() {}
	if fixture.systemComments(test) != 0 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("comments = %d, tasks = %d: work that resumed was given a give-up note and a leader alert", fixture.systemComments(test), fixture.leaderTasks(test))
	}
	if got := fixture.receipt(test, "state"); got != "" {
		test.Fatalf("state = %q, the episode of an issue that resumed must end", got)
	}
}

func TestIdleStartedGuardActGiveUpAlertRecoversWithoutActivity(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.archiveLeaderSquad(test, true)
	fixture.gaveUp(test)
	if got := fixture.receipt(test, "state"); got == "gave_up" {
		test.Fatal("gave_up was committed without a delivered alert")
	}
	if fixture.systemComments(test) != 0 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("comments = %d, tasks = %d, a failed give-up must leave no effect behind", fixture.systemComments(test), fixture.leaderTasks(test))
	}
	if fixture.receipt(test, "alert_attempts") != "1" || fixture.receipt(test, "alert_error") == "" {
		test.Fatalf("alert attempts = %q, alert error = %q, want 1 and the admission error", fixture.receipt(test, "alert_attempts"), fixture.receipt(test, "alert_error"))
	}
	// The cooldown holds the next attempt back.
	fixture.sweep(test)
	if fixture.receipt(test, "alert_attempts") != "1" {
		test.Fatalf("alert attempts = %q, the cooldown was ignored", fixture.receipt(test, "alert_attempts"))
	}
	// The leader recovers; nobody touches the issue.
	fixture.archiveLeaderSquad(test, false)
	fixture.alertDue(test)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "gave_up" || fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("state = %q, comments = %d, tasks = %d, want gave_up with one note and one alert", fixture.receipt(test, "state"), fixture.systemComments(test), fixture.leaderTasks(test))
	}
	if fixture.receipt(test, "alert_id") == "" {
		test.Fatal("the delivered alert is not on the receipt")
	}
}

func TestIdleStartedGuardActGiveUpAlertAttemptsAreBounded(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.archiveLeaderSquad(test, true)
	fixture.gaveUp(test)
	for range 8 {
		fixture.alertDue(test)
		fixture.sweep(test)
	}
	if fixture.receipt(test, "state") != "gave_up" || fixture.receipt(test, "alert_id") != "" {
		test.Fatalf("state = %q, alert = %q, want gave_up without an alert once the attempts are spent", fixture.receipt(test, "state"), fixture.receipt(test, "alert_id"))
	}
	if fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("comments = %d, tasks = %d, want the one note that says the alert failed", fixture.systemComments(test), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActPendingAlertEpisodeEndsOnNewActivity(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.archiveLeaderSquad(test, true)
	fixture.gaveUp(test)
	dbfx.Comment(test, fixture.issue, "back at it", testutil.Cols{"author_type": "member", "author_id": testUserID})
	fixture.alertDue(test)
	fixture.archiveLeaderSquad(test, false)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "" || fixture.systemComments(test) != 0 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("state = %q, comments = %d, tasks = %d, an issue that resumed must get no give-up", fixture.receipt(test, "state"), fixture.systemComments(test), fixture.leaderTasks(test))
	}
}
