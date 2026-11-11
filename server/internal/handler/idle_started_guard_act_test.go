package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

type idleActFixture struct {
	idleGuardFixture
	squad, leader string
}

// newIdleActFixture is the CHE-2433 shape in act mode, owned by a one-squad
// agent so the leader route resolves.
func newIdleActFixture(test *testing.T, hours float64) idleActFixture {
	test.Helper()
	fixture := newIdleGuardFixture(test, hours)
	idleGuardSetting(test, `{"mode":"act"}`)
	for _, status := range []struct{ key, name string }{{"need_decision", "Need Decision"}, {"blocked", "Blocked"}} {
		if dbfx.Count(test, `SELECT count(*) FROM issue_status WHERE workspace_id = $1 AND key = $2`, testWorkspaceID, status.key) == 0 {
			dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": status.key, "name": status.name, "category": "started", "color": "#112233"})
		}
		// The API lists a new status in a stored Default; a raw insert must too,
		// or an earlier test's Default would refuse the move. (MUL-8250)
		if err := issueworkflow.AddToDefault(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), status.key); err != nil {
			test.Fatal(err)
		}
	}
	leader := createHandlerTestAgent(test, "idle leader "+test.Name(), []byte("[]"))
	squad := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "idle squad", "creator_id": testUserID, "leader_id": leader})
	dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squad, fixture.agent)
	return idleActFixture{idleGuardFixture: fixture, squad: squad, leader: leader}
}

func (fixture idleActFixture) sweep(test *testing.T) {
	test.Helper()
	if err := testHandler.SweepIdleStartedIssues(context.Background()); err != nil {
		test.Fatal(err)
	}
}

// sweepFailing runs a pass whose notice step is expected to fail and be counted.
func (fixture idleActFixture) sweepFailing(test *testing.T) {
	test.Helper()
	if err := testHandler.SweepIdleStartedIssues(context.Background()); err == nil {
		test.Fatal("a pass with an unroutable leader reported no error")
	}
}

func (fixture idleActFixture) status(test *testing.T) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issue).Scan(&status)
	return status
}

func (fixture idleActFixture) receipt(test *testing.T, field string) string {
	test.Helper()
	var value string
	dbfx.QueryRow(test, `SELECT COALESCE(to_jsonb(g) ->> $1, '') FROM issue i LEFT JOIN issue_idle_guard g ON g.issue_id = i.id WHERE i.id = $2`, field, fixture.issue).Scan(&value)
	return value
}

func (fixture idleActFixture) systemComments(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE 'Idle started-issue guard:%'`, fixture.issue)
}

func (fixture idleActFixture) leaderTasks(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND is_leader_task AND status = 'queued'`, fixture.issue, fixture.leader)
}

func TestIdleStartedGuardActWaitsOnHumanMovesToNeedDecision(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.wakeup(test, testutil.Cols{"event_types": testutil.Raw(`ARRAY['comment.created']`), "filter_actor_type": "member", "filter_actor_id": testUserID})
	fixture.sweep(test)
	if got := fixture.status(test); got != "need_decision" {
		test.Fatalf("status = %q, want need_decision", got)
	}
	if got := fixture.receipt(test, "state"); got != "notified" {
		test.Fatalf("receipt state = %q, want notified", got)
	}
	if fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("system comments = %d, leader tasks = %d, want 1 and 1", fixture.systemComments(test), fixture.leaderTasks(test))
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM issue i JOIN agent_task_queue t ON t.id::text = i.metadata ->> 'decision_wait_alert_id' WHERE i.id = $1 AND t.agent_id = $2`, fixture.issue, fixture.leader); got != 1 {
		test.Fatal("decision wait guard was not told the leader is already alerted")
	}
}

func TestIdleStartedGuardActBlocksWithoutHumanWait(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE comment SET content = 'finished my part' WHERE issue_id = $1`, fixture.issue)
	fixture.sweep(test)
	if got := fixture.status(test); got != "blocked" {
		test.Fatalf("status = %q, want blocked", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ->> 'blocked_reason' = 'idle-no-continuation'`, fixture.issue); got != 1 {
		test.Fatal("blocked_reason idle-no-continuation missing")
	}
	if fixture.receipt(test, "kind") != "no_continuation" || fixture.leaderTasks(test) != 1 {
		test.Fatalf("kind = %q, leader tasks = %d", fixture.receipt(test, "kind"), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActNotifiesOncePerEpisode(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.sweep(test)
	fixture.sweep(test)
	fixture.sweep(test)
	if fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("system comments = %d, leader tasks = %d, want 1 and 1", fixture.systemComments(test), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActRechecksUnderLock(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	findings, err := testHandler.checkIdleStartedIssues(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	var found *idleGuardFinding
	for index := range findings {
		if uuidToString(findings[index].IssueID) == fixture.issue {
			found = &findings[index]
		}
	}
	if found == nil {
		test.Fatal("fixture issue was not found idle")
	}
	// The scan is stale: an agent run started after the finding was read.
	dbfx.Task(test, fixture.agent, testutil.Cols{"issue_id": fixture.issue, "status": "queued", "runtime_id": handlerTestRuntimeID(test)})
	if err := testHandler.actOnIdleFinding(context.Background(), idleGuardSettings{}, *found); err != nil {
		test.Fatal(err)
	}
	if got := fixture.status(test); got != "in_progress" {
		test.Fatalf("status = %q, a live run must keep the issue where it is", got)
	}
	if fixture.receipt(test, "state") != "" || fixture.systemComments(test) != 0 {
		test.Fatal("a refused move left a receipt or a comment")
	}
}

func TestIdleStartedGuardActRetriesLeaderRoute(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	fixture.sweepFailing(test)
	if fixture.status(test) != "need_decision" || fixture.receipt(test, "state") != "moved" {
		test.Fatalf("status = %q, state = %q, want need_decision and moved", fixture.status(test), fixture.receipt(test, "state"))
	}
	if fixture.leaderTasks(test) != 0 {
		test.Fatal("a leader task was admitted without a route")
	}
	dbfx.Exec(test, `UPDATE squad SET archived_at = NULL WHERE id = $1`, fixture.squad)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "notified" || fixture.leaderTasks(test) != 1 || fixture.systemComments(test) != 1 {
		test.Fatalf("state = %q, tasks = %d, comments = %d, want notified, 1, 1", fixture.receipt(test, "state"), fixture.leaderTasks(test), fixture.systemComments(test))
	}
}

func TestIdleStartedGuardActGivesUpAfterThreeNotifyFailures(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	for range 5 {
		// Errors are the counted failures; the receipt state is the assertion.
		_ = testHandler.SweepIdleStartedIssues(context.Background())
	}
	if got := fixture.receipt(test, "state"); got != "gave_up" {
		test.Fatalf("state = %q, want gave_up", got)
	}
}

func TestIdleStartedGuardActRepairStartsNewEpisode(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "notified" {
		test.Fatal("setup: first episode was not notified")
	}
	// A member puts the issue back to work; the receipt must not mute it forever.
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', created_at = now() - interval '6 hours', completed_at = now() - interval '6 hours' WHERE issue_id = $1 AND agent_id = $2`, fixture.issue, fixture.leader)
	dbfx.Exec(test, `UPDATE comment SET created_at = now() - interval '6 hours' WHERE issue_id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE activity_log SET created_at = now() - interval '6 hours' WHERE issue_id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE issue SET created_at = now() - interval '7 hours' WHERE id = $1`, fixture.issue)
	fixture.sweep(test)
	if got := fixture.status(test); got == "in_progress" {
		test.Fatal("the repaired issue went idle again and was never reconsidered")
	}
}

func TestIdleStartedGuardActModeStillOffWhenOff(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	idleGuardSetting(test, `{"mode":"off"}`)
	fixture.sweep(test)
	if got := fixture.status(test); got != "in_progress" {
		test.Fatalf("status = %q with the guard off", got)
	}
}

// CHE-3947: the receipt lives in issue_idle_guard. issue.metadata holds only
// string, number and bool values, so the issue still opens in the client.
func TestIdleStartedGuardReceiptStaysOutOfIssueMetadata(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE comment SET content = 'finished my part' WHERE issue_id = $1`, fixture.issue)
	fixture.sweep(test)
	if got := fixture.status(test); got != "blocked" {
		test.Fatalf("status = %q, want blocked", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM issue i, jsonb_each(i.metadata) e WHERE i.id = $1 AND jsonb_typeof(e.value) NOT IN ('string','number','boolean')`, fixture.issue); got != 0 {
		test.Fatalf("issue.metadata holds %d non-primitive values, want 0", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'idle_guard'`, fixture.issue); got != 0 {
		test.Fatal("issue.metadata still carries idle_guard")
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM issue_idle_guard WHERE issue_id = $1 AND state = 'notified' AND kind = 'no_continuation' AND from_status = 'in_progress' AND to_status = 'blocked' AND moved_at IS NOT NULL AND comment_id IS NOT NULL AND alert_id IS NOT NULL`, fixture.issue); got != 1 {
		test.Fatalf("issue_idle_guard rows = %d, want 1 filled receipt", got)
	}
}

// The continuation watchdog (multica-dotfiles) reads one primitive metadata key to
// tell an issue the guard already escalated from a stalled one.
func (fixture idleActFixture) notifiedTo(test *testing.T) string {
	test.Helper()
	var value string
	dbfx.QueryRow(test, `SELECT COALESCE(metadata ->> 'idle_guard_notified_to', '') FROM issue WHERE id = $1`, fixture.issue).Scan(&value)
	return value
}

func TestIdleStartedGuardNotifiedSignalFollowsReceipt(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	fixture.sweepFailing(test)
	if fixture.receipt(test, "state") != "moved" || fixture.notifiedTo(test) != "" {
		test.Fatalf("state = %q, signal = %q, want moved and no signal", fixture.receipt(test, "state"), fixture.notifiedTo(test))
	}
	dbfx.Exec(test, `UPDATE squad SET archived_at = NULL WHERE id = $1`, fixture.squad)
	fixture.sweep(test)
	if got, want := fixture.notifiedTo(test), fixture.status(test); got != want || want == "" {
		test.Fatalf("signal = %q, want the status %q the guard notified at", got, want)
	}
}

func TestIdleStartedGuardNotifiedSignalClearsWhenEpisodeEnds(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.sweep(test)
	if fixture.notifiedTo(test) == "" {
		test.Fatal("setup: a notified episode left no signal")
	}
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', created_at = now() - interval '6 hours', completed_at = now() - interval '6 hours' WHERE issue_id = $1 AND agent_id = $2`, fixture.issue, fixture.leader)
	dbfx.Exec(test, `UPDATE comment SET created_at = now() - interval '6 hours' WHERE issue_id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE activity_log SET created_at = now() - interval '6 hours' WHERE issue_id = $1`, fixture.issue)
	dbfx.Exec(test, `UPDATE issue SET created_at = now() - interval '7 hours' WHERE id = $1`, fixture.issue)
	fixture.sweep(test)
	if got := fixture.notifiedTo(test); got != "" && got != fixture.status(test) {
		test.Fatalf("signal = %q for an issue now in %q", got, fixture.status(test))
	}
}

// Ending an episode deletes the receipt and clears the watchdog signal together:
// if the signal cannot be cleared, the receipt stays so a later scan retries.
func TestIdleStartedGuardEndEpisodeKeepsReceiptWhenSignalClearFails(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.sweep(test)
	if fixture.notifiedTo(test) == "" {
		test.Fatal("setup: a notified episode left no signal")
	}
	dbfx.Exec(test, `CREATE FUNCTION pg_temp.refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`)
	dbfx.Exec(test, `CREATE TRIGGER refuse_issue_update BEFORE UPDATE ON issue FOR EACH ROW WHEN (OLD.id = '`+fixture.issue+`') EXECUTE FUNCTION pg_temp.refuse()`)
	test.Cleanup(func() { dbfx.Exec(test, `DROP TRIGGER IF EXISTS refuse_issue_update ON issue`) })
	if err := testHandler.endIdleGuardEpisode(context.Background(), parseUUID(testWorkspaceID), parseUUID(fixture.issue)); err == nil {
		test.Fatal("ending the episode reported no error although the signal could not be cleared")
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue_idle_guard WHERE issue_id = $1`, fixture.issue) != 1 {
		test.Fatal("the receipt row was deleted although the signal clear failed; no later scan can retry")
	}
}
