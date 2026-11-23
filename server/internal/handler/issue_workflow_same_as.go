package handler

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The same_as handler (CHE-9114) hands an issue to the agent that last handled
// another step on it: the issue's current assignee when it sits in that step,
// else the assignee it had when it last left the step (issue_step_assignee
// under issueworkflow.LeftStepKey). An agent that is archived or gone counts
// as not found. Then the step's own agent id applies, then the named step's
// own handler, so a step never lacks an assignee for want of history.

// sameAsHumans fills Humans.SameAs for a same_as step. A failed read is an
// error, so the write fails instead of handing the issue to the wrong agent.
func (h *Handler) sameAsHumans(ctx context.Context, q *db.Queries, step issueworkflow.Step, project db.Project, workspaceID, requestedByID pgtype.UUID, subject workflowSubject) (issueworkflow.Humans, error) {
	handled, err := h.lastHandler(ctx, q, workspaceID, subject, step.Handler.Step)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	if handled != nil {
		return issueworkflow.Humans{SameAsType: handled.Type, SameAsID: handled.ID}, nil
	}
	if step.Handler.ID != "" {
		return issueworkflow.Humans{}, nil // ResolveHandler uses the step's own agent
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, workspaceID, subject.WorkflowID, subject.projectOr(project))
	if err != nil || def == nil {
		return issueworkflow.Humans{}, err
	}
	named, ok := def.Step(step.Handler.Step)
	if !ok || !named.HandsOff() {
		return issueworkflow.Humans{}, nil
	}
	humans, err := h.workflowHumans(ctx, q, named, project, workspaceID, requestedByID, subject)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	typ, id, ok := issueworkflow.ResolveHandler(named, project, "", pgtype.UUID{}, humans)
	if !ok || named.Handler.Type == issueworkflow.HandlerReviewer {
		return issueworkflow.Humans{}, nil
	}
	return issueworkflow.Humans{SameAsType: typ, SameAsID: id}, nil
}

// lastHandler is the live agent that last handled a status on the issue; nil
// when none is on record or it can no longer take work.
func (h *Handler) lastHandler(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, subject workflowSubject, status string) (*issueworkflow.StepAssignee, error) {
	var found *issueworkflow.StepAssignee
	switch {
	case !subject.ID.Valid:
		return nil, nil
	case subject.Status == status && subject.AssigneeID.Valid:
		found = &issueworkflow.StepAssignee{Type: subject.AssigneeType, ID: subject.AssigneeID}
	default:
		var err error
		if found, err = issueworkflow.StickyStepAssignee(ctx, q, workspaceID, subject.ID, issueworkflow.LeftStepKey(status)); err != nil {
			return nil, err
		}
	}
	if found == nil || found.Type != issueworkflow.HandlerAgent {
		return nil, nil
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: found.ID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

// recordLeft keeps who an issue had when it leaves a status, in the
// transaction that moves it, for a same_as step to read later. Only an agent
// is kept, the only assignee a same_as step can resolve to, and only when a
// step of the workflow follows the status.
func (w *workflowWrite) recordLeft(ctx context.Context, q *db.Queries, moved db.Issue) error {
	if w == nil || moved.Status == w.prev.Status || !w.prev.AssigneeID.Valid || w.prev.AssigneeType.String != issueworkflow.HandlerAgent {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, moved.WorkspaceID, moved.WorkflowID, moved.ProjectID)
	if err != nil || def == nil {
		return err
	}
	if !slices.ContainsFunc(def.Steps, func(s issueworkflow.Step) bool {
		return s.Handler.Type == issueworkflow.HandlerSameAs && s.Handler.Step == w.prev.Status
	}) {
		return nil
	}
	var setBy pgtype.UUID
	if id, err := util.ParseUUID(w.entry.actorID); err == nil {
		setBy = id
	}
	left := issueworkflow.StepAssignee{Type: w.prev.AssigneeType.String, ID: w.prev.AssigneeID}
	return issueworkflow.SetStepAssignee(ctx, q, moved.WorkspaceID, moved.ID, issueworkflow.LeftStepKey(w.prev.Status), left, w.entry.actorType, setBy)
}
