package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestProviderBreakerTripThresholdAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	failures := []time.Time{now.Add(-4 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute)}

	if providerBreakerShouldTrip(failures[:2], now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("N-1 failures must not trip the breaker")
	}
	if !providerBreakerShouldTrip(failures, now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("N failures within the window must trip the breaker")
	}
	if providerBreakerShouldTrip([]time.Time{now.Add(-6 * time.Minute), now.Add(-time.Minute), now}, now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("a failure outside the window must not count")
	}
	if !providerBreakerShouldTrip([]time.Time{now.Add(-time.Minute), now.Add(-30 * time.Second), now.Add(time.Second)}, now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("database timestamps slightly ahead of the application clock must count")
	}
	if providerBreakerShouldTrip([]time.Time{now.Add(-3 * time.Second), now.Add(-2 * time.Second), now.Add(-time.Second)}, now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("failures within one burst must count as a single signal")
	}
	if !providerBreakerShouldTrip([]time.Time{now.Add(-time.Minute), now.Add(-time.Minute + time.Second), now.Add(-30 * time.Second), now.Add(-time.Second)}, now, 3, 5*time.Minute, 5*time.Second) {
		t.Fatal("a burst plus two later distinct failures must trip")
	}
}

func TestProviderBreakerDisposition(t *testing.T) {
	for _, state := range []string{"open", "half_open"} {
		if got := providerBreakerDisposition(state, false); got != "hold" {
			t.Errorf("state %q before next_probe_at: disposition = %q, want hold", state, got)
		}
		if got := providerBreakerDisposition(state, true); got != "normal" {
			t.Errorf("state %q once due: disposition = %q, want normal", state, got)
		}
	}
	for _, due := range []bool{false, true} {
		if got := providerBreakerDisposition("draining", due); got != "hold" {
			t.Errorf("draining (due=%v): disposition = %q, want hold", due, got)
		}
	}
	for _, state := range []string{"closed", "unknown"} {
		if got := providerBreakerDisposition(state, false); got != "normal" {
			t.Errorf("state %q: disposition = %q, want normal", state, got)
		}
	}
}

func TestRecordProviderSuccessIgnoresNonCompletedTasks(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			if err := recordProviderSuccess(context.Background(), nil, db.AgentTaskQueue{Status: status}); err != nil {
				t.Fatalf("recordProviderSuccess for %s task: %v", status, err)
			}
		})
	}
}

func TestCompleteTaskWritesProviderSuccessEvidenceOnlyForCompletedTasks(t *testing.T) {
	d := newDeliveryFixture(t)
	ctx := context.Background()
	pool := d.f.Pool
	agentID := util.MustParseUUID(d.agentID)
	if _, err := pool.Exec(ctx, `UPDATE agent SET model='gpt-5.6-sol' WHERE id=$1`, agentID); err != nil {
		t.Fatal(err)
	}
	var workspaceID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM agent WHERE id=$1`, agentID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	complete := func(task pgtype.UUID) {
		t.Helper()
		if !d.complete(t, task, "") {
			t.Fatal("completion did not transition the running task")
		}
	}
	complete(d.runningTask(t, nil))
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("completion without breaker state created %d rows, want 1", count)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_breaker SET state='open', last_success_at=NULL, next_probe_at=now() + interval '5 minutes' WHERE workspace_id=$1 AND provider='openai'`, workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"failed", "cancelled"} {
		task := d.runningTask(t, nil)
		if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status=$1, completed_at=now() WHERE id=$2`, status, task); err != nil {
			t.Fatal(err)
		}
		if d.complete(t, task, "") {
			t.Fatalf("completion transitioned a %s task", status)
		}
		var lastSuccess pgtype.Timestamptz
		if err := pool.QueryRow(ctx, `SELECT last_success_at FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceID).Scan(&lastSuccess); err != nil {
			t.Fatal(err)
		}
		if lastSuccess.Valid {
			t.Fatalf("%s task wrote success evidence at %s", status, lastSuccess.Time)
		}
	}
	complete(d.runningTask(t, nil))
	var lastSuccess pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT last_success_at FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceID).Scan(&lastSuccess); err != nil {
		t.Fatal(err)
	}
	if !lastSuccess.Valid {
		t.Fatal("successful completion did not record provider evidence")
	}
}

func TestProviderBreakerAdmitReadsPersistedState(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, _, _, _ := seedAttributionFixture(t, pool)
	parsedWorkspaceID := util.MustParseUUID(workspaceID)
	if _, err := pool.Exec(ctx, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, parsedWorkspaceID); err != nil {
		t.Fatal(err)
	}
	got, err := ProviderBreakerAdmit(ctx, db.New(pool), parsedWorkspaceID, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hold" {
		t.Fatalf("persisted open breaker admission = %q, want hold", got)
	}
	got, err = ProviderBreakerAdmit(ctx, db.New(pool), parsedWorkspaceID, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if got != "normal" {
		t.Fatalf("Claude provider admission under an OpenAI breaker = %q, want normal", got)
	}
}

func TestRecordProviderSuccessCreatesClosedRowAndPreservesHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, _, agentID, _ := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "")
	agentUUID := util.MustParseUUID(agentID)
	workspaceUUID := util.MustParseUUID(workspaceID)
	task := db.AgentTaskQueue{AgentID: agentUUID, Status: "completed"}
	q := db.New(pool)

	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")
	if err := recordProviderSuccess(ctx, q, task); err != nil {
		t.Fatal(err)
	}
	var state string
	var lastSuccess pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT state, last_success_at FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceUUID).Scan(&state, &lastSuccess); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || !lastSuccess.Valid {
		t.Fatalf("hold success evidence state=%q last_success_valid=%t, want closed/true", state, lastSuccess.Valid)
	}
	var holdPreserved bool
	if err := pool.QueryRow(ctx, `SELECT settings @> '{"provider_holds":[{"provider":"openai","text":"CHE-607 test hold"}]}'::jsonb FROM workspace WHERE id=$1`, workspaceUUID).Scan(&holdPreserved); err != nil {
		t.Fatal(err)
	}
	if !holdPreserved {
		t.Fatal("provider hold settings changed")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_breaker SET state='open' WHERE workspace_id=$1 AND provider='openai'`, workspaceUUID); err != nil {
		t.Fatal(err)
	}
	agent, err := q.GetAgent(ctx, agentUUID)
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := AgentReadiness(ctx, RuntimeLookup{Queries: q}, agent)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Reason != dispatch.ReasonProviderHold {
		t.Fatalf("human hold precedence over open breaker = %q, want %q", verdict.Reason, dispatch.ReasonProviderHold)
	}
}

func TestOpenProviderBreakerUsesPersistedCoolOff(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, _, _, _ := seedAttributionFixture(t, pool)
	workspaceUUID := util.MustParseUUID(workspaceID)
	if _, err := pool.Exec(ctx, `INSERT INTO provider_breaker (workspace_id, provider, state, cool_off_s) VALUES ($1, 'openai', 'closed', 17)`, workspaceUUID); err != nil {
		t.Fatal(err)
	}
	changed, err := db.New(pool).OpenProviderBreaker(ctx, db.OpenProviderBreakerParams{
		WorkspaceID: workspaceUUID, Provider: "openai", TripEvidence: []pgtype.UUID{}, ExpectedVersion: 1, CoolOffSeconds: providerBreakerCoolOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("breaker rows changed = %d, want 1", changed)
	}
	var coolOff int
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM next_probe_at - opened_at)::int FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceUUID).Scan(&coolOff); err != nil {
		t.Fatal(err)
	}
	if coolOff != 17 {
		t.Fatalf("next probe cool-off = %d seconds, want persisted 17", coolOff)
	}
}

func TestProviderBreakerTripsOnlyForSameProviderServerFailuresAndSerializesConcurrentTrips(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	agentUUID := util.MustParseUUID(agentID)
	workspaceUUID := util.MustParseUUID(workspaceID)
	issueUUID := util.MustParseUUID(issueID)
	if _, err := pool.Exec(ctx, `UPDATE agent SET model='gpt-5.6-sol' WHERE id=$1`, agentUUID); err != nil {
		t.Fatal(err)
	}
	var runtimeID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id=$1`, agentUUID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	var claudeAgentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args, model)
		SELECT workspace_id, 'breaker-claude', runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, $2, instructions, custom_env, custom_args, 'claude-sonnet-5[1m]'
		FROM agent WHERE id=$1 RETURNING id`, agentUUID, util.MustParseUUID(userID)).Scan(&claudeAgentID); err != nil {
		t.Fatal(err)
	}
	failures := 0
	insertFailed := func(agent pgtype.UUID, reason string) pgtype.UUID {
		t.Helper()
		failures++ // spaced out: a same-instant burst counts as one signal
		var taskID pgtype.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts, failure_reason, completed_at)
			VALUES ($1, $2, $3, 'failed', 0, 1, 3, $4, now() - make_interval(secs => $5::int)) RETURNING id`, agent, runtimeID, issueUUID, reason, 30*(5-failures)).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}
	first := insertFailed(agentUUID, "agent_error.provider_server_error")
	second := insertFailed(agentUUID, "agent_error.provider_server_error")
	insertFailed(agentUUID, "agent_error.rate_limited")
	insertFailed(claudeAgentID, "agent_error.provider_server_error")
	if err := recordProviderServerFailure(ctx, db.New(pool), db.AgentTaskQueue{AgentID: agentUUID}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_breaker WHERE workspace_id=$1`, workspaceUUID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("non-matching failures tripped breaker; rows=%d, want 0", count)
	}
	third := insertFailed(agentUUID, "agent_error.provider_server_error")
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	for _, taskID := range []pgtype.UUID{first, second, third} {
		wg.Add(1)
		go func(taskID pgtype.UUID) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errCh <- err
				return
			}
			if err = recordProviderServerFailure(ctx, db.New(tx), db.AgentTaskQueue{AgentID: agentUUID, ID: taskID}); err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(ctx)
			}
			errCh <- err
		}(taskID)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_breaker WHERE workspace_id=$1 AND provider='openai'`, workspaceUUID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent trip row count = %d, want 1", count)
	}
}

// CHE-4381: the refusal text for a provider breaker names the provider and the
// opened_at / next_probe_at times in ICT, and is not the CLI-reinstall text.
func TestRuntimeUnusableNoticeProviderBreaker(t *testing.T) {
	openedAt := time.Date(2026, 11, 7, 5, 0, 0, 0, time.UTC)
	detail := providerBreakerNotice("openai", openedAt, openedAt.Add(5*time.Minute))
	notice := RuntimeUnusableNotice("Mika", AgentVerdict{Reason: dispatch.ReasonProviderBreaker, Detail: detail})
	for _, want := range []string{"openai", "2026-11-07 12:00 ICT", "2026-11-07 12:05 ICT"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice lacks %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "Reinstall") {
		t.Errorf("a breaker hold is not a CLI problem:\n%s", notice)
	}
}

// CHE-4408: an open breaker is a wait, not a refusal. The claim path already
// holds queued work until the probe is due, so assign/mention/comment triggers
// must queue instead of leaving a refused trigger with no retry.
func TestAgentReadinessProviderBreakerIsWaitable(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, _, agentID, _ := seedAttributionFixture(t, pool)
	ws, agent := util.MustParseUUID(workspaceID), util.MustParseUUID(agentID)
	if _, err := pool.Exec(ctx, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, ws); err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	a, err := queries.GetAgent(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := AgentReadiness(ctx, RuntimeLookup{Queries: queries}, a)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Blocked() || verdict.Ready() || verdict.Reason != dispatch.ReasonProviderBreaker {
		t.Fatalf("open breaker verdict = %+v, want waitable with reason provider_breaker", verdict)
	}

	// A broken runtime still wins: waiting on the breaker cannot fix it.
	if _, err := pool.Exec(ctx, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	a, _ = queries.GetAgent(ctx, agent)
	verdict, err = AgentReadiness(ctx, RuntimeLookup{Queries: queries}, a)
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Blocked() || verdict.Reason != dispatch.ReasonAgentRuntimeRequired {
		t.Fatalf("unbound agent under an open breaker = %+v, want blocked agent_runtime_required", verdict)
	}
}

// CHE-4408: a squad assignment during a breaker hold must still reach the
// leader's queue. The trigger gate used to skip every non-ready leader, so the
// handoff was lost and nothing could retry it after the probe.
func TestWillEnqueueRunSquadLeaderQueuesDuringBreakerHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	ws, agent := util.MustParseUUID(workspaceID), util.MustParseUUID(agentID)
	if _, err := pool.Exec(ctx, `UPDATE agent SET model = 'gpt-5.6-terra' WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_breaker (workspace_id, provider, state, next_probe_at) VALUES ($1, 'openai', 'open', now() + interval '5 minutes')`, ws); err != nil {
		t.Fatal(err)
	}
	var squadID string
	if err := pool.QueryRow(ctx, `INSERT INTO squad (workspace_id, name, leader_id, creator_id) VALUES ($1, 'che4408 squad', $2, $3) RETURNING id`,
		ws, agent, util.MustParseUUID(userID)).Scan(&squadID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo', assignee_type = 'squad', assignee_id = $2 WHERE id = $1`, util.MustParseUUID(issueID), util.MustParseUUID(squadID)); err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	issue, err := queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	svc := &IssueService{Queries: queries}
	trigger, ok := svc.WillEnqueueRun(ctx, IssueTriggerInput{Issue: issue, AssigneeChanged: true}, IssueTriggerProbe{})
	if !ok || trigger.AgentID != agent {
		t.Fatalf("squad leader under an open breaker = (%+v, %v), want a trigger for the leader", trigger, ok)
	}
}
