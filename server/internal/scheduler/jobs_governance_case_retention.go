package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const JobNameGovernanceCaseEvidenceRetention = "governance_case_evidence_retention"

type governanceCaseRetentionWorkspaceLister interface {
	ListGovernanceCaseRetentionWorkspaces(ctx context.Context) ([]pgtype.UUID, error)
}

type governanceCaseRetentionSweeper interface {
	SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error)
}

func GovernanceCaseEvidenceRetentionJob(queries *db.Queries, pool *pgxpool.Pool) JobSpec {
	return JobSpec{
		Name:              JobNameGovernanceCaseEvidenceRetention,
		Cadence:           time.Hour,
		ScheduleDelay:     5 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        10 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeGovernanceCaseEvidenceRetentionHandler(queries, &governanceCaseRetentionPoolSweeper{queries: queries, pool: pool}),
	}
}

func makeGovernanceCaseEvidenceRetentionHandler(queries governanceCaseRetentionWorkspaceLister, sweeper governanceCaseRetentionSweeper) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		workspaces, err := queries.ListGovernanceCaseRetentionWorkspaces(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("list governance case retention workspaces: %w", err)
		}

		var affected int64
		for i, workspaceID := range workspaces {
			count, err := sweeper.SweepWorkspace(ctx, workspaceID)
			if err != nil {
				return HandlerResult{}, fmt.Errorf("retain governance case evidence for workspace %s: %w", util.UUIDToString(workspaceID), err)
			}
			affected += count
			if in.Heartbeat != nil && i%200 == 199 {
				if err := in.Heartbeat(ctx); err != nil {
					return HandlerResult{}, fmt.Errorf("heartbeat: %w", err)
				}
			}
		}

		return HandlerResult{
			RowsAffected: affected,
			Result:       map[string]any{"workspaces_swept": len(workspaces)},
		}, nil
	}
}

type governanceCaseRetentionPoolSweeper struct {
	queries *db.Queries
	pool    *pgxpool.Pool
}

func (s *governanceCaseRetentionPoolSweeper) SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.RedactExpiredGovernanceEvidenceForWorkspace(ctx, workspaceID); err != nil {
		return 0, fmt.Errorf("redact expired evaluations: %w", err)
	}
	if err := queries.RedactExpiredGovernanceAttemptResultsForWorkspace(ctx, workspaceID); err != nil {
		return 0, fmt.Errorf("redact expired attempts: %w", err)
	}

	var affected int64
	for _, prune := range []struct {
		name string
		run  func(context.Context, pgtype.UUID) (int64, error)
	}{
		{"evaluation sources", queries.DeleteExpiredGovernanceEvaluationSourcesForWorkspace},
		{"evaluations", queries.DeleteExpiredGovernanceEvaluationsForWorkspace},
		{"transitions", queries.DeleteExpiredGovernanceTransitionsForWorkspace},
		{"attempts", queries.DeleteExpiredGovernanceAttemptsForWorkspace},
		{"obligation attempt metadata", queries.PruneExpiredGovernanceObligationAttemptMetadata},
	} {
		count, err := prune.run(ctx, workspaceID)
		if err != nil {
			return 0, fmt.Errorf("prune expired %s: %w", prune.name, err)
		}
		affected += count
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return affected, nil
}
