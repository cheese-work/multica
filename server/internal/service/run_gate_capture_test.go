package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1918 (CHE-1917 Slice 0a): the pr_merged and pr_checks_failed producers
// write one durable run_gate_event row per input. Capture only: the wakeup
// itself behaves as before.

type gateRow struct {
	source, rule, key, status string
	wakeupID, receiptID       string
	agentID, activeTaskID     *string
	foldTaskID                *string
	head                      *string
	revision                  int64
	eligible                  bool
	seen                      int
	snapshot                  []byte
}

func gateRows(t *testing.T, f principalFixture, issue pgtype.UUID) []gateRow {
	t.Helper()
	rows, err := f.Pool.Query(context.Background(), `SELECT source, system_rule, event_key, issue_status, wakeup_id::text, receipt_id::text,
		agent_id::text, active_task_id::text, native_fold_task_id::text, pr_head_sha, issue_revision, native_fold_eligible, seen_count, queue_snapshot
		FROM run_gate_event WHERE issue_id=$1 ORDER BY first_seen_at, id`, issue)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []gateRow
	for rows.Next() {
		var r gateRow
		if err := rows.Scan(&r.source, &r.rule, &r.key, &r.status, &r.wakeupID, &r.receiptID, &r.agentID, &r.activeTaskID, &r.foldTaskID, &r.head, &r.revision, &r.eligible, &r.seen, &r.snapshot); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func gateFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string) {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	f.Cleanup(t, "DELETE FROM run_gate_event WHERE issue_id=$1", issue)
	return f, s, issue, agent
}

func TestRunGateCapturesBothPRWakeupFamilies(t *testing.T) {
	f, s, issue, agent := gateFixture(t)
	ctx := context.Background()
	running := f.Task(t, agent, testutil.Cols{"issue_id": issue, "status": "running", "started_at": testutil.Raw("now()"),
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')")})
	var revision int64
	f.QueryRow(t, `SELECT revision FROM issue WHERE id=$1`, issue).Scan(&revision)

	for _, in := range []PullRequestWakeupInput{
		{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 301, URL: "u", HeadSHA: "head-m1", MergeCommit: "merge-m1"},
		{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 302, URL: "u", HeadSHA: "head-f1", Conclusion: "FAILURE"},
	} {
		if err := s.TriggerPullRequestWakeup(ctx, issue, in); err != nil {
			t.Fatal(err)
		}
	}
	rows := gateRows(t, f, issue)
	if len(rows) != 2 {
		t.Fatalf("captured %d rows, want 2 (one per family)", len(rows))
	}
	want := []struct{ source, rule, key, head string }{
		{"wakeup_pr_merged", SystemRulePRMerged, "github:acme/widget#301", "head-m1"},
		{"wakeup_pr_checks_failed", SystemRulePRChecksFailed, "github:acme/widget#302:head-f1", "head-f1"},
	}
	for i, w := range want {
		r := rows[i]
		if r.source != w.source || r.rule != w.rule || r.key != w.key || r.head == nil || *r.head != w.head {
			t.Fatalf("row %d = %+v, want %+v", i, r, w)
		}
		var receipt, wakeup string
		f.QueryRow(t, `SELECT r.id::text, r.wakeup_id::text FROM issue_wakeup_receipt r WHERE r.wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1 AND system_rule=$2) AND r.event_key=$3`, issue, w.rule, w.key).Scan(&receipt, &wakeup)
		if r.receiptID != receipt || r.wakeupID != wakeup {
			t.Fatalf("row %d receipt/wakeup = %s/%s, want %s/%s", i, r.receiptID, r.wakeupID, receipt, wakeup)
		}
		if r.revision != revision || r.status != "in_progress" {
			t.Fatalf("row %d issue state = rev %d %s, want rev %d in_progress", i, r.revision, r.status, revision)
		}
		if r.agentID == nil || *r.agentID != agent || r.activeTaskID == nil || *r.activeTaskID != running {
			t.Fatalf("row %d agent/active = %v/%v, want %s/%s", i, r.agentID, r.activeTaskID, agent, running)
		}
		var snapshot []map[string]any
		if err := json.Unmarshal(r.snapshot, &snapshot); err != nil || len(snapshot) != 1 || snapshot[0]["id"] != running || snapshot[0]["status"] != "running" {
			t.Fatalf("row %d queue snapshot = %s (%v)", i, r.snapshot, err)
		}
	}
}

func TestRunGateRedeliveryCountsOnceAndKeepsOneRow(t *testing.T) {
	f, s, issue, _ := gateFixture(t)
	in := PullRequestWakeupInput{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 311, URL: "u", HeadSHA: "head-r1", Conclusion: "FAILURE"}
	for i := 0; i < 3; i++ {
		if err := s.TriggerPullRequestWakeup(context.Background(), issue, in); err != nil {
			t.Fatal(err)
		}
	}
	rows := gateRows(t, f, issue)
	if len(rows) != 1 || rows[0].seen != 3 {
		t.Fatalf("rows = %+v, want one row seen 3 times", rows)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1`, issue); got != 1 {
		t.Fatalf("receipts = %d, want 1: redelivery must not add inputs", got)
	}
}

// A queued wakeup run takes later facts of the same rule into its evidence
// (ReplaceWakeupEvidence); capture records that as native fold eligibility.
func TestRunGateRecordsNativeFoldEligibilityForQueuedWakeupRun(t *testing.T) {
	f, s, issue, _ := gateFixture(t)
	ctx := context.Background()
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 321, URL: "u", MergeCommit: "m1"}); err != nil {
		t.Fatal(err)
	}
	var queued string
	f.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_merged' AND status='queued'`, issue).Scan(&queued)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 322, URL: "u", MergeCommit: "m2"}); err != nil {
		t.Fatal(err)
	}
	rows := gateRows(t, f, issue)
	if len(rows) != 2 || rows[0].eligible || rows[0].foldTaskID != nil {
		t.Fatalf("first row must see an empty queue: %+v", rows)
	}
	if !rows[1].eligible || rows[1].foldTaskID == nil || *rows[1].foldTaskID != queued {
		t.Fatalf("second row must see the queued wakeup run %s: %+v", queued, rows[1])
	}
}

// A ledger write that fails must not lose or delay the input it describes.
func TestRunGateCaptureFailureKeepsTheWakeupInput(t *testing.T) {
	f, s, issue, _ := gateFixture(t)
	ctx := context.Background()
	f.Exec(t, `CREATE FUNCTION run_gate_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'run gate test failure'; END $$`)
	f.Cleanup(t, `DROP FUNCTION run_gate_test_fail() CASCADE`)
	f.Exec(t, `CREATE TRIGGER run_gate_test_fail BEFORE INSERT ON run_gate_event FOR EACH ROW WHEN (NEW.event_key LIKE '%#331%') EXECUTE FUNCTION run_gate_test_fail()`)

	for _, n := range []int32{331, 332} {
		in := PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: n, URL: "u", MergeCommit: "m"}
		if err := s.TriggerPullRequestWakeup(ctx, issue, in); err != nil {
			t.Fatalf("PR %d: capture failure leaked into the producer: %v", n, err)
		}
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND r.event_key LIKE 'github:acme/widget#33_'`, issue); got != 2 {
		t.Fatalf("receipts = %d, want both inputs kept", got)
	}
	rows := gateRows(t, f, issue)
	if len(rows) != 1 || !strings.HasSuffix(rows[0].key, "#332") {
		t.Fatalf("rows = %+v, want only the event whose write succeeded", rows)
	}
}

var _ = db.IssueWakeup{}
