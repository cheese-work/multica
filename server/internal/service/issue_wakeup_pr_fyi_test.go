package service

import (
	"context"
	"os"
	"testing"
)

// A member holds the issue and no agent resolves (CHE-5901): the member hears
// about each PR event once, and an agent assigned later still gets the event.

func (e prEventEnv) holdByMember(t *testing.T) {
	t.Helper()
	// No agent worked on the issue that can still run, so none resolves.
	e.endRun(t, e.run)
	e.f.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, e.agent)
	e.f.Exec(t, `UPDATE issue SET status='in_review',assignee_type='member',assignee_id=$2 WHERE id=$1`, e.issue, e.f.UserID)
	e.f.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id=$1`, e.issue)
}

func (e prEventEnv) notices(t *testing.T) (inbox, activity int) {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND archived=false`, e.issue),
		e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered' AND details->>'outcome'='notified'`, e.issue)
}

func TestMemberHeldPREventNotifiesOnce(t *testing.T) {
	e := newPREventEnv(t)
	e.holdByMember(t)

	for range 3 {
		e.tickAll(t, e.s)
	}

	if inbox, activity := e.notices(t); inbox != 1 || activity != 1 {
		t.Fatalf("3 sweeps wrote %d inbox rows and %d notified activity rows, want 1 and 1", inbox, activity)
	}
	if got := e.status(t); got != "notified" {
		t.Fatalf("event is %q after the member notice, want notified", got)
	}
}

func TestNotifiedPREventWakesAnAgentAssignedLater(t *testing.T) {
	e := newPREventEnv(t)
	e.holdByMember(t)
	e.tickAll(t, e.s)
	e.tickAll(t, e.s)
	if e.status(t) != "notified" {
		t.Fatalf("setup: event is %q, want notified", e.status(t))
	}

	e.f.Exec(t, `UPDATE agent SET archived_at=NULL WHERE id=$1`, e.agent)
	e.f.Exec(t, `UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1`, e.issue, e.agent)
	e.tickAll(t, e.s)
	e.tickAll(t, e.s)

	if got := e.tasks(t, SystemRulePRAttention); got != 1 {
		t.Fatalf("the agent got %d runs for the event, want 1", got)
	}
	wake := e.lastTask(t, SystemRulePRAttention)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, wake.ID)
	e.tickAll(t, e.s)
	if got := e.status(t); got != "delivered" {
		t.Fatalf("event is %q after the agent's run started, want delivered", got)
	}
}

func TestTwoEventsOnOnePRKeepOneUnreadNotice(t *testing.T) {
	e := newPREventEnv(t)
	e.holdByMember(t)
	e.tickAll(t, e.s)

	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		SELECT workspace_id, issue_id, pull_request_id, 'head_changed', 'newer-head', 'base-1', now() - interval '1 hour', 'head_changed|newer-head'
		FROM issue_pr_event WHERE id=$1`, e.event)
	e.tickAll(t, e.s)

	if inbox, _ := e.notices(t); inbox != 1 {
		t.Fatalf("two events on one PR left %d unread notices, want 1", inbox)
	}
	if got := e.f.Count(t, `SELECT count(*) FROM issue_pr_event WHERE issue_id=$1 AND status='notified'`, e.issue); got != 2 {
		t.Fatalf("%d events are notified, want 2", got)
	}
}

func TestNotifiedBackfillKeepsOneNoticePerEvent(t *testing.T) {
	e := newPREventEnv(t)
	e.holdByMember(t)
	e.f.Exec(t, `UPDATE issue_pr_event SET status='next_turn' WHERE id=$1`, e.event)
	for range 5 {
		e.f.Exec(t, `INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, issue_id, title, details)
			VALUES ($1,'member',$2,'agent_blocked','info',$3,'dup',jsonb_build_object('outcome','notified','event_id',$4::text))`,
			e.f.WorkspaceID, e.f.UserID, e.issue, e.event)
	}
	e.f.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details)
		VALUES ($1,$2,'system','wakeup_triggered',jsonb_build_object('outcome','notified','event_id',$3::text))`, e.f.WorkspaceID, e.issue, e.event)

	sql, err := os.ReadFile("../../migrations/999h_issue_pr_event_notified_backfill.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.f.Pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if got := e.status(t); got != "notified" {
		t.Fatalf("event is %q after the backfill, want notified", got)
	}
	if inbox, _ := e.notices(t); inbox != 1 {
		t.Fatalf("backfill left %d unarchived notices for the event, want 1", inbox)
	}
}

func TestTwoPRsBatchedInOneWakeKeepOneNoticeEach(t *testing.T) {
	e := newPREventEnv(t)
	e.holdByMember(t)
	var other string
	e.f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'acme', 'widget', 904, 'second', 'open', 'https://example.test/pr2', now(), now(), 'other-head') RETURNING id`, e.f.WorkspaceID).Scan(&other)
	e.f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, other)
	e.f.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, other)
	e.f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, e.issue, other)
	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1,$2,$3,'head_changed','other-head','base-1', now() - interval '1 day','head_changed|other-head')`, e.f.WorkspaceID, e.issue, other)

	for range 3 {
		e.tickAll(t, e.s)
	}

	if inbox, _ := e.notices(t); inbox != 2 {
		t.Fatalf("two PRs in one wake left %d unread notices, want 2 (one per PR)", inbox)
	}
	if got := e.f.Count(t, `SELECT count(*) FROM issue_pr_event WHERE issue_id=$1 AND status='notified'`, e.issue); got != 2 {
		t.Fatalf("%d events are notified, want 2", got)
	}
}
