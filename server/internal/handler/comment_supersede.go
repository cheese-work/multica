package handler

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const maxSupersedes = 20

const (
	supersedeSkipHumanReply   = "human_reply"
	supersedeSkipThreadChange = "thread_changed"
)

// supersedeTarget is one thread a new comment supersedes, keyed by its root.
type supersedeTarget struct {
	root db.Comment
}

// loadSupersedeTargets validates the --supersedes ids before the insert
// transaction opens and returns the distinct thread roots they name, ordered by
// id so concurrent supersedes lock threads in one order. It writes the error
// response itself. A reply id names its whole thread. Naming the thread the new
// comment replies into is rejected: resolving it would fold the new comment
// away.
func (h *Handler) loadSupersedeTargets(w http.ResponseWriter, ctx context.Context, issue db.Issue, ids []string, parentID pgtype.UUID) ([]supersedeTarget, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	if len(ids) > maxSupersedes {
		writeError(w, http.StatusBadRequest, "supersedes accepts at most 20 comments")
		return nil, false
	}
	parsed, ok := parseUUIDSliceOrBadRequest(w, ids, "supersedes")
	if !ok {
		return nil, false
	}
	var parentRoot string
	if parentID.Valid {
		root, err := h.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: parentID, WorkspaceID: issue.WorkspaceID})
		if err == nil {
			parentRoot = uuidToString(root.ID)
		}
	}
	seen := map[string]bool{}
	var targets []supersedeTarget
	for _, id := range parsed {
		c, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err != nil || uuidToString(c.IssueID) != uuidToString(issue.ID) {
			writeError(w, http.StatusBadRequest, "supersedes "+uuidToString(id)+" is not a comment on this issue")
			return nil, false
		}
		root, err := h.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: c.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			writeError(w, http.StatusBadRequest, "supersedes "+uuidToString(id)+" is not a comment on this issue")
			return nil, false
		}
		rootID := uuidToString(root.ID)
		if rootID == parentRoot {
			writeError(w, http.StatusBadRequest, "supersedes cannot name the thread this comment replies into")
			return nil, false
		}
		if seen[rootID] {
			continue
		}
		seen[rootID] = true
		targets = append(targets, supersedeTarget{root: root})
	}
	sort.Slice(targets, func(i, j int) bool {
		return uuidToString(targets[i].root.ID) < uuidToString(targets[j].root.ID)
	})
	return targets, true
}

// supersedeOutcome is the result of superseding one thread inside the comment
// transaction. newlyResolved and cleared drive the post-commit events.
type supersedeOutcome struct {
	result        SupersedeResult
	root          db.Comment
	newlyResolved bool
	cleared       []db.Comment
}

// applySupersedes links each target thread to the new comment and resolves it,
// inside the comment's insert transaction. A thread stays open when a human
// posted its newest comment, or when any comment landed in it after the request
// began (the same guard as resolve --known-as-of); the link is stored either way.
func applySupersedes(ctx context.Context, qtx *db.Queries, newComment db.Comment, targets []supersedeTarget, requestStart time.Time, resolverType string, resolverID pgtype.UUID) ([]supersedeOutcome, error) {
	outcomes := make([]supersedeOutcome, 0, len(targets))
	for _, t := range targets {
		root := t.root
		if err := qtx.LockCommentThread(ctx, root.ID); err != nil {
			return nil, err
		}
		if err := qtx.UpsertCommentSupersession(ctx, db.UpsertCommentSupersessionParams{
			SupersededCommentID:  root.ID,
			SupersedingCommentID: newComment.ID,
			WorkspaceID:          newComment.WorkspaceID,
			IssueID:              newComment.IssueID,
		}); err != nil {
			return nil, err
		}
		out := supersedeOutcome{root: root, result: SupersedeResult{CommentID: uuidToString(root.ID)}}

		latest, err := qtx.GetThreadLatestComment(ctx, db.GetThreadLatestCommentParams{
			TargetID: root.ID, IssueID: root.IssueID, WorkspaceID: root.WorkspaceID,
		})
		if err != nil {
			return nil, err
		}
		newer, err := qtx.CountThreadCommentsSince(ctx, db.CountThreadCommentsSinceParams{
			TargetID: root.ID, Since: pgtype.Timestamptz{Time: requestStart, Valid: true},
			IssueID: root.IssueID, WorkspaceID: root.WorkspaceID,
		})
		if err != nil {
			return nil, err
		}
		switch {
		case latest.AuthorType == "member":
			out.result.SkippedReason = supersedeSkipHumanReply
		case newer > 0:
			out.result.SkippedReason = supersedeSkipThreadChange
		default:
			out.cleared, err = qtx.ClearOtherThreadResolutions(ctx, db.ClearOtherThreadResolutionsParams{
				TargetID: root.ID, IssueID: root.IssueID, WorkspaceID: root.WorkspaceID,
			})
			if err != nil {
				return nil, err
			}
			resolved, err := qtx.ResolveComment(ctx, db.ResolveCommentParams{
				ID:             root.ID,
				ResolvedByType: pgtype.Text{String: resolverType, Valid: true},
				ResolvedByID:   resolverID,
			})
			if err != nil {
				return nil, err
			}
			out.newlyResolved = !root.ResolvedAt.Valid
			out.root = resolved
			out.result.Resolved = true
		}
		outcomes = append(outcomes, out)
	}
	return outcomes, nil
}

// publishSupersedeEvents emits, after commit, comment:resolved for each thread
// the supersede resolved and comment:unresolved for any resolution it replaced.
func (h *Handler) publishSupersedeEvents(r *http.Request, outcomes []supersedeOutcome, newCommentID string, workspaceID, actorType, actorID string) {
	for _, o := range outcomes {
		for _, c := range o.cleared {
			cid := uuidToString(c.ID)
			resp := h.withDecisionFields(r.Context(), c, commentToResponse(c, h.groupReactions(r, []pgtype.UUID{c.ID})[cid], h.groupAttachments(r, []pgtype.UUID{c.ID})[cid]))
			h.publish(protocol.EventCommentUnresolved, workspaceID, actorType, actorID, map[string]any{"comment": resp})
		}
		if !o.newlyResolved {
			continue
		}
		cid := uuidToString(o.root.ID)
		resp := h.withDecisionFields(r.Context(), o.root, commentToResponse(o.root, h.groupReactions(r, []pgtype.UUID{o.root.ID})[cid], h.groupAttachments(r, []pgtype.UUID{o.root.ID})[cid]))
		resp.SupersededByID = &newCommentID
		h.publish(protocol.EventCommentResolved, workspaceID, actorType, actorID, map[string]any{"comment": resp})
	}
}

// attachSupersessions fills SupersededByID and Supersedes on the listed
// comments from the issue's supersession links.
func (h *Handler) attachSupersessions(ctx context.Context, issue db.Issue, resp []CommentResponse) {
	if len(resp) == 0 {
		return
	}
	rows, err := h.Queries.ListCommentSupersessionsForIssue(ctx, db.ListCommentSupersessionsForIssueParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if err != nil || len(rows) == 0 {
		return
	}
	supersededBy := make(map[string]string, len(rows))
	supersedes := make(map[string][]string, len(rows))
	for _, row := range rows {
		old, by := uuidToString(row.SupersededCommentID), uuidToString(row.SupersedingCommentID)
		supersededBy[old] = by
		supersedes[by] = append(supersedes[by], old)
	}
	for i := range resp {
		if by, ok := supersededBy[resp[i].ID]; ok {
			resp[i].SupersededByID = &by
		}
		for _, old := range supersedes[resp[i].ID] {
			resp[i].Supersedes = append(resp[i].Supersedes, SupersedeResult{CommentID: old})
		}
	}
}
