package handler

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type decisionLaneResponse = protocol.DecisionLane

var decisionSnippetURL = regexp.MustCompile(`mention://[^\s)\]]+|/issue/[0-9a-fA-F-]+`)

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
		var topic decisionTopic
		if topic, err = resolveDecisionTopic(request.Context(), handler.Queries, issue); err == nil {
			rows, err = handler.Queries.ListDecisionLanes(request.Context(), db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, TopicID: topic.ID, CreatorID: targetID, IssueID: issue.ID})
		}
		response := make([]decisionLaneResponse, 0, len(rows))
		if err == nil {
			sessionIDs := make([]pgtype.UUID, 0, len(rows))
			for _, row := range rows {
				sessionIDs = append(sessionIDs, row.ID)
			}
			var mentions []db.ListDecisionLaneMentionsRow
			if len(sessionIDs) > 0 {
				mentions, err = handler.Queries.ListDecisionLaneMentions(request.Context(), db.ListDecisionLaneMentionsParams{WorkspaceID: issue.WorkspaceID, AgentID: agent.ID, CreatorID: targetID, SessionIds: sessionIDs})
			}
			bySession := make(map[pgtype.UUID][]protocol.DecisionIssueMention)
			now := time.Now()
			for _, mention := range mentions {
				snippet := decisionSnippetURL.ReplaceAllString(mention.Snippet, "")
				snippet = strings.ReplaceAll(strings.ReplaceAll(snippet, mention.IssueKey, ""), mention.IssueTitle, "")
				bySession[mention.ChatSessionID] = append(bySession[mention.ChatSessionID], protocol.DecisionIssueMention{IssueID: uuidToString(mention.IssueID), IssueKey: mention.IssueKey, IssueTitle: mention.IssueTitle, MentionedAt: timestampToString(mention.MentionedAt), RelativeTime: decisionMentionAge(now.Sub(mention.MentionedAt.Time)), Snippet: strings.Join(strings.Fields(snippet), " ")})
			}
			for _, row := range rows {
				issueMentions := bySession[row.ID]
				if issueMentions == nil {
					issueMentions = []protocol.DecisionIssueMention{}
				}
				lane := decisionLaneResponse{ID: uuidToString(row.ID), Title: row.Title, UpdatedAt: timestampToString(row.UpdatedAt), IssueMentions: issueMentions}
				if row.ReferencedAt.Valid {
					lane.ReferencedAt = row.ReferencedAt.Time.UTC().Format(time.RFC3339Nano)
				}
				lane.Eligible = row.ReferencedAt.Valid || row.DecisionTopicID == topic.ID
				response = append(response, lane)
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

func decisionMentionAge(age time.Duration) string {
	if age < time.Minute {
		return "just now"
	}
	for _, unit := range []struct {
		duration time.Duration
		label    string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if age >= unit.duration {
			return fmt.Sprintf("%d%s ago", age/unit.duration, unit.label)
		}
	}
	return "just now"
}
