package service

import (
	"context"
	"testing"
)

// One PR event wakes an issue at most once (CHE-2540): a cause the legacy PR
// rule already carries is not queued again through the pr.event fallback.

// coalesceEnv links PR #300 (the one failedChecks reports) to the issue.
type coalesceEnv struct {
	builtinEnv
	pr string
}

func newCoalesceEnv(t *testing.T) coalesceEnv {
	t.Helper()
	e := newBuiltinEnv(t)
	var prID string
	e.f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, head_repo_same)
		VALUES ($1, 1, 'acme', 'widget', 300, 'a title', 'open', 'https://example.test/pr', now(), now(), 'h1', true) RETURNING id`, e.f.WorkspaceID).Scan(&prID)
	e.f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, prID)
	e.f.Cleanup(t, `DELETE FROM issue_pr_event WHERE issue_id=$1`, e.issue)
	e.f.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, prID)
	e.f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, e.issue, prID)
	return coalesceEnv{builtinEnv: e, pr: prID}
}

// captured stores the event the snapshot pipeline raises for the PR.
func (e coalesceEnv) captured(t *testing.T, kind string) string {
	t.Helper()
	return e.capturedOn(t, kind, "base-1")
}

// capturedOn stores the event as raised on the given base commit; a conflict is
// deduped per head and base, so a new base is a new event.
func (e coalesceEnv) capturedOn(t *testing.T, kind, base string) string {
	t.Helper()
	dedupe := kind + "|h1"
	if kind == "conflict" {
		dedupe += "|" + base
	}
	var id string
	e.f.QueryRow(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, $4, 'h1', $5, now(), $6) RETURNING id`, e.f.WorkspaceID, e.issue, e.pr, kind, base, dedupe).Scan(&id)
	return id
}

// tick runs one pass: bridge captured events, then let the runs it queued start
// and end.
func (e coalesceEnv) tick(t *testing.T) {
	t.Helper()
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	e.finishTasks(t)
}

func (e coalesceEnv) conflicting(t *testing.T) {
	t.Helper()
	err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRAttention, Reason: PRAttentionMergeability,
		MergeState: "DIRTY", RepoOwner: "acme", RepoName: "widget", Number: 300, URL: "https://github.com/acme/widget/pull/300", HeadSHA: "h1", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
}

// sweep runs the pass that bridges captured events and lets every run it
// queues start and end, until nothing is left to wake. An issue holds one run
// at a time, so a duplicate cause only shows once the first run has ended.
func (e coalesceEnv) sweep(t *testing.T) {
	t.Helper()
	for range 4 {
		if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
			t.Fatal(err)
		}
		e.finishTasks(t)
	}
}

func (e coalesceEnv) runs(t *testing.T) int {
	t.Helper()
	return e.tasks(t, SystemRulePRChecksFailed) + e.tasks(t, SystemRulePRAttention)
}

func (e coalesceEnv) status(t *testing.T, event string) string {
	t.Helper()
	var s string
	e.f.QueryRow(t, `SELECT status FROM issue_pr_event WHERE id=$1`, event).Scan(&s)
	return s
}

func TestChecksFailedWakesOnceWhenLegacyRuleAndPREventBothCarryIt(t *testing.T) {
	e := newCoalesceEnv(t)
	event := e.captured(t, "checks_failed")
	e.failedChecks(t, "h1", "FAILURE")
	e.finishTasks(t) // the legacy run ended before the sweep bridged the event
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("checks_failed started %d runs, want 1", got)
	}
	if got := e.status(t, event); got != "delivered" {
		t.Fatalf("event is %q once the legacy run carried it, want delivered", got)
	}
}

func TestConflictWakesOnceWhenLegacyRuleAndPREventBothCarryIt(t *testing.T) {
	e := newCoalesceEnv(t)
	e.captured(t, "conflict")
	e.conflicting(t)
	e.finishTasks(t) // the legacy run ended before the sweep bridged the event
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("conflict started %d runs, want 1", got)
	}
}

// The fallback ran first: the legacy callback of the same snapshot arrives
// after the event is already carried.
func TestChecksFailedWakesOnceWhenFallbackRunsBeforeLegacyRule(t *testing.T) {
	e := newCoalesceEnv(t)
	event := e.captured(t, "checks_failed")
	e.tick(t)
	e.failedChecks(t, "h1", "FAILURE")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("checks_failed started %d runs, want 1", got)
	}
	if got := e.status(t, event); got != "delivered" {
		t.Fatalf("event is %q, want delivered", got)
	}
}

func TestConflictWakesOnceWhenFallbackRunsBeforeLegacyRule(t *testing.T) {
	e := newCoalesceEnv(t)
	e.captured(t, "conflict")
	e.tick(t)
	e.conflicting(t)
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("conflict started %d runs, want 1", got)
	}
}

// The same head conflicts again on a new base commit: a new event the legacy
// pair key (head and base branch) never sees, so it wakes once more.
func TestConflictOnNewBaseOfSameHeadStillWakes(t *testing.T) {
	e := newCoalesceEnv(t)
	e.captured(t, "conflict")
	e.conflicting(t)
	e.finishTasks(t)
	e.capturedOn(t, "conflict", "base-2")
	e.conflicting(t) // same head and branch: the pair key keeps this one silent
	e.sweep(t)
	if got := e.runs(t); got != 2 {
		t.Fatalf("conflict on a new base started %d runs, want 2", got)
	}
}

// Guards: the paths that must keep their one run.

func TestHeadChangedAloneWakesOnce(t *testing.T) {
	e := newCoalesceEnv(t)
	e.captured(t, "head_changed")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("head_changed started %d runs, want 1", got)
	}
}

func TestLegacyRuleAloneWakesOnceWithoutCapturedEvents(t *testing.T) {
	e := newCoalesceEnv(t)
	e.failedChecks(t, "h1", "FAILURE")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("legacy-only checks_failed started %d runs, want 1", got)
	}
}

// The legacy receipt carries head h1 only: a failure on another head is a
// different cause and still wakes.
func TestLegacyReceiptOfAnotherHeadDoesNotCoverTheEvent(t *testing.T) {
	e := newCoalesceEnv(t)
	e.f.Exec(t, `UPDATE github_pull_request SET head_sha='h2' WHERE id=$1`, e.pr)
	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, 'checks_failed', 'h2', 'base-1', now(), 'checks_failed|h2')`, e.f.WorkspaceID, e.issue, e.pr)
	e.failedChecks(t, "h1", "FAILURE")
	e.sweep(t)
	if got := e.runs(t); got != 2 {
		t.Fatalf("two causes started %d runs, want 2", got)
	}
}
