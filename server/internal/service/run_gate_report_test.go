package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1919: offline 2-, 5- and 10-minute coalescing counterfactuals. They
// measure what a fixed leading-edge window would have folded; they are never a
// delivery receipt and nothing reads them to suppress a run.

func cfJoin(key string, at time.Duration, over func(*RunGateJoin)) RunGateJoin {
	j := RunGateJoin{
		EventID: "e-" + key, Source: "comment_leader", EventKey: key, IssueID: "issue-1", AgentID: "agent-1",
		Origin: "issue_assignee", PRHeadSHA: "h1", EventTime: time.Unix(1_000_000, 0).Add(at), ActiveTaskID: "active-" + key,
	}
	if over != nil {
		over(&j)
	}
	return j
}

func TestSimulateRunGateCoalescing(t *testing.T) {
	// Deliberately out of time order: the simulation sorts its own copy.
	joins := []RunGateJoin{
		cfJoin("e4", 6*time.Minute, nil),
		cfJoin("e1", 0, nil),
		cfJoin("e3", 3*time.Minute, nil),
		cfJoin("e2", time.Minute, nil),
		cfJoin("e5", 30*time.Second, func(j *RunGateJoin) { j.PRHeadSHA = "h2" }),
		cfJoin("e6", 2*time.Minute, func(j *RunGateJoin) { j.NonLeaderReply = true }),
		cfJoin("e7", 2*time.Minute, func(j *RunGateJoin) { j.Origin = "mention_squad_leader" }),
		cfJoin("e8", 2*time.Minute, func(j *RunGateJoin) { j.AgentID = "" }),
	}
	want := []struct {
		window                   time.Duration
		opens, folded, protected int
	}{{2 * time.Minute, 4, 1, 3}, {5 * time.Minute, 3, 2, 3}, {10 * time.Minute, 2, 3, 3}}
	for _, w := range want {
		sim := SimulateRunGateCoalescing(joins, w.window)
		if sim.Events != 8 || sim.Opens != w.opens || sim.Folded != w.folded {
			t.Fatalf("%s window: events/opens/folded = %d/%d/%d, want 8/%d/%d", w.window, sim.Events, sim.Opens, sim.Folded, w.opens, w.folded)
		}
		got := sim.Protected["worker_reply"] + sim.Protected["explicit_mention"] + sim.Protected["no_agent"]
		if got != w.protected || sim.Protected["worker_reply"] != 1 || sim.Protected["explicit_mention"] != 1 || sim.Protected["no_agent"] != 1 {
			t.Fatalf("%s window: protected = %v, want one of each reason", w.window, sim.Protected)
		}
		if sim.WindowSeconds != int64(w.window.Seconds()) {
			t.Fatalf("window seconds = %d", sim.WindowSeconds)
		}
	}

	byKey := map[string]RunGateJoin{}
	for _, j := range joins {
		byKey[j.EventKey] = j
	}
	e2 := byKey["e2"]
	if len(e2.Counterfactuals) != 3 {
		t.Fatalf("e2 carries %d counterfactuals, want one per window", len(e2.Counterfactuals))
	}
	for _, cf := range e2.Counterfactuals {
		if cf.Role != "fold" || cf.OpenerEventKey != "e1" || cf.OpenerActiveID != "active-e1" {
			t.Fatalf("e2 counterfactual = %+v, want a fold into e1 keeping e1's active run", cf)
		}
	}
	if cf := byKey["e3"].Counterfactuals; len(cf) != 3 || cf[0].Role != "open" || cf[1].Role != "fold" || cf[2].Role != "fold" {
		t.Fatalf("e3 roles across 2/5/10 min = %+v, want open, fold, fold", cf)
	}
	if cf := byKey["e6"].Counterfactuals[0]; cf.Role != "protected" || cf.Reason != "worker_reply" {
		t.Fatalf("e6 = %+v, want protected worker_reply", cf)
	}
}

func TestSimulateRunGateCoalescingWindowEdgeStartsNewWindow(t *testing.T) {
	joins := []RunGateJoin{cfJoin("a", 0, nil), cfJoin("b", 2*time.Minute, nil)}
	sim := SimulateRunGateCoalescing(joins, 2*time.Minute)
	if sim.Opens != 2 || sim.Folded != 0 {
		t.Fatalf("an event exactly one window after the opener opens its own window; got opens/folded = %d/%d", sim.Opens, sim.Folded)
	}
}

func TestSimulateRunGateCoalescingMeasuresWhatFoldingWouldTouch(t *testing.T) {
	joins := []RunGateJoin{
		cfJoin("open", 0, func(j *RunGateJoin) { j.Disposition = DispRunCompletedMutated }),
		cfJoin("native", time.Second, func(j *RunGateJoin) {
			j.NativeFoldEligible, j.AdmissionStatus, j.Disposition = true, "coalesced", DispRunCompletedMutated
		}),
		cfJoin("unknown", 2*time.Second, func(j *RunGateJoin) { j.Disposition = DispUnknownRunMissing }),
		cfJoin("plain", 3*time.Second, func(j *RunGateJoin) { j.Disposition = DispRunCompletedNoRecordedMutation }),
	}
	sim := SimulateRunGateCoalescing(joins, 2*time.Minute)
	if sim.Folded != 3 || sim.FoldedNativeEligible != 1 || sim.FoldedAdmissionCoalesced != 1 || sim.FoldedWithMutation != 1 || sim.FoldedUnknown != 1 {
		t.Fatalf("sim = %+v, want 3 folded: 1 native-eligible, 1 coalesced by the producer, 1 whose run acted, 1 unknown", sim)
	}
}

// CHE-1919: the report joins real ledger rows to real runs, receipts and
// mutations, and counts capture completeness per source against producer rows.
func TestBuildRunGateReportJoinsRealRuns(t *testing.T) {
	f, s, issue, agent := gateFixture(t)
	ctx := context.Background()
	issueID := util.UUIDToString(issue)
	f.Cleanup(t, "DELETE FROM run_gate_event WHERE workspace_id=$1", f.WorkspaceID)
	runtimeSQL := testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')")
	ago := func(minutes int) testutil.Raw {
		return testutil.Raw(fmt.Sprintf("now() - interval '%d minutes'", minutes))
	}
	comment := func(minutes int, over ...testutil.Cols) string {
		return f.Comment(t, issueID, "c", append([]testutil.Cols{{"created_at": ago(minutes)}}, over...)...)
	}
	task := func(c testutil.Cols) string {
		c["is_leader_task"], c["issue_id"], c["runtime_id"] = true, issueID, runtimeSQL
		return f.Task(t, agent, c)
	}
	ledger := func(source, key, origin string, minutes int, over testutil.Cols) {
		cols := testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": f.WorkspaceID, "issue_id": issueID, "source": source,
			"event_key": key, "candidate_key": agent, "origin": origin, "agent_id": agent, "issue_revision": 1, "issue_status": "in_progress",
			"event_time": ago(minutes), "native_fold_eligible": false}
		for k, v := range over {
			cols[k] = v
		}
		f.Insert(t, "run_gate_event", cols)
	}
	arr := func(ids ...string) testutil.Raw {
		return testutil.Raw("ARRAY['" + strings.Join(ids, "','") + "']::uuid[]")
	}

	c1, c2, c3, c4, c5, c6, c9 := comment(55), comment(54), comment(53), comment(52), comment(51), comment(50), comment(49)
	// c1: a run that posted a comment, changed the status and owns a wakeup rule.
	t1 := task(testutil.Cols{"status": "completed", "trigger_comment_id": c1, "started_at": ago(48), "completed_at": ago(44)})
	f.Comment(t, issueID, "reply", testutil.Cols{"author_type": "agent", "author_id": agent, "source_task_id": t1})
	f.Insert(t, "activity_log", testutil.Cols{"workspace_id": f.WorkspaceID, "issue_id": issueID, "actor_type": "agent", "actor_id": agent,
		"action": "status_changed", "details": testutil.Raw("'{}'::jsonb"), "created_at": ago(46)})
	// c2: delivered to a run that changed nothing we can see (its own trigger c9 has no ledger row).
	task(testutil.Cols{"status": "completed", "trigger_comment_id": c9, "delivered_comment_ids": arr(c2), "started_at": ago(40), "completed_at": ago(38)})
	// c3: its own run was cancelled and folded into t4's run.
	task(testutil.Cols{"status": "cancelled", "trigger_comment_id": c3, "completed_at": ago(37)})
	task(testutil.Cols{"status": "completed", "trigger_comment_id": c4, "coalesced_comment_ids": arr(c3), "started_at": ago(36), "completed_at": ago(35)})

	ledger("comment_leader", c1, "issue_assignee", 55, testutil.Cols{"trigger_comment_id": c1, "admission_status": "queued", "admission_reason": "queued"})
	ledger("comment_leader", c2, "mention_squad_leader", 54, testutil.Cols{"trigger_comment_id": c2, "admission_status": "coalesced", "admission_reason": "coalesced", "native_fold_eligible": true})
	ledger("comment_leader", c3, "issue_assignee", 53, testutil.Cols{"trigger_comment_id": c3, "admission_status": "queued", "admission_reason": "queued"})
	ledger("comment_leader", c4, "issue_assignee", 52, testutil.Cols{"trigger_comment_id": c4, "admission_status": "queued", "admission_reason": "queued"})
	ledger("comment_leader", c5, "issue_assignee", 51, testutil.Cols{"trigger_comment_id": c5, "admission_status": "blocked", "admission_reason": "runtime_offline"})
	ledger("comment_leader", c6, "issue_assignee", 50, testutil.Cols{"trigger_comment_id": c6})

	// Three PR inputs through the real producer; their receipts are then set to the states under test.
	for i, in := range []PullRequestWakeupInput{
		{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 401, URL: "u", HeadSHA: "h401", MergeCommit: "m401"},
		{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 402, URL: "u", HeadSHA: "h402", Conclusion: "FAILURE"},
		{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 403, URL: "u", HeadSHA: "h403", MergeCommit: "m403"},
	} {
		if err := s.TriggerPullRequestWakeup(ctx, issue, in); err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	t6 := task(testutil.Cols{"status": "completed", "started_at": ago(30), "completed_at": ago(29)})
	f.Insert(t, "issue", testutil.Cols{"workspace_id": f.WorkspaceID, "title": "made by t6", "status": "todo", "priority": "none", "creator_type": "agent",
		"creator_id": agent, "position": 0, "origin_type": "agent_create", "origin_id": t6,
		"number": testutil.Raw("(SELECT COALESCE(MAX(number),0)+1 FROM issue WHERE workspace_id='" + f.WorkspaceID + "')")})
	f.Exec(t, `UPDATE issue_wakeup_receipt SET task_id=$1, processed_at=now() WHERE event_key='github:acme/widget#401'`, t6)
	f.Exec(t, `DELETE FROM issue_wakeup_receipt WHERE event_key='github:acme/widget#402:h402'`)
	f.Exec(t, `UPDATE issue_wakeup_receipt SET task_id=NULL, processed_at=now(), payload=payload||'{"outcome":"paused"}'::jsonb WHERE event_key='github:acme/widget#403'`)
	f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$1 WHERE issue_id=$2 AND system_rule='pr_merged'`, t1, issueID)

	now := time.Now()
	rep, err := BuildRunGateReport(ctx, f.q, parseTestUUID(t, f.WorkspaceID), now.Add(-time.Hour), now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	disp := map[string]string{}
	for _, e := range rep.Events {
		disp[e.EventKey] = e.Disposition + "|" + e.Reason
	}
	want := map[string]string{
		c1: DispRunCompletedMutated + "|", c2: DispRunCompletedNoRecordedMutation + "|", c3: DispRunCompletedNoRecordedMutation + "|",
		c4: DispRunCompletedNoRecordedMutation + "|", c5: DispNoRunBlocked + "|runtime_offline", c6: DispUnknownNoAdmissionRecord + "|",
		"github:acme/widget#401":      DispRunCompletedMutated + "|",
		"github:acme/widget#402:h402": DispUnknownReceiptMissing + "|wakeup_rule_present",
		"github:acme/widget#403":      DispReceiptConsumedNoRun + "|paused",
	}
	for k, v := range want {
		if disp[k] != v {
			t.Errorf("event %s = %q, want %q", k, disp[k], v)
		}
	}
	for _, e := range rep.Events {
		switch e.EventKey {
		case c1:
			kinds := map[string]string{}
			for _, m := range e.Runs[0].Mutations {
				kinds[m.Kind] = m.Attribution
			}
			if kinds[MutationCommentPosted] != AttributionTaskID || kinds[MutationStatusChanged] != AttributionWindow || kinds[MutationWakeupCreated] != AttributionTaskID || len(kinds) != 3 {
				t.Errorf("c1 mutations = %v", kinds)
			}
		case c3:
			if len(e.Runs) != 2 || e.Runs[0].Status != "completed" || e.Runs[1].Status != "cancelled" {
				t.Errorf("c3 runs = %+v, want the covering run first and the cancelled duplicate kept", e.Runs)
			}
		case "github:acme/widget#401":
			if len(e.Runs) != 1 || e.Runs[0].TaskID != t6 || e.Runs[0].Mutations[0].Kind != MutationIssueCreated {
				t.Errorf("#401 runs = %+v", e.Runs)
			}
		}
	}

	bySource := map[string]RunGateSourceReport{}
	for _, sr := range rep.Sources {
		bySource[sr.Source] = sr
	}
	comments := bySource[RunGateSourceComment]
	if comments.Captured != 6 || comments.Producer.Evidenced != 5 || comments.Producer.Captured != 4 || comments.Producer.Missing != 1 {
		t.Errorf("comment_leader completeness = captured %d, producer %+v; want 6 captured, 5 evidenced, 4 matched, 1 missing", comments.Captured, comments.Producer)
	}
	if comments.Admission["blocked"] != 1 || comments.Admission["none"] != 1 || comments.NativeFoldEligible != 1 {
		t.Errorf("comment_leader admission = %v native=%d", comments.Admission, comments.NativeFoldEligible)
	}
	if merged := bySource[RunGateSourcePRMerged]; merged.Captured != 2 || merged.Producer.Evidenced != 2 || merged.Producer.Captured != 2 || merged.Producer.Missing != 0 {
		t.Errorf("wakeup_pr_merged completeness = %d, %+v", merged.Captured, merged.Producer)
	}
	if len(comments.Windows) != 3 || comments.Windows[0].WindowSeconds != 120 || comments.Windows[2].WindowSeconds != 600 {
		t.Errorf("comment_leader windows = %+v, want 2, 5 and 10 minutes", comments.Windows)
	}
	if rep.LedgerFirstEventAt == nil {
		t.Error("ledger_first_event_at missing")
	}
	total := 0
	for _, sr := range rep.Sources {
		total += sr.Captured
	}
	if total != len(rep.Events) || total != 9 {
		t.Errorf("sources capture %d events, report lists %d, want 9", total, len(rep.Events))
	}
}

// CHE-1919 review round: deleting an issue (and with it the PR wakeup rule)
// removes the rule's receipts at any age. The ledger row survives. The report
// must not call that expiry, and must not offer the surviving receipts as a
// complete denominator.
func TestBuildRunGateReportRecentRuleDeletionIsNotExpiry(t *testing.T) {
	f, s, issue, _ := gateFixture(t)
	ctx := context.Background()
	for i, in := range []PullRequestWakeupInput{
		{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 501, URL: "u", HeadSHA: "h501", MergeCommit: "m501"},
		{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 502, URL: "u", HeadSHA: "h502", Conclusion: "FAILURE"},
	} {
		if err := s.TriggerPullRequestWakeup(ctx, issue, in); err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	if err := f.q.DeleteIssue(ctx, db.DeleteIssueParams{ID: issue, WorkspaceID: parseTestUUID(t, f.WorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM run_gate_event WHERE workspace_id=$1 AND source <> 'comment_leader'`, f.WorkspaceID); got != 2 {
		t.Fatalf("ledger rows after issue deletion = %d, want 2 (the ledger has no foreign key)", got)
	}

	now := time.Now()
	rep, err := BuildRunGateReport(ctx, f.q, parseTestUUID(t, f.WorkspaceID), now.Add(-time.Hour), now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rep.Events {
		if e.Disposition != "unknown_receipt_missing" || e.Reason != "wakeup_rule_deleted" {
			t.Errorf("event %s = %s|%s, want unknown_receipt_missing|wakeup_rule_deleted (cause is deletion, not expiry)", e.EventKey, e.Disposition, e.Reason)
		}
	}
	for _, sr := range rep.Sources {
		if sr.Source == RunGateSourceComment {
			continue
		}
		raw, _ := json.Marshal(sr.Producer)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		if p["denominator_complete"] != false {
			t.Errorf("%s producer = %s, want denominator_complete=false", sr.Source, raw)
		}
		if p["evidenced"] != float64(0) || p["ledger_rule_gone"] != float64(1) {
			t.Errorf("%s producer = %s, want 0 surviving receipts and 1 ledger event whose rule is gone", sr.Source, raw)
		}
		if limits, _ := p["limits"].([]any); len(limits) != 1 || limits[0] != "receipts_deleted_with_rule" {
			t.Errorf("%s producer = %s, want exactly the receipts_deleted_with_rule limit for a recent window", sr.Source, raw)
		}
	}
}
