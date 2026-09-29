package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// processChildEvents runs the sub-issue rules of these parents (the child_done
// system rule and people's sub-issue conditions) right after a write. The
// write recorded its sub-issue changes in its own transaction; a failure here
// is retried by the scheduler sweep.
func (h *Handler) processChildEvents(ctx context.Context, parentIDs ...pgtype.UUID) {
	_ = (&service.IssueWakeupService{Tasks: h.TaskService}).ProcessChildEvents(ctx, parentIDs...)
}

func (h *Handler) childDoneSystemRuleEnabled(ctx context.Context, parentID pgtype.UUID) bool {
	rule, err := h.Queries.GetSystemWakeup(ctx, db.GetSystemWakeupParams{
		IssueID: parentID, SystemRule: pgtype.Text{String: service.SystemRuleChildDone, Valid: true},
	})
	return err == nil && rule.Enabled
}
