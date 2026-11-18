package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5046 slice C acceptance: a queued merge_eligibility run goes through the
// real daemon claim (ClaimTaskByRuntime) and start (StartTask) handlers, and
// the number of runs that actually start is counted from the task rows.

type mergeFactsStub struct {
	facts service.MergeFacts
	err   error
}

func (m *mergeFactsStub) MergeFacts(context.Context, service.MergeCandidate) (service.MergeFacts, error) {
	return m.facts, m.err
}

func mergeBool(v bool) *bool { return &v }

const mergeDaemonPolicy = `{"merge_eligibility":{"revision":"p1","max_fact_age_seconds":300,"security_gates":["success","disabled"]}}`

type mergeDaemonFix struct {
	issue, agent, rule, pr string
	facts                  *mergeFactsStub
}

func newMergeDaemonFix(t *testing.T) *mergeDaemonFix {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "merge dispatch daemon path")
	t.Cleanup(func() { cleanupChildDoneIssue(issue) })
	fix := &mergeDaemonFix{issue: issue, agent: handlerTestAgentID(t)}
	fix.facts = &mergeFactsStub{facts: service.MergeFacts{
		Tier: new(int), ClassifierTrusted: mergeBool(true), CoverageComplete: mergeBool(true), PRBound: mergeBool(true),
		ChecksGreen: mergeBool(true), SecurityGate: "success", CrossLabPass: mergeBool(true),
		Paused: mergeBool(false), MainRedPaused: mergeBool(false), MergerIsAuthor: mergeBool(false),
		Paths: []string{"server/internal/service/foo.go"}, Labels: &[]string{},
		ScopeClassified: mergeBool(true), GateVetoElapsed: mergeBool(true), GateVetoed: mergeBool(false),
		Evidence: "receipt-1", ObservedAt: time.Now(),
	}}

	var oldSettings []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&oldSettings)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id=$1`, testWorkspaceID, mergeDaemonPolicy)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2 WHERE id=$1`, testWorkspaceID, oldSettings)
	})

	fix.pr = dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 5046,
		"repo_owner": "che5046", "repo_name": "daemon", "pr_number": 9,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/9",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "a", "snapshot_head_sha": "a", "snapshot_base_ref": "main",
		"api_mergeable": "MERGEABLE", "checks_rollup_state": "SUCCESS",
	})
	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_fetched_at=now() WHERE id=$1`, fix.pr)
	dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)`, issue, fix.pr)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, fix.pr)
		testPool.Exec(context.Background(), `DELETE FROM merge_pause_state WHERE repo_owner='che5046'`)
		testPool.Exec(context.Background(), `DELETE FROM merge_veto_deadline WHERE repo_owner='che5046'`)
	})
	fix.setPauses(t, false, false)

	testHandler.TaskService.PRSnapshotsEnabled = true
	testHandler.TaskService.MergeFacts = fix.facts
	t.Cleanup(func() {
		testHandler.TaskService.PRSnapshotsEnabled = false
		testHandler.TaskService.MergeFacts = nil
	})

	fix.rule = createRule(t, issue, fix.agent, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Merge when eligible", Kind: "event", Mode: "continuous",
		Condition: json.RawMessage(`{"type":"merge_eligibility"}`),
	})
	return fix
}

func (f *mergeDaemonFix) setPauses(t *testing.T, agentPaused, mainRed bool) {
	t.Helper()
	for name, set := range map[string]bool{"AGENT_MERGE_PAUSED": agentPaused, "MAIN_RED_PAUSED": mainRed} {
		dbfx.Exec(t, `INSERT INTO merge_pause_state(repo_owner,repo_name,variable_name,is_set,fetched_at) VALUES('che5046','daemon',$1,$2,now())
			ON CONFLICT (repo_owner,repo_name,variable_name) DO UPDATE SET is_set=$2, fetched_at=now()`, name, set)
	}
}

func (f *mergeDaemonFix) poll(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND next_fire_at IS NOT NULL`, f.rule)
	runWakeupTick(t)
}

func (f *mergeDaemonFix) taskID(t *testing.T) string {
	t.Helper()
	if n := wakeupRunsOf(t, f.rule); n != 1 {
		t.Fatalf("%d runs queued, want 1", n)
	}
	var id string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, f.rule).Scan(&id)
	return id
}

func (f *mergeDaemonFix) status(t *testing.T) (status string, started bool) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT status, started_at IS NOT NULL FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, f.rule).Scan(&status, &started)
	return
}

// claim runs the daemon claim handler for the fixture's runtime. It returns
// the claimed task of this rule, or nil when the daemon was handed none of it.
func (f *mergeDaemonFix) claim(t *testing.T, taskID string) *AgentTaskResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+testRuntimeID+"/tasks/claim", nil), "runtimeId", testRuntimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	var resp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("claim decode: %v %s", err, w.Body.String())
		}
	}
	if resp.Task != nil && resp.Task.ID != taskID {
		return nil
	}
	return resp.Task
}

func (f *mergeDaemonFix) start(t *testing.T, claimed *AgentTaskResponse) int {
	t.Helper()
	if claimed.DispatchedAt == nil {
		t.Fatal("claim carried no dispatched_at")
	}
	w := httptest.NewRecorder()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+claimed.ID+"/start", map[string]string{
		"runtime_id": testRuntimeID, "dispatched_at": *claimed.DispatchedAt,
	}), "taskId", claimed.ID)
	testHandler.StartTask(w, req)
	return w.Code
}

type mergeDaemonChange struct {
	name string
	do   func(*testing.T, *mergeDaemonFix)
}

func mergeDaemonChanges() []mergeDaemonChange {
	return []mergeDaemonChange{
		{"head changed", func(t *testing.T, f *mergeDaemonFix) {
			dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1`, f.pr)
		}},
		{"base changed", func(t *testing.T, f *mergeDaemonFix) {
			dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_base_ref='release' WHERE id=$1`, f.pr)
		}},
		{"evidence revoked", func(_ *testing.T, f *mergeDaemonFix) { f.facts.facts.CrossLabPass = mergeBool(false) }},
		{"pause set", func(t *testing.T, f *mergeDaemonFix) { f.setPauses(t, false, true) }},
		{"facts went stale", func(_ *testing.T, f *mergeDaemonFix) {
			f.facts.facts.ObservedAt = time.Now().Add(-301 * time.Second)
		}},
	}
}

func TestMergeDispatchDaemonClaimAndStartOneEligibleRun(t *testing.T) {
	f := newMergeDaemonFix(t)
	f.poll(t)
	f.poll(t)
	id := f.taskID(t)
	claimed := f.claim(t, id)
	if claimed == nil {
		t.Fatalf("the daemon claim did not return the eligible run; status %v", func() string { s, _ := f.status(t); return s }())
	}
	if code := f.start(t, claimed); code != http.StatusOK {
		t.Fatalf("start = %d, want 200", code)
	}
	if status, started := f.status(t); status != "running" || !started {
		t.Fatalf("status %q started %v after the start call, want running and started", status, started)
	}
	f.poll(t)
	if n := wakeupRunsOf(t, f.rule); n != 1 {
		t.Fatalf("%d runs for the candidate after start, want 1", n)
	}
}

// The agent claims long after the poll that queued the run; nothing starts
// when the candidate no longer qualifies at the claim.
func TestMergeDispatchDaemonClaimRefusesAStaleCandidate(t *testing.T) {
	for _, tc := range mergeDaemonChanges() {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeDaemonFix(t)
			f.poll(t)
			id := f.taskID(t)
			tc.do(t, f)
			if claimed := f.claim(t, id); claimed != nil {
				t.Fatalf("the daemon claimed run %s for a stale candidate", claimed.ID)
			}
			if status, started := f.status(t); status == "running" || started || status == "dispatched" {
				t.Fatalf("status %q started %v after a refused claim, want no start", status, started)
			}
		})
	}
}

// The candidate changes between the claim and the start call: the start is
// refused and the run is failed, never running.
func TestMergeDispatchDaemonStartRefusesAStaleCandidate(t *testing.T) {
	for _, tc := range mergeDaemonChanges() {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeDaemonFix(t)
			f.poll(t)
			id := f.taskID(t)
			claimed := f.claim(t, id)
			if claimed == nil {
				t.Fatal("the daemon claim did not return the eligible run")
			}
			tc.do(t, f)
			if code := f.start(t, claimed); code != http.StatusConflict {
				t.Fatalf("start = %d, want 409", code)
			}
			if status, started := f.status(t); status != "failed" || started {
				t.Fatalf("status %q started %v after a refused start, want failed and not started", status, started)
			}
		})
	}
}
