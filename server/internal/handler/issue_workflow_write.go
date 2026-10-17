package handler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// errWorkflowChanged signals that the project's workflow, or the issue's place
// in it, changed between the write's workflow decision and its transaction. The
// request was valid when it arrived; retrying decides against the new state.
var errWorkflowChanged = errors.New("the project workflow changed while the write was in flight")

// workflowWrite is the workflow decision of one issue write, kept so the write
// transaction can check it again under locks. The decision runs before the
// transaction, on an unlocked read: a workflow edit or project switch can
// commit after it, and a member can move the issue before the write lands.
//
// Locks, in the order every writer takes them: the status catalog (shared; a
// workflow edit holds it exclusively), the project row (key share; a workflow
// switch holds it for update), then the issue row.
type workflowWrite struct {
	prev db.Issue
	// pre is the update before the workflow rewrote it, got after.
	pre, got                                 db.UpdateIssueParams
	handoff                                  *workflowHandoff
	statusExplicit, assigneeTouched, enterOn bool
	run                                      *db.AgentTaskQueue
}

// decideIssueWorkflow applies the project's workflow to params, like
// applyIssueWorkflow, and returns what the write transaction needs to verify
// the decision. The write is nil when the issue lands in no project.
func (h *Handler) decideIssueWorkflow(ctx context.Context, prev db.Issue, params *db.UpdateIssueParams, statusExplicit, assigneeTouched, enterOnMove bool, run *db.AgentTaskQueue) (*workflowHandoff, *workflowWrite, error) {
	write := &workflowWrite{prev: prev, pre: *params, statusExplicit: statusExplicit, assigneeTouched: assigneeTouched, enterOn: enterOnMove, run: run}
	handoff, err := h.applyIssueWorkflow(ctx, h.Queries, prev, params, statusExplicit, assigneeTouched, enterOnMove, run)
	if err != nil || !params.ProjectID.Valid {
		return handoff, nil, err
	}
	write.got, write.handoff = *params, handoff
	return handoff, write, nil
}

// lockProject takes the catalog and project locks. Call it before any row lock.
func (w *workflowWrite) lockProject(ctx context.Context, q *db.Queries) error {
	if w == nil {
		return nil
	}
	if err := q.LockIssueStatusCatalogShared(ctx, w.prev.WorkspaceID); err != nil {
		return err
	}
	if _, err := q.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: w.pre.ProjectID, WorkspaceID: w.prev.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errWorkflowChanged
		}
		return err
	}
	return nil
}

// verify decides the write again against the locked issue row and the
// workflow as it stands under the locks. A decision that now fails returns its
// own error (status not in the workflow, step moved); one that now comes out
// differently returns errWorkflowChanged.
func (w *workflowWrite) verify(ctx context.Context, h *Handler, q *db.Queries, locked db.Issue) error {
	if w == nil {
		return nil
	}
	params := w.pre
	handoff, err := h.applyIssueWorkflow(ctx, q, locked, &params, w.statusExplicit, w.assigneeTouched, w.enterOn, w.run)
	if err != nil {
		return err
	}
	if params.Status != w.got.Status || params.AssigneeType != w.got.AssigneeType || params.AssigneeID != w.got.AssigneeID ||
		(handoff == nil) != (w.handoff == nil) {
		return errWorkflowChanged
	}
	return nil
}

// lockAndVerify is lockProject, the issue row lock and verify, for writes that
// are not already locking the issue row themselves.
func (w *workflowWrite) lockAndVerify(ctx context.Context, h *Handler, q *db.Queries) error {
	if w == nil {
		return nil
	}
	if err := w.lockProject(ctx, q); err != nil {
		return err
	}
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: w.prev.ID, WorkspaceID: w.prev.WorkspaceID})
	if err != nil {
		return err
	}
	return w.verify(ctx, h, q, locked)
}
