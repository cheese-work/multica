package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
	for _, key := range []string{askguard.SettingKey, askguard.ReversibleSettingKey} {
		v, present := config[key]
		if !present {
			continue
		}
		s, _ := v.(string)
		if _, err := askguard.ParseMode(s); err != nil {
			return fmt.Errorf("%s must be off, warn or block", key)
		}
	}
	return nil
}

const reversibleAskFix = "Standing Delivery Authorization (CHE-584) lets agents decide and execute reversible work without asking: do it and record it. Ask a person only for spend, deploy, publish, irreversible or product_direction, or use an approval-gated action request"

// refuseReversibleAsk (CHE-5754) refuses a plain agent question whose class is
// reversible: the ask is for work Standing Delivery Authorization already
// allows. Action, proposal and batch requests are the approval-gated path
// (CHE-5167) and stay open. The reversible_ask_guard setting decides: off allows,
// warn logs, block refuses. Set it to off to revert; no data changes.
func (handler *Handler) refuseReversibleAsk(ctx context.Context, issue db.Issue, input decisionRequestInput) error {
	if input.EscalationClass != "reversible" || input.Action != nil || input.Proposal != nil || input.Batch != nil || input.Kind == decisiontx.KindAction {
		return nil
	}
	workspace, err := handler.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	mode, err := askguard.ReversibleModeFromSettings(workspace.Settings)
	if err != nil {
		return err
	}
	switch mode {
	case askguard.ModeBlock:
		return decisionIntentRefusal{http.StatusUnprocessableEntity, "this asks a person to approve reversible work; " + reversibleAskFix}
	case askguard.ModeWarn:
		slog.Warn("ask guard: reversible decision request", "issue_id", uuidToString(issue.ID))
	}
	return nil
}
