package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-4460: a scheduled run that meets an open provider breaker waits in the
// queue (the claim gate holds it until the probe is due) instead of being
// skipped.
func TestAutopilotScheduledRunQueuesDuringProviderBreakerHold(t *testing.T) {
	for _, mode := range []string{"run_only", "create_issue"} {
		t.Run(mode, func(t *testing.T) {
			fx, owner := newPrincipalFixture(t)
			agentID := fx.privateAgentOwnedBy(t, owner, "breaker")
			fx.Exec(t, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agentID)
			fx.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, fx.WorkspaceID)
			autopilotID, triggerID := fx.autopilotWithTrigger(t, agentID, owner, owner)
			fx.Exec(t, `UPDATE autopilot SET execution_mode = $2 WHERE id = $1`, autopilotID, mode)

			run := fx.dispatch(t, autopilotID, triggerID)
			if run.Status == "skipped" || run.Status == "failed" {
				t.Fatalf("first dispatch status = %q (%v), want a queued run", run.Status, run.FailureReason)
			}
			if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status = 'queued'`, agentID); n != 1 {
				t.Fatalf("queued tasks = %d, want 1", n)
			}
		})
	}
}

// CHE-4460: the next schedule tick replaces a run still held by the breaker, so
// a long outage never leaves stale work to run after recovery. Once the breaker
// has closed, the queued run is about to start and the new tick is the duplicate.
func TestAutopilotNextTickSupersedesBreakerHeldRun(t *testing.T) {
	fx, owner := newPrincipalFixture(t)
	agentID := fx.privateAgentOwnedBy(t, owner, "breaker")
	fx.Exec(t, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agentID)
	fx.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, fx.WorkspaceID)
	autopilotID, triggerID := fx.autopilotWithTrigger(t, agentID, owner, owner)

	first := fx.dispatch(t, autopilotID, triggerID)
	second := fx.dispatch(t, autopilotID, triggerID)
	if second.Status == "skipped" || second.Status == "failed" {
		t.Fatalf("second tick status = %q (%v), want a queued run", second.Status, second.FailureReason)
	}
	var oldStatus string
	fx.QueryRow(t, `SELECT status FROM autopilot_run WHERE id = $1`, util.UUIDToString(first.ID)).Scan(&oldStatus)
	if oldStatus != "skipped" {
		t.Errorf("held run status = %q, want skipped (superseded)", oldStatus)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status = 'queued'`, agentID); n != 1 {
		t.Errorf("queued tasks = %d, want exactly 1", n)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE autopilot_run_id = $1 AND status = 'cancelled'`, util.UUIDToString(first.ID)); n != 1 {
		t.Errorf("cancelled tasks of the held run = %d, want 1", n)
	}

	fx.Exec(t, `DELETE FROM provider_breaker WHERE workspace_id = $1`, fx.WorkspaceID)
	third := fx.dispatch(t, autopilotID, triggerID)
	if third.Status != "skipped" {
		t.Errorf("tick after the breaker closed = %q, want skipped as a duplicate of the queued run", third.Status)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status = 'queued'`, agentID); n != 1 {
		t.Errorf("queued tasks after the closed-breaker tick = %d, want still 1", n)
	}
}
