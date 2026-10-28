package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	ghDeploySHA = "59819ba721b4d542791db013ec2bb43ed9d66503"
	ghQualSHA   = "7f6712c77a32f4d45b51182169ab05c5a18af0f5"
)

// ghRunFixture is a wakeup fixture whose workspace has the GitHub App
// installed for "fixture-org".
type ghRunFixture struct {
	f     principalFixture
	s     *IssueWakeupService
	issue pgtype.UUID
	agent string
}

func newGHRunFixture(t *testing.T) ghRunFixture {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	f.Insert(t, "github_installation", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 9_000_000 + principalSeq.Add(1), "account_login": "Fixture-Org",
	})
	return ghRunFixture{f, s, issue, agent}
}

// run mirrors one webhook-fed run row; stub columns default as in production.
func (g ghRunFixture) run(t *testing.T, id int64, workflow, sha string) {
	t.Helper()
	g.f.Insert(t, "github_workflow_run", testutil.Cols{
		"workspace_id": g.f.WorkspaceID, "installation_id": 1, "repo_owner": "fixture-org", "repo_name": "repo",
		"run_id": id, "run_attempt": 1, "workflow_path": ".github/workflows/" + workflow, "head_sha": sha,
		"html_url": "https://github.example/run",
	})
}

func (g ghRunFixture) setRun(t *testing.T, id int64, attempt, rank int, status, conclusion string, gate int) {
	t.Helper()
	g.f.Exec(t, "UPDATE github_workflow_run SET run_attempt=$2,status_rank=$3,status=$4,conclusion=$5,gate_generation=$6,updated_at=now() WHERE run_id=$1 AND workspace_id=$7",
		id, attempt, rank, status, conclusion, gate, g.f.WorkspaceID)
}

func (g ghRunFixture) job(t *testing.T, runID int64, attempt int, jobID int64, name string, phase int) {
	t.Helper()
	g.f.Insert(t, "github_workflow_job", testutil.Cols{
		"workspace_id": g.f.WorkspaceID, "repo_owner": "fixture-org", "repo_name": "repo",
		"run_id": runID, "run_attempt": attempt, "job_id": jobID, "job_name": name, "phase": phase,
	})
}

func (g ghRunFixture) rule(t *testing.T, cond map[string]any, in WakeupInput) db.IssueWakeup {
	t.Helper()
	cond["type"], cond["repo"] = "github_run", "Fixture-Org/Repo"
	in.AgentID, in.Kind, in.Instruction, in.Condition = g.agent, "event", "Check the run", condition(t, cond)
	return wakeCreate(t, g.f, g.s, g.issue, in)
}

func (g ghRunFixture) finishRuns(t *testing.T, w db.IssueWakeup) {
	t.Helper()
	g.f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1 AND status<>'completed'", util.UUIDToString(w.ID))
}

func (g ghRunFixture) expectRuns(t *testing.T, w db.IssueWakeup, want int, step string) {
	t.Helper()
	if n := wakeRuns(t, g.f, w.ID); n != want {
		t.Fatalf("%s: %d runs, want %d", step, n, want)
	}
}

func (g ghRunFixture) note(t *testing.T, w db.IssueWakeup) string {
	t.Helper()
	var note string
	err := g.f.Pool.QueryRow(context.Background(), "SELECT handoff_note FROM agent_task_queue WHERE context->>'wakeup_id'=$1 ORDER BY created_at DESC LIMIT 1", util.UUIDToString(w.ID)).Scan(&note)
	if err != nil {
		t.Fatal(err)
	}
	return note
}

func TestWakeupConditionGitHubRunValidation(t *testing.T) {
	g := newGHRunFixture(t)
	ctx := context.Background()
	member := parseTestUUID(t, g.f.UserID)
	valid := map[string]any{"type": "github_run", "repo": "fixture-org/repo", "run_id": 40343069164, "state": "waiting"}
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for key, val := range valid {
			c[key] = val
		}
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	for name, c := range map[string]map[string]any{
		"repo without name":     with("repo", "fixture-org"),
		"repo with extra part":  with("repo", "fixture-org/repo/x"),
		"repo with space":       with("repo", "fixture-org/re po"),
		"foreign owner":         with("repo", "someone-else/repo"),
		"no selector":           with("run_id", nil),
		"both selectors":        with("workflow", "cd.yml"),
		"workflow without sha":  {"type": "github_run", "repo": "fixture-org/repo", "workflow": "cd.yml", "state": "completed"},
		"short sha":             {"type": "github_run", "repo": "fixture-org/repo", "workflow": "cd.yml", "head_sha": "59819ba", "state": "completed"},
		"sha without workflow":  {"type": "github_run", "repo": "fixture-org/repo", "head_sha": ghDeploySHA, "state": "completed"},
		"workflow with a path":  {"type": "github_run", "repo": "fixture-org/repo", "workflow": ".github/workflows/cd.yml", "head_sha": ghDeploySHA, "state": "completed"},
		"workflow not yaml":     {"type": "github_run", "repo": "fixture-org/repo", "workflow": "cd.txt", "head_sha": ghDeploySHA, "state": "completed"},
		"unknown state":         with("state", "queued"),
		"missing state":         with("state", nil),
		"negative run":          with("run_id", -5),
		"foreign field":         with("event", "merged"),
		"pull request keywords": with("issue_id", util.UUIDToString(g.issue)),
	} {
		in := WakeupInput{AgentID: g.agent, Kind: "event", Instruction: "x", Condition: condition(t, c)}
		if _, err := g.s.Create(ctx, g.issue, member, pgtype.UUID{}, in); !errors.Is(err, ErrWakeupInput) {
			t.Errorf("%s: err = %v, want ErrWakeupInput", name, err)
		}
	}
	// Owner and repo are stored lowercased; the SHA selector is normalized too.
	w := wakeCreate(t, g.f, g.s, g.issue, WakeupInput{AgentID: g.agent, Kind: "event", Instruction: "x",
		Condition: condition(t, map[string]any{"type": "github_run", "repo": "Fixture-Org/Repo", "workflow": "cd-qualification.yml", "head_sha": strings.ToUpper(ghQualSHA), "state": "completed"})})
	if len(w.EventTypes) != 0 {
		t.Fatalf("github_run is polled only; event types = %v", w.EventTypes)
	}
	var got WakeupCondition
	if err := json.Unmarshal(w.Condition, &got); err != nil || got.Repo != "fixture-org/repo" || got.HeadSHA != ghQualSHA {
		t.Fatalf("condition not normalized: %s (%v)", w.Condition, err)
	}
}

func TestWakeupConditionGitHubRunCompletedSelectors(t *testing.T) {
	g := newGHRunFixture(t)
	other := newGHRunFixture(t)
	g.run(t, 40343069164, "cd-deploy.yml", ghDeploySHA)
	g.run(t, 40339614920, "cd-qualification.yml", ghQualSHA)
	// A run of the same id in another workspace must never satisfy this one.
	other.f.Insert(t, "github_workflow_run", testutil.Cols{
		"workspace_id": other.f.WorkspaceID, "installation_id": 1, "repo_owner": "fixture-org", "repo_name": "repo",
		"run_id": 77, "run_attempt": 1, "status_rank": 2, "status": "completed", "conclusion": "success",
		"workflow_path": ".github/workflows/cd-qualification.yml", "head_sha": ghQualSHA,
	})
	g.setRun(t, 40339614920, 1, 2, "completed", "success", 0)

	// The deploy run carries head_sha of the merge, not of the deployed commit.
	wrong := g.rule(t, map[string]any{"workflow": "cd-deploy.yml", "head_sha": ghQualSHA, "state": "completed"}, WakeupInput{})
	byRun := g.rule(t, map[string]any{"run_id": 40343069164, "state": "completed"}, WakeupInput{})
	bySHA := g.rule(t, map[string]any{"workflow": "cd-qualification.yml", "head_sha": ghQualSHA, "state": "completed"}, WakeupInput{})
	crossWS := g.rule(t, map[string]any{"run_id": 77, "state": "completed"}, WakeupInput{})
	for _, w := range []db.IssueWakeup{wrong, byRun, bySHA, crossWS} {
		wakeTick(t, g.f, g.s, w.ID)
	}
	g.expectRuns(t, wrong, 0, "workflow path and SHA of different runs")
	g.expectRuns(t, byRun, 0, "deploy run still queued")
	g.expectRuns(t, bySHA, 1, "qualification run completed")
	g.expectRuns(t, crossWS, 0, "other workspace's run")

	note := g.note(t, bySHA)
	for _, want := range []string{`"run_id":40339614920`, `"run_attempt":1`, `"gate_generation":0`, `"waiting_now":false`, `"conclusion":"success"`, `"html_url":"https://github.example/run"`} {
		if !strings.Contains(note, want) {
			t.Fatalf("fact %s missing from the run: %s", want, note)
		}
	}

	// Runs of one agent on one issue queue behind each other.
	g.finishRuns(t, bySHA)
	g.setRun(t, 40343069164, 1, 2, "completed", "failure", 0)
	wakeTick(t, g.f, g.s, byRun.ID)
	g.expectRuns(t, byRun, 1, "deploy run completed")
}

// Test 13: waiting episodes in continuous mode.
func TestWakeupConditionGitHubRunWaitingEpisodes(t *testing.T) {
	g := newGHRunFixture(t)
	g.run(t, 100, "cd-deploy.yml", ghDeploySHA)
	w := g.rule(t, map[string]any{"run_id": 100, "state": "waiting"}, WakeupInput{Mode: "continuous"})
	tick := func(step string, want int) {
		t.Helper()
		wakeTick(t, g.f, g.s, w.ID)
		g.expectRuns(t, w, want, step)
		g.finishRuns(t, w)
	}

	tick("no gate yet", 0)
	// (a) A waits.
	g.job(t, 100, 1, 1, "approve-a", 1)
	g.setRun(t, 100, 1, 1, "in_progress", "", 1)
	tick("a waits", 1)
	if note := g.note(t, w); !strings.Contains(note, `"waiting_now":true`) || !strings.Contains(note, "approve-a") || !strings.Contains(note, `"gate_generation":1`) {
		t.Fatalf("waiting facts missing: %s", note)
	}
	// (f) Repeat poll.
	tick("repeat poll", 1)
	// (e) Duplicate delivery changes nothing.
	tick("duplicate delivery", 1)
	// (b) A leaves and B waits with no poll in between: one new receipt.
	g.f.Exec(t, "UPDATE github_workflow_job SET phase=2 WHERE job_id=1 AND workspace_id=$1", g.f.WorkspaceID)
	g.job(t, 100, 1, 2, "approve-b", 1)
	g.setRun(t, 100, 1, 1, "in_progress", "", 2)
	tick("b replaces a between polls", 2)
	// (d) Shrink only: B leaves, the counter never goes down.
	g.f.Exec(t, "UPDATE github_workflow_job SET phase=2 WHERE job_id=2 AND workspace_id=$1", g.f.WorkspaceID)
	tick("gate closed", 2)
	// (c) A gate opens and closes between polls: one receipt, waiting_now=false.
	g.job(t, 100, 1, 3, "approve-c", 3)
	g.setRun(t, 100, 1, 1, "in_progress", "", 3)
	tick("opened and closed between polls", 3)
	if note := g.note(t, w); !strings.Contains(note, `"waiting_now":false`) || !strings.Contains(note, `"gate_generation":3`) {
		t.Fatalf("closed-gate facts: %s", note)
	}
	// Several gates between two polls: one receipt.
	g.setRun(t, 100, 1, 1, "in_progress", "", 5)
	tick("two gates between polls", 4)

	// (g) Once mode: one fire, then the rule ends.
	once := g.rule(t, map[string]any{"run_id": 100, "state": "waiting"}, WakeupInput{})
	wakeTick(t, g.f, g.s, once.ID)
	got := wakeTick(t, g.f, g.s, once.ID)
	g.expectRuns(t, once, 1, "once mode")
	if got.Enabled {
		t.Fatal("a once rule keeps polling after it fired")
	}
}

// Test 14: a job-led attempt raise is visible before any attempt-2 run event.
func TestWakeupConditionGitHubRunAttemptPollBoundary(t *testing.T) {
	g := newGHRunFixture(t)
	g.run(t, 200, "cd-deploy.yml", ghDeploySHA)
	g.setRun(t, 200, 1, 2, "completed", "failure", 0)
	waiting := g.rule(t, map[string]any{"run_id": 200, "state": "waiting"}, WakeupInput{})
	completed := g.rule(t, map[string]any{"run_id": 200, "state": "completed"}, WakeupInput{Mode: "continuous"})
	wakeTick(t, g.f, g.s, completed.ID)
	g.expectRuns(t, completed, 1, "attempt 1 completed")
	g.finishRuns(t, completed)

	// Attempt-2 job `waiting` ingested, run envelope not yet: the stub row.
	g.job(t, 200, 2, 20, "approve", 1)
	g.f.Exec(t, "UPDATE github_workflow_run SET run_attempt=2,status_rank=-1,status='',conclusion='',gh_updated_at=NULL,gate_generation=1 WHERE run_id=200 AND workspace_id=$1", g.f.WorkspaceID)
	wakeTick(t, g.f, g.s, waiting.ID)
	wakeTick(t, g.f, g.s, completed.ID)
	g.expectRuns(t, waiting, 1, "waiting from the job-led attempt")
	g.expectRuns(t, completed, 1, "attempt 1's terminal status cannot satisfy completed")
	if note := g.note(t, waiting); !strings.Contains(note, `"run_attempt":2`) || !strings.Contains(note, `"gate_generation":1`) {
		t.Fatalf("attempt facts: %s", note)
	}
	// Attempt 2 completes: a new key, so the continuous rule fires again.
	g.finishRuns(t, waiting)
	g.setRun(t, 200, 2, 2, "completed", "success", 1)
	wakeTick(t, g.f, g.s, completed.ID)
	g.expectRuns(t, completed, 2, "attempt 2 completed")
}

// Test 15: a lost terminal event ends or wakes at the deadline, never before.
func TestWakeupConditionGitHubRunLostTerminalEvent(t *testing.T) {
	g := newGHRunFixture(t)
	g.run(t, 300, "cd-deploy.yml", ghDeploySHA)
	g.setRun(t, 300, 1, 1, "in_progress", "", 0)
	wake := g.rule(t, map[string]any{"run_id": 300, "state": "completed"}, WakeupInput{ExpiresInSeconds: 3600, OnTimeout: "wake"})
	end := g.rule(t, map[string]any{"run_id": 300, "state": "completed"}, WakeupInput{ExpiresInSeconds: 3600})

	for _, w := range []db.IssueWakeup{wake, end} {
		if got := wakeTick(t, g.f, g.s, w.ID); got.LastError.Valid {
			t.Fatalf("a fresh in_progress run is not stale: %q", got.LastError.String)
		}
	}
	g.f.Exec(t, "UPDATE github_workflow_run SET updated_at=now()-interval '7 hours' WHERE run_id=300 AND workspace_id=$1", g.f.WorkspaceID)
	got := wakeTick(t, g.f, g.s, wake.ID)
	if got.LastError.String != conditionDiagnosticPrefix+errGHRunStale.Error() {
		t.Fatalf("stale diagnostic = %+v", got.LastError)
	}
	g.expectRuns(t, wake, 0, "stale run before the deadline")

	g.f.Exec(t, "UPDATE issue_wakeup SET expires_at=now()-interval '1 second' WHERE id IN ($1,$2)", wake.ID, end.ID)
	wakeTick(t, g.f, g.s, wake.ID)
	wakeTick(t, g.f, g.s, end.ID)
	g.expectRuns(t, wake, 1, "on-timeout wake")
	g.expectRuns(t, end, 0, "default end")
	if note := g.note(t, wake); !strings.Contains(note, "wakeup.timeout") {
		t.Fatalf("timeout trigger missing: %s", note)
	}
}

// Test 16: a deleted row never fires and keeps the rule's memory.
func TestWakeupConditionGitHubRunAgedOutRow(t *testing.T) {
	g := newGHRunFixture(t)
	g.run(t, 400, "cd-deploy.yml", ghDeploySHA)
	g.setRun(t, 400, 1, 2, "completed", "success", 0)
	w := g.rule(t, map[string]any{"run_id": 400, "state": "completed"}, WakeupInput{Mode: "continuous"})
	got := wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 1, "completed")
	g.finishRuns(t, w)
	remembered := got.ConditionState
	if remembered == "" {
		t.Fatal("fire left no condition state")
	}

	g.f.Exec(t, "DELETE FROM github_workflow_run WHERE run_id=400 AND workspace_id=$1", g.f.WorkspaceID)
	got = wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 1, "aged-out row")
	if got.LastError.String != conditionDiagnosticPrefix+errGHRunAgedOut.Error() || got.ConditionState != remembered {
		t.Fatalf("aged-out: error=%+v state=%q (want %q)", got.LastError, got.ConditionState, remembered)
	}
	// The same row coming back is not news.
	g.run(t, 400, "cd-deploy.yml", ghDeploySHA)
	g.setRun(t, 400, 1, 2, "completed", "success", 0)
	got = wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 1, "row restored")
	if got.LastError.Valid {
		t.Fatalf("diagnostic not cleared: %+v", got.LastError)
	}
}

func TestWakeupConditionGitHubRunDiagnostics(t *testing.T) {
	g := newGHRunFixture(t)
	w := g.rule(t, map[string]any{"run_id": 500, "state": "completed"}, WakeupInput{})
	got := wakeTick(t, g.f, g.s, w.ID)
	if got.LastError.String != conditionDiagnosticPrefix+errGHRunNoEvents.Error() {
		t.Fatalf("no events: %+v", got.LastError)
	}
	g.run(t, 501, "cd-deploy.yml", ghDeploySHA)
	got = wakeTick(t, g.f, g.s, w.ID)
	if got.LastError.String != conditionDiagnosticPrefix+errGHRunNoMatch.Error() {
		t.Fatalf("no matching run: %+v", got.LastError)
	}
}

// Test 17: register, nothing before ingest, ingest, exactly one task.
func TestWakeupConditionGitHubRunCoreFlow(t *testing.T) {
	g := newGHRunFixture(t)
	w := g.rule(t, map[string]any{"workflow": "cd-deploy.yml", "head_sha": ghDeploySHA, "state": "completed"}, WakeupInput{})
	wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 0, "before ingest")
	g.run(t, 600, "cd-deploy.yml", ghDeploySHA)
	wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 0, "run requested")
	g.setRun(t, 600, 1, 2, "completed", "success", 0)
	got := wakeTick(t, g.f, g.s, w.ID)
	wakeTick(t, g.f, g.s, w.ID)
	g.expectRuns(t, w, 1, "after completion")
	if got.Enabled {
		t.Fatal("once rule still enabled")
	}
	if n := g.f.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND event_type=$2", w.ID, wakeupConditionEventType); n != 1 {
		t.Fatalf("%d condition.met receipts, want 1", n)
	}
}

func TestWakeupConditionGitHubRunPartialEvictionKeepsSeenEpisodes(t *testing.T) {
	review := newGHRunFixture(t)
	for _, runID := range []int64{701, 702} {
		review.run(t, runID, "cd-deploy.yml", ghDeploySHA)
		review.setRun(t, runID, 1, 2, "completed", "success", 0)
	}
	rule := review.rule(t, map[string]any{"workflow": "cd-deploy.yml", "head_sha": ghDeploySHA, "state": "completed"}, WakeupInput{Mode: "continuous"})
	first := wakeTick(t, review.f, review.s, rule.ID)
	review.expectRuns(t, rule, 1, "two completed runs observed together")
	review.finishRuns(t, rule)
	review.f.Exec(t, "DELETE FROM github_workflow_run WHERE run_id=701 AND workspace_id=$1", review.f.WorkspaceID)
	second := wakeTick(t, review.f, review.s, rule.ID)
	if second.ConditionState != first.ConditionState {
		t.Errorf("partial cache eviction forgot seen episode: before=%q after=%q", first.ConditionState, second.ConditionState)
	}
	review.run(t, 701, "cd-deploy.yml", ghDeploySHA)
	review.setRun(t, 701, 1, 2, "completed", "success", 0)
	wakeTick(t, review.f, review.s, rule.ID)
	review.expectRuns(t, rule, 1, "redelivery of the same completed episode after partial eviction")
}

func TestWakeupConditionGitHubRunStaleAfterObservedGate(t *testing.T) {
	review := newGHRunFixture(t)
	review.run(t, 703, "cd-deploy.yml", ghDeploySHA)
	review.job(t, 703, 1, 7031, "approve", 1)
	review.setRun(t, 703, 1, 1, "in_progress", "", 1)
	rule := review.rule(t, map[string]any{"run_id": 703, "state": "waiting"}, WakeupInput{Mode: "continuous"})
	first := wakeTick(t, review.f, review.s, rule.ID)
	review.expectRuns(t, rule, 1, "observed gate")
	review.finishRuns(t, rule)
	review.f.Exec(t, "UPDATE github_workflow_job SET phase=2 WHERE job_id=7031 AND workspace_id=$1", review.f.WorkspaceID)
	review.f.Exec(t, "UPDATE github_workflow_run SET updated_at=now()-interval '7 hours' WHERE run_id=703 AND workspace_id=$1", review.f.WorkspaceID)
	second := wakeTick(t, review.f, review.s, rule.ID)
	if second.LastError.String != conditionDiagnosticPrefix+errGHRunStale.Error() {
		t.Errorf("lost-terminal diagnostic after a closed gate: got=%q want=%q", second.LastError.String, conditionDiagnosticPrefix+errGHRunStale.Error())
	}
	if second.ConditionState != first.ConditionState {
		t.Errorf("diagnostic forgot the previously observed gate: got=%q want=%q", second.ConditionState, first.ConditionState)
	}
	review.expectRuns(t, rule, 1, "stale gate episode does not refire")
}
