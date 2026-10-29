package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GitHub routing authorisation (CHE-2126 plan 5.8). can_route decides whether a
// GitHub author may route work to an agent on an issue. Only routeAllowed
// authorises; routeDeferred means a lookup failed and the caller must retry
// later, never treat it as allowed.

type routeVerdict string

const (
	routeAllowed  routeVerdict = "allowed"
	routeDenied   routeVerdict = "denied"
	routeDeferred routeVerdict = "deferred"
)

type routeDecision struct {
	Verdict routeVerdict
	Reason  string
	UserID  pgtype.UUID
}

func routeDeny(reason string) routeDecision {
	return routeDecision{Verdict: routeDenied, Reason: reason}
}
func routeDefer(reason string) routeDecision {
	return routeDecision{Verdict: routeDeferred, Reason: reason}
}

// projectInactive mirrors lock_active_project: these statuses are "archived"
// for admission purposes.
func projectInactive(status string) bool {
	return status == "paused" || status == "cancelled" || status == "completed"
}

// canRoute is true only when the GitHub numeric author maps to a user, that
// user is an active member of the issue's workspace, holds a grant on the
// issue's CURRENT project, the project is active, and the user may invoke the
// agent. Each lookup error yields routeDeferred.
func (h *Handler) canRoute(ctx context.Context, githubUserID int64, issue db.Issue, agent db.Agent) routeDecision {
	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return routeDefer("workspace lookup failed")
	}
	var settings struct {
		ReviewMembers map[string]string `json:"github_review_members"`
	}
	if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
		return routeDefer("workspace settings unreadable")
	}
	userID, err := util.ParseUUID(settings.ReviewMembers[strconv.FormatInt(githubUserID, 10)])
	if err != nil {
		return routeDeny("github user not mapped")
	}

	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: issue.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return routeDeny("not a workspace member")
		}
		return routeDefer("member lookup failed")
	}

	if !issue.ProjectID.Valid {
		return routeDeny("issue has no project")
	}
	project, err := h.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return routeDeny("project not found")
		}
		return routeDefer("project lookup failed")
	}
	if projectInactive(project.Status) {
		return routeDeny("project archived")
	}

	granted, err := h.Queries.HasGitHubRouteGrant(ctx, db.HasGitHubRouteGrantParams{ProjectID: project.ID, UserID: userID})
	if err != nil {
		return routeDefer("grant lookup failed")
	}
	if !granted {
		return routeDeny("no grant for project")
	}

	allowed, err := h.invokeAgentDecision(ctx, agent, "member", uuidToString(userID), "", uuidToString(issue.WorkspaceID))
	if err != nil {
		return routeDefer("agent invocation lookup failed")
	}
	if !allowed {
		return routeDeny("user may not invoke agent")
	}
	return routeDecision{Verdict: routeAllowed, UserID: userID}
}

// routeTargetIssues walks the binding chain from authoritative records only:
// installation -> bound workspaces -> PR row (must carry the same installation)
// -> linked issues, each with its CURRENT project. Comment text never feeds it.
// ponytail: the PR row has no GitHub repo-ID or fork column, so owner/name stands
// in for repo identity here; the repo-ID check lands with the column that stores it.
func (h *Handler) routeTargetIssues(ctx context.Context, installationID int64, repoOwner, repoName string, prNumber int32) ([]db.Issue, error) {
	installations, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var issues []db.Issue
	for _, installation := range installations {
		pull, err := h.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
			WorkspaceID: installation.WorkspaceID, RepoOwner: repoOwner, RepoName: repoName, PrNumber: prNumber,
		})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && pull.InstallationID != installationID) {
			continue
		}
		if err != nil {
			return nil, err
		}
		linked, err := h.Queries.ListGitHubPullRequestIssues(ctx, db.ListGitHubPullRequestIssuesParams{WorkspaceID: installation.WorkspaceID, PullRequestID: pull.ID})
		if err != nil {
			return nil, err
		}
		issues = append(issues, linked...)
	}
	return issues, nil
}

type gitHubRouteGrantResponse struct {
	ProjectID string  `json:"project_id"`
	UserID    string  `json:"user_id"`
	GrantedBy *string `json:"granted_by"`
	CreatedAt string  `json:"created_at"`
}

func gitHubRouteGrantToResponse(g db.GithubRouteGrant) gitHubRouteGrantResponse {
	resp := gitHubRouteGrantResponse{ProjectID: uuidToString(g.ProjectID), UserID: uuidToString(g.UserID), CreatedAt: g.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")}
	if g.GrantedBy.Valid {
		by := uuidToString(g.GrantedBy)
		resp.GrantedBy = &by
	}
	return resp
}

// routeGrantProject gates every grant endpoint: workspace owner/admin only, and
// the project must belong to that workspace.
func (h *Handler) routeGrantProject(w http.ResponseWriter, r *http.Request) (db.Member, db.Project, bool) {
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin")
	if !ok {
		return db.Member{}, db.Project{}, false
	}
	projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return db.Member{}, db.Project{}, false
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: member.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return db.Member{}, db.Project{}, false
	}
	return member, project, true
}

func (h *Handler) SetGitHubRouteGrant(w http.ResponseWriter, r *http.Request) {
	member, project, ok := h.routeGrantProject(w, r)
	if !ok {
		return
	}
	userID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "userId"), "user id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: project.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "user is not a workspace member")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to look up member")
		return
	}
	grant, err := h.Queries.UpsertGitHubRouteGrant(r.Context(), db.UpsertGitHubRouteGrantParams{
		ProjectID: project.ID, UserID: userID, GrantedBy: pgtype.UUID{Bytes: member.UserID.Bytes, Valid: true},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set route grant")
		return
	}
	writeJSON(w, http.StatusOK, gitHubRouteGrantToResponse(grant))
}

func (h *Handler) DeleteGitHubRouteGrant(w http.ResponseWriter, r *http.Request) {
	_, project, ok := h.routeGrantProject(w, r)
	if !ok {
		return
	}
	userID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "userId"), "user id")
	if !ok {
		return
	}
	if _, err := h.Queries.DeleteGitHubRouteGrant(r.Context(), db.DeleteGitHubRouteGrantParams{ProjectID: project.ID, UserID: userID}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete route grant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListGitHubRouteGrants(w http.ResponseWriter, r *http.Request) {
	_, project, ok := h.routeGrantProject(w, r)
	if !ok {
		return
	}
	grants, err := h.Queries.ListGitHubRouteGrants(r.Context(), project.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list route grants")
		return
	}
	resp := make([]gitHubRouteGrantResponse, 0, len(grants))
	for _, g := range grants {
		resp = append(resp, gitHubRouteGrantToResponse(g))
	}
	writeJSON(w, http.StatusOK, resp)
}
