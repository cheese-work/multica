package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
)

// askGuardHit runs the guard for one agent operation and reports whether the
// operation may proceed. It writes the response when it may not: 503 with
// Retry-After when the event could not be written, 422 with fix in block mode.
// The event is committed before the 422, so the caller rolls back its own
// transaction afterwards without losing it.
func (h *Handler) askGuardHit(w http.ResponseWriter, r *http.Request, ws pgtype.UUID, op askguard.Op, fix string) bool {
	return refuseOnAskGuard(w, r.Context(), askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}, ws, op, fix)
}

func refuseOnAskGuard(w http.ResponseWriter, ctx context.Context, g askguard.Guard, ws pgtype.UUID, op askguard.Op, fix string) bool {
	v, err := g.Hit(ctx, ws, op)
	if err != nil {
		slog.Warn("ask guard hit failed", "error", err, "kind", op.Kind)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
		return false
	}
	if v.Block {
		writeError(w, http.StatusUnprocessableEntity, "this operation asks a person to act without a decision request; run: "+fix)
		return false
	}
	return true
}

// validateAskGuardSetting checks the ask_guard_mode key of a workspace
// settings object. An absent key is valid; null is not.
func validateAskGuardSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[askguard.SettingKey]
	if !present {
		return nil
	}
	s, _ := v.(string)
	_, err := askguard.ParseMode(s)
	return err
}
