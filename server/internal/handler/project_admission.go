package handler

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type projectWriteScopeKey struct{}
type projectWriteScope struct {
	workspaceID pgtype.UUID
	projectIDs  []pgtype.UUID
	issues      []db.Issue
}

func writeProjectAdmissionError(writer http.ResponseWriter, err error) bool {
	if !service.IsProjectInactive(err) {
		return false
	}
	writeJSON(writer, http.StatusForbidden, map[string]string{"error": "project_inactive", "reason_code": "project_inactive"})
	return true
}

func (handler *Handler) allowProjectWrite(writer http.ResponseWriter, request *http.Request, workspaceID, projectID pgtype.UUID) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodOptions {
		return true
	}
	actorType, _ := handler.resolveActor(request, requestUserID(request), uuidToString(workspaceID))
	if actorType != "agent" {
		return true
	}
	if err := service.RequireActiveProject(request.Context(), handler.Queries, workspaceID, projectID); err != nil {
		if !writeProjectAdmissionError(writer, err) {
			writeError(writer, http.StatusInternalServerError, "failed to verify project status")
		}
		return false
	}
	scope, _ := request.Context().Value(projectWriteScopeKey{}).(projectWriteScope)
	scope.workspaceID = workspaceID
	scope.projectIDs = append(scope.projectIDs, projectID)
	*request = *request.WithContext(context.WithValue(request.Context(), projectWriteScopeKey{}, scope))
	return true
}

func (handler *Handler) allowIssueProjectWrite(writer http.ResponseWriter, request *http.Request, issue db.Issue) bool {
	if !handler.allowProjectWrite(writer, request, issue.WorkspaceID, issue.ProjectID) {
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
