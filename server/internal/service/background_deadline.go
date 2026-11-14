package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// ErrBackgroundAttemptExpired: a completion arrived after its attempt's run
// deadline. The completion rolls back and writes nothing; the sweep fails the
// attempt and the chain moves on (CHE-4103).
var ErrBackgroundAttemptExpired = errors.New("background task: completion after the run deadline")

// backgroundDeadlineReasons are the failure reasons of a missed claim or run
// deadline. Like a provider failure, they move the primary slot to the fallback.
var backgroundDeadlineReasons = map[string]bool{
	taskfailure.ReasonBgRunTimeout.String():   true,
	taskfailure.ReasonBgClaimTimeout.String(): true,
}

// backgroundRunExpired reports whether a just-completed task ran past the run
// deadline its context carries. Both times are database clock readings
// (started_at, completed_at), so no clock skew enters.
func backgroundRunExpired(task db.AgentTaskQueue) bool {
	var c BackgroundTaskContext
	if json.Unmarshal(task.Context, &c) != nil || c.Type != BackgroundTaskContextType || c.RunSeconds <= 0 {
		return false
	}
	if !task.StartedAt.Valid || !task.CompletedAt.Valid {
		return false
	}
	return task.CompletedAt.Time.Sub(task.StartedAt.Time) > time.Duration(c.RunSeconds)*time.Second
}

// SweepBackgroundDeadlines enforces the deadlines of output-only background
// tasks (CHE-4103). A task still queued past its claim deadline is cancelled;
// a run past its run deadline is failed with bg_run_timeout. Each then goes
// through the failover rule: the next slot, or the job's no-agent path. The
// client skeleton window is not enforcement; these deadlines are.
func (s *TaskService) SweepBackgroundDeadlines(ctx context.Context, batch int) (cancelled, failed int, err error) {
	unclaimed, err := s.terminateTasksInTx(ctx, func(qtx *db.Queries) ([]db.AgentTaskQueue, error) {
		return qtx.CancelUnclaimedBackgroundTasks(ctx, int32(batch))
	})
	if err != nil {
		return 0, 0, err
	}
	for _, task := range unclaimed {
		s.failOverTask(ctx, task, taskfailure.ReasonBgClaimTimeout.String())
	}

	expired, err := s.Queries.ListRunExpiredBackgroundTasks(ctx, int32(batch))
	if err != nil {
		return len(unclaimed), 0, err
	}
	for _, task := range expired {
		// FailTask guards the status and runs failOverTask: a run that completed
		// meanwhile is left alone.
		if _, err := s.FailTask(ctx, task.ID, "background run exceeded its run deadline", "", "", "", taskfailure.ReasonBgRunTimeout.String(), false, "", ""); err != nil {
			slog.Warn("background sweep: fail expired run", "task_id", util.UUIDToString(task.ID), "error", err)
			continue
		}
		failed++
	}
	// Claimed in time but never started: same claim timeout, counted from the claim.
	unstarted, err := s.Queries.ListStartExpiredBackgroundTasks(ctx, int32(batch))
	if err != nil {
		return len(unclaimed), failed, err
	}
	for _, task := range unstarted {
		if _, err := s.FailTask(ctx, task.ID, "background task was not started before its start bound", "", "", "", taskfailure.ReasonBgClaimTimeout.String(), false, "", ""); err != nil {
			slog.Warn("background sweep: fail unstarted claim", "task_id", util.UUIDToString(task.ID), "error", err)
			continue
		}
		failed++
	}
	return len(unclaimed), failed, nil
}
