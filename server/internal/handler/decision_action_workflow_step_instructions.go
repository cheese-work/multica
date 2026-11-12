package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workflow.step.instructions.set (CHE-2949 D5b) rides the config-change core
// and writes one column of one step. The server builds the whole change from
// the live row: nothing but that step's instructions differs, so the agent has
// no after digest to claim. An approval takes the status catalog lock first,
// like a workflow save, because it validates the workflow against the catalog.
func init() {
	registerConfigChange(decisionaction.WorkflowStepInstructionsSet, configChange{
		Fields:      []string{workflowInstructionsField},
		ServerAfter: true, StaleBaseCode: "stale_base", CatalogLock: true, DelegatedField: workflowInstructionsField,
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			_, err = queries.LockWorkflowTargetNowait(ctx, db.LockWorkflowTargetNowaitParams{ID: workflow.ID, WorkspaceID: workspaceID})
			return err
		},
		Read: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
			workflow, live, after, index, err := resolveStepInstructionsChange(ctx, queries, workspaceID, params, http.StatusBadRequest)
			if err != nil {
				return configChangeRead{}, err
			}
			return configChangeRead{
				Field: workflowInstructionsField, TargetID: uuidToString(workflow.ID),
				Before: stepInstructionsView(live[index]), After: stepInstructionsView(after[index]),
				LiveSHA256: workflowStepsHash(workflow.ID, live), AfterSHA256: workflowStepsHash(workflow.ID, after),
				HumanBlocksIntact: protectedStepBlocksUnchanged(live, after),
				// Named from the live row, inside the preview digest: the approver
				// sees which workflow and step this lands on, not the ask's claim.
				Meta: map[string]any{"workflow_name": workflow.Name, "status_key": live[index].StatusKey},
			}, nil
		},
		Apply: func(ctx context.Context, execution decisionActionExecution, _ configChangeRead) error {
			workflow, _, after, _, err := resolveStepInstructionsChange(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict)
			if err != nil {
				return err
			}
			encoded, err := issueworkflow.EncodeSteps(after)
			if err != nil {
				return err
			}
			_, err = execution.Queries.UpdateIssueWorkflow(ctx, db.UpdateIssueWorkflowParams{
				Name: workflow.Name, Description: workflow.Description, InitialStatusKey: workflow.InitialStatusKey,
				Steps: encoded, MaxRunsPerIssue: workflow.MaxRunsPerIssue, IncludeChildPrs: pgtype.Bool{Bool: workflow.IncludeChildPrs, Valid: true},
				ID: workflow.ID, WorkspaceID: workflow.WorkspaceID,
			})
			return err
		},
		Publish: func(_ context.Context, handler *Handler, action db.DecisionAction) error {
			handler.publishIssueWorkflowChanged(uuidToString(action.WorkspaceID), db.Member{UserID: action.ApprovedByUserID}, "updated")
			return nil
		},
	})
}

func stepInstructionsView(step issueworkflow.Step) map[string]string {
	return map[string]string{"status_key": step.StatusKey, "instructions": step.Instructions}
}

// resolveStepInstructionsChange loads the workflow and returns its live steps,
// the steps with only the named step's instructions replaced (read back through
// the stored encoding and validated against the catalog), and that step's index.
// A step the workflow lacks is drift when the base digest moved, otherwise a
// bad status key.
func resolveStepInstructionsChange(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage, status int) (db.IssueWorkflow, []issueworkflow.Step, []issueworkflow.Step, int, error) {
	workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, status, false)
	if err != nil {
		return workflow, nil, nil, 0, err
	}
	var wanted decisionaction.WorkflowStepInstructionsSetParams
	if err := json.Unmarshal(params, &wanted); err != nil {
		return workflow, nil, nil, 0, err
	}
	live, err := issueworkflow.DecodeSteps(workflow.Steps)
	if err != nil {
		return workflow, nil, nil, 0, err
	}
	index := slices.IndexFunc(live, func(step issueworkflow.Step) bool { return step.StatusKey == wanted.StatusKey })
	if index < 0 {
		if workflowStepsHash(workflow.ID, live) != wanted.BaseSHA256 {
			return workflow, live, nil, 0, errPreconditionDrift
		}
		return workflow, live, nil, 0, invalidWorkflowSteps("unknown_status_key", "the workflow has no step for status_key "+wanted.StatusKey)
	}
	edited := slices.Clone(live)
	edited[index].Instructions = wanted.Instructions
	catalog, err := workflowSteps.workflowStatusCatalog(ctx, queries, workspaceID)
	if err != nil {
		return workflow, live, nil, 0, err
	}
	if err := issueworkflow.Validate(workflow.Name, workflow.Description, edited, workflow.IsDefault, catalog); err != nil {
		return workflow, live, nil, 0, invalidWorkflowSteps("invalid_workflow", err.Error())
	}
	encoded, err := issueworkflow.EncodeSteps(edited)
	if err != nil {
		return workflow, live, nil, 0, err
	}
	after, err := issueworkflow.DecodeSteps(encoded)
	return workflow, live, after, index, err
}
