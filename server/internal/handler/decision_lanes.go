package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type decisionLaneResponse struct {
	ID                   string  `json:"id"`
	Title                string  `json:"title"`
	LaneSummary          string  `json:"lane_summary"`
	LaneSummaryUpdatedAt *string `json:"lane_summary_updated_at"`
	UpdatedAt            string  `json:"updated_at"`
}

func (handler *Handler) decisionLaneWindow(request *http.Request, queries *db.Queries, issue db.Issue, targetID pgtype.UUID) (db.Agent, pgtype.UUID, error) {
	ctx := request.Context()
	deny := decisionIntentRefusal{http.StatusForbidden, "lane access requires this issue's running task and an open window naming an unconsumed person"}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		return db.Agent{}, pgtype.UUID{}, deny
	}
	agentID, err := util.ParseUUID(request.Header.Get("X-Agent-ID"))
	if err != nil {
		return db.Agent{}, pgtype.UUID{}, deny
	}
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: agentID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return agent, targetID, deny
	}
	if err != nil {
		return agent, targetID, err
	}
	task, err := queries.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (task.Status != "running" || task.ChatSessionID.Valid || task.IssueID != issue.ID || task.AgentID != agent.ID)) {
		return agent, targetID, deny
	}
	if err != nil {
		return agent, targetID, err
	}
	issue, err = queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return agent, targetID, deny
	}
	if err != nil {
		return agent, targetID, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return agent, targetID, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return agent, targetID, deny
	}
	assigned := issue.AssigneeType.String == "agent" && issue.AssigneeID == agent.ID
	if issue.AssigneeType.String == "squad" && issue.AssigneeID.Valid {
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && squad.ArchivedAt.Valid) {
			return agent, targetID, deny
		}
		if err != nil {
			return agent, targetID, err
		}
		assigned, err = queries.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agent.ID})
		if err != nil {
			return agent, targetID, err
		}
	}
	if !assigned {
		return agent, targetID, deny
	}
	intent, err := queries.GetDecisionIntentForTask(ctx, db.GetDecisionIntentForTaskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: task.ID, AgentID: agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return agent, targetID, deny
	}
	if err != nil {
		return agent, targetID, err
	}
	targets, err := queries.ListDecisionIntentTargets(ctx, db.ListDecisionIntentTargetsParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID})
	if err != nil {
		return agent, targetID, err
	}
	if !targetID.Valid {
		if len(targets) != 1 {
			return agent, targetID, deny
		}
		targetID = targets[0].HumanUserID
	}
	unconsumed := false
	for _, target := range targets {
		if target.HumanUserID == targetID && !target.ConsumedAt.Valid {
			unconsumed = true
		}
	}
	if !unconsumed {
		return agent, targetID, deny
	}
	if _, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: targetID, WorkspaceID: issue.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agent, targetID, deny
		}
		return agent, targetID, err
	}
	if !handler.canInvokeAgent(ctx, agent, "member", uuidToString(targetID), "", uuidToString(issue.WorkspaceID)) {
		return agent, targetID, deny
	}
	open, err := queries.DecisionLaneWindowOpen(ctx, db.DecisionLaneWindowOpenParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID, HumanUserID: targetID})
	if err == nil && !open {
		return agent, targetID, deny
	}
	return agent, targetID, err
}

func (handler *Handler) listDecisionLanes(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "lane access requires an issue task token")
		return
	}
	issue, ok := handler.loadIssueForUser(writer, request, request.URL.Query().Get("issue"))
	if !ok {
		return
	}
	targetID := pgtype.UUID{}
	if request.URL.Query().Has("for") {
		var err error
		targetID, err = util.ParseUUID(request.URL.Query().Get("for"))
		if err != nil {
			writeError(writer, http.StatusForbidden, "lane target must name a person in the window")
			return
		}
	}
	requestedTarget := targetID
	agent, targetID, err := handler.decisionLaneWindow(request, handler.Queries, issue, requestedTarget)
	if err == nil {
		var rows []db.ListDecisionLanesRow
		rows, err = handler.Queries.ListDecisionLanes(request.Context(), db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, CreatorID: targetID})
		if err == nil && len(rows) == 0 {
			err = handler.createDecisionDefaultLane(request, issue, requestedTarget, targetID)
			if err == nil {
				rows, err = handler.Queries.ListDecisionLanes(request.Context(), db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, CreatorID: targetID})
			}
		}
		if err == nil {
			var currentTarget pgtype.UUID
			_, currentTarget, err = handler.decisionLaneWindow(request, handler.Queries, issue, requestedTarget)
			if err == nil && currentTarget != targetID {
				err = decisionIntentRefusal{http.StatusForbidden, "lane window target changed"}
			}
		}
		if err == nil {
			response := make([]decisionLaneResponse, 0, len(rows))
			for _, row := range rows {
				response = append(response, decisionLaneResponse{ID: uuidToString(row.ID), Title: row.Title, LaneSummary: row.LaneSummary, LaneSummaryUpdatedAt: timestampToPtr(row.LaneSummaryUpdatedAt), UpdatedAt: timestampToString(row.UpdatedAt)})
			}
			writeJSON(writer, http.StatusOK, response)
			return
		}
	}
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		writeError(writer, refusal.status, refusal.message)
	} else {
		writeError(writer, http.StatusInternalServerError, "failed to list decision lanes")
	}
}

func (handler *Handler) createDecisionDefaultLane(request *http.Request, issue db.Issue, requestedTarget, expectedTarget pgtype.UUID) error {
	ctx := request.Context()
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		return err
	}
	agent, targetID, err := handler.decisionLaneWindow(request, queries, issue, requestedTarget)
	if err != nil {
		return err
	}
	if targetID != expectedTarget {
		return decisionIntentRefusal{http.StatusForbidden, "lane window target changed"}
	}
	lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, CreatorID: targetID})
	if err != nil {
		return err
	}
	if len(lanes) == 0 {
		session, err := queries.CreateChatSession(ctx, db.CreateChatSessionParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, CreatorID: targetID, Title: "Decisions: " + agent.Name})
		if err != nil {
			return err
		}
		if _, err := queries.MarkChatSessionExplicitlyCreated(ctx, session.ID); err != nil {
			return err
		}
	}
	_, currentTarget, err := handler.decisionLaneWindow(request, queries, issue, requestedTarget)
	if err != nil {
		return err
	}
	if currentTarget != expectedTarget {
		return decisionIntentRefusal{http.StatusForbidden, "lane window target changed"}
	}
	return transaction.Commit(ctx)
}
