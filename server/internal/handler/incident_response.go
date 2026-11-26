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

// validateIncidentResponseSetting rejects a settings.incident_response that the sweep could not use:
// an unknown mode, a malformed ID, or act mode with nobody to tell.
func validateIncidentResponseSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config["incident_response"]
	if !present {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"incident_response": v})
	if err != nil {
		return err
	}
	var settings incidentSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("incident_response: %w", err)
	}
	cfg := settings.Incident
	switch cfg.Mode {
	case "off", "shadow", "act":
	default:
		return fmt.Errorf("incident_response.mode must be off, shadow or act, got %q", cfg.Mode)
	}
	if cfg.ProjectID != "" {
		if _, err := util.ParseUUID(cfg.ProjectID); err != nil {
			return fmt.Errorf("incident_response.project_id: %w", err)
		}
	}
	if cfg.Mode == "act" || cfg.NotifyUserID != "" {
		if _, err := util.ParseUUID(cfg.NotifyUserID); err != nil {
			return fmt.Errorf("incident_response.notify_user_id is required in act mode: %w", err)
		}
	}
	return nil
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
	if settings.Incident.Mode == "act" {
		failures = handler.deliverPendingIncidentDMs(ctx, workspaceID, settings)
	}
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
	link := fmt.Sprintf("[%s-%d](mention://issue/%s)", handler.getIssuePrefix(ctx, workspaceID), issue.Number, issueID)
	text := incident.DMText(step.Incident, link)
	message, session, err := sendIncidentDM(ctx, q, workspaceID, notifyUser, text)
	if err != nil {
		return err
	}
	if message == nil {
		// No active DM yet: keep the text on the incident so each sweep retries until it is delivered.
		if err := writeIssueMetadata(ctx, tx, id, map[string]any{incidentDMTextKey: text}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	pctx := context.WithoutCancel(ctx)
	handler.publish(protocol.EventIssueCreated, uuidToString(workspaceID), "member", cfg.NotifyUserID, map[string]any{"issue": issueToResponse(issue, handler.getIssuePrefix(pctx, workspaceID))})
	handler.publishIncidentDM(workspaceID, cfg.NotifyUserID, message, session)
	return nil
}

const incidentDMTextKey = "incident_dm_text"

// sendIncidentDM writes the DM into the member's newest active DM. It returns a nil message when the member has none.
func sendIncidentDM(ctx context.Context, q *db.Queries, workspaceID, notifyUser pgtype.UUID, text string) (*db.ChatMessage, db.GetIncidentDMSessionRow, error) {
	session, err := q.GetIncidentDMSession(ctx, db.GetIncidentDMSessionParams{WorkspaceID: workspaceID, CreatorID: notifyUser})
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("incident_response: notify user has no DM yet, the message is held for the next sweep", "workspace_id", uuidToString(workspaceID))
		return nil, session, nil
	}
	if err != nil {
		return nil, session, err
	}
	created, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: text,
		SentVia: pgtype.Text{String: "system", Valid: true},
	})
	if err != nil {
		return nil, session, fmt.Errorf("incident DM: %w", err)
	}
	if err := q.TouchChatSession(ctx, session.ID); err != nil {
		return nil, session, err
	}
	return &created, session, nil
}

func (handler *Handler) publishIncidentDM(workspaceID pgtype.UUID, notifyUserID string, message *db.ChatMessage, session db.GetIncidentDMSessionRow) {
	if message == nil {
		return
	}
	sessionID := uuidToString(session.ID)
	handler.publishChatFor(protocol.EventChatMessage, uuidToString(workspaceID), "agent", uuidToString(session.AgentID), notifyUserID, sessionID, protocol.ChatMessagePayload{
		MessageKind: message.MessageKind, ChatSessionID: sessionID, MessageID: uuidToString(message.ID),
		Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt),
	})
}

// deliverPendingIncidentDMs sends the held DM of each incident once the member has an active DM,
// and clears the held text in the same transaction so a DM is never sent twice.
func (handler *Handler) deliverPendingIncidentDMs(ctx context.Context, workspaceID pgtype.UUID, settings incidentSettings) error {
	notifyUser, err := util.ParseUUID(settings.Incident.NotifyUserID)
	if err != nil {
		return fmt.Errorf("incident_response.notify_user_id: %w", err)
	}
	pending, err := handler.Queries.ListIncidentPendingDMs(ctx, workspaceID)
	if err != nil {
		return err
	}
	var failures error
	for _, row := range pending {
		failures = errors.Join(failures, handler.deliverPendingIncidentDM(ctx, workspaceID, notifyUser, settings.Incident.NotifyUserID, row))
	}
	return failures
}

func (handler *Handler) deliverPendingIncidentDM(ctx context.Context, workspaceID, notifyUser pgtype.UUID, notifyUserID string, row db.ListIncidentPendingDMsRow) error {
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	message, session, err := sendIncidentDM(ctx, handler.Queries.WithTx(tx), workspaceID, notifyUser, row.DmText)
	if err != nil || message == nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET metadata = metadata - $2::text WHERE id = $1`, row.ID, incidentDMTextKey); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	handler.publishIncidentDM(workspaceID, notifyUserID, message, session)
	return nil
}

// commentIncident adds// commentIncident adds the newer failures to the open incident, not a new issue.
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
