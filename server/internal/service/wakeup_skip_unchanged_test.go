package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-3590: a wakeup whose watched inputs are unchanged since the last
// completed run starts no run, and says so on the timeline.

type skipEnv struct {
	f     principalFixture
	s     *IssueWakeupService
	issue pgtype.UUID
	agent string
	prID  string
}

func newSkipEnv(t *testing.T, linkPR bool) skipEnv {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	e := skipEnv{f: f, s: s, issue: issue, agent: agent}
	if !linkPR {
		return e
	}
	f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, api_merge_state_status, checks_rollup_state)
		VALUES ($1, 1, 'acme', 'widget', 157, 't', 'open', 'https://github.com/acme/widget/pull/157', now(), now(), 'h1', 'CLEAN', 'FAILURE') RETURNING id`, f.WorkspaceID).Scan(&e.prID)
	f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, e.prID)
	f.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, e.prID)
	f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issue, e.prID)
	return e
}

func (e skipEnv) checksFailed(t *testing.T, head string) {
	t.Helper()
	if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{
		Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 157,
		URL: "https://github.com/acme/widget/pull/157", HeadSHA: head, Conclusion: "FAILURE",
	}); err != nil {
		t.Fatal(err)
	}
}

func (e skipEnv) conflict(t *testing.T, base string) {
	t.Helper()
	if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{
		Rule: SystemRulePRAttention, Reason: PRAttentionMergeability, MergeState: "DIRTY", RepoOwner: "acme", RepoName: "widget", Number: 157,
		URL: "https://github.com/acme/widget/pull/157", HeadSHA: "h1", BaseBranch: base,
	}); err != nil {
		t.Fatal(err)
	}
}

func (e skipEnv) systemRuns(t *testing.T, rule string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, e.issue, rule)
}

func (e skipEnv) finishRuns(t *testing.T, status string) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status=$2,completed_at=clock_timestamp() WHERE issue_id=$1 AND status='queued'`, e.issue, status)
}

func (e skipEnv) skipped(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped' AND details->>'reason'='unchanged_since_last_run'`, e.issue)
}

func TestPRWatchdogSkipsWhenPRStateUnchangedSinceLastRun(t *testing.T) {
	e := newSkipEnv(t, true)
	e.checksFailed(t, "e1")
	e.finishRuns(t, "completed")
	e.checksFailed(t, "e2")
	if got := e.systemRuns(t, SystemRulePRChecksFailed); got != 1 {
		t.Fatalf("runs after an unchanged PR = %d, want 1", got)
	}
	if got := e.skipped(t); got != 1 {
		t.Fatalf("skip activities = %d, want 1", got)
	}
	var details []byte
	e.f.QueryRow(t, `SELECT details FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped' ORDER BY created_at DESC LIMIT 1`, e.issue).Scan(&details)
	var audit struct {
		PullRequests []struct {
			HeadSHA      string `json:"head_sha"`
			ChecksRollup string `json:"checks_rollup_state"`
			ReviewState  string `json:"review_state"`
		} `json:"pull_requests"`
	}
	if err := json.Unmarshal(details, &audit); err != nil {
		t.Fatal(err)
	}
	if len(audit.PullRequests) != 1 || audit.PullRequests[0].HeadSHA != "h1" || audit.PullRequests[0].ChecksRollup != "FAILURE" {
		t.Fatalf("skip audit PR state = %+v, want head h1 and FAILURE rollup", audit.PullRequests)
	}
	var pending int
	e.f.QueryRow(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND r.processed_at IS NULL`, e.issue).Scan(&pending)
	if pending != 0 {
		t.Fatalf("pending receipts after a skip = %d, want 0", pending)
	}
}

func TestPRAttentionWatchdogSkipsWhenPRStateUnchangedSinceLastRun(t *testing.T) {
	e := newSkipEnv(t, true)
	e.conflict(t, "main")
	e.finishRuns(t, "completed")
	e.conflict(t, "release")
	if got := e.systemRuns(t, SystemRulePRAttention); got != 1 {
		t.Fatalf("attention runs after an unchanged PR = %d, want 1", got)
	}
	if got := e.skipped(t); got != 1 {
		t.Fatalf("skip activities = %d, want 1", got)
	}
}

// Returning to draft is an event, not a state: a second conversion at the same
// head and rollup still starts a run.
func TestPRDraftReturnIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	e.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, e.issue)
	draft := func(at string) {
		if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{
			Rule: SystemRulePRAttention, Reason: PRAttentionDraft, RepoOwner: "acme", RepoName: "widget", Number: 157,
			URL: "https://github.com/acme/widget/pull/157", HeadSHA: "h1", EventAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	draft("2026-11-04T10:00:00Z")
	e.finishRuns(t, "completed")
	draft("2026-11-04T10:05:00Z")
	if got := e.systemRuns(t, SystemRulePRAttention); got != 2 {
		t.Fatalf("attention runs for two draft returns = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}

// The skip decides for the whole batch of pending facts: a draft return that
// waited behind a running turn must not be consumed with an unchanged
// mergeability fact that arrived after it.
func TestPRDraftReturnInAMixedBatchIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	e.conflict(t, "main")
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, e.issue)
	running := e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "running", "started_at": testutil.Raw("clock_timestamp()"),
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')")})
	if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{
		Rule: SystemRulePRAttention, Reason: PRAttentionDraft, RepoOwner: "acme", RepoName: "widget", Number: 157,
		URL: "https://github.com/acme/widget/pull/157", HeadSHA: "h1", EventAt: "2026-11-04T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	e.conflict(t, "release")
	e.f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1`, running)
	e.finishRuns(t, "completed")
	w, err := e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: e.issue, SystemRule: systemRuleText(SystemRulePRAttention)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.dispatchSystem(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities for a batch holding a draft return = %d, want 0", got)
	}
	if got := e.systemRuns(t, SystemRulePRAttention); got < 2 {
		t.Fatalf("attention runs = %d, want the draft return to start one", got)
	}
}

// Negative controls: each of these must still start a run.
func TestPRWatchdogStillRunsWhenSomethingChanged(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, e skipEnv)
		link  bool
	}{
		{"head moved", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE github_pull_request SET head_sha='h2' WHERE id=$1`, e.prID)
		}, true},
		{"check rollup changed", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE github_pull_request SET checks_rollup_state='PENDING' WHERE id=$1`, e.prID)
		}, true},
		{"review state changed", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE github_pull_request SET review_state='changes_requested', review_head_sha='h1', review_id=42, review_at=clock_timestamp() WHERE id=$1`, e.prID)
		}, true},
		{"comment added", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'member', gen_random_uuid(), 'Please check the PR')`, e.f.WorkspaceID, e.issue)
		}, true},
		{"comment edited", func(t *testing.T, e skipEnv) {
			var commentID string
			e.f.QueryRow(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content) VALUES ($1, $2, 'member', gen_random_uuid(), 'Please check the PR') RETURNING id`, e.f.WorkspaceID, e.issue).Scan(&commentID)
			e.f.Exec(t, `UPDATE comment SET content='The PR is ready now', revision=revision+1 WHERE id=$1`, commentID)
		}, true},
		{"merge state changed", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE github_pull_request SET api_merge_state_status='DIRTY' WHERE id=$1`, e.prID)
		}, true},
		{"returned to draft", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE github_pull_request SET state='draft' WHERE id=$1`, e.prID)
		}, true},
		{"previous run failed", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE agent_task_queue SET status='failed' WHERE issue_id=$1`, e.issue)
		}, true},
		{"previous run cancelled", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE issue_id=$1`, e.issue)
		}, true},
		{"previous run recorded no state", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE agent_task_queue SET context=context-'wakeup_input_fingerprint' WHERE issue_id=$1`, e.issue)
		}, true},
		{"last run older than the skip window", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE agent_task_queue SET created_at=now()-interval '7 hours' WHERE issue_id=$1`, e.issue)
		}, true},
		{"rule instruction edited", func(t *testing.T, e skipEnv) {
			e.f.Exec(t, `UPDATE issue_wakeup SET instruction=instruction||' (edited)' WHERE issue_id=$1 AND system_rule IS NOT NULL`, e.issue)
		}, true},
		{"no linked PR", func(t *testing.T, e skipEnv) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSkipEnv(t, tc.link)
			e.checksFailed(t, "e1")
			e.finishRuns(t, "completed")
			tc.setup(t, e)
			e.checksFailed(t, "e2")
			if got := e.systemRuns(t, SystemRulePRChecksFailed); got != 2 {
				t.Fatalf("runs = %d, want 2", got)
			}
			if got := e.skipped(t); got != 0 {
				t.Fatalf("skip activities = %d, want 0", got)
			}
		})
	}
}

func TestPRWatchdogFirstRunIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	e.checksFailed(t, "e1")
	if got := e.systemRuns(t, SystemRulePRChecksFailed); got != 1 {
		t.Fatalf("first run = %d, want 1", got)
	}
}

func (e skipEnv) heartbeat(t *testing.T) pgtype.UUID {
	t.Helper()
	w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "every", IntervalSeconds: 3600, Instruction: "Check the other issue and the external CI"})
	e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2 WHERE id=$1`, w.ID, dbid.NewV7())
	return w.ID
}

// A free-text heartbeat can watch anything (other issues, external CI, a
// deadline), so the platform cannot tell that nothing changed: every tick runs.
func TestAgentHeartbeatAlwaysRunsEvenWhenPRStateUnchanged(t *testing.T) {
	e := newSkipEnv(t, true)
	id := e.heartbeat(t)
	wakeTick(t, e.f, e.s, id)
	e.finishRuns(t, "completed")
	wakeTick(t, e.f, e.s, id)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("heartbeat runs = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}

func TestPersonWakeNowIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	id := e.heartbeat(t)
	wakeTick(t, e.f, e.s, id)
	e.finishRuns(t, "completed")
	if err := e.s.Trigger(context.Background(), e.issue, id, parseTestUUID(t, e.f.UserID)); err != nil {
		t.Fatal(err)
	}
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("heartbeat runs after wake now = %d, want 2", got)
	}
}
