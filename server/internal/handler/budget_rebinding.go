package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A non-interactive credential cannot widen an issue's run budget by moving the
// issue or its project to a looser workflow (CHE-2798 P8/E17). Every write that
// rebinds issue.project_id, issue.workflow_id or project.workflow_id, or deletes
// a workflow, compares the binding before and after inside its own transaction,
// after the rows are locked.

// budgetBinding is the run budget an issue gets from the workflow it follows.
type budgetBinding struct {
	budgeted bool
	cap      int
	start    time.Time
}

// budgetWidens reports whether after gives the issue more runs than before:
// the budget is dropped, the cap is higher, or the window starts later.
func budgetWidens(before, after budgetBinding) bool {
	return before.budgeted && (!after.budgeted || after.cap > before.cap || after.start.After(before.start))
}

const budgetRebindingCode = "budget_rebinding_requires_interactive"

var errBudgetRebinding = errors.New(budgetRebindingCode)

func writeBudgetRebindingRefused(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusForbidden, budgetRebindingCode,
		"widening a run budget needs an interactive sign-in (web or desktop app) or an owner-approved issue.workflow decision: run `multica issue update <id> --workflow <workflow-id> --print-hash`, submit it with `multica decision request`, then repeat the command with --decision-id")
}

// writeBudgetRebindingError renders errBudgetRebinding and reports whether it
// handled the error.
func writeBudgetRebindingError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errBudgetRebinding) {
		return false
	}
	writeBudgetRebindingRefused(w)
	return true
}

// isInteractiveRequest is true only for a person signed in through the app. The
// auth middleware strips and re-stamps X-Actor-Source, so a missing or unknown
// value is non-interactive.
func isInteractiveRequest(r *http.Request) bool {
	return r.Header.Get("X-Actor-Source") == "interactive_jwt"
}

type budgetActorKey struct{}

// withBudgetActor carries the request's actor source to updateIssueStoppingWakeups,
// the one place every UpdateIssue write passes through.
func withBudgetActor(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, budgetActorKey{}, isInteractiveRequest(r))
}

// verifyBoundRebinding admits a widening that a workspace owner approved for
// the issue.workflow field (CHE-5228). Only an agent task token that names a
// decision gets this far; anything else keeps the interactive-only refusal.
// issueID is empty on a create. It records the single-use row in qtx's
// transaction, so the caller commits it with the write.
func (h *Handler) verifyBoundRebinding(ctx context.Context, qtx *db.Queries, r *http.Request, workspaceID pgtype.UUID, binding DecisionBindingFields, issueID, projectID, beforeWorkflowID, afterWorkflowID pgtype.UUID) error {
	if r.Header.Get("X-Actor-Source") != "task_token" || strings.TrimSpace(binding.DecisionID) == "" {
		return errBudgetRebinding
	}
	digest := func(workflowID pgtype.UUID) string {
		return decisionaction.IssueWorkflowDigest(uuidToString(issueID), uuidToString(projectID), uuidToString(workflowID))
	}
	return h.verifyBoundDigests(ctx, qtx, r, workspaceID, binding, decisionaction.IssueWorkflowField, digest(beforeWorkflowID), digest(afterWorkflowID))
}

// writeBoundRebindingError renders the refusal verifyBoundRebinding returns.
func writeBoundRebindingError(w http.ResponseWriter, err error) {
	if !writeBudgetRebindingError(w, err) {
		writeDecisionError(w, err, "issue workflow write")
	}
}

// workflowBinding is the budget a workflow gives an issue with the given
// max_runs_override. A workflow with no stored budget start is not budgeted.
func workflowBinding(def *issueworkflow.Definition, override pgtype.Int4) budgetBinding {
	if def == nil || def.RunBudgetSince.IsZero() {
		return budgetBinding{}
	}
	b := budgetBinding{budgeted: true, cap: def.EffectiveMaxRuns(), start: def.RunBudgetSince}
	if override.Valid && int(override.Int32) > b.cap {
		b.cap = int(override.Int32)
	}
	return b
}

// issueBinding resolves the budget of an issue that follows workflowID, or its
// project's workflow when that is unset. A missing override workflow is
// unbudgeted, as the run guard reads it.
func issueBinding(ctx context.Context, q issueworkflow.Querier, workspaceID, workflowID, projectID pgtype.UUID, override pgtype.Int4) (budgetBinding, error) {
	if !workflowID.Valid && !projectID.Valid {
		return budgetBinding{}, nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, workspaceID, workflowID, projectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) {
		return budgetBinding{}, nil
	}
	if err != nil {
		return budgetBinding{}, err
	}
	return workflowBinding(def, override), nil
}

// guardIssueProjectRebinding refuses a non-interactive project change that
// widens the issue's budget. It locks the issue row so the comparison and the
// write see the same binding.
func guardIssueProjectRebinding(ctx context.Context, q *db.Queries, params db.UpdateIssueParams) error {
	if interactive, ok := ctx.Value(budgetActorKey{}).(bool); !ok || interactive {
		return nil
	}
	found, err := q.GetIssue(ctx, params.ID)
	if err != nil {
		return err
	}
	current, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: found.ID, WorkspaceID: found.WorkspaceID})
	if err != nil {
		return err
	}
	if current.ProjectID == params.ProjectID {
		return nil
	}
	before, err := issueBinding(ctx, q, current.WorkspaceID, current.WorkflowID, current.ProjectID, current.MaxRunsOverride)
	if err != nil {
		return err
	}
	after, err := issueBinding(ctx, q, current.WorkspaceID, current.WorkflowID, params.ProjectID, current.MaxRunsOverride)
	if err != nil {
		return err
	}
	if budgetWidens(before, after) {
		return errBudgetRebinding
	}
	return nil
}

// projectSwitchWidensBudget compares the workflow a project uses now with the
// one it would use after the switch (nil target is Default). The issue that
// gains most is the one with the smallest max_runs_override among those that
// follow the project, so that override is applied on both sides.
func projectSwitchWidensBudget(ctx context.Context, q *db.Queries, project db.Project, target *issueworkflow.Definition) (bool, error) {
	current, err := issueworkflow.ForProjectRow(ctx, q, project)
	if err != nil {
		return false, err
	}
	next := target
	if next == nil {
		def, err := issueworkflow.DefaultFor(ctx, q, project.WorkspaceID)
		if err != nil {
			return false, err
		}
		next = &def
	}
	least, err := q.MinInheritedRunsOverrideForProject(ctx, db.MinInheritedRunsOverrideForProjectParams{WorkspaceID: project.WorkspaceID, ProjectID: project.ID})
	if err != nil {
		return false, err
	}
	override := pgtype.Int4{Int32: least, Valid: least > 0}
	return budgetWidens(workflowBinding(current, override), workflowBinding(next, override)), nil
}

// workflowDeleteWidensBudget covers a delete that would strand references:
// projects fall back to Default and override issues to unbudgeted. A delete
// with no reference changes no issue's budget. The in-use check that follows
// still refuses a referenced workflow, so this decides only the status code a
// non-interactive caller sees.
func workflowDeleteWidensBudget(ctx context.Context, q *db.Queries, row db.IssueWorkflow, usedByProject bool) (bool, error) {
	def, err := issueworkflow.Decode(row)
	if err != nil {
		return false, err
	}
	before := workflowBinding(&def, pgtype.Int4{})
	if !before.budgeted {
		return false, nil
	}
	overrides, err := q.CountIssuesOverridingToIssueWorkflow(ctx, db.CountIssuesOverridingToIssueWorkflowParams{WorkspaceID: row.WorkspaceID, WorkflowID: row.ID})
	if err != nil {
		return false, err
	}
	if overrides > 0 {
		return true, nil
	}
	if !usedByProject {
		return false, nil
	}
	fallback, err := issueworkflow.DefaultFor(ctx, q, row.WorkspaceID)
	if err != nil {
		return false, err
	}
	return budgetWidens(before, workflowBinding(&fallback, pgtype.Int4{})), nil
}
