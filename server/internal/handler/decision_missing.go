package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) processMissingDecisionRequests(ctx context.Context) error {
	if err := handler.Queries.ReconcileDecisionMissingWaits(ctx); err != nil {
		return err
	}
	if err := handler.Queries.SeedDecisionMissingWaits(ctx); err != nil {
		return err
	}
	waits, err := handler.Queries.ListDecisionMissingAlerts(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, wait := range waits {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.deliverMissingDecisionAlert(ctx, wait.WorkspaceID, wait.IssueID)
		}))
	}
	return failures
}

func (handler *Handler) deliverMissingDecisionAlert(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := handler.Queries.WithTx(tx)
	// Match transition-trigger lock order: issue first, episode second.
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: issueID}); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	wait, err := queries.LockDecisionMissingWait(ctx, db.LockDecisionMissingWaitParams{WorkspaceID: workspaceID, IssueID: issueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return err
	}
	squads, err := queries.DecisionMissingOwningSquads(ctx, db.DecisionMissingOwningSquadsParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return err
	}
	var squad db.Squad
	if len(squads) == 1 {
		squad = squads[0]
	}
	var leader db.Agent
	if squad.ID.Valid {
		leader, err = queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: workspaceID, ID: squad.LeaderID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if leader.ArchivedAt.Valid || !leader.RuntimeID.Valid {
			leader = db.Agent{}
		}
	}
	originator := issue.RequestedByID
	if !originator.Valid && issue.CreatorType == "member" {
		originator = issue.CreatorID
	}
	canRun := leader.ID.Valid && handler.canInvokeAgent(ctx, leader, "system", "", uuidToString(originator), uuidToString(workspaceID))
	content := "Decision request missing: this issue has waited for a human for more than 10 minutes without an open decision request. Open the decision request before continuing the human wait."
	if !leader.ID.Valid {
		content += " The owning squad is unresolved: no unique active squad with a runnable leader. Repair ownership; the guard will retry delivery."
	} else {
		content += fmt.Sprintf(" Owning squad: %s; leader: %s.", uuidToString(squad.ID), uuidToString(leader.ID))
		if !canRun {
			content += " Automatic continuation withheld: the requester cannot invoke the leader. The leader receives an inbox alert only."
		}
	}
	var posted *db.Comment
	if !wait.CommentID.Valid {
		comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "comment", Content: content})
		if err != nil {
			return err
		}
		stored := comment.Comment()
		wait.CommentID, posted = comment.ID, &stored
	}
	var item *db.InboxItem
	var task *db.AgentTaskQueue
	if leader.ID.Valid {
		if canRun {
			queued, err := handler.TaskService.EnqueueSquadAlertInTx(ctx, queries, issue, leader.ID, squad.ID, wait.CommentID, originator, content)
			if err != nil {
				return err
			}
			task = &queued
		}
		details, err := json.Marshal(map[string]any{"kind": "decision_request_missing", "started_at": wait.StartedAt, "comment_id": wait.CommentID, "squad_id": squad.ID})
		if err != nil {
			return err
		}
		created, err := queries.CreateInboxItem(ctx, db.CreateInboxItemParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID,
			RecipientType: "agent", RecipientID: leader.ID, Type: "mentioned", Severity: "attention", Title: "Decision request missing",
			Body: pgtype.Text{String: content, Valid: true}, Details: details})
		if err != nil {
			return err
		}
		item = &created
	}
	if err := queries.StampDecisionMissingWait(ctx, db.StampDecisionMissingWaitParams{WorkspaceID: workspaceID, IssueID: issueID, CommentID: wait.CommentID, Delivered: item != nil}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if posted != nil {
		handler.publish(protocol.EventCommentCreated, uuidToString(workspaceID), "system", "", map[string]any{"comment": commentToResponse(*posted, nil, nil)})
	}
	if item != nil {
		handler.publish(protocol.EventInboxNew, uuidToString(workspaceID), "system", "", map[string]any{"item": inboxToResponse(*item)})
	}
	if task != nil {
		handler.TaskService.PublishTaskEnqueued(ctx, *task)
	}
	return nil
}

func (handler *Handler) ListMissingDecisions(writer http.ResponseWriter, request *http.Request) {
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return
	}
	workspace, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return
	}
	var project pgtype.UUID
	if value := request.URL.Query().Get("project_id"); value != "" {
		project, ok = parseUUIDOrBadRequest(writer, value, "project_id")
		if !ok {
			return
		}
	}
	waits, err := handler.Queries.ListMissingDecisionRequests(request.Context(), db.ListMissingDecisionRequestsParams{WorkspaceID: workspace, ProjectID: project})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list missing decision requests")
		return
	}
	rows := make([]map[string]any, 0, len(waits))
	for _, wait := range waits {
		if _, ok := handler.loadIssueForUser(writer, request, uuidToString(wait.IssueID)); !ok {
			return
		}
		squads, err := handler.Queries.DecisionMissingOwningSquads(request.Context(), db.DecisionMissingOwningSquadsParams{WorkspaceID: workspace, ID: wait.IssueID})
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to resolve decision wait ownership")
			return
		}
		ownership := "unresolved"
		var squadID, leaderID pgtype.UUID
		if len(squads) == 1 {
			leader, err := handler.Queries.GetAgentInWorkspace(request.Context(), db.GetAgentInWorkspaceParams{WorkspaceID: workspace, ID: squads[0].LeaderID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				writeError(writer, http.StatusInternalServerError, "failed to resolve decision wait leader")
				return
			}
			if leader.ID.Valid && !leader.ArchivedAt.Valid && leader.RuntimeID.Valid {
				ownership, squadID, leaderID = "squad", squads[0].ID, leader.ID
			}
		}
		rows = append(rows, map[string]any{"issue_id": wait.IssueID, "workspace_id": wait.WorkspaceID, "project_id": wait.ProjectID,
			"started_at": wait.StartedAt, "alerted_at": wait.AlertedAt, "ownership": ownership, "squad_id": squadID, "leader_id": leaderID})
	}
	writeJSON(writer, http.StatusOK, rows)
}
