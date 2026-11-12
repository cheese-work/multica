package handler

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// autoRun is one finished run seeded into an issue's open window.
type autoRun struct {
	status    string
	err       string
	delivered bool     // the run's shell committed (a git commit in a tool result)
	comment   string   // the agent's comment text: never progress, whatever it says
	checks    []string // failed check names on the head the run pushed (needs prUpdate)
	ci        string   // a ci_failure_event workflow on the head the run pushed (needs prUpdate)
	prUpdate  bool     // a linked PR's head moved while the run ran
	noChecks  bool     // the pushed head has no retained check results (an old head, or none reported); a CI failure event does not replace them
	prTouch   bool     // a linked PR's updated_at moved (a PR comment) while the run ran
	commit    bool     // the run's shell committed, with no PR involved
	move      bool     // the agent moved the issue's status while the run ran
	tokens    int64
}

type autoCase struct {
	c       edgeCase
	agentID string
	since   time.Time
	seq     *int // runs seeded so far: each run gets its own 3-minute slot so comments match one run only
}

// newAutoCase is a budgeted issue (cap 5) in a window that opened an hour ago.
func newAutoCase(t *testing.T, installationID int64) autoCase {
	t.Helper()
	return newAutoCaseSteps(t, installationID, mergeEdgeSteps)
}

// newAutoCaseSteps is newAutoCase on a workflow with the given steps.
func newAutoCaseSteps(t *testing.T, installationID int64, steps []map[string]any) autoCase {
	t.Helper()
	// The shared test workspace keeps the auto-approver off, so the per-issue
	// card tests exercise the path the kill switch leaves.
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c := newEdgeCase(t, installationID, steps)
	agentID := seededReadyAgentID(t)
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=5, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID)
	dbfx.Exec(t, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2, requested_by_id=$3 WHERE id=$1`, c.issueID, agentID, testUserID)
	return autoCase{c: c, agentID: agentID, since: time.Now().Add(-time.Hour), seq: new(int)}
}

func (a autoCase) seed(t *testing.T, issueID string, runs ...autoRun) {
	t.Helper()
	for _, r := range runs {
		done := time.Now().Add(-time.Duration(5+3**a.seq) * time.Minute)
		*a.seq++
		taskID := dbfx.Task(t, a.agentID, testutil.Cols{
			"issue_id": issueID, "status": r.status, "error": r.err,
			"created_at": done.Add(-time.Minute), "started_at": done.Add(-time.Minute), "completed_at": done,
		})
		if r.comment != "" {
			dbfx.Comment(t, issueID, r.comment, testutil.Cols{"author_type": "agent", "author_id": a.agentID, "created_at": done})
		}
		if r.prUpdate || r.prTouch {
			prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
				"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "test", "repo_name": "auto",
				"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "auto", "state": "open",
				"html_url": "https://example.test/pr", "pr_created_at": done.Add(-time.Hour), "pr_updated_at": done,
			})
			dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, prID)
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
			})
			if r.prUpdate {
				head := fmt.Sprintf("%07x", *a.seq+rand.Intn(1<<24))
				dbfx.Insert(t, "issue_pr_event", testutil.Cols{
					"workspace_id": testWorkspaceID, "issue_id": issueID, "pull_request_id": prID, "kind": "head_changed",
					"head_sha": head, "observed_at": done, "dedupe_key": fmt.Sprintf("auto-%d-%s", *a.seq, head),
				})
				for i, name := range r.checks {
					dbfx.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion) VALUES ($1, $2, $3, $4, 'completed', 'failure')`, prID, head, i, name)
				}
				if len(r.checks) == 0 && r.ci == "" && !r.noChecks {
					// A head whose results are retained and all passed.
					dbfx.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion) VALUES ($1, $2, 0, 'go-test', 'completed', 'success')`, prID, head)
				}
				t.Cleanup(func() {
					_, _ = testPool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, prID)
					_, _ = testPool.Exec(context.Background(), `DELETE FROM ci_failure_event WHERE head_sha = $1`, head)
				})
				if r.ci != "" {
					dbfx.Insert(t, "ci_failure_event", testutil.Cols{
						"workspace_id": testWorkspaceID, "repo_owner": "test", "repo_name": "auto",
						"run_id": testutil.Raw("(floor(random() * 1000000000000))::bigint"), "run_attempt": 1,
						"workflow_name": r.ci, "head_sha": head, "conclusion": "failure",
					})
				}
			}
		}
		if r.commit || r.delivered {
			dbfx.Insert(t, "task_message", testutil.Cols{"task_id": taskID, "seq": 1, "type": "tool_result", "output": "[agent/x 1a2b3c4] feat: change\n 1 file changed, 2 insertions(+)"})
		}
		if r.move {
			dbfx.Insert(t, "activity_log", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "actor_type": "agent", "actor_id": a.agentID, "action": "status_changed", "details": `{"from":"todo","to":"in_progress"}`, "created_at": done})
		}
		if r.tokens > 0 {
			dbfx.Insert(t, "task_usage", testutil.Cols{"task_id": taskID, "model": "m", "input_tokens": r.tokens})
		}
	}
}

func (a autoCase) verdict(t *testing.T) service.RunBudgetAutoVerdict {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(a.c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	v, err := service.EvaluateRunBudgetAuto(context.Background(), testHandler.Queries, issue, a.since, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setAutoSettings(t *testing.T, settingsJSON string) {
	t.Helper()
	var previous string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings=settings || $2::jsonb WHERE id=$1`, testWorkspaceID, settingsJSON)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, previous)
	})
}

func goodRuns(n int) []autoRun {
	runs := make([]autoRun, n)
	for i := range runs {
		runs[i] = autoRun{status: "completed", delivered: true, tokens: 1000}
	}
	return runs
}

func TestRunBudgetAutoApprovesAWindowWithProgress(t *testing.T) {
	a := newAutoCase(t, 2226900)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	v := a.verdict(t)
	if !v.Approve || v.Anomaly != "" || v.Runs != 5 {
		t.Fatalf("verdict = %+v, want approve of 5 runs", v)
	}
}

func TestRunBudgetAutoHoldsAWindowWithoutRuns(t *testing.T) {
	a := newAutoCase(t, 2226900)
	v := a.verdict(t)
	if v.Approve || v.Anomaly != "" || v.Reason != "no_progress" {
		t.Fatalf("verdict = %+v, want a no_progress hold", v)
	}
}

func TestRunBudgetAutoEscalatesEachAnomalyKind(t *testing.T) {
	failing := func(n int) []autoRun {
		runs := goodRuns(1)
		for i := 0; i < n; i++ {
			runs = append(runs, autoRun{status: "failed", err: fmt.Sprintf("go test ./x: exit status %d", i+1), delivered: false, tokens: 1000})
		}
		return runs
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, a autoCase)
		want  string
	}{
		{"loop", func(t *testing.T, a autoCase) { a.seed(t, a.c.issueID, failing(3)...) }, service.RunBudgetAnomalyLoop},
		{"noop", func(t *testing.T, a autoCase) {
			a.seed(t, a.c.issueID, goodRuns(1)...)
			a.seed(t, a.c.issueID, autoRun{status: "completed"}, autoRun{status: "completed"}, autoRun{status: "completed"}, autoRun{status: "completed"})
		}, service.RunBudgetAnomalyNoop},
		{"spike", func(t *testing.T, a autoCase) {
			other := dbfx.Issue(t, "baseline runs")
			a.seed(t, other, goodRuns(9)...)
			a.seed(t, a.c.issueID, autoRun{status: "completed", delivered: true, tokens: 60000}, autoRun{status: "completed", delivered: true, tokens: 60000}, autoRun{status: "completed", delivered: true, tokens: 60000})
		}, service.RunBudgetAnomalySpike},
		{"churn", func(t *testing.T, a autoCase) {
			a.seed(t, a.c.issueID, goodRuns(4)...)
			for i := 0; i < 3; i++ {
				dbfx.Insert(t, "activity_log", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": a.c.issueID, "actor_type": "member", "actor_id": testUserID, "action": "run_budget_top_up", "created_at": time.Now().Add(-time.Duration(i+2) * time.Hour)})
			}
		}, service.RunBudgetAnomalyChurn},
		{"openai_pool", func(t *testing.T, a autoCase) {
			a.seed(t, a.c.issueID, goodRuns(4)...)
			dbfx.Exec(t, `UPDATE agent SET model='gpt-6.1-sol' WHERE id=$1`, a.agentID)
			t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `UPDATE agent SET model='' WHERE id=$1`, a.agentID) })
			setAutoSettings(t, `{"provider_holds":[{"provider":"OpenAI","text":"Stop all routing to OpenAI based agents"}]}`)
		}, service.RunBudgetAnomalyOpenAIPool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			tc.setup(t, a)
			v := a.verdict(t)
			if v.Approve || v.Anomaly != tc.want {
				t.Fatalf("verdict = %+v, want anomaly %q and no approval", v, tc.want)
			}
		})
	}
}

func TestRunBudgetAutoEnvelopeStopsApproval(t *testing.T) {
	a := newAutoCase(t, 2226900)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	// 296 granted today plus this window's 5 passes the 300-run envelope; a
	// grant from two days ago does not count.
	for _, row := range []struct {
		runs int
		ago  time.Duration
	}{{296, time.Minute}, {1000, 48 * time.Hour}} {
		dbfx.Insert(t, "activity_log", testutil.Cols{
			"workspace_id": testWorkspaceID, "actor_type": "system", "action": "run_budget_top_up",
			"details": fmt.Sprintf(`{"auto":%q,"runs":%d}`, service.RunBudgetAutoRule, row.runs), "created_at": time.Now().Add(-row.ago),
		})
	}
	v := a.verdict(t)
	if v.Approve || v.Anomaly != "" || v.Reason != "envelope" {
		t.Fatalf("verdict = %+v, want an envelope hold", v)
	}
	setAutoSettings(t, `{"run_budget_auto":{"daily_envelope":400}}`)
	if v := a.verdict(t); !v.Approve {
		t.Fatalf("with a 400-run envelope verdict = %+v, want approve", v)
	}
}

func TestRunBudgetAutoKillSwitchHolds(t *testing.T) {
	a := newAutoCase(t, 2226900)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	setAutoSettings(t, `{"run_budget_auto":{"enabled":false}}`)
	v := a.verdict(t)
	if v.Approve || v.Anomaly != "" || v.Reason != "disabled" {
		t.Fatalf("verdict = %+v, want a disabled hold", v)
	}
}

// A comment that says nothing was done is not progress; a PR change or a status
// move by the agent is, with or without a comment.
func TestRunBudgetAutoProgressSignals(t *testing.T) {
	noop := func(n int) []autoRun {
		runs := make([]autoRun, n)
		for i := range runs {
			runs[i] = autoRun{status: "completed", comment: "No changes needed, nothing to do. Waiting."}
		}
		return runs
	}
	cases := []struct {
		name string
		runs []autoRun
		want bool
	}{
		{"no-op comments only", noop(5), false},
		{"Completed no work.", repeatComment("Completed no work.", 5), false},
		{"a delivery-sounding comment alone", repeatComment("Implemented the change, merged and pushed it.", 5), false},
		{"pr updated, no comment", []autoRun{{status: "completed", prUpdate: true}, {status: "completed", prUpdate: true}, {status: "completed", prUpdate: true}, {status: "completed", prUpdate: true}, {status: "completed", prUpdate: true}}, true},
		{"status moved, no comment", []autoRun{{status: "completed", move: true}, {status: "completed", move: true}, {status: "completed", move: true}, {status: "completed", move: true}, {status: "completed", move: true}}, true},
		{"no-op comment with a pr update", []autoRun{{status: "completed", comment: "No changes needed.", prUpdate: true}, {status: "completed", comment: "No changes needed.", prUpdate: true}, {status: "completed", comment: "No changes needed.", prUpdate: true}, {status: "completed", comment: "No changes needed.", prUpdate: true}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			a.seed(t, a.c.issueID, tc.runs...)
			if v := a.verdict(t); v.Approve != tc.want {
				t.Fatalf("verdict = %+v, want approve %v", v, tc.want)
			}
		})
	}
}

func repeatComment(text string, n int) []autoRun {
	runs := make([]autoRun, n)
	for i := range runs {
		runs[i] = autoRun{status: "completed", comment: text}
	}
	return runs
}

// A comment never counts as progress, in either direction: five delivery-sounding
// comments and no commit, PR event or status move are a no-op window.
func TestRunBudgetAutoCommentsAreNeverProgress(t *testing.T) {
	for _, text := range []string{"Completed no work.", "Implemented the change and pushed it.", "Looked at the issue again."} {
		t.Run(text, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			a.seed(t, a.c.issueID, repeatComment(text, 5)...)
			if v := a.verdict(t); v.Approve || v.Anomaly != service.RunBudgetAnomalyNoop {
				t.Fatalf("verdict = %+v, want a noop anomaly", v)
			}
		})
	}
}

// A commit with no linked PR is progress; a PR comment (updated_at moves, the
// head does not) is not.
func TestRunBudgetAutoCommitAndPRCommentSignals(t *testing.T) {
	cases := []struct {
		name string
		run  autoRun
		want bool
	}{
		{"commit without a PR", autoRun{status: "completed", commit: true}, true},
		{"pr head moved", autoRun{status: "completed", prUpdate: true}, true},
		{"pr comment only", autoRun{status: "completed", prTouch: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			runs := make([]autoRun, 5)
			for i := range runs {
				runs[i] = tc.run
			}
			a.seed(t, a.c.issueID, runs...)
			if v := a.verdict(t); v.Approve != tc.want {
				t.Fatalf("verdict = %+v, want approve %v", v, tc.want)
			}
		})
	}
}

// The loop key is a structured field: a failed check on the head a run pushed, a
// CI failure on that head, or a failed run's error. Runs that all make progress
// still loop when the same key repeats; comment text is never parsed.
func TestRunBudgetAutoLoopKeysAreStructured(t *testing.T) {
	pushed := func(checks []string, ci string, comment string) autoRun {
		return autoRun{status: "completed", prUpdate: true, checks: checks, ci: ci, comment: comment}
	}
	failed := func(err string) autoRun { return autoRun{status: "failed", err: err} }
	cases := []struct {
		name string
		runs []autoRun
		want bool
	}{
		{"same failed check on three pushed heads", []autoRun{
			pushed([]string{"go-test"}, "", ""), pushed([]string{"go-lint", "go-test"}, "", ""), pushed([]string{"go-test"}, "", ""),
		}, true},
		{"same CI workflow failing on three pushed heads", []autoRun{
			pushed(nil, "CI", ""), pushed(nil, "CI", ""), pushed(nil, "CI", ""),
		}, true},
		{"a different failed check each run", []autoRun{
			pushed([]string{"go-test"}, "", ""), pushed([]string{"web-e2e"}, "", ""), pushed([]string{"android-build"}, "", ""),
		}, false},
		{"failure words in comments alone are not a key", []autoRun{
			pushed(nil, "", "go-test failed again: connection refused"), pushed(nil, "", "go-test failed again: connection refused"), pushed(nil, "", "go-test failed again: connection refused"),
		}, false},
		{"the same error with varying context", []autoRun{
			failed("dial tcp 10.0.0.1:5432: connect: connection refused"), failed("dial tcp 10.0.0.7:5433: connect: connection refused"), failed("dial tcp 10.0.0.9:5434: connect: connection refused"),
		}, true},
		{"different errors", []autoRun{
			failed("connection refused"), failed("context deadline exceeded"), failed("permission denied"),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			a.seed(t, a.c.issueID, goodRuns(2)...)
			a.seed(t, a.c.issueID, tc.runs...)
			v := a.verdict(t)
			if got := v.Anomaly == service.RunBudgetAnomalyLoop; got != tc.want {
				t.Fatalf("verdict = %+v, want loop %v", v, tc.want)
			}
		})
	}
}

// A head that a run pushed has no retained check results once the PR moved on,
// and CI failure events exist only while the ci_watcher flag is on, and then only
// for the workflows that failed. Three runs that each pushed a head whose results
// are gone could all have failed the same check, so the window is not approved,
// whatever failure events exist for those heads.
func TestRunBudgetAutoHoldsWhenPushedHeadsHaveNoCheckHistory(t *testing.T) {
	cases := []struct {
		name string
		runs []autoRun
		want bool
	}{
		{"three pushed heads with no retained results", []autoRun{
			{status: "completed", prUpdate: true, noChecks: true}, {status: "completed", prUpdate: true, noChecks: true}, {status: "completed", prUpdate: true, noChecks: true},
		}, false},
		{"one pushed head with no retained results among passing ones", []autoRun{
			{status: "completed", prUpdate: true}, {status: "completed", prUpdate: true, noChecks: true}, {status: "completed", prUpdate: true},
		}, false},
		{"three pushed heads with only a CI failure event each, results gone", []autoRun{
			{status: "completed", prUpdate: true, ci: "CI-a"}, {status: "completed", prUpdate: true, ci: "CI-b"}, {status: "completed", prUpdate: true, ci: "CI-c"},
		}, false},
		{"pushed heads whose results are retained and passed", []autoRun{
			{status: "completed", prUpdate: true}, {status: "completed", prUpdate: true}, {status: "completed", prUpdate: true},
		}, true},
		{"no push at all needs no check history", []autoRun{
			{status: "completed", commit: true}, {status: "completed", commit: true}, {status: "completed", commit: true},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutoCase(t, 2226900)
			a.seed(t, a.c.issueID, tc.runs...)
			v := a.verdict(t)
			if v.Approve != tc.want {
				t.Fatalf("verdict = %+v, want approve %v", v, tc.want)
			}
			if !tc.want && v.Reason != "ci_unknown" {
				t.Fatalf("verdict = %+v, want reason ci_unknown", v)
			}
		})
	}
}
