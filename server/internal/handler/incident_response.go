package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/incident"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// incidentSettings is settings.incident_response (CHE-9892): mode off | shadow | act, the
// project of the incident issues and the member who is told once per incident.
type incidentSettings struct {
	Incident struct {
		Mode         string `json:"mode"`
		ProjectID    string `json:"project_id"`
		NotifyUserID string `json:"notify_user_id"`
	} `json:"incident_response"`
}

// SweepIncidents is the scheduler entry point: per workspace it reads the failed runs of the
// last incident.Window, plans open, comment and close steps, and applies them in act mode.
func (handler *Handler) SweepIncidents(ctx context.Context) error {
	workspaces, err := handler.Queries.ListIncidentWorkspaces(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, workspace := range workspaces {
		var settings incidentSettings
		if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
			failures = errors.Join(failures, fmt.Errorf("incident settings %s: %w", uuidToString(workspace.ID), err))
			continue
		}
		failures = errors.Join(failures, handler.sweepIncidentWorkspace(ctx, workspace.ID, settings, time.Now()))
	}
	return failures
}

func (handler *Handler) sweepIncidentWorkspace(ctx context.Context, workspaceID pgtype.UUID, settings incidentSettings, now time.Time) error {
	rows, err := handler.Queries.ListIncidentFailedRuns(ctx, db.ListIncidentFailedRunsParams{WorkspaceID: workspaceID, WindowSeconds: incident.Window.Seconds()})
	if err != nil {
		return err
	}
	runs := make([]incident.FailedRun, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, incident.FailedRun{ID: uuidToString(row.ID), IssueID: uuidToString(row.IssueID), AgentID: uuidToString(row.AgentID), At: row.CompletedAt.Time, Error: row.Error.String})
	}
	openRows, err := handler.Queries.ListOpenIncidentIssues(ctx, workspaceID)
	if err != nil {
		return err
	}
	open := map[string]incident.Open{}
	issues := map[string]pgtype.UUID{}
	for _, row := range openRows {
		if _, dup := open[row.Class]; dup {
			continue // the oldest open issue of a class is the incident
		}
		last, _ := time.Parse(time.RFC3339Nano, row.LastFailureAt)
		open[row.Class], issues[row.Class] = incident.Open{LastFailure: last}, row.ID
	}
	var failures error
	for _, step := range incident.Plan(runs, open, now) {
		slog.Info("incident_response step", "mode", settings.Incident.Mode, "workspace_id", uuidToString(workspaceID), "kind", step.Kind, "class", step.Class)
		if settings.Incident.Mode != "act" {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		var err error
		switch step.Kind {
		case incident.StepOpen:
			err = handler.openIncident(ctx, workspaceID, settings, step)
		case incident.StepComment:
			err = handler.commentIncident(ctx, workspaceID, issues[step.Class], step)
		case incident.StepClose:
			err = handler.closeIncident(ctx, workspaceID, issues[step.Class], step)
		}
		failures = errors.Join(failures, err)
	}
	return failures
}

// openIncident creates the urgent incident issue and sends the one DM in a single transaction,
// so a failed DM leaves no issue behind and the next sweep retries both.
func (handler *Handler) openIncident(ctx context.Context, workspaceID pgtype.UUID, settings incidentSettings, step incident.Step) error {
	cfg := settings.Incident
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	issueID, err := ciWatchDest{h: handler}.CreateIssue(ctx, tx, ciwatch.IssueSpec{
		WorkspaceID: uuidToString(workspaceID), Title: incident.Title(step.Class), Body: incident.Body(step.Incident), Priority: "urgent",
		CreatorID: cfg.NotifyUserID,
		Metadata:  map[string]any{"incident_class": step.Class, "incident_last_failure_at": step.LastFailure.UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		return err
	}
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return err
	}
	if project, err := util.ParseUUID(cfg.ProjectID); err == nil {
		if _, err := tx.Exec(ctx, `UPDATE issue SET project_id = $2 WHERE id = $1`, id, project); err != nil {
			return err
		}
	}
	q := handler.Queries.WithTx(tx)
	issue, err := q.GetIssue(ctx, id)
	if err != nil {
		return err
	}
	notifyUser, err := util.ParseUUID(cfg.NotifyUserID)
	if err != nil {
		return fmt.Errorf("incident_response.notify_user_id: %w", err)
	}
	var message *db.ChatMessage
	var session db.GetIncidentDMSessionRow
	switch session, err = q.GetIncidentDMSession(ctx, db.GetIncidentDMSessionParams{WorkspaceID: workspaceID, CreatorID: notifyUser}); {
	case errors.Is(err, pgx.ErrNoRows):
		slog.Warn("incident_response: notify user has no DM to receive the message", "workspace_id", uuidToString(workspaceID))
	case err != nil:
		return err
	default:
		link := fmt.Sprintf("[%s-%d](mention://issue/%s)", handler.getIssuePrefix(ctx, workspaceID), issue.Number, issueID)
		created, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{
			ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: incident.DMText(step.Incident, link),
			SentVia: pgtype.Text{String: "system", Valid: true},
		})
		if err != nil {
			return fmt.Errorf("incident DM: %w", err)
		}
		if err := q.TouchChatSession(ctx, session.ID); err != nil {
			return err
		}
		message = &created
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	pctx := context.WithoutCancel(ctx)
	handler.publish(protocol.EventIssueCreated, uuidToString(workspaceID), "member", cfg.NotifyUserID, map[string]any{"issue": issueToResponse(issue, handler.getIssuePrefix(pctx, workspaceID))})
	if message != nil {
		sessionID := uuidToString(session.ID)
		handler.publishChatFor(protocol.EventChatMessage, uuidToString(workspaceID), "agent", uuidToString(session.AgentID), cfg.NotifyUserID, sessionID, protocol.ChatMessagePayload{
			MessageKind: message.MessageKind, ChatSessionID: sessionID, MessageID: uuidToString(message.ID),
			Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt),
		})
	}
	return nil
}

// commentIncident adds the newer failures to the open incident, not a new issue.
func (handler *Handler) commentIncident(ctx context.Context, workspaceID, issueID pgtype.UUID, step incident.Step) error {
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	created, err := handler.Queries.WithTx(tx).CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: incident.Body(step.Incident), Type: "system",
	})
	if err != nil {
		return err
	}
	if err := writeIssueMetadata(ctx, tx, issueID, map[string]any{"incident_last_failure_at": step.LastFailure.UTC().Format(time.RFC3339Nano)}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	handler.publishIdleGuardComment(context.WithoutCancel(ctx), workspaceID, issueID, created.Comment())
	return nil
}

// closeIncident moves the issue to done, then says what was retried. A failed move is retried by the next sweep.
func (handler *Handler) closeIncident(ctx context.Context, workspaceID, issueID pgtype.UUID, step incident.Step) error {
	issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return err
	}
	moved, _, err := handler.tryMoveIssueOnEdge(ctx, issue, "done", "incident_response", map[string]any{"class": step.Class}, false)
	if err != nil || !moved {
		return err
	}
	created, err := handler.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: incident.CloseBody(step.Class, step.LastFailure), Type: "system",
	})
	if err != nil {
		return err
	}
	handler.publishIdleGuardComment(context.WithoutCancel(ctx), workspaceID, issueID, created.Comment())
	return nil
}
