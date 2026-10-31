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
