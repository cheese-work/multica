package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// triggerThreadContextReplies caps the replies shown after the thread root.
	triggerThreadContextReplies = 5
	// triggerThreadContextMessageRunes caps one message so a long comment cannot
	// crowd out the rest of the thread.
	triggerThreadContextMessageRunes = 1500
)

// buildTriggerThreadContext renders the triggering comment's thread — the root
// plus the last few earlier replies, oldest first — as plain text for the run
// prompt (CHE-2070). The trigger itself is excluded: the prompt already
// embeds it. Best-effort: any read error yields "" and the prompt falls back
// to the comment-list hint.
func (h *Handler) buildTriggerThreadContext(ctx context.Context, trigger db.Comment, rootID pgtype.UUID) string {
	earlier, err := h.Queries.ListCommentThreadContext(ctx, db.ListCommentThreadContextParams{
		ReplyLimit:      triggerThreadContextReplies,
		RootID:          rootID,
		WorkspaceID:     trigger.WorkspaceID,
		IssueID:         trigger.IssueID,
		AnchorCreatedAt: trigger.CreatedAt,
		AnchorID:        trigger.ID,
	})
	if err != nil {
		return ""
	}
	names := map[pgtype.UUID]string{}
	lines := make([]string, 0, len(earlier))
	for _, row := range earlier {
		lines = append(lines, fmt.Sprintf("%s: %s", h.commentAuthorLabel(ctx, row, names), truncateRunes(row.Content, triggerThreadContextMessageRunes)))
	}
	return strings.Join(lines, "\n\n")
}

func (h *Handler) commentAuthorLabel(ctx context.Context, c db.Comment, cache map[pgtype.UUID]string) string {
	if name, ok := cache[c.AuthorID]; ok {
		return name
	}
	name := c.AuthorType
	if c.AuthorID.Valid {
		switch c.AuthorType {
		case "agent":
			if a, err := h.Queries.GetAgent(ctx, c.AuthorID); err == nil {
				name = a.Name
			}
		case "member":
			if u, err := h.Queries.GetUser(ctx, c.AuthorID); err == nil {
				name = u.Name
			}
		}
	}
	cache[c.AuthorID] = name
	return name
}
