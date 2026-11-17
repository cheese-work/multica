package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-5489 (S2): a status move can create its handoff comment in the status
// transaction and name existing comments. All of them are context for the run
// the move starts. They give no authority: attribution comes from the status
// actor (CHE-5485).

// HandoffCommentRequest is the comment a status move creates.
type HandoffCommentRequest struct {
	Content string `json:"content"`
}

// handoffCommentSeed is what the status transaction needs to write the comment.
type handoffCommentSeed struct {
	content           string
	authorType        string
	authorID          pgtype.UUID
	sourceTask        pgtype.UUID
	prevType          string
	prevID            pgtype.UUID
	originatorUserID  string
	suppressedAgentID []pgtype.UUID // set when the comment is written: the old and the new assignee
}

// attachedComments is the existing comments a move carries, in order. revisions
// is what the handoff preview showed the person. Every attached comment needs a
// revision there: a comment with none is not bound to what was reviewed.
type attachedComments struct {
	ids       []pgtype.UUID
	revisions map[pgtype.UUID]int64
}

// handoffCommentPlan is what prepareHandoffComment decides for one move.
type handoffCommentPlan struct {
	seed   *handoffCommentSeed
	attach *attachedComments
}

var (
	errAttachedCommentGone  = errors.New("an attached comment is no longer a live comment of this issue")
	errAttachedCommentStale = errors.New("an attached comment changed since the handoff was previewed")
)

// verifyAttachedComments checks the attached comments in the status
// transaction. The issue row is locked by then, and a comment edit or delete
// takes that lock first, so what this reads is what commits.
func verifyAttachedComments(ctx context.Context, q *db.Queries, issue db.Issue, a *attachedComments) error {
	if a == nil {
		return nil
	}
	for _, id := range a.ids {
		c, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (c.IssueID != issue.ID || c.DeletedAt.Valid)) {
			return errAttachedCommentGone
		}
		if err != nil {
			return err
		}
		if want, bound := a.revisions[id]; !bound || want != c.Revision {
			return errAttachedCommentStale
		}
	}
	return nil
}

func writeHandoffCommentError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errAttachedCommentGone):
		writeErrorCode(w, http.StatusBadRequest, "comment_not_on_issue", err.Error())
	case errors.Is(err, errAttachedCommentStale):
		writeErrorCode(w, http.StatusConflict, "stale_comment", err.Error()+"; review the move again")
	default:
		return false
	}
	return true
}

// prepareHandoffComment validates the comment fields of a status move before
// anything is written. It returns the seed of the comment to create (nil when
// none) and the existing comments to attach, in order. A refusal is written to w.
func (h *Handler) prepareHandoffComment(w http.ResponseWriter, r *http.Request, prev db.Issue, req UpdateIssueRequest, actorType, actorID string) (*handoffCommentPlan, bool) {
	if req.HandoffComment == nil && len(req.AttachCommentIDs) == 0 {
		return nil, true
	}
	if req.Status == nil || *req.Status == prev.Status {
		writeErrorCode(w, http.StatusBadRequest, "handoff_comment_needs_status_move", "handoff_comment and attach_comment_ids need a status change")
		return nil, false
	}
	attach, ok := parseUUIDSliceOrBadRequest(w, req.AttachCommentIDs, "attach_comment_ids")
	if !ok {
		return nil, false
	}
	plan := &handoffCommentPlan{attach: &attachedComments{revisions: map[pgtype.UUID]int64{}}}
	if req.HandoffPreview != nil {
		for key, rev := range req.HandoffPreview.CommentRevisions {
			id, err := util.ParseUUID(key)
			if err != nil {
				writeErrorCode(w, http.StatusBadRequest, "invalid_comment_revisions", "handoff_preview.comment_revisions has an invalid comment id")
				return nil, false
			}
			plan.attach.revisions[id] = rev
		}
	}
	seen := map[pgtype.UUID]bool{}
	for _, id := range attach {
		if seen[id] {
			continue
		}
		seen[id] = true
		live, err := service.HandoffCommentBelongsTo(r.Context(), h.Queries, prev, uuidToString(id))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check attached comment")
			return nil, false
		}
		if !live {
			writeErrorCode(w, http.StatusBadRequest, "comment_not_on_issue", "attach_comment_ids names a comment that is not a comment of this issue")
			return nil, false
		}
		if _, shown := plan.attach.revisions[id]; !shown {
			writeErrorCode(w, http.StatusBadRequest, "comment_revision_required", "handoff_preview.comment_revisions needs a revision for every attached comment")
			return nil, false
		}
		plan.attach.ids = append(plan.attach.ids, id)
	}
	if req.HandoffComment == nil {
		return plan, true
	}
	content := sanitizeNullBytes(req.HandoffComment.Content)
	if strings.TrimSpace(content) == "" {
		writeErrorCode(w, http.StatusBadRequest, "handoff_comment_empty", "handoff_comment.content is required")
		return nil, false
	}
	originator := h.invokeOriginatorFromRequest(r, actorType, actorID)
	if h.rejectMissingRequiredSourceLink(w, r, actorType, true, prev, content, "handoff_comment.content") ||
		h.rejectInvalidAgentMentions(r.Context(), w, prev, content, actorType, actorID, originator, nil) {
		return nil, false
	}
	plan.seed = &handoffCommentSeed{
		content: content, authorType: actorType, authorID: optionalActorUUID(actorID),
		prevType: prev.AssigneeType.String, prevID: prev.AssigneeID, originatorUserID: originator,
	}
	if actorType == "agent" {
		if task, ok := h.taskFromRequestHeader(r); ok {
			plan.seed.sourceTask = task.ID
		}
	}
	return plan, true
}

// createHandoffComment writes the move's comment in the status transaction. The
// comment starts no run through the comment path for the old or the new
// assignee: both are named in suppressed_agent_ids, and the post-commit trigger
// skips them. The new assignee's run is the handoff run.
func createHandoffComment(ctx context.Context, q *db.Queries, rec *statusRecord, issue db.Issue) error {
	seed := rec.comment
	if rec.created != nil {
		return nil
	}
	suppress := []pgtype.UUID{}
	for _, t := range []struct {
		kind string
		id   pgtype.UUID
	}{{seed.prevType, seed.prevID}, {issue.AssigneeType.String, issue.AssigneeID}} {
		switch {
		case !t.id.Valid:
		case t.kind == "agent":
			suppress = append(suppress, t.id)
		case t.kind == "squad":
			squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: t.id, WorkspaceID: issue.WorkspaceID})
			if err == nil && squad.LeaderID.Valid {
				suppress = append(suppress, squad.LeaderID)
			}
		}
	}
	seed.suppressedAgentID = suppress
	row, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID:                 dbid.NewV7(),
		IssueID:            issue.ID,
		WorkspaceID:        issue.WorkspaceID,
		AuthorType:         seed.authorType,
		AuthorID:           seed.authorID,
		Content:            seed.content,
		Type:               "comment",
		SourceTaskID:       seed.sourceTask,
		SuppressedAgentIds: suppress,
	})
	if err != nil {
		return fmt.Errorf("create handoff comment: %w", err)
	}
	rec.created = &row
	return nil
}

// handoffContextIDs is the run's handoff context in order: the comment the move
// created, then the existing ones.
func handoffContextIDs(rec *statusRecord, existing []pgtype.UUID) []pgtype.UUID {
	if rec == nil || rec.created == nil {
		return existing
	}
	return append([]pgtype.UUID{rec.created.ID}, existing...) // fresh slice: existing is never aliased
}

// publishHandoffComment announces the created comment and starts the agents it
// names. Only explicit mentions fire, under the comment's own author: the old
// and the new assignee are suppressed.
func (h *Handler) publishHandoffComment(ctx context.Context, issue db.Issue, rec *statusRecord, actorType, actorID string) {
	if rec == nil || rec.created == nil || rec.comment == nil {
		return
	}
	comment := rec.created.Comment()
	resp := commentToResponse(comment, nil, nil)
	resp.IssueRevision = rec.created.IssueRevision
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), actorType, actorID, map[string]any{
		"comment":             resp,
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      rec.created.IssueRevision,
	})
	h.triggerTasksForComment(ctx, issue, comment, nil, actorType, actorID, rec.comment.originatorUserID, rec.comment.suppressedAgentID, commentSteerRequest{})
}
