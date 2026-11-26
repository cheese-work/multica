package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// closeModeView is the effective auto_close mode and where it came from.
type closeModeView struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// closeCheckResponse is GET /api/issues/{id}/close-check: every close predicate
// on the issue's current rows ("current"), or on the inputs of one recorded
// auto_close_skipped activity ("activity"). It writes nothing.
type closeCheckResponse struct {
	IssueID              string                 `json:"issue_id"`
	Source               string                 `json:"source"`
	ActivityID           string                 `json:"activity_id,omitempty"`
	CheckedAt            string                 `json:"checked_at"`
	Mode                 closeModeView          `json:"mode"`
	TopLevel             bool                   `json:"top_level"`
	Outcome              string                 `json:"outcome"`
	FirstFailure         string                 `json:"first_failure"`
	RecordedFirstFailure string                 `json:"recorded_first_failure,omitempty"`
	Predicates           []closePredicateResult `json:"predicates"`
	Inputs               closeInputs            `json:"inputs"`
}

func closeOutcomeName(ev closeEvaluation) string {
	switch ev.State {
	case closeStateHolds:
		return "holds"
	case closeStateAwaitingMerge:
		return "awaiting_merge"
	}
	return "rejected"
}

// GetIssueCloseCheck answers `multica issue close-check <id> [--activity <row>]`.
func (h *Handler) GetIssueCloseCheck(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	ctx := r.Context()
	resp := closeCheckResponse{IssueID: uuidToString(issue.ID)}
	if rowID := r.URL.Query().Get("activity"); rowID != "" {
		id, ok := parseUUIDOrBadRequest(w, rowID, "activity")
		if !ok {
			return
		}
		row, err := h.Queries.GetActivity(ctx, id)
		if err != nil || row.IssueID != issue.ID || row.Action != activityAutoCloseSkipped {
			writeError(w, http.StatusNotFound, "no auto_close_skipped activity with that id on this issue")
			return
		}
		var stored struct {
			FirstFailure string        `json:"first_failure"`
			Mode         closeModeView `json:"mode"`
			CheckedAt    string        `json:"checked_at"`
			Inputs       closeInputs   `json:"inputs"`
		}
		if err := json.Unmarshal(row.Details, &stored); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "the activity details cannot be read")
			return
		}
		ev := evaluateClose(&stored.Inputs, len(closePredicates))
		resp.Source, resp.ActivityID = "activity", rowID
		resp.CheckedAt, resp.Mode, resp.TopLevel = stored.CheckedAt, stored.Mode, stored.Inputs.Issue.TopLevel
		resp.RecordedFirstFailure = stored.FirstFailure
		resp.Inputs = stored.Inputs
		fillCloseCheck(&resp, ev)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	mode, topLevel, source := h.autoCloseSettingsSource(ctx, issue)
	inputs, ev := h.collectCloseInputs(ctx, issue, topLevel, true)
	resp.Source, resp.CheckedAt = "current", time.Now().UTC().Format(time.RFC3339Nano)
	resp.Mode, resp.TopLevel, resp.Inputs = closeModeView{Value: mode, Source: source}, topLevel, inputs
	fillCloseCheck(&resp, ev)
	writeJSON(w, http.StatusOK, resp)
}

func fillCloseCheck(resp *closeCheckResponse, ev closeEvaluation) {
	resp.Outcome, resp.FirstFailure, resp.Predicates = closeOutcomeName(ev), ev.FirstFailure, ev.Predicates
}
