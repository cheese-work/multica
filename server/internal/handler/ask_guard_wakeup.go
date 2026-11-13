package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/service"
)

const askGuardWakeupFix = "multica decision request ... to ask the person, then register the wait with --decision <request-id>; add --purpose observe if the rule never waits on a person; or add --filter-actor-type agent to react to agents only"

// wakeupAskGuard is S1 for an agent caller: nil for a person. A run with no
// task, or a chat-lane task, only warns: there is no run to recover through a
// refusal.
func (h *Handler) wakeupAskGuard(r *http.Request, actorType string) *service.WakeupAskGuard {
	if actorType != "agent" {
		return nil
	}
	g := &service.WakeupAskGuard{Guard: askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}, TaskID: h.commentSourceTaskID(r)}
	if !g.TaskID.Valid {
		g.TaskID, g.WarnOnly = pgtype.UUID{Valid: true}, true
		return g
	}
	if task, err := h.Queries.GetAgentTask(r.Context(), g.TaskID); err == nil {
		g.WarnOnly = task.ChatSessionID.Valid
	}
	return g
}

// askGuardWakeupError writes the guard's refusal. It reports false for any
// other error.
func askGuardWakeupError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, service.ErrWakeupAskGuardBlocked):
		writeError(w, http.StatusUnprocessableEntity, "this wakeup waits on a person without a decision request; run: "+askGuardWakeupFix)
	case errors.Is(err, service.ErrWakeupAskGuardUnavailable):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
	default:
		return false
	}
	return true
}
