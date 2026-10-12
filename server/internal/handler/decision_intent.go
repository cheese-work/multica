package handler

import (
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
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		writeError(writer, refusal.status, refusal.message)
	} else if decisiontx.IsRetryable(err) {
		writeError(writer, http.StatusServiceUnavailable, "decision intent is busy; retry")
	} else {
		writeError(writer, http.StatusInternalServerError, "failed to create decision intent")
	}
}

func (handler *Handler) prepareDecisionIntent(request *http.Request, queries *db.Queries, issue db.Issue, authorType string, authorID pgtype.UUID, content string) (db.Issue, []pgtype.UUID, error) {
	ctx := request.Context()
	deny := decisionIntentRefusal{http.StatusForbidden, "decision intent requires a running assigned issue task on this issue"}
	if request.Header.Get("X-Actor-Source") != "task_token" || authorType != "agent" {
		return issue, nil, deny
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		return issue, nil, err
	}
	issue, err := queries.GetIssue(ctx, issue.ID)
	if err != nil {
		return issue, nil, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return issue, nil, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return issue, nil, deny
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		return issue, nil, deny
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: taskID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return issue, nil, deny
		}
		return issue, nil, err
	}
	task, err := queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return issue, nil, err
	}
	if task.Status != "running" || task.ChatSessionID.Valid || task.IssueID != issue.ID || task.AgentID != authorID {
		return issue, nil, deny
	}
	assigned := issue.AssigneeType.String == "agent" && issue.AssigneeID == task.AgentID
	if issue.AssigneeType.String == "squad" && issue.AssigneeID.Valid {
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && squad.ArchivedAt.Valid) {
			return issue, nil, deny
		}
		if err != nil {
			return issue, nil, err
		}
		assigned, err = queries.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: task.AgentID})
		if err != nil {
			return issue, nil, err
		}
	}
	if !assigned {
		return issue, nil, deny
	}
	agent, err := queries.GetAgent(ctx, task.AgentID)
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
		if _, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: humanID, WorkspaceID: issue.WorkspaceID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return issue, nil, decisionIntentRefusal{http.StatusBadRequest, "named person is not an active workspace member: " + mention.ID}
			}
			return issue, nil, err
		}
		if !handler.canInvokeAgent(ctx, agent, "member", uuidToString(humanID), "", uuidToString(issue.WorkspaceID)) {
			return issue, nil, decisionIntentRefusal{http.StatusBadRequest, "named person cannot invoke the asking agent: " + mention.ID}
		}
		seen[humanID] = true
		targets = append(targets, humanID)
	}
	if len(targets) == 0 {
		return issue, nil, decisionIntentRefusal{http.StatusBadRequest, "decision intent must name at least one person with a member mention"}
	}
	sort.Slice(targets, func(first, second int) bool { return uuidToString(targets[first]) < uuidToString(targets[second]) })
	previous, err := queries.LockDecisionPendingIntent(ctx, db.LockDecisionPendingIntentParams{WorkspaceID: issue.WorkspaceID, TaskID: task.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return issue, nil, err
	}
	if err == nil {
		if err := queries.CloseDecisionIntent(ctx, db.CloseDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: previous.ID, CloseReason: pgtype.Text{String: "superseded", Valid: true}}); err != nil {
			return issue, nil, err
		}
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
