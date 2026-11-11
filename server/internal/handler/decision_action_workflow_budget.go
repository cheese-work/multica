package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const workflowBudgetField = "workflow.budget"

// workflow.budget.set (CHE-5228) is how an agent changes a workflow's run
// budget: the cap and the child-PR switch. It rides the config-change core, so
// the owner's passkey card is the gate and a budget that moved since the ask
// fails the approval as stale_base. The server computes the whole change from
// the live row; the agent claims no after digest. Owner sessions still save the
// same fields directly.
func init() {
	registerConfigChange(decisionaction.WorkflowBudgetSet, configChange{
		Fields:      []string{workflowBudgetField},
		ServerAfter: true, StaleBaseCode: "stale_base", CatalogLock: true,
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			_, err = queries.LockWorkflowTargetNowait(ctx, db.LockWorkflowTargetNowaitParams{ID: workflow.ID, WorkspaceID: workspaceID})
			return err
		},
		Read: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusBadRequest, false)
			if err != nil {
				return configChangeRead{}, err
			}
			var wanted decisionaction.WorkflowBudgetSetParams
			if err := json.Unmarshal(params, &wanted); err != nil {
				return configChangeRead{}, err
			}
			liveCap, liveChildren := storedBudget(workflow)
			afterCap, afterChildren := liveCap, liveChildren
			if wanted.MaxRunsPerIssue != nil {
				afterCap = nil
				if *wanted.MaxRunsPerIssue > 0 {
					afterCap = wanted.MaxRunsPerIssue
				}
			}
			if wanted.IncludeChildPRs != nil {
				afterChildren = *wanted.IncludeChildPRs
			}
			id := uuidToString(workflow.ID)
			return configChangeRead{
				Field: workflowBudgetField, TargetID: id,
				Before: budgetView(liveCap, liveChildren), After: budgetView(afterCap, afterChildren),
				LiveSHA256:  decisionaction.WorkflowBudgetDigest(id, liveCap, liveChildren),
				AfterSHA256: decisionaction.WorkflowBudgetDigest(id, afterCap, afterChildren),
				// The change never touches a step, so every human-authored block stays.
				HumanBlocksIntact: true,
				Meta:              map[string]any{"workflow_name": workflow.Name},
			}, nil
		},
		Apply: func(ctx context.Context, execution decisionActionExecution, _ configChangeRead) error {
			workflow, err := loadWorkflowTarget(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			var wanted decisionaction.WorkflowBudgetSetParams
			if err := json.Unmarshal(execution.Action.Params, &wanted); err != nil {
				return err
			}
			params := db.UpdateIssueWorkflowParams{
				Name: workflow.Name, Description: workflow.Description, InitialStatusKey: workflow.InitialStatusKey,
				Steps: workflow.Steps, IncludeChildPrs: pgtype.Bool{Bool: workflow.IncludeChildPrs, Valid: true},
				ID: workflow.ID, WorkspaceID: workflow.WorkspaceID,
			}
			// A NULL cap keeps the stored one; 0 clears it, as a workflow save does.
			if wanted.MaxRunsPerIssue != nil {
				params.MaxRunsPerIssue = pgtype.Int4{Int32: int32(*wanted.MaxRunsPerIssue), Valid: true}
			}
			if wanted.IncludeChildPRs != nil {
				params.IncludeChildPrs = pgtype.Bool{Bool: *wanted.IncludeChildPRs, Valid: true}
			}
			_, err = execution.Queries.UpdateIssueWorkflow(ctx, params)
			return err
		},
		Publish: func(_ context.Context, handler *Handler, action db.DecisionAction) error {
			handler.publishIssueWorkflowChanged(uuidToString(action.WorkspaceID), db.Member{UserID: action.ApprovedByUserID}, "updated")
			return nil
		},
	})
}

func storedBudget(workflow db.IssueWorkflow) (*int, bool) {
	if !workflow.MaxRunsPerIssue.Valid {
		return nil, workflow.IncludeChildPrs
	}
	cap := int(workflow.MaxRunsPerIssue.Int32)
	return &cap, workflow.IncludeChildPrs
}

func budgetView(cap *int, includeChildPRs bool) map[string]any {
	return map[string]any{"max_runs_per_issue": cap, "include_child_prs": includeChildPRs}
}
