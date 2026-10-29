package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Sub-workflow step (CHE-2031): the edge watch of an issue on a step with a
// sub_workflow spawns the step's child before it reads the join, and the join
// counts only the children of the step's stage.

// maxSubWorkflowDepth bounds how many sub-workflow levels deep an issue may
// spawn: a stored definition that loops would otherwise grow forever.
const maxSubWorkflowDepth = 5

// errSubWorkflowMissing: the step names a workflow this workspace does not have.
var errSubWorkflowMissing = errors.New("sub-workflow not found in this workspace")

// errSubWorkflowRefused: the child cannot be made as the workflow stands, and
// a retry would not change that (its start-step handler fails the assignee
// check). The watch logs it and stops asking.
var errSubWorkflowRefused = errors.New("sub-workflow child refused")

// A spawned child is created unassigned, with no handler resolved for its start
// step, so no run starts for it. Auto-assigning it needs the trusted originator
// of the spawn, which the edge path does not carry (no request, no task), and a
// parent link a caller can set by hand is not proof of a spawn. Unattended
// child-agent execution is deferred until the spawn records that provenance
// (CHE-2031, scope decision A). The child is still made, joins the parent's
// stage and closes the join when it is done; a person or a later step moves it.

// subWorkflowTargetsMessage checks every sub-workflow step of a definition
// being saved: its workflow must exist in the workspace, and following the
// sub-workflows from it must never lead back to the workflow being saved
// (selfID; invalid on create, where nothing can name it yet). It returns the
// 400 message, or "" when the steps are fine.
func (h *Handler) subWorkflowTargetsMessage(ctx context.Context, qtx *db.Queries, workspaceID, selfID pgtype.UUID, steps []issueworkflow.Step) (string, error) {
	seen := map[pgtype.UUID]bool{}
	for _, s := range steps {
		if s.SubWorkflow == nil {
			continue
		}
		id, err := parseUUIDString(s.SubWorkflow.WorkflowID)
		if err != nil {
			return fmt.Sprintf("step %s: sub-workflow id is not a valid id", s.StatusKey), nil
		}
		if selfID.Valid && id == selfID {
			return fmt.Sprintf("step %s: a workflow cannot be its own sub-workflow", s.StatusKey), nil
		}
		reaches, err := h.subWorkflowReaches(ctx, qtx, workspaceID, id, selfID, seen)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Sprintf("step %s: sub-workflow not found in this workspace", s.StatusKey), nil
		}
		if err != nil {
			return "", err
		}
		if reaches {
			return fmt.Sprintf("step %s: sub-workflow leads back to this workflow", s.StatusKey), nil
		}
	}
	return "", nil
}

// subWorkflowReaches reports whether workflow id, or any workflow it runs as a
// sub-workflow, is target. Each workflow is read once (seen). A missing
// workflow is pgx.ErrNoRows.
func (h *Handler) subWorkflowReaches(ctx context.Context, qtx *db.Queries, workspaceID, id, target pgtype.UUID, seen map[pgtype.UUID]bool) (bool, error) {
	if seen[id] {
		return false, nil
	}
	seen[id] = true
	row, err := qtx.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: id, WorkspaceID: workspaceID})
	if err != nil {
		return false, err
	}
	def, err := issueworkflow.Decode(row)
	if err != nil {
		return false, err
	}
	for _, s := range def.Steps {
		if s.SubWorkflow == nil {
			continue
		}
		next, err := parseUUIDString(s.SubWorkflow.WorkflowID)
		if err != nil {
			continue
		}
		if target.Valid && next == target {
			return true, nil
		}
		if reaches, err := h.subWorkflowReaches(ctx, qtx, workspaceID, next, target, seen); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		} else if reaches {
			return true, nil
		}
	}
	return false, nil
}

// subWorkflowDepth counts the sub-workflow children above the issue: ancestors
// that were themselves spawned onto a workflow in a stage.
func (h *Handler) subWorkflowDepth(ctx context.Context, issue db.Issue) (int, error) {
	depth := 0
	for cur := issue; cur.ParentIssueID.Valid && depth <= maxSubWorkflowDepth; {
		parent, err := h.Queries.GetIssue(ctx, cur.ParentIssueID)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return 0, err
		}
		if parent.WorkflowID.Valid && parent.Stage.Valid {
			depth++
		}
		cur = parent
	}
	return depth, nil
}

// spawnSubWorkflowChild gives the issue the child its sub-workflow step starts:
// one child of the issue on the target workflow in the step's stage. A child
// already there (an earlier pass, a re-entry, one made by hand) is the child,
// so a second pass makes none. The child starts at the workflow's start status
// with that step's handler, admitted like any new issue.
func (h *Handler) spawnSubWorkflowChild(ctx context.Context, parent db.Issue, sub issueworkflow.SubWorkflow) error {
	targetID, err := parseUUIDString(sub.WorkflowID)
	if err != nil {
		return errSubWorkflowMissing
	}
	row, err := h.Queries.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: targetID, WorkspaceID: parent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errSubWorkflowMissing
	}
	if err != nil {
		return err
	}
	children, err := h.Queries.ListChildIssues(ctx, parent.ID)
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.WorkflowID == targetID && c.Stage.Valid && c.Stage.Int32 == int32(sub.Stage) {
			return nil
		}
	}
	depth, err := h.subWorkflowDepth(ctx, parent)
	if err != nil {
		return err
	}
	if depth >= maxSubWorkflowDepth {
		return fmt.Errorf("%w: already %d sub-workflow levels deep", errSubWorkflowRefused, depth)
	}
	def, err := issueworkflow.Decode(row)
	if err != nil {
		return err
	}
	status := issueworkflow.StartStatus
	if !def.Has(status) && len(def.Steps) > 0 {
		status = def.Steps[0].StatusKey
	}
	_, err = h.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:     parent.WorkspaceID,
		Title:           parent.Title + " — " + def.Name,
		Status:          status,
		Priority:        parent.Priority,
		CreatorType:     parent.CreatorType,
		CreatorID:       parent.CreatorID,
		RequestedByID:   parent.RequestedByID,
		ParentIssueID:   parent.ID,
		ProjectID:       parent.ProjectID,
		WorkflowID:      targetID,
		Stage:           pgtype.Int4{Int32: int32(sub.Stage), Valid: true},
		AllowDuplicate:  true,
	}, service.IssueCreateOpts{
		ActorID: uuidToString(parent.CreatorID),
		BroadcastPayload: func(issue db.Issue, _ []db.Attachment, _ []db.IssueLabel) map[string]any {
			return map[string]any{"issue": h.issueResponseWithCategory(ctx, issue)}
		},
	})
	return err
}
