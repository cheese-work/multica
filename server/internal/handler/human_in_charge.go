package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type humanInCharge struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

type humanInChargeResponse struct {
	Human  *humanInCharge `json:"human"`
	Reason string         `json:"reason,omitempty"`
}

// resolveHumanInCharge returns the first eligible person in Cheese's order:
// creator, requester, project lead, workspace owner. Eligible means an active
// member of the issue's workspace who may invoke asking. It returns nil when
// nobody is eligible, never an agent or squad, and fails closed on any lookup
// error (CHE-2591 L5 design §4.1).
func (h *Handler) resolveHumanInCharge(ctx context.Context, q *db.Queries, issue db.Issue, asking db.Agent) (*humanInCharge, error) {
	candidates := []struct {
		source string
		id     func() (pgtype.UUID, error)
	}{
		{"creator", func() (pgtype.UUID, error) {
			if issue.CreatorType != "member" {
				return pgtype.UUID{}, nil
			}
			return issue.CreatorID, nil
		}},
		{"requested_by", func() (pgtype.UUID, error) { return issue.RequestedByID, nil }},
		{"project_lead", func() (pgtype.UUID, error) {
			if !issue.ProjectID.Valid {
				return pgtype.UUID{}, nil
			}
			project, err := q.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
			if errors.Is(err, pgx.ErrNoRows) {
				return pgtype.UUID{}, nil
			}
			if err != nil {
				return pgtype.UUID{}, err
			}
			if project.LeadType.String != "member" {
				return pgtype.UUID{}, nil
			}
			return project.LeadID, nil
		}},
		{"workspace_owner", func() (pgtype.UUID, error) {
			owner, err := q.GetWorkspaceOwnerUserID(ctx, issue.WorkspaceID)
			if errors.Is(err, pgx.ErrNoRows) {
				return pgtype.UUID{}, nil
			}
			return owner, err
		}},
	}
	for _, candidate := range candidates {
		id, err := candidate.id()
		if err != nil {
			return nil, err
		}
		if !id.Valid {
			continue
		}
		eligible, err := h.eligibleHumanInCharge(ctx, q, issue, asking, id)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		user, err := q.GetUser(ctx, id)
		if err != nil {
			return nil, err
		}
		return &humanInCharge{UserID: uuidToString(id), Name: user.Name, Source: candidate.source}, nil
	}
	return nil, nil
}

// eligibleHumanInCharge runs the checks a decision target passes
// (checkDecisionTarget), but separates a failed lookup from a refusal.
func (h *Handler) eligibleHumanInCharge(ctx context.Context, q *db.Queries, issue db.Issue, asking db.Agent, userID pgtype.UUID) (bool, error) {
	_, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return h.invokeAgentDecisionVia(ctx, q, asking, "member", uuidToString(userID), "", uuidToString(issue.WorkspaceID))
}

// GetHumanInCharge answers an agent's running task: who it should ask for a
// decision on this issue. Only a running, non-chat task on the issue may ask.
func (h *Handler) GetHumanInCharge(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	asking, ok := h.humanInChargeAsker(w, r, issue)
	if !ok {
		return
	}
	human, err := h.resolveHumanInCharge(r.Context(), h.Queries, issue, asking)
	if err != nil {
		slog.Error("resolve human in charge", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to resolve the human in charge")
		return
	}
	response := humanInChargeResponse{Human: human}
	if human == nil {
		response.Reason = "no creator, requester, project lead or workspace owner is an active member who may invoke the asking agent"
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) humanInChargeAsker(w http.ResponseWriter, r *http.Request, issue db.Issue) (db.Agent, bool) {
	deny := func() (db.Agent, bool) {
		writeError(w, http.StatusForbidden, "human-in-charge requires a running non-chat task on this issue")
		return db.Agent{}, false
	}
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return deny()
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return deny()
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to read the task")
		return db.Agent{}, false
	}
	if err != nil || task.Status != "running" || task.ChatSessionID.Valid || task.IssueID != issue.ID || uuidToString(task.AgentID) != r.Header.Get("X-Agent-ID") {
		return deny()
	}
	agent, err := h.Queries.GetAgent(r.Context(), task.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the agent")
		return db.Agent{}, false
	}
	if agent.WorkspaceID != issue.WorkspaceID {
		return deny()
	}
	return agent, true
}
