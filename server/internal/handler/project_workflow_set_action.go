package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Owner approval for switching the workflow of one or many projects when the
// switch widens a run budget (CHE-10137). The card seals every project's binding
// before and after; the executor recomputes each under the locks and, when any
// differs, ends the whole action as stale_base with no project changed.

// A literal, not a decisionaction constant: the preview contract test resolves
// only string literals in this package.
const projectWorkflowSetType = "project.workflow.set"

const workflowBatchDirectCode = "workflow_batch_direct"

// projectSwitchSeal is one project's sealed switch.
type projectSwitchSeal struct {
	project db.Project
	params  decisionaction.ProjectWorkflowQuickParams
}

func (h *Handler) openProjectWorkflowSetCard(ctx context.Context, host db.Issue, seals []projectSwitchSeal) (RunBudgetActionResponse, error) {
	params := decisionaction.ProjectWorkflowSetParams{WorkflowID: seals[0].params.WorkflowID}
	titles := make([]string, 0, len(seals))
	for _, seal := range seals {
		params.Projects = append(params.Projects, seal.params)
		titles = append(titles, strconv.Quote(seal.project.Title))
	}
	ask := fmt.Sprintf("**Project workflow request**\nAn agent asked to switch %d project(s) to one workflow: %s.\nThe card lists each project with its workflow and run cap before and after. Approve it with your passkey in the web app, or reject it. Approval changes every project or none.",
		len(seals), strings.Join(titles, ", "))
	return h.openOwnerCard(ctx, host, ownerCard{actionType: decisionaction.ProjectWorkflowSet, name: "project_workflow_set", ask: ask, params: params})
}

type SetProjectsWorkflowRequest struct {
	ProjectIDs    []string          `json:"project_ids"`
	WorkflowID    *string           `json:"workflow_id"`
	StatusMapping map[string]string `json:"status_mapping,omitempty"`
}

// SetProjectsWorkflow asks the owners to switch several projects to one
// workflow with a single card. It changes nothing itself: a switch that needs no
// approval is made one project at a time, so the call answers
// workflow_batch_direct and writes nothing when none of the projects needs the
// owners.
func (h *Handler) SetProjectsWorkflow(w http.ResponseWriter, r *http.Request) {
	var req SetProjectsWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if n := len(req.ProjectIDs); n < 1 || n > decisionaction.MaxProjectWorkflowSetProjects {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("project_ids takes 1 to %d projects", decisionaction.MaxProjectWorkflowSetProjects))
		return
	}
	var targetID pgtype.UUID
	if req.WorkflowID != nil && *req.WorkflowID != "" {
		if !h.requireProjectWorkflowsV1(w, r) {
			return
		}
		id, ok := parseUUIDOrBadRequest(w, *req.WorkflowID, "workflow_id")
		if !ok {
			return
		}
		targetID = id
	}
	loaded := make([]db.Project, 0, len(req.ProjectIDs))
	seen := map[string]bool{}
	var workspaceUUID pgtype.UUID
	for _, raw := range req.ProjectIDs {
		project, _, _, ok := h.loadProjectIDForWorkflowWrite(w, r, raw)
		if !ok {
			return
		}
		if seen[uuidToString(project.ID)] {
			writeError(w, http.StatusBadRequest, "project "+uuidToString(project.ID)+" is listed twice")
			return
		}
		seen[uuidToString(project.ID)] = true
		workspaceUUID = project.WorkspaceID
		loaded = append(loaded, project)
	}
	// One lock order for every writer of these rows.
	sort.Slice(loaded, func(i, j int) bool { return uuidToString(loaded[i].ID) < uuidToString(loaded[j].ID) })

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to change workflow")
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	if err := qtx.LockIssueStatusCatalogShared(ctx, workspaceUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to change workflow")
		return
	}
	var seals []projectSwitchSeal
	needsCard := false
	for _, candidate := range loaded {
		project, err := qtx.LockProjectForWorkflowChange(ctx, db.LockProjectForWorkflowChangeParams{ID: candidate.ID, WorkspaceID: workspaceUUID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to change workflow")
			return
		}
		def, normalized, err := loadSwitchTarget(ctx, qtx, workspaceUUID, targetID)
		if errors.Is(err, errSwitchTargetMissing) {
			writeError(w, http.StatusBadRequest, "workflow not found in this workspace")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read workflow")
			return
		}
		if !isInteractiveRequest(r) {
			verdict, err := projectSwitchWidensBudget(ctx, qtx, project, def)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to change workflow")
				return
			}
			needsCard = needsCard || verdict.refused()
		}
		switchPlan, err := planProjectSwitch(ctx, qtx, project, def)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load issue statuses")
			return
		}
		if msg := checkMapping(switchPlan.plan, req.StatusMapping, switchPlan.accepts); msg != "" {
			writeMappingRequired(w, switchPlan.plan, fmt.Sprintf("project %q: %s", project.Title, msg))
			return
		}
		params, err := projectWorkflowQuickParams(ctx, qtx, switchPlan, def, normalized, req.StatusMapping)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to change workflow")
			return
		}
		seals = append(seals, projectSwitchSeal{project: project, params: params})
	}
	if !needsCard {
		if refuseCardOnlyWrite(w, r) {
			return
		}
		writeErrorCode(w, http.StatusConflict, workflowBatchDirectCode, "none of these switches needs owner approval; switch the projects one at a time")
		return
	}
	var host db.Issue
	hosted := false
	if r.Header.Get("X-Actor-Source") == "task_token" {
		host, hosted = h.runIssue(r)
	}
	if !hosted {
		writeProjectSwitchRefused(w)
		return
	}
	// Release the project and catalog locks before the card takes its own.
	if err := tx.Rollback(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to change workflow")
		return
	}
	opened, err := h.openProjectWorkflowSetCard(ctx, host, seals)
	if err != nil {
		writeDecisionError(w, err, "project workflow request")
		return
	}
	writeJSON(w, http.StatusAccepted, opened)
}

// sameProjectSwitch reports whether the switch read under the locks is the one
// the card sealed.
func sameProjectSwitch(current, sealed decisionaction.ProjectWorkflowQuickParams) bool {
	return current.ProjectID == sealed.ProjectID && sameOptionalID(current.WorkflowID, sealed.WorkflowID) &&
		current.TotalIssues == sealed.TotalIssues && current.Before == sealed.Before && current.After == sealed.After &&
		sameStatusCounts(current.Required, sealed.Required) && slices.Equal(current.Overrides, sealed.Overrides)
}

// executeProjectWorkflowSetAction recomputes every sealed switch, and only when
// all still match applies them in the approval's transaction.
func executeProjectWorkflowSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.ProjectWorkflowSetParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	var targetID pgtype.UUID
	if params.WorkflowID != nil {
		var err error
		if targetID, err = parseUUIDString(*params.WorkflowID); err != nil {
			return nil, err
		}
	}
	queries := execution.Queries
	workspaceID := execution.Action.WorkspaceID
	target, normalized, err := loadSwitchTarget(ctx, queries, workspaceID, targetID)
	if errors.Is(err, errSwitchTargetMissing) {
		return nil, staleBase("the target workflow no longer exists in this workspace")
	}
	if err != nil {
		return nil, err
	}
	plans := make([]projectSwitchPlan, 0, len(params.Projects))
	for _, sealed := range params.Projects {
		projectID, err := parseUUIDString(sealed.ProjectID)
		if err != nil {
			return nil, err
		}
		project, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, staleBase("project %s no longer exists", sealed.ProjectID)
		}
		if err != nil {
			return nil, err
		}
		sw, err := planProjectSwitch(ctx, queries, project, target)
		if err != nil {
			return nil, err
		}
		current, err := projectWorkflowQuickParams(ctx, queries, sw, target, normalized, sealed.StatusMapping)
		if err != nil {
			return nil, err
		}
		if !sameProjectSwitch(current, sealed) {
			return nil, staleBase("project %q, its issues or the run budget changed since the request; ask again", project.Title)
		}
		if msg := checkMapping(sw.plan, sealed.StatusMapping, sw.accepts); msg != "" {
			return nil, staleBase("project %q: %s", project.Title, msg)
		}
		plans = append(plans, sw)
	}
	projectIDs, issueIDs, taskIDs := []string{}, []string{}, []string{}
	for i, sw := range plans {
		updated, changed, cancelled, err := applyProjectSwitch(ctx, queries, sw, normalized, params.Projects[i].StatusMapping, uuidToString(execution.Approver.UserID))
		if err != nil {
			return nil, err
		}
		if err := insertAuditRow(ctx, queries, auditActor{Type: "member", ID: execution.Approver.UserID, Source: "decision_action"}, projectSwitchAudit(sw.project, updated, len(changed))); err != nil {
			return nil, err
		}
		projectIDs = append(projectIDs, uuidToString(sw.project.ID))
		for _, issue := range changed {
			issueIDs = append(issueIDs, uuidToString(issue.ID))
		}
		for _, task := range cancelled {
			taskIDs = append(taskIDs, uuidToString(task.ID))
		}
	}
	return json.Marshal(map[string]any{
		"project_ids": projectIDs, "workflow_id": params.WorkflowID,
		"issues_remapped": len(issueIDs), "issue_ids": issueIDs, "cancelled_task_ids": taskIDs,
	})
}

// projectWorkflowSetPreview lists every project with its workflow and run cap
// before and after, and how many of its issues move to another status.
func (h *Handler) projectWorkflowSetPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.ProjectWorkflowSetParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	fields := []ActionPreviewField{{"Projects", "0", strconv.Itoa(len(params.Projects))}}
	for _, sealed := range params.Projects {
		projectID, err := parseUUIDString(sealed.ProjectID)
		if err != nil {
			return nil, err
		}
		project, err := h.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: action.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // the executor ends the action with a failure
		}
		if err != nil {
			return nil, err
		}
		from, ok, err := h.workflowSetName(ctx, action.WorkspaceID, sealed.Before)
		if err != nil || !ok {
			return nil, err
		}
		to, ok, err := h.workflowSetName(ctx, action.WorkspaceID, sealed.After)
		if err != nil || !ok {
			return nil, err
		}
		fields = append(fields,
			ActionPreviewField{"Project " + project.Title, from + ", cap " + workflowSetCapLabel(sealed.Before), to + ", cap " + workflowSetCapLabel(sealed.After)},
			ActionPreviewField{project.Title + ": issues moved to another status", "0", strconv.FormatInt(movedIssueCount(sealed), 10)})
	}
	return fields, nil
}
