package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestProviderHoldMediaAdmissionAndRelease(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	q := db.New(r.pool)
	r.breaker(t, "open", time.Hour, 300, pgtype.UUID{})
	issue, err := q.GetIssue(ctx, r.issue)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewTaskService(q, r.pool, nil, events.New())
	fireAt := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	task, err := svc.EnqueueDeferredChannelIssueTask(ctx, issue, fireAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(task.Context), "provider_hold") || !strings.Contains(string(task.Context), "channel_issue_media_pending") {
		t.Fatalf("media admission lost a gate: %s", task.Context)
	}
	if _, err := q.PromoteDeferredChannelIssueTask(ctx, task.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("held media promotion: %v", err)
	}
	r.breaker(t, "closed", time.Hour, 300, pgtype.UUID{})
	row, err := q.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: r.workspaceID, Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	released, err := q.ReleaseProviderHeldTask(ctx, db.ReleaseProviderHeldTaskParams{WorkspaceID: r.workspaceID, Provider: "openai", ExpectedVersion: row.Version, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "deferred" || !released.FireAt.Time.Equal(fireAt) || !strings.Contains(string(released.Context), "channel_issue_media_pending") {
		t.Fatalf("release removed the media gate: %+v", released)
	}
	rows, err := q.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{RuntimeID: r.runtimeID, RuntimeStaleSecs: 60})
	if err != nil || len(rows) != 0 {
		t.Fatalf("media fallback fired early: %d %v", len(rows), err)
	}
	if _, err := q.PromoteDeferredChannelIssueTask(ctx, task.ID); err != nil {
		t.Fatalf("media-ready promotion after release: %v", err)
	}
}

func TestProviderHoldCanaryRestartAndDrainingAdmission(t *testing.T) {
	for _, state := range []string{"half_open", "draining"} {
		t.Run(state, func(t *testing.T) {
			r := newBreakerRig(t)
			ctx := context.Background()
			q := db.New(r.pool)
			canary := r.task(t, "queued", "", 0)
			if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, canary); err != nil {
				t.Fatal(err)
			}
			r.breaker(t, state, -time.Hour, 300, canary)
			issue, err := q.GetIssue(ctx, r.issue)
			if err != nil {
				t.Fatal(err)
			}
			svc := NewTaskService(q, r.pool, nil, events.New())
			task, err := svc.EnqueueTaskForIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != "deferred" {
				t.Fatalf("new arrival bypassed %s: %s", state, task.Status)
			}
			// Recreate the service while the canary is in flight, past its probe deadline.
			restarted := NewTaskService(q, r.pool, nil, events.New())
			if task, err := restarted.ClaimTask(ctx, r.agentID); err != nil || task != nil {
				t.Fatalf("restart admitted a second task: %v %v", task, err)
			}
			breaker, err := q.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: r.workspaceID, Provider: "openai"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.ReleaseProviderHeldTask(ctx, db.ReleaseProviderHeldTaskParams{WorkspaceID: r.workspaceID, Provider: "openai", ExpectedVersion: breaker.Version, TaskID: task.ID}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("released while %s: %v", state, err)
			}
		})
	}
}

func TestProviderHoldReleaseRechecksState(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	q := db.New(r.pool)
	id := r.task(t, "deferred", "", 0)
	if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET context='{"provider_hold":{"provider":"openai","since":"2026-11-12T00:00:00Z","max_hold_until":"2099-01-01T00:00:00Z"}}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	r.breaker(t, "closed", time.Hour, 300, pgtype.UUID{})
	decision, err := q.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: r.workspaceID, Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	// A trip between the controller's decision and its release must win.
	r.breaker(t, "open", time.Hour, 300, pgtype.UUID{})
	if _, err := q.ReleaseProviderHeldTask(ctx, db.ReleaseProviderHeldTaskParams{WorkspaceID: r.workspaceID, Provider: "openai", ExpectedVersion: decision.Version, TaskID: id}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale closed decision released a task: %v", err)
	}
	stored, err := q.GetAgentTask(ctx, id)
	if err != nil || stored.Status != "deferred" || !strings.Contains(string(stored.Context), "provider_hold") {
		t.Fatalf("stale release changed task: %s %s %v", stored.Status, stored.Context, err)
	}
}

func TestProviderHoldExpiryFailsOnceAndNotifiesOwner(t *testing.T) {
	r := newBreakerRig(t)
	ctx := context.Background()
	q := db.New(r.pool)
	r.breaker(t, "open", time.Hour, 300, pgtype.UUID{})
	if _, err := r.pool.Exec(ctx, `UPDATE workspace SET settings=COALESCE(settings,'{}') || '{"provider_breaker_max_hold_seconds":60}' WHERE id=$1`, r.workspaceID); err != nil {
		t.Fatal(err)
	}
	issue, err := q.GetIssue(ctx, r.issue)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewTaskService(q, r.pool, nil, events.New())
	task, err := svc.EnqueueTaskForIssueByActor(ctx, issue, issue.CreatorID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Hold struct {
			Provider string
			Since    time.Time
			Until    time.Time `json:"max_hold_until"`
		} `json:"provider_hold"`
	}
	if err := json.Unmarshal(task.Context, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Hold.Provider != "openai" || metadata.Hold.Until.Sub(metadata.Hold.Since) != time.Minute {
		t.Fatalf("configured deadline: %s", task.Context)
	}
	// A runtime_offline retry with an overdue fire_at has no provider marker.
	parent := r.task(t, "failed", "runtime_offline", 0)
	retry := r.task(t, "deferred", "", 0)
	if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET parent_task_id=$2, fire_at=now()-interval '1 day' WHERE id=$1`, retry, parent); err != nil {
		t.Fatal(err)
	}
	if rows, err := svc.FailExpiredProviderHolds(ctx, metadata.Hold.Until.Add(-time.Microsecond), 10); err != nil || len(rows) != 0 {
		t.Fatalf("expired early: %d %v", len(rows), err)
	}
	rows, err := svc.FailExpiredProviderHolds(ctx, metadata.Hold.Until, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expiry: %d %v", len(rows), err)
	}
	failed := rows[0]
	if failed.ID != task.ID || failed.Status != "failed" || failed.FailureReason.String != "provider_breaker_hold_expired" || retryEligible(failed.FailureReason.String, failed) {
		t.Fatalf("expiry state: %+v", failed)
	}
	if child, err := svc.MaybeRetryFailedTask(ctx, failed); err != nil || child != nil {
		t.Fatalf("expiry retried: %v %v", child, err)
	}
	if rows, err := svc.FailExpiredProviderHolds(ctx, metadata.Hold.Until.Add(time.Hour), 10); err != nil || len(rows) != 0 {
		t.Fatalf("duplicate expiry: %d %v", len(rows), err)
	}
	item, err := q.GetInboxItem(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.RecipientID != task.AccountableUserID || item.Type != "provider_failure_recovery_blocked" || !strings.Contains(string(item.Details), "provider_breaker_hold_expired") {
		t.Fatalf("owner notice: %+v", item)
	}
	stored, err := q.GetAgentTask(ctx, retry)
	if err != nil || stored.Status != "deferred" {
		t.Fatalf("runtime_offline retry changed: %s %v", stored.Status, err)
	}
}

func TestProviderHoldSecondsSettings(t *testing.T) {
	for _, tc := range []struct {
		settings string
		seconds  int32
		invalid  bool
	}{
		{`{}`, 21600, false}, {`{"provider_breaker_max_hold_seconds":120}`, 120, false},
		{`{"provider_breaker_max_hold_seconds":0}`, 0, true}, {`{"provider_breaker_max_hold_seconds":"bad"}`, 0, true},
	} {
		got, err := ProviderHoldSecondsFromSettings([]byte(tc.settings))
		if (err != nil) != tc.invalid || got != tc.seconds {
			t.Fatalf("settings %s = %d %v", tc.settings, got, err)
		}
	}
}
