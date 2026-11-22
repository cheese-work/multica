package handler

import (
	"context"
	"net/http"
	"testing"
)

// CHE-8879: an applied decision action that changes an issue fires the
// issue.updated event wakeups armed on that issue, once. A rejected or failed
// action fires none.

// armIssueUpdatedWakeup arms a once-mode issue.updated event wakeup on the issue.
func armIssueUpdatedWakeup(t *testing.T, issueID string) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(context.Background(), `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, event_types, timezone)
		VALUES (gen_random_uuid(), $1, $2, $3, 'canary', 'event', 'once', ARRAY['issue.updated'], 'UTC') RETURNING id::text`,
		testWorkspaceID, issueID, seededReadyAgentID(t)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, id)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, id)
	return id
}

func wakeupReceipts(t *testing.T, wakeupID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND event_type = 'issue.updated'`, wakeupID)
}

func TestApprovedWorkflowSetFiresIssueUpdatedWakeupOnce(t *testing.T) {
	f := newRebindFixture(t, 8879100)
	card := openWorkflowSetCard(t, f)
	wakeup := armIssueUpdatedWakeup(t, f.c.issueID)
	if out := card.approve(t); out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	if got := wakeupReceipts(t, wakeup); got != 1 {
		t.Fatalf("issue.updated receipts after an applied workflow set = %d, want 1", got)
	}
	var changed string
	dbfx.QueryRow(t, `SELECT payload->'changed_fields'::text FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup).Scan(&changed)
	if changed != `["workflow_id"]` {
		t.Fatalf("changed_fields = %s, want [\"workflow_id\"]", changed)
	}
}

func TestFailedWorkflowSetFiresNoIssueUpdatedWakeup(t *testing.T) {
	f := newRebindFixture(t, 8879200)
	card := openWorkflowSetCard(t, f)
	wakeup := armIssueUpdatedWakeup(t, f.c.issueID)
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 60 WHERE id = $1`, f.wide)
	if out := card.approve(t); out.State != "failed" {
		t.Fatalf("outcome = %+v, want failed", out)
	}
	if got := wakeupReceipts(t, wakeup); got != 0 {
		t.Fatalf("issue.updated receipts after a failed workflow set = %d, want 0", got)
	}
}

func TestApprovedRunBudgetRaiseFiresIssueUpdatedWakeupOnce(t *testing.T) {
	raise := newPendingRaise(t, baseCap+5)
	wakeup := armIssueUpdatedWakeup(t, raise.issueID)
	if out := raise.approveAccepted(t); out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	if got := wakeupReceipts(t, wakeup); got != 1 {
		t.Fatalf("issue.updated receipts after an applied run-budget raise = %d, want 1", got)
	}
}

func TestRejectedRunBudgetRaiseFiresNoIssueUpdatedWakeup(t *testing.T) {
	raise := newPendingRaise(t, baseCap+5)
	wakeup := armIssueUpdatedWakeup(t, raise.issueID)
	raise.reject(t, testHandler, http.StatusOK)
	if got := wakeupReceipts(t, wakeup); got != 0 {
		t.Fatalf("issue.updated receipts after a rejected action = %d, want 0", got)
	}
}
