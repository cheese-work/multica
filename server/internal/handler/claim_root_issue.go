package handler

import (
	"context"
	"log/slog"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxRootIssueHops bounds the parent walk. A real tree is a few levels deep;
// the cap only stops a corrupt parent cycle from looping.
const maxRootIssueHops = 16

// resolveRootIssueNumber returns the number of the root of an issue tree
// (CHE-6498 S6a). selfNumber is the claimed issue and parentID its parent
// ("" for a root issue, which is its own root). lookup returns an issue's
// number and parent id ("" when it has none). It returns 0, meaning unknown,
// when the chain is unreadable or cyclic: the daemon then logs the arm as
// unknown instead of guessing a wrong one.
func resolveRootIssueNumber(ctx context.Context, selfNumber int32, parentID string, lookup func(context.Context, string) (int32, string, error)) int32 {
	root := selfNumber
	for hops := 0; parentID != ""; hops++ {
		if hops >= maxRootIssueHops {
			return 0
		}
		number, next, err := lookup(ctx, parentID)
		if err != nil {
			return 0
		}
		root, parentID = number, next
	}
	return root
}

// claimRootIssueNumber walks a claimed issue's parent chain in its workspace.
func (h *Handler) claimRootIssueNumber(ctx context.Context, issue db.Issue) int32 {
	lookup := func(ctx context.Context, id string) (int32, string, error) {
		parent, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
			ID:          parseUUID(id),
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			slog.Warn("task claim: failed to walk parent chain", "parent_issue_id", id, "error", err)
			return 0, "", err
		}
		next := ""
		if parent.ParentIssueID.Valid {
			next = uuidToString(parent.ParentIssueID)
		}
		return parent.Number, next, nil
	}
	parentID := ""
	if issue.ParentIssueID.Valid {
		parentID = uuidToString(issue.ParentIssueID)
	}
	return resolveRootIssueNumber(ctx, issue.Number, parentID, lookup)
}
