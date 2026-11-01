package handler

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type decisionIntentRefusal struct {
	status  int
	message string
}

func (refusal decisionIntentRefusal) Error() string { return refusal.message }

func writeDecisionIntentError(writer http.ResponseWriter, err error) {
	writeDecisionError(writer, err, "decision intent")
}

func writeDecisionError(writer http.ResponseWriter, err error, operation string) {
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		writeError(writer, refusal.status, refusal.message)
	} else if decisiontx.IsRetryable(err) {
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, operation+" is busy; retry")
	} else {
		writeError(writer, http.StatusInternalServerError, "failed to create "+operation)
	}
}

// authorizeDecisionTask locks and re-reads the issue, then proves the caller is
// authorID's running, non-chat task on this open issue. Issue assignment
// does not grant or revoke a task's decision authority.
func (handler *Handler) authorizeDecisionTask(request *http.Request, queries *db.Queries, issue db.Issue, authorType string, authorID pgtype.UUID) (db.Issue, db.Agent, db.AgentTaskQueue, error) {
	ctx := request.Context()
	deny := decisionIntentRefusal{http.StatusForbidden, "decision intent requires a running non-chat issue task on this issue"}
	if request.Header.Get("X-Actor-Source") != "task_token" || authorType != "agent" {
		return issue, db.Agent{}, db.AgentTaskQueue{}, deny
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return issue, db.Agent{}, db.AgentTaskQueue{}, decisionIntentRefusal{http.StatusNotFound, "issue not found"}
		}
		return issue, db.Agent{}, db.AgentTaskQueue{}, err
	}
	issue, err := queries.GetIssue(ctx, issue.ID)
	if err != nil {
		return issue, db.Agent{}, db.AgentTaskQueue{}, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return issue, db.Agent{}, db.AgentTaskQueue{}, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return issue, db.Agent{}, db.AgentTaskQueue{}, deny
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		return issue, db.Agent{}, db.AgentTaskQueue{}, deny
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: taskID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return issue, db.Agent{}, db.AgentTaskQueue{}, deny
		}
		return issue, db.Agent{}, db.AgentTaskQueue{}, err
	}
	task, err := queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return issue, db.Agent{}, db.AgentTaskQueue{}, err
	}
	if task.Status != "running" || task.ChatSessionID.Valid || task.IssueID != issue.ID || task.AgentID != authorID {
		return issue, db.Agent{}, db.AgentTaskQueue{}, deny
	}

	agent, err := queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return issue, agent, task, err
	}
	return issue, agent, task, nil
}

// checkDecisionTarget refuses a person who is not an active member who may
// invoke the asking agent.
func (handler *Handler) checkDecisionTarget(request *http.Request, queries *db.Queries, issue db.Issue, agent db.Agent, humanID pgtype.UUID) error {
	ctx := request.Context()
	if _, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: humanID, WorkspaceID: issue.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return decisionIntentRefusal{http.StatusBadRequest, "named person is not an active workspace member: " + uuidToString(humanID)}
		}
		return err
	}
	if !handler.canInvokeAgent(ctx, agent, "member", uuidToString(humanID), "", uuidToString(issue.WorkspaceID)) {
		return decisionIntentRefusal{http.StatusBadRequest, "named person cannot invoke the asking agent: " + uuidToString(humanID)}
	}
	return nil
}

// supersedeDecisionIntent closes the task's still-pending intent, if any.
func supersedeDecisionIntent(ctx context.Context, queries *db.Queries, issue db.Issue, taskID pgtype.UUID) error {
	previous, err := queries.LockDecisionPendingIntent(ctx, db.LockDecisionPendingIntentParams{WorkspaceID: issue.WorkspaceID, TaskID: taskID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return queries.CloseDecisionIntent(ctx, db.CloseDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: previous.ID, CloseReason: pgtype.Text{String: "superseded", Valid: true}})
}

func (handler *Handler) prepareDecisionIntent(request *http.Request, queries *db.Queries, issue db.Issue, authorType string, authorID pgtype.UUID, content string) (db.Issue, []pgtype.UUID, error) {
	issue, agent, task, err := handler.authorizeDecisionTask(request, queries, issue, authorType, authorID)
	if err != nil {
		return issue, nil, err
	}
	targets := make([]pgtype.UUID, 0)
	seen := make(map[pgtype.UUID]bool)
	for _, mention := range util.ParseMentions(content) {
		if mention.Type != "member" {
			continue
		}
		humanID, err := util.ParseUUID(mention.ID)
		if err != nil {
			return issue, nil, decisionIntentRefusal{http.StatusBadRequest, "invalid named person " + mention.ID}
		}
		if seen[humanID] {
			continue
		}
		if err := handler.checkDecisionTarget(request, queries, issue, agent, humanID); err != nil {
			return issue, nil, err
		}
		if err := requireDecisionAuthority(request.Context(), queries, issue, humanID, "reversible", http.StatusBadRequest); err != nil {
			return issue, nil, err
		}
		seen[humanID] = true
		targets = append(targets, humanID)
	}
	if len(targets) == 0 {
		return issue, nil, decisionIntentRefusal{http.StatusBadRequest, "decision intent must name at least one person with a member mention"}
	}
	sort.Slice(targets, func(first, second int) bool { return uuidToString(targets[first]) < uuidToString(targets[second]) })
	if err := supersedeDecisionIntent(request.Context(), queries, issue, task.ID); err != nil {
		return issue, nil, err
	}
	return issue, targets, nil
}

func createCommentDecisionIntent(request *http.Request, queries *db.Queries, comment db.Comment, targets []pgtype.UUID) error {
	ctx := request.Context()
	intent, err := queries.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: comment.WorkspaceID, IssueID: comment.IssueID, TaskID: comment.SourceTaskID, AgentID: comment.AuthorID, CommentID: comment.ID})
	if err != nil {
		return err
	}
	for _, humanID := range targets {
		if _, err := queries.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: humanID}); err != nil {
			return err
		}
	}
	return queries.StampCommentDecisionIntent(ctx, db.StampCommentDecisionIntentParams{WorkspaceID: comment.WorkspaceID, CommentID: comment.ID, DecisionIntentID: intent.ID})
}
