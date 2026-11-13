package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProjectWorkflowSplit is the CHE-5238 A/B setting: between StartsAt and EndsAt
// issues created in the project may follow arm B's workflow instead of the
// project's own (arm A). Selection happens at issue create (S1b), not here.
type ProjectWorkflowSplit struct {
	ArmBWorkflowID      string    `json:"arm_b_workflow_id"`
	StartsAt            time.Time `json:"starts_at"`
	EndsAt              time.Time `json:"ends_at"`
	ExcludedAssigneeIDs []string  `json:"excluded_assignee_ids"`
	ExcludedSquadIDs    []string  `json:"excluded_squad_ids"`
}

func decodeWorkflowSplit(raw []byte) *ProjectWorkflowSplit {
	if len(raw) == 0 {
		return nil
	}
	var s ProjectWorkflowSplit
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// SetProjectWorkflowSplit stores the project's split. Same access rule as the
// project's workflow (canManageProjectWorkflow). Arm B must exist in the
// workspace and list every status of the project's own workflow.
func (h *Handler) SetProjectWorkflowSplit(w http.ResponseWriter, r *http.Request) {
	project, member, workspaceID, ok := h.loadProjectForWorkflowWrite(w, r)
	if !ok {
		return
	}
	var req ProjectWorkflowSplit
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.StartsAt.Before(req.EndsAt) {
		writeError(w, http.StatusBadRequest, "ends_at must be after starts_at")
		return
	}
	armB, ok := parseUUIDOrBadRequest(w, req.ArmBWorkflowID, "arm_b_workflow_id")
	if !ok {
		return
	}
	for field, ids := range map[string][]string{"excluded_assignee_ids": req.ExcludedAssigneeIDs, "excluded_squad_ids": req.ExcludedSquadIDs} {
		for _, id := range ids {
			if _, ok := parseUUIDOrBadRequest(w, id, field); !ok {
				return
			}
		}
	}
	ctx := r.Context()
	row, err := h.Queries.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: armB, WorkspaceID: project.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusBadRequest, "arm B workflow not found in this workspace")
		return
	}
	armBDef, err := issueworkflow.Decode(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read workflow")
		return
	}
	armADef, err := issueworkflow.ForProjectRow(ctx, h.Queries, project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project workflow")
		return
	}
	for _, key := range armADef.StatusKeys() {
		if !armBDef.Has(key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("arm B workflow lacks the project's status %q", key))
			return
		}
	}
	h.storeWorkflowSplit(w, r, workspaceID, member, project.ID, project.WorkspaceID, &req)
}

// ClearProjectWorkflowSplit returns the project to no split.
func (h *Handler) ClearProjectWorkflowSplit(w http.ResponseWriter, r *http.Request) {
	project, member, workspaceID, ok := h.loadProjectForWorkflowWrite(w, r)
	if !ok {
		return
	}
	h.storeWorkflowSplit(w, r, workspaceID, member, project.ID, project.WorkspaceID, nil)
}

// storeWorkflowSplit writes split (nil clears) and answers with the project.
func (h *Handler) storeWorkflowSplit(w http.ResponseWriter, r *http.Request, workspaceID string, member db.Member, id, wsID pgtype.UUID, split *ProjectWorkflowSplit) {
	var raw []byte
	if split != nil {
		if split.ExcludedAssigneeIDs == nil {
			split.ExcludedAssigneeIDs = []string{}
		}
		if split.ExcludedSquadIDs == nil {
			split.ExcludedSquadIDs = []string{}
		}
		raw, _ = json.Marshal(split)
	}
	updated, err := h.Queries.SetProjectWorkflowSplit(r.Context(), db.SetProjectWorkflowSplitParams{ID: id, WorkspaceID: wsID, WorkflowSplit: raw})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save workflow split")
		return
	}
	writeJSON(w, http.StatusOK, h.publishProjectWorkflowUpdate(r.Context(), workspaceID, member, updated))
}
