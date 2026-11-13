package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5092 (CHE-4678 S9 P3): a script tick creates a normal agent task and skips
// only the model-only admission checks (provider hold, provider breaker). A
// run_only tick keeps every check.

const scriptTestHold = `{"provider_holds":[{"provider":"openai","text":"model budget spent"}]}`

// modelGatedFixture builds an autopilot whose agent runs on a model that has an
// open provider breaker AND a workspace provider hold.
func modelGatedFixture(t *testing.T, mode string) (fx principalFixture, agentID, autopilotID, triggerID string) {
	t.Helper()
	fx, owner := newPrincipalFixture(t)
	agentID = fx.privateAgentOwnedBy(t, owner, "script")
	fx.Exec(t, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agentID)
	fx.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, fx.WorkspaceID)
	fx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, fx.WorkspaceID, scriptTestHold)
	autopilotID, triggerID = fx.autopilotWithTrigger(t, agentID, owner, owner)
	if mode == "script" {
		fx.Exec(t, `UPDATE autopilot SET execution_mode = 'script', script_config = '{"name":"reconcile","args":[]}'::jsonb WHERE id = $1`, autopilotID)
	}
	return fx, agentID, autopilotID, triggerID
}

func TestAutopilotScriptTickIgnoresModelAdmission(t *testing.T) {
	fx, agentID, autopilotID, triggerID := modelGatedFixture(t, "script")

	run := fx.dispatch(t, autopilotID, triggerID)
	if run.Status != "running" {
		t.Fatalf("script run status = %q (%v), want running", run.Status, run.FailureReason)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND autopilot_run_id = $2 AND status = 'queued'`, agentID, util.UUIDToString(run.ID)); n != 1 {
		t.Fatalf("queued tasks for the run = %d, want exactly 1", n)
	}
	if !run.TaskID.Valid {
		t.Error("run must reference its task")
	}
}

func TestAutopilotRunOnlyTickStillRefusedByModelHold(t *testing.T) {
	fx, agentID, autopilotID, triggerID := modelGatedFixture(t, "run_only")

	run := fx.dispatch(t, autopilotID, triggerID)
	if run.Status != "skipped" {
		t.Fatalf("run_only status = %q, want skipped by the provider hold", run.Status)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1`, agentID); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestAutopilotScriptTickStillNeedsAReadyAgent(t *testing.T) {
	fx, agentID, autopilotID, triggerID := modelGatedFixture(t, "script")
	fx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, agentID)

	run := fx.dispatch(t, autopilotID, triggerID)
	if run.Status != "skipped" {
		t.Fatalf("script run with an archived agent = %q, want skipped", run.Status)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1`, agentID); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestAutopilotScriptRunSettlesFromTask(t *testing.T) {
	for _, tc := range []struct{ taskStatus, runStatus string }{
		{"completed", "completed"},
		{"failed", "failed"},
	} {
		t.Run(tc.taskStatus, func(t *testing.T) {
			fx, _, autopilotID, triggerID := modelGatedFixture(t, "script")
			run := fx.dispatch(t, autopilotID, triggerID)
			task, err := fx.q.GetAutopilotTaskByRun(context.Background(), run.ID)
			if err != nil {
				t.Fatalf("task for run: %v", err)
			}
			task.Status = tc.taskStatus
			fx.svc.SyncRunFromTask(context.Background(), task)

			var got string
			fx.QueryRow(t, `SELECT status FROM autopilot_run WHERE id = $1`, util.UUIDToString(run.ID)).Scan(&got)
			if got != tc.runStatus {
				t.Errorf("run status = %q, want %q", got, tc.runStatus)
			}
		})
	}
}
