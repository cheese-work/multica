package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2912 (CHE-2894 step 9, SEC-2): a compaction task is a side job on the
// DM. It never reads the DM's resume state, never writes it, and claims with a
// fresh provider session and workdir. SEC-1 rule 2 is judged again at claim,
// after the epoch rebase and before any payload read.

const (
	dmSession   = "S_dm"
	dmWorkDir   = "/tmp/dm-workdir"
	compSession = "S_compaction"
	compWorkDir = "/tmp/compaction-workdir"
)

// isolationDM is a DM at epoch 0 whose resume pointer names S_dm (recorded by a
// finished ordinary task), with a compaction agent configured on the same
// registered runtime.
type isolationDM struct {
	*authorityFixture
	agentID, chatID, runtimeID, daemonID string
	compactAgent                         string
}

func newIsolationDM(t *testing.T) *isolationDM {
	t.Helper()
	a := newAuthorityFixture(t) // clears and restores background_agents
	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	compactAgent := dbfx.Agent(t, "isolation-compact-agent", runtimeID, testutil.Cols{
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2)`, compactAgent, testWorkspaceID)
	a.setCompact(compactAgent)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{
		"title":                 "isolation",
		"explicitly_created_at": testutil.Raw("now()"),
	})
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "completed",
		"started_at": testutil.Raw("now() - interval '10 minutes'"), "completed_at": testutil.Raw("now() - interval '9 minutes'"),
		"chat_compaction_epoch": 0, "session_id": dmSession, "work_dir": dmWorkDir,
	})
	dbfx.Exec(t, `UPDATE chat_session SET session_id = $2, work_dir = $3, runtime_id = $4 WHERE id = $1`, chatID, dmSession, dmWorkDir, runtimeID)
	return &isolationDM{authorityFixture: a, agentID: agentID, chatID: chatID, runtimeID: runtimeID, daemonID: daemonID, compactAgent: compactAgent}
}

func (d *isolationDM) compactionTask(t *testing.T, status string, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"runtime_id": d.runtimeID, "chat_session_id": d.chatID, "status": status,
		"purpose": taskPurposeCompaction, "task_kind": "compact", "chat_compaction_epoch": 0,
	}
	if status != "queued" && status != "dispatched" {
		cols["started_at"] = testutil.Raw("now() - interval '1 minute'")
	}
	for k, v := range over {
		cols[k] = v
	}
	return dbfx.Task(t, d.compactAgent, cols)
}

func (d *isolationDM) pointer(t *testing.T) (session, workDir, runtime string) {
	t.Helper()
	var s, w, r *string
	dbfx.QueryRow(t, `SELECT session_id, work_dir, runtime_id::text FROM chat_session WHERE id = $1`, d.chatID).Scan(&s, &w, &r)
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return val(s), val(w), val(r)
}

func (d *isolationDM) requirePointerUntouched(t *testing.T) {
	t.Helper()
	s, w, r := d.pointer(t)
	if s != dmSession || w != dmWorkDir || r != d.runtimeID {
		t.Fatalf("DM pointer = (%q, %q, %q), want (%q, %q, %q): a compaction task moved it", s, w, r, dmSession, dmWorkDir, d.runtimeID)
	}
}

// ---- readers (SEC-2: resume readers exclude purpose='compaction') ----------

// A newer compaction row holding a session must not become the DM's fallback.
func TestCompactionIsolation_FallbackReaderSkipsCompactionRows(t *testing.T) {
	d := newIsolationDM(t)
	dbfx.Exec(t, `UPDATE chat_session SET session_id = NULL, work_dir = NULL WHERE id = $1`, d.chatID)
	d.compactionTask(t, "completed", testutil.Cols{"session_id": compSession, "work_dir": compWorkDir, "completed_at": testutil.Raw("now()")})
	sendDirectChat(t, context.Background(), d.agentID, d.chatID, "NEXT")

	got := claimTaskForRuntimeGuard(t, d.runtimeID, d.daemonID)
	if got.PriorSessionID != dmSession || got.PriorWorkDir != dmWorkDir {
		t.Fatalf("prior = (%q, %q), want the ordinary task's (%q, %q)", got.PriorSessionID, got.PriorWorkDir, dmSession, dmWorkDir)
	}
}

// After a compact (epoch > 0) a pointer is trusted only when a task of the
// current epoch recorded it; a compaction row must not vouch for it.
func TestCompactionIsolation_PointerCheckIgnoresCompactionRows(t *testing.T) {
	d := newIsolationDM(t)
	dbfx.Exec(t, `UPDATE chat_session SET compaction_epoch = 1, session_id = $2, work_dir = $3 WHERE id = $1`, d.chatID, compSession, compWorkDir)
	dbfx.Task(t, d.agentID, testutil.Cols{
		"runtime_id": d.runtimeID, "chat_session_id": d.chatID, "status": "completed",
		"started_at": testutil.Raw("now() - interval '3 minutes'"), "completed_at": testutil.Raw("now() - interval '2 minutes'"),
		"chat_compaction_epoch": 1, "session_id": "S_dm_epoch1", "work_dir": dmWorkDir,
	})
	d.compactionTask(t, "completed", testutil.Cols{"chat_compaction_epoch": 1, "session_id": compSession, "work_dir": compWorkDir, "completed_at": testutil.Raw("now()")})
	sendDirectChat(t, context.Background(), d.agentID, d.chatID, "NEXT")

	got := claimTaskForRuntimeGuard(t, d.runtimeID, d.daemonID)
	if got.PriorSessionID != "S_dm_epoch1" {
		t.Fatalf("prior session = %q, want S_dm_epoch1: a pointer only a compaction row backs was trusted", got.PriorSessionID)
	}
}

// A compaction row that withheld its Codex rollout is not a DM continuity gap.
func TestCompactionIsolation_RolloutMissingIgnoresCompactionRows(t *testing.T) {
	d := newIsolationDM(t)
	d.compactionTask(t, "failed", testutil.Cols{"session_rollout_missing": true, "completed_at": testutil.Raw("now()")})
	sendDirectChat(t, context.Background(), d.agentID, d.chatID, "NEXT")

	got := claimTaskForRuntimeGuard(t, d.runtimeID, d.daemonID)
	if got.PriorSessionResumeUnavailable {
		t.Fatal("a compaction row's rollout-missing flag disclosed a continuity gap on the DM")
	}
	if got.PriorSessionID != dmSession {
		t.Fatalf("prior session = %q, want %q", got.PriorSessionID, dmSession)
	}
}

// ---- writers (SEC-2: no compaction-task path writes the DM's pointer) ------

func TestCompactionIsolation_CompleteLeavesPointer(t *testing.T) {
	d := newIsolationDM(t)
	task := d.compactionTask(t, "running", nil)
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), util.MustParseUUID(task), []byte(`{}`), compSession, compWorkDir, "", false, dmSession, ""); err != nil {
		t.Fatal(err)
	}
	d.requirePointerUntouched(t)
}

func TestCompactionIsolation_FailLeavesPointer(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"resume-safe failure pins", "agent_error"},
		{"resume-unsafe failure clears", "iteration_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newIsolationDM(t)
			task := d.compactionTask(t, "running", nil)
			if _, err := testHandler.TaskService.FailTask(context.Background(), util.MustParseUUID(task), "boom", compSession, compWorkDir, "", tc.reason, false, dmSession, ""); err != nil {
				t.Fatal(err)
			}
			d.requirePointerUntouched(t)
		})
	}
}

func TestCompactionIsolation_CancelAndLatePinLeavePointer(t *testing.T) {
	d := newIsolationDM(t)
	task := d.compactionTask(t, "running", testutil.Cols{"session_id": compSession, "work_dir": compWorkDir})
	if _, err := testHandler.TaskService.CancelTask(context.Background(), util.MustParseUUID(task)); err != nil {
		t.Fatal(err)
	}
	d.requirePointerUntouched(t)
	// The mid-flight pin path (PinTaskSession) runs this statement after the
	// cancel; it must not move the pointer either.
	if err := testHandler.Queries.AdvanceCancelledChatSessionPointer(context.Background(), util.MustParseUUID(task)); err != nil {
		t.Fatal(err)
	}
	d.requirePointerUntouched(t)
}

func TestCompactionIsolation_RetireCancelledSessionLeavesPointer(t *testing.T) {
	d := newIsolationDM(t)
	task := d.compactionTask(t, "cancelled", testutil.Cols{"session_id": compSession, "completed_at": testutil.Raw("now()")})
	if err := testHandler.TaskService.RetireCancelledTaskSession(context.Background(), util.MustParseUUID(task), dmSession); err != nil {
		t.Fatal(err)
	}
	d.requirePointerUntouched(t)
}

// A compaction row must not shadow the cancelled ordinary turn's pointer move.
func TestCompactionIsolation_NewerCompactionSessionDoesNotBlockCancelledPointer(t *testing.T) {
	d := newIsolationDM(t)
	cancelled := dbfx.Task(t, d.agentID, testutil.Cols{
		"runtime_id": d.runtimeID, "chat_session_id": d.chatID, "status": "cancelled",
		"started_at": testutil.Raw("now() - interval '5 minutes'"), "completed_at": testutil.Raw("now() - interval '4 minutes'"),
		"chat_compaction_epoch": 0, "session_id": "S_cancelled", "work_dir": "/tmp/cancelled-workdir",
	})
	d.compactionTask(t, "completed", testutil.Cols{"session_id": compSession, "completed_at": testutil.Raw("now()")})
	if err := testHandler.Queries.AdvanceCancelledChatSessionPointer(context.Background(), util.MustParseUUID(cancelled)); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := d.pointer(t); s != "S_cancelled" {
		t.Fatalf("pointer = %q, want S_cancelled: a newer compaction row blocked the advance", s)
	}
}

// ---- claim (SEC-2: fresh provider session and workdir; SEC-1 rule 5) -------

func (d *isolationDM) claimRaw(t *testing.T, caps string) (*httptest.ResponseRecorder, *claimRuntimeGuardTask) {
	t.Helper()
	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+d.runtimeID+"/claim", nil)
	if caps != "" {
		req.Header.Set("X-Client-Capabilities", caps)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", d.runtimeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	testHandler.ClaimTaskByRuntime(w, req)
	var resp struct {
		Task *claimRuntimeGuardTask `json:"task"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
	}
	return w, resp.Task
}

func TestCompactionIsolation_ClaimStartsFresh(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry bool
	}{
		{"first attempt", false},
		{"manual or deferred retry child", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newIsolationDM(t)
			var over testutil.Cols
			if tc.retry {
				parent := d.compactionTask(t, "failed", testutil.Cols{"work_dir": compWorkDir, "completed_at": testutil.Raw("now()")})
				over = testutil.Cols{"force_fresh_session": true, "work_dir": compWorkDir, "retry_of_task_id": parent}
			}
			d.compactionTask(t, "queued", over)

			w, got := d.claimRaw(t, protocol.DaemonCapabilityCheckoutKeepsWorkV1)
			if got == nil {
				t.Fatalf("no task claimed: %d %s", w.Code, w.Body.String())
			}
			if got.PriorSessionID != "" || got.PriorWorkDir != "" || got.PriorSessionResumeUnavailable {
				t.Fatalf("compaction claim carried resume state: session=%q workdir=%q unavailable=%v", got.PriorSessionID, got.PriorWorkDir, got.PriorSessionResumeUnavailable)
			}
		})
	}
}

// The setting moved between enqueue and claim: the claim fails the task, hands
// the daemon no payload, and the DM's own state is untouched.
func TestCompactionIsolation_ClaimRechecksRule2AfterSettingChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(d *isolationDM)
	}{
		{"setting cleared", func(d *isolationDM) { d.clearCompact() }},
		{"setting moved", func(d *isolationDM) { d.setCompact(eligibleAgent(t, "isolation-other-agent")) }},
		{"compaction agent archived", func(d *isolationDM) {
			dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, d.compactAgent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newIsolationDM(t)
			task := d.compactionTask(t, "queued", nil)
			tc.edit(d)
			// The chain change cancels queued compaction tasks; put it back so the
			// claim-time check, not the cancel, is what is under test.
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued', completed_at = NULL WHERE id = $1`, task)

			w, got := d.claimRaw(t, "")
			if got != nil {
				t.Fatalf("claim delivered a payload to a stale compaction agent: %+v", got)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("claim status = %d, want 200 with a null task: %s", w.Code, w.Body.String())
			}
			var status string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, task).Scan(&status)
			if status != "failed" {
				t.Fatalf("task status = %q, want failed", status)
			}
			d.requirePointerUntouched(t)
		})
	}
}
