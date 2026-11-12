package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const defaultProviderHoldSeconds = 6 * 60 * 60

// ProviderHoldSecondsFromSettings reads workspace-scoped seconds. The default
// bounds outage waiting to six hours; existing task deadlines are immutable.
func ProviderHoldSecondsFromSettings(settings []byte) (int32, error) {
	var config struct {
		Seconds *int32 `json:"provider_breaker_max_hold_seconds"`
	}
	if len(settings) != 0 {
		if err := json.Unmarshal(settings, &config); err != nil {
			return 0, fmt.Errorf("provider breaker hold settings: %w", err)
		}
	}
	if config.Seconds == nil {
		return defaultProviderHoldSeconds, nil
	}
	if *config.Seconds <= 0 {
		return 0, fmt.Errorf("provider_breaker_max_hold_seconds must be positive")
	}
	return *config.Seconds, nil
}

func issueProviderHoldConfig(ctx context.Context, q *db.Queries, agentID pgtype.UUID) (pgtype.Text, pgtype.Int4, error) {
	agent, err := q.GetAgent(ctx, agentID)
	if err != nil {
		return pgtype.Text{}, pgtype.Int4{}, err
	}
	provider, known := ResolveModelProvider(agent.Model.String)
	if !known {
		return pgtype.Text{}, pgtype.Int4{}, nil
	}
	workspace, err := q.GetWorkspace(ctx, agent.WorkspaceID)
	if err != nil {
		return pgtype.Text{}, pgtype.Int4{}, err
	}
	seconds, err := ProviderHoldSecondsFromSettings(workspace.Settings)
	return pgtype.Text{String: provider, Valid: true}, pgtype.Int4{Int32: seconds, Valid: true}, err
}

// All ordinary issue insertions, including transaction-bound mention and
// delegation paths, decide status and marker in the INSERT itself. Retry clones
// keep their existing policy; chat and quick-create do not use this hold.
func createProviderHeldAgentTask(ctx context.Context, q *db.Queries, p db.CreateAgentTaskParams) (db.AgentTaskQueue, error) {
	if p.IssueID.Valid {
		var err error
		p.HoldProvider, p.ProviderHoldSeconds, err = issueProviderHoldConfig(ctx, q, p.AgentID)
		if err != nil {
			return db.AgentTaskQueue{}, err
		}
	}
	return q.CreateAgentTask(ctx, p)
}

// FailExpiredProviderHolds uses the same terminal settlement as other sweepers.
// Expiry is not a retryable reason. Its durable inbox notice commits with it.
func (s *TaskService) FailExpiredProviderHolds(ctx context.Context, now time.Time, limit int32) ([]db.AgentTaskQueue, error) {
	tasks, err := s.terminateTasksInTx(ctx, func(q *db.Queries) ([]db.AgentTaskQueue, error) {
		rows, err := q.FailExpiredProviderHolds(ctx, db.FailExpiredProviderHoldsParams{
			Now: pgtype.Timestamptz{Time: now, Valid: true}, MaxPerTick: limit,
		})
		tasks := make([]db.AgentTaskQueue, len(rows))
		for i, row := range rows {
			tasks[i] = db.AgentTaskQueue(row)
		}
		return tasks, err
	})
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		target, err := s.Queries.GetProviderFailureNoticeTarget(ctx, task.ID)
		if err == nil {
			s.publishProviderFailureNotice(ctx, s.Queries, task.ID, target.WorkspaceID)
		}
	}
	return tasks, nil
}
