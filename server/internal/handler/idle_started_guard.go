package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	idleGuardDefaultHours = 4
	idleGuardMaxHours     = 72
)

var idleGuardDefaultStatuses = []string{"in_progress", "agent_review"}

type idleGuardSettings struct {
	Guard struct {
		Mode       string   `json:"mode"`
		AfterHours float64  `json:"after_hours"`
		Statuses   []string `json:"statuses"`
	} `json:"idle_started_guard"`
}

func (settings idleGuardSettings) threshold() time.Duration {
	hours := settings.Guard.AfterHours
	if hours < 1 || hours > idleGuardMaxHours {
		hours = idleGuardDefaultHours
	}
	return time.Duration(hours * float64(time.Hour))
}

func (settings idleGuardSettings) statuses() []string {
	if len(settings.Guard.Statuses) == 0 {
		return idleGuardDefaultStatuses
	}
	return settings.Guard.Statuses
}

// idleGuardFinding is one issue the guard found idle. Shadow mode only reports
// it; act mode moves it.
type idleGuardFinding struct {
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	Status      string
	IdleSince   time.Time
	settings    idleGuardSettings
}

// SweepIdleStartedIssues is the scheduler entry point. Shadow workspaces log
// what would move; act workspaces settle earlier episodes, then move.
func (handler *Handler) SweepIdleStartedIssues(ctx context.Context) error {
	failures := handler.settleIdleGuardEpisodes(ctx)
	findings, err := handler.checkIdleStartedIssues(ctx)
	failures = errors.Join(failures, err)
	for _, finding := range findings {
		if finding.settings.Guard.Mode != "act" {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		failures = errors.Join(failures, handler.actOnIdleFinding(ctx, finding.settings, finding))
	}
	return failures
}

func (handler *Handler) checkIdleStartedIssues(ctx context.Context) ([]idleGuardFinding, error) {
	workspaces, err := handler.Queries.ListIdleStartedGuardWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var findings []idleGuardFinding
	var failures error
	for _, workspace := range workspaces {
		var settings idleGuardSettings
		if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
			failures = errors.Join(failures, fmt.Errorf("idle started guard settings %s: %w", uuidToString(workspace.ID), err))
			continue
		}
		threshold := settings.threshold()
		issues, err := handler.Queries.ListIdleStartedGuardIssues(ctx, db.ListIdleStartedGuardIssuesParams{
			WorkspaceID: workspace.ID, Statuses: settings.statuses(), AfterSeconds: threshold.Seconds(),
		})
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		for _, issue := range issues {
			finding := idleGuardFinding{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Status: issue.Status, IdleSince: issue.IdleSince.Time, settings: settings}
			findings = append(findings, finding)
			slog.Info("idle_started_guard would_move", "mode", settings.Guard.Mode, "workspace_id", uuidToString(finding.WorkspaceID),
				"issue_id", uuidToString(finding.IssueID), "status", finding.Status, "idle_since", finding.IdleSince.UTC().Format(time.RFC3339), "threshold", threshold)
		}
	}
	return findings, failures
}
