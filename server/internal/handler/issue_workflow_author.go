package handler

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/capabilityrouting"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The author handler (CHE-7677) resolves an issue's author in this order: the
// sticky row under issueworkflow.AuthorStepKey, then capability_routing for the
// issue's type, then nobody. The first resolution is written back as the
// sticky row, so a later change to the table keeps the author; a person who
// reassigns the issue while it sits in an author step replaces it.

// workflowSubject is the issue a handler resolves for. ID is invalid for an
// issue that does not exist yet (a create), which has no sticky author.
type workflowSubject struct {
	ID    pgtype.UUID
	Title string
}

func subjectOf(issue db.Issue) workflowSubject {
	return workflowSubject{ID: issue.ID, Title: issue.Title}
}

// authorHumans fills Humans.Author for an author step. A failed read is an
// error, so the write fails instead of leaving the issue unassigned; a missing
// row or table entry is not.
func authorHumans(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, subject workflowSubject) (issueworkflow.Humans, error) {
	if subject.ID.Valid {
		sticky, err := issueworkflow.StickyStepAssignee(ctx, q, workspaceID, subject.ID, issueworkflow.AuthorStepKey)
		if err != nil {
			return issueworkflow.Humans{}, err
		}
		if sticky != nil {
			return issueworkflow.Humans{AuthorType: sticky.Type, AuthorID: sticky.ID}, nil
		}
	}
	typ, ok := issueworkflow.TitleType(subject.Title)
	if !ok {
		return issueworkflow.Humans{}, nil
	}
	row, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	var settings map[string]any
	_ = json.Unmarshal(row.Settings, &settings)
	entry, ok := capabilityrouting.FromSettings(settings)[typ]
	if !ok {
		return issueworkflow.Humans{}, nil
	}
	id, err := util.ParseUUID(entry.Author)
	if err != nil {
		return issueworkflow.Humans{}, nil
	}
	return issueworkflow.Humans{AuthorType: issueworkflow.HandlerAgent, AuthorID: id}, nil
}

// recordAuthor keeps the issue's author in the transaction that moves it: the
// assignee an author step handed the issue to, or the one a person gave the
// issue while it sits in an author step.
func (w *workflowWrite) recordAuthor(ctx context.Context, q *db.Queries, moved db.Issue) error {
	if w == nil || !moved.AssigneeID.Valid || !moved.AssigneeType.Valid {
		return nil
	}
	switch {
	case w.handoff != nil:
		if w.handoff.step.Handler.Type != issueworkflow.HandlerAuthor {
			return nil
		}
	case w.assigneeTouched && !w.caller.agent &&
		(moved.AssigneeType != w.prev.AssigneeType || moved.AssigneeID != w.prev.AssigneeID):
		def, _, err := issueworkflow.ForIssue(ctx, q, moved.WorkspaceID, moved.WorkflowID, moved.ProjectID)
		if err != nil || def == nil {
			return err
		}
		if step, ok := def.Step(moved.Status); !ok || step.Handler.Type != issueworkflow.HandlerAuthor {
			return nil
		}
	default:
		return nil
	}
	return w.setAuthor(ctx, q, moved)
}

func (w *workflowWrite) setAuthor(ctx context.Context, q *db.Queries, moved db.Issue) error {
	var setBy pgtype.UUID
	if id, err := util.ParseUUID(w.entry.actorID); err == nil {
		setBy = id
	}
	author := issueworkflow.StepAssignee{Type: moved.AssigneeType.String, ID: moved.AssigneeID}
	return issueworkflow.SetStepAssignee(ctx, q, moved.WorkspaceID, moved.ID, issueworkflow.AuthorStepKey, author, w.entry.actorType, setBy)
}
