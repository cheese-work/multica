package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// weeklyRunGuardAction is the activity row the brief reads.
const weeklyRunGuardAction = "weekly_run_guard_tripped"

// ictLocation is the day boundary of the incident claim.
var ictLocation = time.FixedZone("ICT", 7*60*60)

// onWeeklyRunGuardTripped records one incident per issue and ICT day (CHE-5720):
// an activity row with the run count and the status of the last three runs.
// The refusal itself changes no status and writes no comment; the brief carries
// the incident.
func (h *Handler) onWeeklyRunGuardTripped(ctx context.Context, e service.WeeklyRunGuardTripped) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("weekly run guard: load issue failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return
	}
	details, err := json.Marshal(map[string]any{"runs": e.Runs, "outcomes": e.Outcomes})
	if err != nil {
		return
	}
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		qtx := h.Queries.WithTx(tx)
		_, err := qtx.ClaimWeeklyRunGuardIncident(ctx, db.ClaimWeeklyRunGuardIncidentParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			WindowID: time.Now().In(ictLocation).Format("2006-01-02"),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim incident: %w", err)
		}
		_, err = qtx.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
			Action: weeklyRunGuardAction, Details: details,
		})
		return err
	})
	if err != nil {
		slog.Warn("weekly run guard: write incident failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

// weeklyRunGuardBrief is the guard's incident line for the brief: each issue it
// stopped since a time, with its run count and last three run outcomes. It is
// empty when the guard stopped nothing.
func (h *Handler) weeklyRunGuardBrief(ctx context.Context, workspaceID pgtype.UUID, since time.Time) (string, error) {
	rows, err := h.Queries.ListWeeklyRunGuardIncidentsSince(ctx, db.ListWeeklyRunGuardIncidentsSinceParams{
		WorkspaceID: workspaceID, Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil || len(rows) == 0 {
		return "", err
	}
	prefix := h.getIssuePrefix(ctx, workspaceID)
	items := make([]string, 0, len(rows))
	for _, r := range rows {
		var d struct {
			Runs     int64    `json:"runs"`
			Outcomes []string `json:"outcomes"`
		}
		if err := json.Unmarshal(r.Details, &d); err != nil {
			continue
		}
		items = append(items, fmt.Sprintf("%s-%d %d runs in 7 days, last 3: %s", prefix, r.Number, d.Runs, strings.Join(d.Outcomes, ", ")))
	}
	if len(items) == 0 {
		return "", nil
	}
	return "Run guard stopped runs on: " + strings.Join(items, "; ") + ".", nil
}
