package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workflow.steps.set (CHE-3348 step 6) rides the config-change core. The agent
// sends the full new steps array; the server resolves, normalizes and validates
// it the way a workflow save does, then hashes the result with the CHE-2456
// workflowStepsHash recipe, so the digest an owner signs is the digest of the
// rows that will be stored.
//
// Narrower than a workflow save on purpose: a step may not create a status and
// the change may not remove one, because removing a status needs the
// issue-remap plan, which stays with the interactive editor. The governed:human
// rule is the same protectedStepBlocksUnchanged a bound write uses.

// workflowSteps is the receiver for the workflow checks that read only the
// queries they are given (the status catalog, sub-workflow targets); the
// config-change closures have no Handler to call them on.
var workflowSteps = &Handler{}

func init() {
	registerConfigChange(decisionaction.WorkflowStepsSet, configChange{
		Fields: []string{workflowStepsField, workflowInstructionsField},
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			_, err = queries.LockWorkflowTargetNowait(ctx, db.LockWorkflowTargetNowaitParams{ID: workflow.ID, WorkspaceID: workspaceID})
			return err
		},
		Read: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
			workflow, live, after, err := resolveWorkflowStepsChange(ctx, queries, workspaceID, params, http.StatusBadRequest)
			if err != nil {
				return configChangeRead{}, err
			}
			return configChangeRead{
				Field: workflowWriteField(live, after), TargetID: uuidToString(workflow.ID),
				Before: stepsPayload(live), After: stepsPayload(after),
				LiveSHA256: workflowStepsHash(workflow.ID, live), AfterSHA256: workflowStepsHash(workflow.ID, after),
				HumanBlocksIntact: protectedStepBlocksUnchanged(live, after),
			}, nil
		},
		Apply: func(ctx context.Context, execution decisionActionExecution, _ configChangeRead) error {
			workflow, _, after, err := resolveWorkflowStepsChange(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict)
			if err != nil {
				return err
			}
			live, err := issueworkflow.DecodeSteps(workflow.Steps)
			if err != nil {
				return err
			}
			// The approver stands in for the editor: they may hand work only to what
			// they could assign it to themselves. A refusal rolls the approval back.
			approver := uuidToString(execution.Approver.UserID)
			workspaceID := uuidToString(workflow.WorkspaceID)
			gate := func(agent db.Agent) bool {
				return (&Handler{Queries: execution.Queries}).canInvokeAgent(ctx, agent, "member", approver, approver, workspaceID)
			}
			if status, message := validateChangedHandlers(ctx, execution.Queries, workflow.WorkspaceID, live, after, gate); status != 0 {
				return actionFailure{Code: "invalid_handler", Message: message}
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

func stepsPayload(steps []issueworkflow.Step) []IssueWorkflowStepPayload {
	payload := make([]IssueWorkflowStepPayload, len(steps))
	for i, step := range steps {
		payload[i] = stepToPayload(step)
	}
	return payload
}

// resolveWorkflowStepsChange loads the target workflow and returns its live
// steps with the steps the params would store. status is the code for a target
// outside the workspace.
func resolveWorkflowStepsChange(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage, status int) (db.IssueWorkflow, []issueworkflow.Step, []issueworkflow.Step, error) {
	workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, status, false)
	if err != nil {
		return workflow, nil, nil, err
	}
	var wanted decisionaction.WorkflowStepsSetParams
	if err := json.Unmarshal(params, &wanted); err != nil {
		return workflow, nil, nil, err
	}
	live, err := issueworkflow.DecodeSteps(workflow.Steps)
	if err != nil {
		return workflow, nil, nil, err
	}
	after, err := workflowStepsAfter(ctx, queries, workflow, wanted.Steps)
	return workflow, live, after, err
}

func invalidWorkflowSteps(code, message string) decisionIntentRefusal {
	return decisionIntentRefusal{http.StatusBadRequest, code + ": " + message}
}

// workflowStepsAfter turns the agent's steps into the steps the workflow would
// store, or refuses with the reason. The result is read back through the stored
// encoding, so its digest is the digest of the row after Apply.
func workflowStepsAfter(ctx context.Context, queries *db.Queries, workflow db.IssueWorkflow, raw json.RawMessage) ([]issueworkflow.Step, error) {
	var inputs []IssueWorkflowStepInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inputs); err != nil {
		return nil, invalidWorkflowSteps("invalid_params", "steps are not workflow steps")
	}
	for _, input := range inputs {
		if input.NewStatus != nil {
			return nil, invalidWorkflowSteps("invalid_params", "a step may not create a status")
		}
	}
	steps, _, message, err := resolveStepInputs(ctx, queries, workflow.WorkspaceID, inputs, false)
	if err != nil {
		return nil, err
	}
	if message != "" {
		return nil, invalidWorkflowSteps("invalid_workflow", message)
	}
	live, err := issueworkflow.DecodeSteps(workflow.Steps)
	if err != nil {
		return nil, err
	}
	keepStoredEntry(steps, live)
	keepStoredWaits(steps, inputs, live)
	name, description, steps := issueworkflow.Normalize(workflow.Name, workflow.Description, steps)
	catalog, err := workflowSteps.workflowStatusCatalog(ctx, queries, workflow.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := issueworkflow.Validate(name, description, steps, workflow.IsDefault, catalog); err != nil {
		return nil, invalidWorkflowSteps("invalid_workflow", err.Error())
	}
	if message, err := workflowSteps.refuseNewPRMergedEdge(ctx, queries, workflow.WorkspaceID, live, steps); err != nil {
		return nil, err
	} else if message != "" {
		return nil, invalidWorkflowSteps("invalid_workflow", message)
	}
	kept := make([]string, len(steps))
	for i, step := range steps {
		kept[i] = step.StatusKey
	}
	for _, step := range live {
		if !slices.Contains(kept, step.StatusKey) {
			return nil, invalidWorkflowSteps("status_removed", "this action may not remove a status; use the workflow editor")
		}
	}
	if status, message := validateChangedHandlers(ctx, queries, workflow.WorkspaceID, live, steps, func(db.Agent) bool { return true }); status != 0 {
		return nil, decisionIntentRefusal{status, "invalid_handler: " + message}
	}
	if message, err := workflowSteps.subWorkflowTargetsMessage(ctx, queries, workflow.WorkspaceID, workflow.ID, steps); err != nil {
		return nil, err
	} else if message != "" {
		return nil, invalidWorkflowSteps("invalid_workflow", message)
	}
	if message, err := missingWebhookEndpoint(ctx, queries, workflow.WorkspaceID, workflow.ID, steps); err != nil {
		return nil, err
	} else if message != "" {
		return nil, invalidWorkflowSteps("invalid_workflow", message)
	}
	encoded, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		return nil, errors.Join(errors.New("encode workflow steps"), err)
	}
	return issueworkflow.DecodeSteps(encoded)
}

// validateChangedHandlers is validateHandoffHandlers for a write with no HTTP
// caller: every handler the steps add must be a member, agent or squad of the
// workspace that is not archived, and canInvoke must admit each agent. Only
// changed handlers are checked, so an edit never fails on a handler someone
// else picked. The request has no approver yet, so it passes a gate that admits
// everyone and leaves the invoke check to the approval.
func validateChangedHandlers(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, previous, steps []issueworkflow.Step, canInvoke func(db.Agent) bool) (int, string) {
	before := map[issueworkflow.Handler]bool{}
	for _, step := range previous {
		before[step.Handler] = true
	}
	checker := &Handler{Queries: queries}
	for _, step := range steps {
		switch step.Handler.Type {
		case issueworkflow.HandlerAgent, issueworkflow.HandlerSquad, issueworkflow.HandlerMember:
		default:
			continue
		}
		if before[step.Handler] {
			continue
		}
		id, err := parseUUIDString(step.Handler.ID)
		if err != nil {
			return http.StatusBadRequest, "invalid handler id"
		}
		if status, message := checker.validateAssignee(ctx, uuidToString(workspaceID), pgtype.Text{String: step.Handler.Type, Valid: true}, id, canInvoke); status != 0 {
			return status, message
		}
	}
	return 0, ""
}
