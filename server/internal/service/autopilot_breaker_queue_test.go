package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

func breakerHeldFixture(t *testing.T) (fx principalFixture, agentID, autopilotID, triggerID string) {
	t.Helper()
	fx, owner := newPrincipalFixture(t)
	agentID = fx.privateAgentOwnedBy(t, owner, "breaker")
	fx.Exec(t, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agentID)
	fx.Exec(t, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, fx.WorkspaceID)
	autopilotID, triggerID = fx.autopilotWithTrigger(t, agentID, owner, owner)
	return fx, agentID, autopilotID, triggerID
}

// CHE-4460: retiring the held run is one transaction. When the run update
// fails, the task cancel rolls back, so the run is not left running behind a
// cancelled task that every later tick would skip against forever.
func TestAutopilotBreakerHeldRetirementIsAtomic(t *testing.T) {
	fx, _, autopilotID, triggerID := breakerHeldFixture(t)
	first := fx.dispatch(t, autopilotID, triggerID)
	firstID := util.UUIDToString(first.ID)

	suffix := strings.ReplaceAll(firstID, "-", "")
	fn, trg := "che4460_fail_"+suffix, "che4460_trg_"+suffix
	fx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected run update failure'; END $$ LANGUAGE plpgsql`, fn))
	fx.Exec(t, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON autopilot_run FOR EACH ROW WHEN (OLD.id = '%s' AND NEW.status = 'skipped') EXECUTE FUNCTION %s()`, trg, firstID, fn))
	t.Cleanup(func() {
		ctx := context.Background()
		fx.Pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON autopilot_run`, trg))
		fx.Pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})

	second := fx.dispatch(t, autopilotID, triggerID)
	if second.Status != "skipped" {
		t.Errorf("second tick = %q, want skipped while the held run could not be retired", second.Status)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE autopilot_run_id = $1 AND status = 'queued'`, firstID); n != 1 {
		t.Errorf("held run's queued tasks = %d, want 1 (cancel rolled back)", n)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE autopilot_run_id = $1 AND status = 'cancelled'`, firstID); n != 0 {
		t.Errorf("held run's cancelled tasks = %d, want 0", n)
	}
}

// CHE-4460: a claim that wins between the queued check and the cancel moves the
// task to dispatched; the next tick must not cancel that active task.
func TestAutopilotBreakerHeldRetirementSparesAClaimedTask(t *testing.T) {
	fx, _, autopilotID, triggerID := breakerHeldFixture(t)
	first := fx.dispatch(t, autopilotID, triggerID)
	firstID := util.UUIDToString(first.ID)

	// Hold the task row the way a claim does: the tick's queued check still
	// reads 'queued', then its cancel waits on this lock.
	ctx := context.Background()
	tx, err := fx.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE autopilot_run_id = $1 FOR UPDATE`, firstID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *db.AutopilotRun, 1)
	go func() { done <- fx.dispatch(t, autopilotID, triggerID) }()
	time.Sleep(500 * time.Millisecond)
	if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status = 'dispatched' WHERE autopilot_run_id = $1`, firstID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := <-done

	if second.Status != "skipped" {
		t.Errorf("second tick = %q, want skipped (the claimed run is active)", second.Status)
	}
	var taskStatus string
	fx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE autopilot_run_id = $1`, firstID).Scan(&taskStatus)
	if taskStatus != "dispatched" {
		t.Errorf("claimed task status = %q, want dispatched (not cancelled)", taskStatus)
	}
	var runStatus string
	fx.QueryRow(t, `SELECT status FROM autopilot_run WHERE id = $1`, firstID).Scan(&runStatus)
	if runStatus == "skipped" {
		t.Errorf("claimed run was retired")
	}
}
