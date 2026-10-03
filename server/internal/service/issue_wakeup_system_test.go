package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func sub(stage int32, closed, cancelled bool) subIssue {
	s := subIssue{Closed: closed, Cancelled: cancelled}
	if stage > 0 {
		s.Stage = pgtype.Int4{Int32: stage, Valid: true}
	}
	return s
}

// The system rule's reading: a stage closing while a later one waits, then
// every sub-issue closing. People's children_done conditions keep theirs.
func TestStageProgress(t *testing.T) {
	cases := []struct {
		name        string
		children    []subIssue
		met         bool
		fingerprint string
		next        int32
	}{
		{"no sub-issues", nil, false, "", 0},
		{"unstaged, one open", []subIssue{sub(0, true, false), sub(0, false, false)}, false, "", 0},
		{"unstaged, all closed", []subIssue{sub(0, true, false), sub(0, true, true)}, true, "all", 0},
		{"stage 1 closed, stage 2 waits", []subIssue{sub(1, true, false), sub(1, true, true), sub(2, false, false)}, true, "stage:1", 2},
		{"two stages closed, stage 3 waits", []subIssue{sub(1, true, false), sub(2, true, false), sub(3, false, false)}, true, "stage:2", 3},
		{"a later stage closed first", []subIssue{sub(1, false, false), sub(2, true, false)}, false, "", 0},
		{"last stage closed, unstaged open", []subIssue{sub(1, true, false), sub(2, true, false), sub(0, false, false)}, false, "", 0},
		{"everything closed", []subIssue{sub(1, true, false), sub(2, true, true), sub(0, true, false)}, true, "all", 0},
		{"stage waits on unstaged-free set", []subIssue{sub(1, false, false), sub(0, true, false)}, false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			met, fingerprint, observed := stageProgress(tc.children)
			if met != tc.met || fingerprint != tc.fingerprint {
				t.Fatalf("stageProgress = %v %q, want %v %q (%v)", met, fingerprint, tc.met, tc.fingerprint, observed)
			}
			if tc.next != 0 && observed["next_stage"] != tc.next {
				t.Fatalf("next_stage = %v, want %d", observed["next_stage"], tc.next)
			}
		})
	}
}

func TestStageProgressCountsCancellations(t *testing.T) {
	_, _, observed := stageProgress([]subIssue{sub(1, true, true), sub(1, true, false), sub(2, false, false)})
	stages := observed["stages"].([]stageCount)
	if observed["cancelled"] != 1 || len(stages) != 2 || stages[0] != (stageCount{Stage: 1, Total: 2, Closed: 2, Cancelled: 1}) {
		t.Fatalf("observed = %+v", observed)
	}
}

func TestChildrenDoneStageCondition(t *testing.T) {
	stage := int32(1)
	children := []subIssue{sub(1, true, false), sub(2, false, false), sub(0, false, false)}
	if met, _, _ := childrenDone(WakeupCondition{Type: "children_done", Stage: &stage}, children); !met {
		t.Fatal("stage 1 closed but the stage condition did not hold")
	}
	if met, _, _ := childrenDone(WakeupCondition{Type: "children_done"}, children); met {
		t.Fatal("the all-children condition held while sub-issues were open")
	}
	missing := int32(3)
	if met, _, _ := childrenDone(WakeupCondition{Type: "children_done", Stage: &missing}, children); met {
		t.Fatal("a stage without sub-issues held")
	}
}

func TestChildDoneInstructionPrecedence(t *testing.T) {
	settings := []byte(`{"system_wakeup_child_done":false,"system_wakeup_child_done_instruction":"  Workspace way  "}`)
	if enabled, instruction := SystemWakeupDefault(settings); enabled || instruction != "Workspace way" {
		t.Fatalf("default = %v %q", enabled, instruction)
	}
	if got := ChildDoneInstruction(" Issue way ", settings); got != "Issue way" {
		t.Fatalf("issue instruction = %q", got)
	}
	if got := ChildDoneInstruction("", settings); got != "Workspace way" {
		t.Fatalf("workspace instruction = %q", got)
	}
	if got := ChildDoneInstruction("", []byte(`{}`)); got != ChildDoneDefaultInstruction {
		t.Fatalf("built-in instruction = %q", got)
	}
	if enabled, _ := SystemWakeupDefault([]byte(`not json`)); !enabled {
		t.Fatal("unreadable settings must keep the rule on")
	}
}

func TestPRWakeupSettingsDefaultOnAndIndependent(t *testing.T) {
	if enabled, err := PRWakeupEnabled([]byte(`{}`), SystemRulePRMerged); err != nil || !enabled {
		t.Fatalf("merge default = %v, %v; want enabled", enabled, err)
	}
	if enabled, err := PRWakeupEnabled([]byte(`{"github_wake_on_pr_merge":false}`), SystemRulePRMerged); err != nil || enabled {
		t.Fatalf("merge override = %v, %v; want disabled", enabled, err)
	}
	if enabled, err := PRWakeupEnabled([]byte(`{"github_wake_on_pr_merge":false}`), SystemRulePRChecksFailed); err != nil || !enabled {
		t.Fatalf("CI failure setting inherited merge override: %v, %v", enabled, err)
	}
	if enabled, err := PRWakeupEnabled([]byte(`{"github_enabled":false}`), SystemRulePRChecksFailed); err != nil || enabled {
		t.Fatalf("GitHub master switch = %v, %v; want disabled", enabled, err)
	}
}

func TestPRWakeupLedgerHasDetachedUniqueIndex(t *testing.T) {
	f, _, _, _ := wakeFixture(t)
	if got := f.Count(t, `SELECT count(*) FROM pg_constraint WHERE conrelid='issue_wakeup_pr_event'::regclass AND contype IN ('f','p')`); got != 0 {
		t.Fatalf("ledger foreign-key/primary-key constraints = %d, want 0", got)
	}
	if got := f.Count(t, `SELECT count(*) FROM pg_index WHERE indrelid='issue_wakeup_pr_event'::regclass AND indisunique AND indisvalid AND NOT indisprimary`); got != 1 {
		t.Fatalf("valid standalone unique ledger indexes = %d, want 1", got)
	}
}

func TestPRWakeupRatePauseRetainsEventsAndRecovers(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	ctx := context.Background()
	trigger := func(head string) {
		t.Helper()
		if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
			Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 157,
			URL: "https://github.com/acme/widget/pull/157", HeadSHA: head, Conclusion: "FAILURE",
		}); err != nil {
			t.Fatal(err)
		}
	}
	completeQueued := func() {
		t.Helper()
		f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND context->>'wakeup_system'=$2 AND status='queued'`, issue, SystemRulePRChecksFailed)
	}
	for index := 0; index < wakeupHourlyRunLimit; index++ {
		trigger(fmt.Sprintf("head-%02d", index))
		completeQueued()
	}
	trigger("head-at-cap")
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Enabled || !w.PausedReason.Valid || w.PausedReason.String != wakeupPausedRate {
		t.Fatalf("rule at hourly cap = enabled %v, pause %q; want rate-paused", w.Enabled, w.PausedReason.String)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); got != 1 {
		t.Fatalf("receipt at rate pause = %d pending, want 1", got)
	}
	trigger("head-while-paused")
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); got != 2 {
		t.Fatalf("receipts captured while paused = %d pending, want 2", got)
	}
	f.Exec(t, `UPDATE agent_task_queue SET created_at=now()-interval '2 hours' WHERE issue_id=$1 AND context->>'wakeup_id'=$2`, issue, w.ID)
	trigger("head-after-recovery")
	w, err = f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.PausedReason.Valid {
		t.Fatalf("rule after rate recovery = enabled %v, pause %q; want enabled and cleared", w.Enabled, w.PausedReason.String)
	}
	if got := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRChecksFailed); got != wakeupHourlyRunLimit+1 {
		t.Fatalf("wake tasks after recovery = %d, want %d", got, wakeupHourlyRunLimit+1)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); got != 0 {
		t.Fatalf("pending receipts after recovered dispatch = %d, want 0", got)
	}
}

func TestPRWakeupLedgerIsRemovedByIssueWorkspaceAndRuleDeletion(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	ctx := context.Background()
	makePRLedger := func(issueID pgtype.UUID, number int32) pgtype.UUID {
		t.Helper()
		assignPRWakeupIssue(t, f, issueID, agent)
		if err := s.TriggerPullRequestWakeup(ctx, issueID, PullRequestWakeupInput{
			Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: number,
			URL: fmt.Sprintf("https://github.com/acme/widget/pull/%d", number), MergeCommit: fmt.Sprintf("merge-%d", number),
		}); err != nil {
			t.Fatal(err)
		}
		w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issueID, SystemRule: systemRuleText(SystemRulePRMerged)})
		if err != nil {
			t.Fatal(err)
		}
		return w.ID
	}

	ruleIssue := f.Issue(t, "delete wakeup ledger")
	ruleID := parseTestUUID(t, ruleIssue)
	rule := wakeCreate(t, f, s, ruleID, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "Check the captured event"})
	f.Exec(t, `INSERT INTO issue_wakeup_pr_event(wakeup_id,event_key) VALUES($1,'rule-delete')`, rule.ID)
	if err := s.Delete(ctx, ruleID, rule.ID, parseTestUUID(t, f.UserID)); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_pr_event WHERE wakeup_id=$1`, rule.ID); got != 0 {
		t.Fatalf("ledger rows after individual wakeup deletion = %d, want 0", got)
	}

	issueLedgerID := makePRLedger(issue, 158)
	if err := f.q.DeleteIssue(ctx, db.DeleteIssueParams{ID: issue, WorkspaceID: parseTestUUID(t, f.WorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_pr_event WHERE wakeup_id=$1`, issueLedgerID); got != 0 {
		t.Fatalf("ledger rows after issue deletion = %d, want 0", got)
	}

	workspaceIssue := parseTestUUID(t, f.Issue(t, "delete workspace ledger"))
	workspaceLedgerID := makePRLedger(workspaceIssue, 159)
	if err := f.q.DeleteWorkspaceIssueRoots(ctx, parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_pr_event WHERE wakeup_id=$1`, workspaceLedgerID); got != 0 {
		t.Fatalf("ledger rows after workspace issue teardown = %d, want 0", got)
	}
}

func TestPRWakeupOrphanCleanupRemovesLedger(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	ctx := context.Background()
	assignPRWakeupIssue(t, f, issue, agent)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
		Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 160,
		URL: "https://github.com/acme/widget/pull/160", MergeCommit: "merge-160",
	}); err != nil {
		t.Fatal(err)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRMerged)})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id=$1", w.ID)
	f.Cleanup(t, "DELETE FROM issue_wakeup_pr_event WHERE wakeup_id=$1", w.ID)
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE id=$1", w.ID)

	orphanIssueID := dbid.NewV7()
	f.Exec(t, `UPDATE issue_wakeup SET issue_id=$2 WHERE id=$1`, w.ID, orphanIssueID)
	orphan, err := f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.dispatchSystem(ctx, orphan); err != nil {
		t.Fatalf("dispatch orphan wakeup cleanup: %v", err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1`, w.ID); got != 0 {
		t.Fatalf("orphan cleanup left %d receipts", got)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_pr_event WHERE wakeup_id=$1`, w.ID); got != 0 {
		t.Fatalf("orphan cleanup left %d ledger rows", got)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id=$1`, w.ID); got != 0 {
		t.Fatalf("orphan cleanup left %d wakeups", got)
	}
}

func TestPRFailureReplayAfterReceiptExpiryRemainsDeduplicated(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	ctx := context.Background()
	input := PullRequestWakeupInput{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 157, URL: "https://github.com/acme/widget/pull/157", HeadSHA: "head-x", Conclusion: "FAILURE"}
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1`, issue)
	f.Exec(t, `UPDATE issue_wakeup_receipt r SET created_at=now()-interval '10 days',processed_at=now()-interval '9 days' FROM issue_wakeup w WHERE w.id=r.wakeup_id AND w.issue_id=$1 AND w.system_rule='pr_checks_failed'`, issue)
	removed, err := f.q.DeleteExpiredWakeupReceipts(ctx, pgtype.Timestamptz{Time: time.Now().Add(-7 * 24 * time.Hour), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expired receipts removed = %d, want 1", removed)
	}
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		return f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_checks_failed'`, issue)
	}
	if got := count(); got != 1 {
		t.Fatalf("replay after receipt expiry queued %d runs, want 1 total", got)
	}
	input.HeadSHA = "head-y"
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2 {
		t.Fatalf("different failing head queued %d runs, want 2 total", got)
	}
}

func assignPRWakeupIssue(t *testing.T, f principalFixture, issue pgtype.UUID, agent string) {
	t.Helper()
	f.Exec(t, `UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2 WHERE id=$1`, issue, agent)
}
