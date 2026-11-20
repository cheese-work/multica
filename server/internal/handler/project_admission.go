package handler

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type projectWriteScopeKey struct{}
type projectWriteScope struct {
	workspaceID pgtype.UUID
	projectIDs  []pgtype.UUID
	// runProjectIDs are admitted for the run taskID, which may keep writing while its project drains.
	runProjectIDs []pgtype.UUID
	taskID        pgtype.UUID
	issues        []db.Issue
}

func writeProjectAdmissionError(writer http.ResponseWriter, err error) bool {
	if !service.IsProjectInactive(err) {
		return false
	}
	writeJSON(writer, http.StatusForbidden, map[string]string{"error": "project_inactive", "reason_code": "project_inactive"})
	return true
}

func (handler *Handler) allowProjectWrite(writer http.ResponseWriter, request *http.Request, workspaceID, projectID pgtype.UUID) bool {
	return handler.allowProjectWriteForRun(writer, request, workspaceID, projectID, false)
}

func (handler *Handler) allowProjectWriteForRun(writer http.ResponseWriter, request *http.Request, workspaceID, projectID pgtype.UUID, runWrite bool) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodOptions {
		return true
	}
	actorType, _ := handler.resolveActor(request, requestUserID(request), uuidToString(workspaceID))
	if actorType != "agent" {
		return true
	}
	var taskID pgtype.UUID
	if runWrite {
		taskID, _ = util.ParseUUID(request.Header.Get("X-Task-ID"))
	}
	if err := service.RequireProjectWrite(request.Context(), handler.Queries, workspaceID, projectID, taskID); err != nil {
		if !writeProjectAdmissionError(writer, err) {
			writeError(writer, http.StatusInternalServerError, "failed to verify project status")
		}
		return false
	}
	scope, _ := request.Context().Value(projectWriteScopeKey{}).(projectWriteScope)
	scope.workspaceID = workspaceID
	if runWrite && taskID.Valid {
		scope.runProjectIDs = append(scope.runProjectIDs, projectID)
		scope.taskID = taskID
	} else {
		scope.projectIDs = append(scope.projectIDs, projectID)
	}
	*request = *request.WithContext(context.WithValue(request.Context(), projectWriteScopeKey{}, scope))
	return true
}

func (handler *Handler) allowIssueProjectWrite(writer http.ResponseWriter, request *http.Request, issue db.Issue) bool {
	if !handler.allowProjectWriteForRun(writer, request, issue.WorkspaceID, issue.ProjectID, true) {
		return false
	}
	if scope, ok := request.Context().Value(projectWriteScopeKey{}).(projectWriteScope); ok {
		scope.issues = append(scope.issues, issue)
		*request = *request.WithContext(context.WithValue(request.Context(), projectWriteScopeKey{}, scope))
	}
	return true
}

func (handler *Handler) fillProjectTriggerOutcome(ctx context.Context, issue db.Issue, response *IssueResponse) {
	if !issue.AssigneeID.Valid || !issue.AssigneeType.Valid {
		return
	}
	if err := service.RequireActiveIssueProject(ctx, handler.Queries, issue.ID); service.IsProjectInactive(err) {
		response.TriggerOutcomes = []CommentTriggerOutcome{{TargetType: issue.AssigneeType.String,
			TargetID: uuidToString(issue.AssigneeID), Status: DispatchBlocked, ReasonCode: ReasonProjectInactive}}
	}
}
