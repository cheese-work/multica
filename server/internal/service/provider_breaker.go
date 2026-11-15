package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	providerBreakerThreshold = 3
	providerBreakerWindow    = 5 * time.Minute
	// One provider outage fails every in-flight task at about the same time;
	// those failures are one signal, not several.
	providerBreakerBurstWindow = 5 * time.Second
	providerBreakerCoolOff     = 300
	providerBreakerMaxCoolOff  = 3600
	// A half_open breaker waits this long for its canary before another
	// canary may replace it, so a lost canary cannot hold the breaker forever.
	providerBreakerCanaryDeadline = 30 * time.Minute
)

// providerBreakerShouldTrip reports whether failures hold at least threshold
// distinct signals inside window. Failures within burst of the first failure
// of a signal belong to that signal.
func providerBreakerShouldTrip(failures []time.Time, now time.Time, threshold int, window, burst time.Duration) bool {
	if threshold <= 0 || window <= 0 {
		return false
	}
	cutoff := now.Add(-window)
	var inWindow []time.Time
	for _, failedAt := range failures {
		if !failedAt.Before(cutoff) {
			inWindow = append(inWindow, failedAt)
		}
	}
	sort.Slice(inWindow, func(i, j int) bool { return inWindow[i].Before(inWindow[j]) })
	signals := 0
	var signalStart time.Time
	for _, failedAt := range inWindow {
		if signals == 0 || failedAt.Sub(signalStart) >= burst {
			signals++
			signalStart = failedAt
			if signals >= threshold {
				return true
			}
		}
	}
	return false
}

// providerBreakerDisposition: an open or half_open breaker holds until its
// next_probe_at passes; after that a probe may be admitted.
func providerBreakerDisposition(state string, probeDue bool) string {
	switch state {
	case "open", "half_open":
		if probeDue {
			return "normal"
		}
		return "hold"
	case "draining":
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
	row, exists, err := providerBreakerRow(ctx, queries, workspaceID, provider)
	if err != nil {
		return "", err
	}
	if !exists {
		return "normal", nil
	}
	return providerBreakerDisposition(row.State, row.ProbeDue), nil
}

func providerBreakerVerdict(ctx context.Context, lookup RuntimeLookup, agent db.Agent) (AgentVerdict, error) {
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok || lookup.Queries == nil {
		return AgentVerdict{}, nil
	}
	row, exists, err := providerBreakerRow(ctx, lookup.Queries, agent.WorkspaceID, provider)
	if err != nil {
		return AgentVerdict{}, err
	}
	if !exists || providerBreakerDisposition(row.State, row.ProbeDue) != "hold" {
		return AgentVerdict{}, nil
	}
	return AgentVerdict{
		// Waitable, not blocked: the claim gate keeps queued work until the probe
		// is due and then lets one task through as the canary, so a trigger that
		// queues now retries itself (CHE-4408). A refusal would leave no retry.
		Availability: AgentWaitable,
		Reason:       dispatch.ReasonProviderBreaker,
		Detail:       providerBreakerNotice(provider, row.OpenedAt.Time, row.NextProbeAt.Time),
	}, nil
}

// providerBreakerNotice is the finished refusal text; RuntimeUnusableNotice passes it through.
func providerBreakerNotice(provider string, openedAt, nextProbeAt time.Time) string {
	ict := time.FixedZone("ICT", 7*60*60)
	const layout = "2006-01-02 15:04 MST"
	return fmt.Sprintf("This agent's provider (%s) is temporarily unavailable: its circuit breaker opened at %s and the next probe is at %s. Work queued for it starts after that probe, or a workspace admin can reset the breaker.",
		provider, openedAt.In(ict).Format(layout), nextProbeAt.In(ict).Format(layout))
}

// recordProviderBreakerTransition writes the audit row of one breaker transition.
// actor is the zero UUID for automatic transitions; task is the zero UUID when no task caused it.
func recordProviderBreakerTransition(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider, event, previous, next string, actor, task pgtype.UUID) error {
	if err := queries.InsertProviderBreakerAudit(ctx, db.InsertProviderBreakerAuditParams{
		WorkspaceID: workspaceID, Provider: provider, Event: event, PreviousState: previous, NewState: next, ActorID: actor, TaskID: task,
	}); err != nil {
		return fmt.Errorf("audit provider breaker %s: %w", event, err)
	}
	slog.Warn("provider breaker transition", "provider", provider, "event", event, "from", previous, "to", next,
		"actor_id", util.UUIDToString(actor), "task_id", util.UUIDToString(task))
	return nil
}

// ResetProviderBreaker force-closes a breaker (admin action) and audits who did it
// and the state it was in. found is false when the provider has no breaker row.
// Callers pass transaction-bound queries so the reset and its audit row commit together.
func ResetProviderBreaker(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider string, actor pgtype.UUID) (previous string, found bool, err error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := queries.LockProviderBreakerTrip(ctx, fmt.Sprintf("%x:%s", workspaceID.Bytes, provider)); err != nil {
		return "", false, fmt.Errorf("lock provider breaker reset: %w", err)
	}
	row, exists, err := providerBreakerRow(ctx, queries, workspaceID, provider)
	if err != nil || !exists {
		return "", false, err
	}
	if _, err := queries.ResetProviderBreaker(ctx, db.ResetProviderBreakerParams{WorkspaceID: workspaceID, Provider: provider, DefaultCoolOffSeconds: providerBreakerCoolOff}); err != nil {
		return "", false, fmt.Errorf("reset provider breaker: %w", err)
	}
	if err := recordProviderBreakerTransition(ctx, queries, workspaceID, provider, "reset", row.State, "closed", actor, pgtype.UUID{}); err != nil {
		return "", false, err
	}
	return row.State, true, nil
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
	row, exists, err := providerBreakerRow(ctx, queries, agent.WorkspaceID, provider)
	if err != nil {
		return err
	}
	if exists && isProviderBreakerCanary(row, task) {
		changed, err := queries.ReopenProviderBreaker(ctx, db.ReopenProviderBreakerParams{
			WorkspaceID: agent.WorkspaceID, Provider: provider, TaskID: task.ID, ExpectedVersion: row.Version,
			MaxCoolOffSeconds: providerBreakerMaxCoolOff,
		})
		if err != nil {
			return fmt.Errorf("reopen provider breaker: %w", err)
		}
		if changed == 1 {
			slog.Warn("provider breaker reopened after canary failure", "provider", provider, "canary_task_id", util.UUIDToString(task.ID), "prev_cool_off_s", row.CoolOffS)
			return recordProviderBreakerTransition(ctx, queries, agent.WorkspaceID, provider, "reopen", "half_open", "open", pgtype.UUID{}, task.ID)
		}
		return nil
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
	if !providerBreakerShouldTrip(failedAt, time.Now(), providerBreakerThreshold, providerBreakerWindow, providerBreakerBurstWindow) {
		return nil
	}
	changed, err := queries.OpenProviderBreaker(ctx, db.OpenProviderBreakerParams{WorkspaceID: agent.WorkspaceID, Provider: provider, TripEvidence: evidence, ExpectedVersion: row.Version, CoolOffSeconds: providerBreakerCoolOff})
	if err != nil {
		return fmt.Errorf("open provider breaker: %w", err)
	}
	if changed != 1 {
		slog.Warn("provider breaker trip skipped after concurrent version change", "provider", provider)
		return nil
	}
	if !exists || row.State == "closed" {
		slog.Warn("provider breaker opened", "provider", provider, "evidence_tasks", len(evidence))
		return recordProviderBreakerTransition(ctx, queries, agent.WorkspaceID, provider, "open", "closed", "open", pgtype.UUID{}, task.ID)
	}
	return nil
}

func recordProviderSuccess(ctx context.Context, queries *db.Queries, task db.AgentTaskQueue) error {
	if task.Status != "completed" {
		return nil
	}
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
	row, _, err := providerBreakerRow(ctx, queries, agent.WorkspaceID, provider)
	if err != nil {
		return err
	}
	if isProviderBreakerCanary(row, task) {
		changed, err := queries.CloseProviderBreakerOnCanary(ctx, db.CloseProviderBreakerOnCanaryParams{
			WorkspaceID: agent.WorkspaceID, Provider: provider, TaskID: task.ID, ExpectedVersion: row.Version,
			DefaultCoolOffSeconds: providerBreakerCoolOff,
		})
		if err != nil {
			return fmt.Errorf("close provider breaker: %w", err)
		}
		if changed == 1 {
			after, _, err := providerBreakerRow(ctx, queries, agent.WorkspaceID, provider)
			if err != nil {
				return err
			}
			slog.Info("provider breaker left half_open after canary success", "provider", provider, "to", after.State, "canary_task_id", util.UUIDToString(task.ID))
			return recordProviderBreakerTransition(ctx, queries, agent.WorkspaceID, provider, "close", "half_open", after.State, pgtype.UUID{}, task.ID)
		}
		return nil
	}
	changed, err := queries.RecordProviderSuccess(ctx, db.RecordProviderSuccessParams{WorkspaceID: agent.WorkspaceID, Provider: provider, ExpectedVersion: row.Version})
	if err != nil {
		return fmt.Errorf("record provider success: %w", err)
	}
	if changed != 1 {
		slog.Warn("provider success evidence skipped after concurrent version change", "provider", provider)
		return nil
	}
	return nil
}

// isProviderBreakerCanary: task.ID.Valid guards against the zero pgtype.UUID
// equalling the zero canary_task_id of a breaker with no canary.
func isProviderBreakerCanary(row db.GetProviderBreakerRow, task db.AgentTaskQueue) bool {
	return task.ID.Valid && row.State == "half_open" && row.CanaryTaskID == task.ID
}

// providerBreakerRow returns the breaker row; version is 0 when it does not exist.
func providerBreakerRow(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider string) (db.GetProviderBreakerRow, bool, error) {
	row, err := queries.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: workspaceID, Provider: provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetProviderBreakerRow{}, false, nil
	}
	if err != nil {
		return db.GetProviderBreakerRow{}, false, fmt.Errorf("read provider breaker: %w", err)
	}
	return row, true, nil
}

// errProviderBreakerCanaryTaken rolls back a claim that lost the canary race.
var errProviderBreakerCanaryTaken = errors.New("provider breaker canary taken")

// providerBreakerClaimGate decides a claim for agent. held: the breaker is open
// or half_open and not yet due, so the claim must be skipped. canary: the
// breaker is due, so the claimed task must be bound as its canary
// (acquireProviderBreakerCanary) in the same transaction.
func providerBreakerClaimGate(ctx context.Context, queries *db.Queries, agent db.Agent) (provider string, held, canary bool, err error) {
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok {
		return "", false, false, nil
	}
	row, exists, err := providerBreakerRow(ctx, queries, agent.WorkspaceID, provider)
	if err != nil || !exists || (row.State != "open" && row.State != "half_open") {
		return provider, false, false, err
	}
	return provider, !row.ProbeDue, row.ProbeDue, nil
}

func acquireProviderBreakerCanary(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, provider string, taskID pgtype.UUID) error {
	prior, _, err := providerBreakerRow(ctx, queries, workspaceID, provider)
	if err != nil {
		return err
	}
	changed, err := queries.AcquireProviderBreakerCanary(ctx, db.AcquireProviderBreakerCanaryParams{
		WorkspaceID: workspaceID, Provider: provider, TaskID: taskID,
		CanaryDeadlineSeconds: int32(providerBreakerCanaryDeadline.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("acquire provider breaker canary: %w", err)
	}
	if changed != 1 {
		return errProviderBreakerCanaryTaken
	}
	slog.Warn("provider breaker half_open: canary admitted", "provider", provider, "canary_task_id", util.UUIDToString(taskID))
	return recordProviderBreakerTransition(ctx, queries, workspaceID, provider, "half_open", prior.State, "half_open", pgtype.UUID{}, taskID)
}
