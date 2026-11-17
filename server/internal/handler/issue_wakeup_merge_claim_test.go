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

// CHE-6751 (CHE-6710 finding on slice C): the service tests call CheckClaim and
// CheckStart directly. These drive the real daemon claim and start handlers
// and count tasks that actually started, not queue rows.

type claimMergeFacts struct {
	facts service.MergeFacts
	clock func() time.Time // the facts' observation time, so they stay fresh
}

func (f *claimMergeFacts) MergeFacts(context.Context, service.MergeCandidate) (service.MergeFacts, error) {
	out := f.facts
	out.ObservedAt = f.clock()
	return out, nil
}

func ptrTo[T any](v T) *T { return &v }

const claimMergePolicy = `{"merge_eligibility":{"revision":"p1","max_fact_age_seconds":300,"security_gates":["success"]}}`

type claimMergeFix struct {
	facts           *claimMergeFacts
	pr, runtimeID   string
	daemonID        string
	workspaceUUID   string
	wakeupID        string
	startedAtLeast  int
	wakeupIDForTask string
}

func newClaimMergeFix(t *testing.T) *claimMergeFix {
	t.Helper()
	ctx := context.Background()
	agent, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	issue := dbfx.Issue(t, "merge claim evidence")
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue)

	// The handlers build their own wakeup service from the task service, so
	// the fake facts and snapshot switch go on the task service, restored after.
	// Observed a moment ago: facts dated after the evaluator's clock are refused.
	now := func() time.Time { return time.Now().Add(-time.Second) }
	facts := &claimMergeFacts{clock: now, facts: service.MergeFacts{
		Tier: ptrTo(1), ClassifierTrusted: ptrTo(true), CoverageComplete: ptrTo(true), PRBound: ptrTo(true),
		ChecksGreen: ptrTo(true), SecurityGate: "success", CrossLabPass: ptrTo(true),
		Paused: ptrTo(false), MainRedPaused: ptrTo(false), MergerIsAuthor: ptrTo(false),
		Paths: []string{"server/internal/service/foo.go"}, Labels: ptrTo([]string{}),
		ScopeClassified: ptrTo(true), GateVetoElapsed: ptrTo(true), GateVetoed: ptrTo(false),
		Evidence: "receipt-1",
	}}
	tasks := testHandler.TaskService
	oldFacts, oldSnap := tasks.MergeFacts, tasks.PRSnapshotsEnabled
	tasks.MergeFacts, tasks.PRSnapshotsEnabled = facts, true
	t.Cleanup(func() { tasks.MergeFacts, tasks.PRSnapshotsEnabled = oldFacts, oldSnap })

	var oldSettings []byte
	dbfx.QueryRow(t, "SELECT settings FROM workspace WHERE id=$1", testWorkspaceID).Scan(&oldSettings)
	dbfx.Exec(t, "UPDATE workspace SET settings=settings||$2::jsonb WHERE id=$1", testWorkspaceID, claimMergePolicy)
	t.Cleanup(func() {
		testPool.Exec(ctx, "UPDATE workspace SET settings=$2 WHERE id=$1", testWorkspaceID, oldSettings)
	})

	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "claim-merge", "pr_number": 9,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/9",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "a", "snapshot_head_sha": "a", "snapshot_base_ref": "main",
		"api_mergeable": "MERGEABLE", "checks_rollup_state": "SUCCESS",
		"snapshot_fetched_at": testutil.Raw("now()"),
	})
	dbfx.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	dbfx.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	// Both pause variables read as unset, just now.
	dbfx.Cleanup(t, "DELETE FROM merge_pause_state WHERE repo_owner='fixture' AND repo_name='claim-merge'")
	for _, v := range []string{"AGENT_MERGE_PAUSED", "MAIN_RED_PAUSED"} {
		dbfx.Exec(t, "INSERT INTO merge_pause_state(repo_owner,repo_name,variable_name,is_set,fetched_at) VALUES('fixture','claim-merge',$1,false,now())", v)
	}

	s := &service.IssueWakeupService{Tasks: tasks}
	w, err := s.Create(ctx, parseUUID(issue), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Merge when eligible",
		Condition: json.RawMessage(`{"type":"merge_eligibility"}`)})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND next_fire_at IS NOT NULL", w.ID)
	if err := s.DispatchRule(ctx, w.WorkspaceID, w.ID); err != nil {
		t.Fatal(err)
	}
	return &claimMergeFix{facts: facts, pr: pr, runtimeID: runtimeID, daemonID: daemonID, wakeupID: uuidToString(w.ID)}
}

func (m *claimMergeFix) count(t *testing.T, where string) int {
	t.Helper()
	return dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND "+where, m.wakeupID)
}

// claim is one daemon claim poll; it returns the wakeup's task if this claim handed it out.
func (m *claimMergeFix) claim(t *testing.T) *AgentTaskResponse {
	t.Helper()
	req := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+m.runtimeID+"/claim", nil, testWorkspaceID, m.daemonID), "runtimeId", m.runtimeID)
	rec := httptest.NewRecorder()
	testHandler.ClaimTaskByRuntime(rec, req)
	var out struct {
		Task *AgentTaskResponse `json:"task"`
	}
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	if out.Task != nil && out.Task.WakeupID != m.wakeupID {
		t.Fatalf("claimed a task of another wakeup: %+v", out.Task)
	}
	return out.Task
}

func (m *claimMergeFix) start(t *testing.T, task *AgentTaskResponse) int {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+task.ID+"/start", map[string]string{
		"runtime_id": m.runtimeID, "dispatched_at": *task.DispatchedAt}), "taskId", task.ID)
	rec := httptest.NewRecorder()
	testHandler.StartTask(rec, req)
	return rec.Code
}

func TestMergeEligibilityDaemonClaimAndStartHandlers(t *testing.T) {
	changes := []struct {
		name   string
		change func(*claimMergeFix, *testing.T)
	}{
		{"head change", func(m *claimMergeFix, t *testing.T) {
			dbfx.Exec(t, "UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1", m.pr)
		}},
		{"base change", func(m *claimMergeFix, t *testing.T) {
			dbfx.Exec(t, "UPDATE github_pull_request SET snapshot_base_ref='release' WHERE id=$1", m.pr)
		}},
		{"revoked evidence", func(m *claimMergeFix, _ *testing.T) { m.facts.facts.CrossLabPass = ptrTo(false) }},
		{"pause set", func(_ *claimMergeFix, t *testing.T) {
			dbfx.Exec(t, "UPDATE merge_pause_state SET is_set=true,fetched_at=now() WHERE repo_owner='fixture' AND repo_name='claim-merge' AND variable_name='MAIN_RED_PAUSED'")
		}},
	}

	t.Run("eligible candidate starts exactly once", func(t *testing.T) {
		m := newClaimMergeFix(t)
		if n := m.count(t, "true"); n != 1 {
			var lastErr string
			dbfx.QueryRow(t, "SELECT COALESCE(last_error,'') FROM issue_wakeup WHERE id=$1", m.wakeupID).Scan(&lastErr)
			t.Fatalf("%d queued runs at registration, want 1 (last_error %q)", n, lastErr)
		}
		task := m.claim(t)
		if task == nil || task.DispatchedAt == nil {
			t.Fatal("the eligible run was not claimed")
		}
		if code := m.start(t, task); code != http.StatusOK {
			t.Fatalf("start = %d, want 200", code)
		}
		if started := m.count(t, "started_at IS NOT NULL"); started != 1 {
			t.Fatalf("%d started runs, want 1", started)
		}
	})

	for _, tc := range changes {
		// The claim comes long after the poll that queued the run.
		t.Run(tc.name+" before the claim: zero starts", func(t *testing.T) {
			m := newClaimMergeFix(t)
			tc.change(m, t)
			if task := m.claim(t); task != nil {
				t.Fatalf("the claim handed out an invalidated run %s", task.ID)
			}
			if started := m.count(t, "started_at IS NOT NULL OR status IN ('dispatched','running')"); started != 0 {
				t.Fatalf("%d started runs, want 0", started)
			}
		})
		// Claimed while still eligible; invalidated before the daemon starts it.
		t.Run(tc.name+" between claim and start: zero starts", func(t *testing.T) {
			m := newClaimMergeFix(t)
			task := m.claim(t)
			if task == nil || task.DispatchedAt == nil {
				t.Fatal("the eligible run was not claimed")
			}
			tc.change(m, t)
			if code := m.start(t, task); code == http.StatusOK {
				t.Fatal("start returned 200 for an invalidated run")
			}
			if started := m.count(t, "started_at IS NOT NULL OR status='running'"); started != 0 {
				t.Fatalf("%d started runs, want 0", started)
			}
		})
	}
}
