package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	providerBreakerThreshold = 3
	providerBreakerWindow    = 5 * time.Minute
)

func providerBreakerShouldTrip(failures []time.Time, now time.Time, threshold int, window time.Duration) bool {
	if threshold <= 0 || window <= 0 {
		return false
	}
	cutoff := now.Add(-window)
	count := 0
	for _, failedAt := range failures {
		if !failedAt.Before(cutoff) && !failedAt.After(now) {
			count++
			if count >= threshold {
				return true
			}
		}
	}
	return false
}

func providerBreakerDisposition(state string) string {
	switch state {
	case "open", "half_open", "draining":
		return "hold"
	default:
		return "normal"
	}
}

// ProviderBreakerAdmit is the M3c admission contract. Unknown providers and
// workspaces without breaker state remain normal; database errors propagate.
func ProviderBreakerAdmit(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if queries == nil || !workspaceID.Valid || provider == "" {
		return "normal", nil
	}
	state, err := queries.GetProviderBreakerState(ctx, db.GetProviderBreakerStateParams{WorkspaceID: workspaceID, Provider: provider})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "normal", nil
		}
		return "", fmt.Errorf("read provider breaker: %w", err)
	}
	return providerBreakerDisposition(state), nil
}

func providerBreakerVerdict(ctx context.Context, lookup RuntimeLookup, agent db.Agent) (AgentVerdict, error) {
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok || lookup.Queries == nil {
		return AgentVerdict{}, nil
	}
	state, err := ProviderBreakerAdmit(ctx, lookup.Queries, agent.WorkspaceID, provider)
	if err != nil {
		return AgentVerdict{}, err
	}
	if state != "hold" {
		return AgentVerdict{}, nil
	}
	return AgentVerdict{
		Availability: AgentBlocked,
		Reason:       dispatch.ReasonProviderBreaker,
		Detail:       fmt.Sprintf("This agent's provider (%s) is temporarily unavailable because its circuit breaker is open.", provider),
	}, nil
}

func recordProviderServerFailure(ctx context.Context, queries *db.Queries, task db.AgentTaskQueue) error {
	agent, err := queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return fmt.Errorf("load agent for provider breaker: %w", err)
	}
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok {
		return nil
	}
	lockKey := fmt.Sprintf("%x:%s", agent.WorkspaceID.Bytes, provider)
	if err := queries.LockProviderBreakerTrip(ctx, lockKey); err != nil {
		return fmt.Errorf("lock provider breaker trip: %w", err)
	}
	rows, err := queries.ListRecentProviderServerFailures(ctx, db.ListRecentProviderServerFailuresParams{WorkspaceID: agent.WorkspaceID, WindowSeconds: int32(providerBreakerWindow.Seconds())})
	if err != nil {
		return fmt.Errorf("load recent provider failures: %w", err)
	}
	var failedAt []time.Time
	var evidence []pgtype.UUID
	for _, row := range rows {
		if row.Model.Valid {
			if failedProvider, found := ResolveModelProvider(row.Model.String); found && failedProvider == provider {
				failedAt = append(failedAt, row.FailedAt.Time)
				evidence = append(evidence, row.ID)
			}
		}
	}
	if !providerBreakerShouldTrip(failedAt, time.Now(), providerBreakerThreshold, providerBreakerWindow) {
		return nil
	}
	version, _, err := providerBreakerVersion(ctx, queries, agent.WorkspaceID, provider)
	if err != nil {
		return err
	}
	changed, err := queries.OpenProviderBreaker(ctx, db.OpenProviderBreakerParams{WorkspaceID: agent.WorkspaceID, Provider: provider, TripEvidence: evidence, ExpectedVersion: version})
	if err != nil {
		return fmt.Errorf("open provider breaker: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("open provider breaker: version compare-and-swap failed")
	}
	return nil
}

func recordProviderSuccess(ctx context.Context, queries *db.Queries, task db.AgentTaskQueue) error {
	agent, err := queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return fmt.Errorf("load agent for provider success: %w", err)
	}
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok {
		return nil
	}
	lockKey := fmt.Sprintf("%x:%s", agent.WorkspaceID.Bytes, provider)
	if err := queries.LockProviderBreakerTrip(ctx, lockKey); err != nil {
		return fmt.Errorf("lock provider breaker success: %w", err)
	}
	version, exists, err := providerBreakerVersion(ctx, queries, agent.WorkspaceID, provider)
	if err != nil {
		return err
	}
	if !exists {
		_, held, err := providerHoldBlocksAgent(ctx, RuntimeLookup{Queries: queries}, agent)
		if err != nil {
			return fmt.Errorf("check provider hold for successful task: %w", err)
		}
		if !held {
			return nil
		}
	}
	changed, err := queries.RecordProviderSuccess(ctx, db.RecordProviderSuccessParams{WorkspaceID: agent.WorkspaceID, Provider: provider, ExpectedVersion: version})
	if err != nil {
		return fmt.Errorf("record provider success: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("record provider success: version compare-and-swap failed")
	}
	return nil
}

func providerBreakerVersion(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider string) (int64, bool, error) {
	version, err := queries.GetProviderBreakerVersion(ctx, db.GetProviderBreakerVersionParams{WorkspaceID: workspaceID, Provider: provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read provider breaker version: %w", err)
	}
	return version, true, nil
}
