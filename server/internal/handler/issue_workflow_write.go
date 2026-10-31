package handler

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// errWorkflowChanged signals that the project's workflow, or the issue's place
// in it, changed between the write's workflow decision and its transaction. The
// request was valid when it arrived; retrying decides against the new state.
var errWorkflowChanged = errors.New("the project workflow changed while the write was in flight")

// errHandoffPreviewStale signals that the issue or its workflow no longer match
// what the person saw when they confirmed a handoff: who the issue is taken
// from, or who it goes to. The write is refused whole; a fresh preview decides.
var errHandoffPreviewStale = errors.New("the issue or its workflow changed since the handoff was previewed")

// HandoffPreviewBinding is what a handoff confirmation was shown: the issue's
// assignee at preview time and the handler the workflow resolved. A client
// sends it with `handoff_preview`; absent, the write keeps its earlier
// behavior and acts on the issue as it finds it. A null handler binds a preview
// that found no handoff.
type HandoffPreviewBinding struct {
	PreviousAssigneeType *string `json:"previous_assignee_type"`
	PreviousAssigneeID   *string `json:"previous_assignee_id"`
	HandlerType          *string `json:"handler_type"`
	HandlerID            *string `json:"handler_id"`
}

func sameOptional(a *string, b string) bool {
	if a == nil {
		return b == ""
	}
	return *a == b
}

// workflowWrite is the workflow decision of one issue write, kept so the write
// transaction can check it again under locks. The decision runs before the
// transaction, on an unlocked read: a workflow edit or project switch can
// commit after it, and a member can move the issue, to another project too,
// before the write lands.
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
	caller                                   workflowCaller
	// preview is what the person confirmed, when the client sent it.
	preview *HandoffPreviewBinding
	// stopPrevious is the caller's request to stop the previous assignee's runs.
	// verify then records, under the issue lock, the runs that existed and who
	// held the issue, so the cancellation after the commit reaches those runs
	// and no later ones.
	stopPrevious bool
	stopRuns     []db.AgentTaskQueue
	stopFrom     service.TaskAssignee
	// system marks a move the platform makes by itself (edge, CI, run budget).
	// Step entry gates bind explicit status writes only.
	system bool
	entry  entryWrite
}

// decideIssueWorkflow applies the project's workflow to params, like
// applyIssueWorkflow, and returns what the write transaction needs to verify
// the decision. An issue that lands in no project still gets a write: it
// checks that no one moved the issue into a project in the meantime.
func (h *Handler) decideIssueWorkflow(ctx context.Context, prev db.Issue, params *db.UpdateIssueParams, statusExplicit, assigneeTouched, enterOnMove bool, caller workflowCaller) (*workflowHandoff, *workflowWrite, error) {
	write := &workflowWrite{prev: prev, pre: *params, statusExplicit: statusExplicit, assigneeTouched: assigneeTouched, enterOn: enterOnMove, caller: caller}
	handoff, err := h.applyIssueWorkflow(ctx, h.Queries, prev, params, statusExplicit, assigneeTouched, enterOnMove, caller)
	if err != nil {
		return handoff, nil, err
	}
	write.got, write.handoff = *params, handoff
	return handoff, write, nil
}

// lockProject takes the catalog and project locks of the project the write
// lands the issue in. Call it before any row lock.
func (w *workflowWrite) lockProject(ctx context.Context, q *db.Queries) error {
	if w == nil || (!w.pre.ProjectID.Valid && !w.prev.ProjectID.Valid && !w.prev.WorkflowID.Valid) {
		return nil
	}
	// An override reads the workflow row, so a workflow edit (exclusive on the
	// catalog) must not interleave, in a project or not.
	if err := q.LockIssueStatusCatalogShared(ctx, w.prev.WorkspaceID); err != nil {
		return err
	}
	projectIDs := []pgtype.UUID{w.pre.ProjectID}
	if w.prev.ProjectID != w.pre.ProjectID {
		projectIDs = append(projectIDs, w.prev.ProjectID)
	}
	sort.Slice(projectIDs, func(first, second int) bool {
		return uuidToString(projectIDs[first]) < uuidToString(projectIDs[second])
	})
	for _, projectID := range projectIDs {
		if !projectID.Valid {
			continue
		}
		if _, err := q.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: projectID, WorkspaceID: w.prev.WorkspaceID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if projectID == w.pre.ProjectID {
					return errWorkflowChanged
				}
				continue
			}
			return err
		}
	}
	return nil
}

// verify decides the write again against the locked issue row and the
// workflow as it stands under the locks. A decision that now fails returns its
// own error (status not in the workflow, step moved); one that now comes out
// differently returns errWorkflowChanged.
//
// The locks cover the project the write decided for. An issue someone moved to
// another project since then sits under a workflow this write neither locked
// nor judged, and a write that persisted it would validate under one project
// and land in another, or move the issue back. It is refused instead: the
// retry decides for the project the issue is in. Taking the new project's
// lock here would invert the lock order against a workflow switch.
func (w *workflowWrite) verify(ctx context.Context, h *Handler, q *db.Queries, locked db.Issue) error {
	if w == nil {
		return nil
	}
	if locked.ProjectID != w.prev.ProjectID {
		return errWorkflowChanged
	}
	params := w.pre
	handoff, err := h.applyIssueWorkflow(ctx, q, locked, &params, w.statusExplicit, w.assigneeTouched, w.enterOn, w.caller)
	if err != nil {
		return err
	}
	if params.Status != w.got.Status || params.AssigneeType != w.got.AssigneeType || params.AssigneeID != w.got.AssigneeID ||
		(handoff == nil) != (w.handoff == nil) {
		return errWorkflowChanged
	}
	// The confirmation named who the issue was taken from and who it goes to.
	// Checked against the locked row and the workflow as it stands, so a
	// reassignment or a handler edit after the preview cannot make the write
	// stop, or start, a run the person never saw.
	if w.preview != nil && handoff != nil {
		if !sameOptional(w.preview.PreviousAssigneeType, locked.AssigneeType.String) ||
			!sameOptional(w.preview.PreviousAssigneeID, uuidToString(locked.AssigneeID)) ||
			!sameOptional(w.preview.HandlerType, params.AssigneeType.String) ||
			!sameOptional(w.preview.HandlerID, uuidToString(params.AssigneeID)) {
			return errHandoffPreviewStale
		}
	}
	if err := w.checkEntry(ctx, h, q, locked, params); err != nil {
		return err
	}
	w.stopRuns, w.stopFrom = nil, service.TaskAssignee{}
	if w.stopPrevious && handoff != nil &&
		(locked.AssigneeType != params.AssigneeType || locked.AssigneeID != params.AssigneeID) {
		w.stopRuns = h.assigneeActiveRuns(ctx, q, locked)
		w.stopFrom = service.TaskAssignee{Type: locked.AssigneeType.String, ID: locked.AssigneeID}
	}
	return nil
}

// lockIssueAndVerify locks the issue row and verifies the decision, for writes
// that are not already locking the issue row themselves. It follows lockProject
// and any lock that takes several issue rows in id order (a duplicate mark), so
// every writer meets the issue rows in one order.
func (w *workflowWrite) lockIssueAndVerify(ctx context.Context, h *Handler, q *db.Queries) error {
	if w == nil {
		return nil
	}
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: w.prev.ID, WorkspaceID: w.prev.WorkspaceID})
	if err != nil {
		return err
	}
	return w.verify(ctx, h, q, locked)
}
