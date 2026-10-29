package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestPRResolverAuthorRouting(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		rule       string
		assignee   string
		author     bool
		configured bool
		reason     string
		mergeState string
		binding    bool
		archived   bool
		target     bool
		wantRun    bool
		wantFYI    bool
	}{
		{name: "member conflict wakes author", rule: SystemRulePRAttention, assignee: "member", author: true, wantRun: true},
		{name: "member failed checks wake author", rule: SystemRulePRChecksFailed, assignee: "member", author: true, wantRun: true},
		{name: "agent fallback without author", rule: SystemRulePRAttention, assignee: "agent", wantRun: true},
		{name: "member conflict without author gets FYI", rule: SystemRulePRAttention, assignee: "member", wantFYI: true},
		{name: "member failed checks without author get FYI", rule: SystemRulePRChecksFailed, assignee: "member", wantFYI: true},
		{name: "member merged notification unchanged", rule: SystemRulePRMerged, assignee: "member", author: true},
		{name: "member passed checks unchanged", rule: SystemRulePRChecksPassed, assignee: "member", author: true},
		{name: "configured default still wakes author", rule: SystemRulePRAttention, assignee: "member", author: true, configured: true, wantRun: true},
		{name: "behind wakes author", rule: SystemRulePRAttention, assignee: "member", author: true, mergeState: "BEHIND", wantRun: true},
		{name: "returned to draft wakes author", rule: SystemRulePRAttention, assignee: "member", author: true, reason: PRAttentionDraft, wantRun: true},
		{name: "author takes precedence over agent assignee", rule: SystemRulePRAttention, assignee: "agent", author: true, wantRun: true},
		{name: "squad fallback without author", rule: SystemRulePRChecksFailed, assignee: "squad", wantRun: true},
		{name: "workspace author binding", rule: SystemRulePRChecksFailed, assignee: "member", author: true, binding: true, wantRun: true},
		{name: "unavailable author falls back to member FYI", rule: SystemRulePRAttention, assignee: "member", author: true, archived: true, wantFYI: true},
		{name: "explicit assignee target stays unchanged", rule: SystemRulePRAttention, assignee: "member", author: true, target: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			env := newBuiltinEnv(t)
			fixture := env.f
			assigneeID := fixture.UserID
			if scenario.assignee == "agent" {
				assigneeID = env.agent
				if scenario.author {
					assigneeID = fixture.privateAgentOwnedBy(t, fixture.UserID, "other-assignee")
				}
			} else if scenario.assignee == "squad" {
				assigneeID = fixture.Squad(t, "resolver squad", env.agent)
			}
			fixture.Exec(t, `UPDATE issue SET status='in_review',assignee_type=$2,assignee_id=$3 WHERE id=$1`, env.issue, scenario.assignee, assigneeID)
			fixture.Cleanup(t, "DELETE FROM inbox_item WHERE issue_id=$1", env.issue)
			if scenario.configured {
				env.define(t, WakeupScopeWorkspace, scenario.rule, `"enabled":true`)
			}
			if scenario.target {
				env.define(t, WakeupScopeWorkspace, scenario.rule, `"target":{"type":"assignee"}`)
			}
			if scenario.author {
				var runtimeID string
				if err := fixture.Pool.QueryRow(context.Background(), "SELECT runtime_id FROM agent WHERE id=$1", env.agent).Scan(&runtimeID); err != nil {
					t.Fatal(err)
				}
				task := fixture.Task(t, env.agent, testutil.Cols{"issue_id": util.UUIDToString(env.issue), "runtime_id": runtimeID, "status": "completed"})
				branch := "agent/pr-author/che-2382-" + strings.ReplaceAll(task, "-", "")[20:]
				fixture.Exec(t, "UPDATE agent_task_queue SET branch_name=$2 WHERE id=$1", task, branch)
				pull := fixture.Insert(t, "github_pull_request", testutil.Cols{
					"workspace_id": fixture.WorkspaceID, "installation_id": 1,
					"repo_owner": "acme", "repo_name": "widget", "pr_number": 2382,
					"title": "PR author routing", "state": "open", "html_url": "https://github.com/acme/widget/pull/2382",
					"branch": branch, "head_sha": "author-head", "pr_created_at": "2030-01-01T00:00:00Z", "pr_updated_at": "2030-01-01T00:00:00Z",
				})
				fixture.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pull)
				fixture.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", env.issue, pull)
				if scenario.binding {
					fixture.Exec(t, "UPDATE github_pull_request SET branch='feature' WHERE id=$1", pull)
					fixture.Exec(t, `UPDATE workspace SET settings=settings || jsonb_build_object('github_review_authors',jsonb_build_object('acme/widget#2382',$2::text)) WHERE id=$1`, fixture.WorkspaceID, env.agent)
				}
				if scenario.archived {
					fixture.Exec(t, "UPDATE agent SET archived_at=now() WHERE id=$1", env.agent)
				}
			}
			input := PullRequestWakeupInput{
				Rule: scenario.rule, RepoOwner: "acme", RepoName: "widget", Number: 2382,
				URL: "https://github.com/acme/widget/pull/2382", HeadSHA: "author-head", BaseBranch: "main",
				Reason: PRAttentionMergeability, MergeState: "DIRTY", MergeCommit: "merge-head", Conclusion: "FAILURE",
			}
			if scenario.rule == SystemRulePRChecksPassed {
				input.Conclusion = "SUCCESS"
			}
			if scenario.mergeState != "" {
				input.MergeState = scenario.mergeState
			}
			if scenario.reason != "" {
				fixture.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", env.issue)
				input.Reason, input.EventAt = scenario.reason, "2026-10-29T01:00:00Z"
			}
			if err := env.s.TriggerPullRequestWakeup(context.Background(), env.issue, input); err != nil {
				t.Fatal(err)
			}
			runs := fixture.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, env.issue, scenario.rule)
			if runs != map[bool]int{true: 1, false: 0}[scenario.wantRun] {
				t.Errorf("resolver runs = %d, want run = %t", runs, scenario.wantRun)
			}
			if scenario.wantRun {
				if count := fixture.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'=$3`, env.issue, env.agent, scenario.rule); count != 1 {
					t.Errorf("author/assignee agent runs = %d, want 1", count)
				}
				activityType, activityID := "agent", env.agent
				if scenario.assignee == "squad" {
					activityType, activityID = "squad", assigneeID
				}
				if count := fixture.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered' AND details->>'target_type'=$2 AND details->>'target_id'=$3 AND details->>'outcome'='woke'`, env.issue, activityType, activityID); count != 1 {
					t.Errorf("timeline entries naming woken agent = %d, want 1", count)
				}
			}
			notifications := fixture.Count(t, "SELECT count(*) FROM inbox_item WHERE issue_id=$1", env.issue)
			if notifications != map[bool]int{true: 1, false: 0}[scenario.wantFYI] {
				t.Errorf("member inbox items = %d, want FYI = %t", notifications, scenario.wantFYI)
			}
			if scenario.wantFYI {
				var body string
				if err := fixture.Pool.QueryRow(context.Background(), "SELECT body FROM inbox_item WHERE issue_id=$1", env.issue).Scan(&body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(strings.ToLower(body), "no resolver agent was found") || strings.Contains(body, "Update the branch") || strings.Contains(body, "Inspect the current check details") {
					t.Errorf("FYI must explain missing resolver without a resolve instruction: %q", body)
				}
				if count := fixture.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered' AND details->>'outcome'='notified'`, env.issue); count != 1 {
					t.Errorf("FYI timeline entries = %d, want 1", count)
				}
			}
			if count := fixture.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND body LIKE $2`, env.issue, "%Update the branch%"); count != 0 {
				t.Errorf("member received resolver instruction in %d inbox items", count)
			}
		})
	}
}

func TestPRResolverKeepsDifferentAuthorsSeparate(t *testing.T) {
	env := newBuiltinEnv(t)
	fixture := env.f
	fixture.Exec(t, `UPDATE issue SET status='backlog',assignee_type='member',assignee_id=$2 WHERE id=$1`, env.issue, fixture.UserID)
	other := fixture.privateAgentOwnedBy(t, fixture.UserID, "second-author")
	fixture.Exec(t, `UPDATE workspace SET settings=settings || jsonb_build_object('github_review_authors',jsonb_build_object('acme/widget#1',$2::text,'acme/widget#2',$3::text)) WHERE id=$1`, fixture.WorkspaceID, env.agent, other)
	for _, number := range []int32{1, 2} {
		pull := fixture.Insert(t, "github_pull_request", testutil.Cols{
			"workspace_id": fixture.WorkspaceID, "installation_id": 1,
			"repo_owner": "acme", "repo_name": "widget", "pr_number": number,
			"title": "Different PR owners", "state": "open", "html_url": fmt.Sprintf("https://github.com/acme/widget/pull/%d", number),
			"branch": "feature", "head_sha": "head", "pr_created_at": "2026-10-29T00:00:00Z", "pr_updated_at": "2026-10-29T00:00:00Z",
		})
		fixture.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pull)
		fixture.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", env.issue, pull)
		if err := env.s.TriggerPullRequestWakeup(context.Background(), env.issue, PullRequestWakeupInput{
			Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: number,
			URL: fmt.Sprintf("https://github.com/acme/widget/pull/%d", number), HeadSHA: "head", Conclusion: "FAILURE",
		}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Exec(t, "UPDATE issue SET status='in_review' WHERE id=$1", env.issue)
	fixture.Exec(t, `UPDATE issue_wakeup_receipt SET processed_at=NULL WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1)`, env.issue)
	for index, author := range []string{env.agent, other} {
		wakeup, err := fixture.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: env.issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
		if err != nil {
			t.Fatal(err)
		}
		if err := env.s.dispatchSystem(context.Background(), wakeup); err != nil {
			t.Fatal(err)
		}
		var note string
		if err := fixture.Pool.QueryRow(context.Background(), `SELECT handoff_note FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND status='queued'`, env.issue, author).Scan(&note); err != nil {
			t.Fatalf("author %d queue missing: %v; pending receipts=%d, tasks=%d", index+1, err,
				fixture.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, wakeup.ID),
				fixture.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, env.issue))
		}
		ownURL := fmt.Sprintf("https://github.com/acme/widget/pull/%d", index+1)
		otherURL := fmt.Sprintf("https://github.com/acme/widget/pull/%d", 2-index)
		if !strings.Contains(note, ownURL) || strings.Contains(note, otherURL) {
			t.Errorf("author %s received another PR owner's facts: %s", author, note)
		}
		fixture.Exec(t, "UPDATE agent_task_queue SET status='completed',started_at=now(),completed_at=now() WHERE issue_id=$1 AND agent_id=$2", env.issue, author)
	}
}
