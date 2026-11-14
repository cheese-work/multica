package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/system1"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type system1ScopeReport struct {
	Scope            string  `json:"scope"`
	PredictorID      string  `json:"predictor_id"`
	PredictorVersion string  `json:"predictor_version"`
	EpochStartedAt   string  `json:"epoch_started_at"`
	EpochAgeDays     float64 `json:"epoch_age_days"`
	Status           string  `json:"status"`
	Denominator      int64   `json:"denominator"`
	Agreed           int64   `json:"agreed"`
	Agreement        float64 `json:"agreement"`
	Abstained        int64   `json:"abstained"`
	Coverage         float64 `json:"coverage"`
	Unmapped         int64   `json:"unmapped"`
	Reversed         int64   `json:"reversed"`
}

// GetSystem1QualificationReport answers the CHE-3239 qualification state per
// scope and predictor version over the latest epoch of each (CHE-4383 B5).
// Workspace members read it; it records and decides nothing.
func (h *Handler) GetSystem1QualificationReport(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	params := db.System1QualificationReportParams{WorkspaceID: parseUUID(workspaceID)}
	if scope := r.URL.Query().Get("scope"); scope != "" {
		params.Scope = pgtype.Text{String: scope, Valid: true}
	}
	rows, err := h.Queries.System1QualificationReport(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build report")
		return
	}
	now := time.Now()
	scopes := make([]system1ScopeReport, 0, len(rows))
	for _, row := range rows {
		age := now.Sub(row.StartedAt.Time)
		scopes = append(scopes, system1ScopeReport{
			Scope: row.Scope, PredictorID: row.PredictorID, PredictorVersion: row.PredictorVersion,
			EpochStartedAt: row.StartedAt.Time.Format(time.RFC3339), EpochAgeDays: age.Hours() / 24,
			Status:      system1.Status(age, row.Denominator, row.Agreed, row.Demoted),
			Denominator: row.Denominator, Agreed: row.Agreed, Agreement: ratio(row.Agreed, row.Denominator),
			Abstained: row.Abstained, Coverage: ratio(row.Denominator, row.Denominator+row.Abstained),
			Unmapped: row.Unmapped, Reversed: row.Reversed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes})
}
