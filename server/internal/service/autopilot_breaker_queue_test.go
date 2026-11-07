package service

import (
	"testing"
)

// CHE-4460: a scheduled run that meets an open provider breaker waits in the
// queue (the claim gate holds it until the probe is due) instead of being
// skipped, and a second schedule tick does not queue a second run.
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
			queued := `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1 AND status = 'queued'`
			if n := fx.Count(t, queued, agentID); n != 1 {
				t.Fatalf("queued tasks = %d, want 1", n)
			}

			again := fx.dispatch(t, autopilotID, triggerID)
			if again.Status != "skipped" {
				t.Errorf("second tick status = %q, want skipped as a duplicate of the queued run", again.Status)
			}
			if n := fx.Count(t, queued, agentID); n != 1 {
				t.Errorf("queued tasks after second tick = %d, want still 1", n)
			}
		})
	}
}
