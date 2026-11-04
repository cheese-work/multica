package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/system1"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Shadow System 1 capture (CHE-3628). Facts only: no ask text, no answer text.
// A prediction is stored and reported, never applied. Option labels are ask
// text and may hold secrets, so only the option count and the recommended
// position are kept.

// recordDecisionObservation runs inside the request transaction, so the
// prediction exists before the human can answer. Action requests (no options)
// and finished-text requests have nothing to predict and record nothing.
func recordDecisionObservation(ctx context.Context, q *db.Queries, issue db.Issue, request db.DecisionRequest, ask *decisiontx.Ask) error {
	if ask == nil || len(ask.Options) < 2 {
		return nil
	}
	labels := make([]string, len(ask.Options))
	for i, option := range ask.Options {
		labels[i] = option.Label
	}
	recommended := decisiontx.RecommendedIndex(ask.Recommendation, labels)
	precedents, err := q.ListSystem1PrecedentKeys(ctx, db.ListSystem1PrecedentKeysParams{WorkspaceID: issue.WorkspaceID, Kind: "decision", Class: request.EscalationClass})
	if err != nil {
		return err
	}
	prediction := system1.Predict(precedents)
	if prediction.Key == system1.KeyRecommended && recommended < 0 {
		prediction.Key = "" // no recommendation to predict the human taking
	}
	inputs, _ := json.Marshal(map[string]any{
		"origin": request.Origin, "issue_status": issue.Status, "project_id": uuidToString(issue.ProjectID),
		"asking_agent_id": uuidToString(request.AskingAgentID), "option_count": len(labels),
	})
	params := db.CreateSystem1ObservationParams{
		WorkspaceID: issue.WorkspaceID, Kind: "decision", SubjectID: request.ID, Class: request.EscalationClass,
		Inputs: inputs, OptionCount: int32(len(labels)),
		PredictedKey: pgtype.Text{String: prediction.Key, Valid: true}, PredictedConfidence: pgtype.Float4{Float32: float32(prediction.Confidence), Valid: prediction.Key != ""},
		PredictedPrecedents: pgtype.Int4{Int32: int32(prediction.Precedents), Valid: true},
	}
	if recommended >= 0 {
		params.RecommendedIndex = pgtype.Int4{Int32: int32(recommended), Valid: true}
	}
	if ask.Confidence != nil {
		params.AgentConfidence = pgtype.Float4{Float32: float32(*ask.Confidence), Valid: true}
	}
	_, err = q.CreateSystem1Observation(ctx, params)
	return err
}

// recordSystem1Routing notes a workflow handoff (a rule chose the handler) or a
// person replacing the handler a handoff just chose. It runs after the write and
// never fails it.
func (h *Handler) recordSystem1Routing(ctx context.Context, prev, issue db.Issue, handoff *workflowHandoff, actorType string) {
	switch {
	case handoff != nil:
		class := handoff.def.Name + "/" + handoff.step.StatusKey
		choice := issue.AssigneeType.String + ":" + uuidToString(issue.AssigneeID)
		precedents, err := h.Queries.ListSystem1PrecedentKeys(ctx, db.ListSystem1PrecedentKeysParams{WorkspaceID: issue.WorkspaceID, Kind: "routing", Class: class})
		if err == nil {
			prediction := system1.Predict(precedents)
			inputs, _ := json.Marshal(map[string]string{
				"workflow": handoff.def.Name, "from_status": prev.Status, "to_status": issue.Status, "actor_type": actorType,
				"from_assignee": prev.AssigneeType.String + ":" + uuidToString(prev.AssigneeID), "project_id": uuidToString(issue.ProjectID),
			})
			_, err = h.Queries.CreateSystem1Observation(ctx, db.CreateSystem1ObservationParams{
				WorkspaceID: issue.WorkspaceID, Kind: "routing", SubjectID: issue.ID, Class: class,
				DecisionSource: pgtype.Text{String: "rule", Valid: true}, Inputs: inputs,
				PredictedKey: pgtype.Text{String: prediction.Key, Valid: true}, PredictedConfidence: pgtype.Float4{Float32: float32(prediction.Confidence), Valid: prediction.Key != ""},
				PredictedPrecedents: pgtype.Int4{Int32: int32(prediction.Precedents), Valid: true},
				ChoiceKey:           pgtype.Text{String: choice, Valid: true}, Outcome: pgtype.Text{String: "applied", Valid: true},
			})
		}
		if err != nil {
			slog.Warn("system1 routing observation", "issue_id", uuidToString(issue.ID), "error", err)
		}
	case actorType == "member":
		if err := h.Queries.MarkSystem1RoutingOverridden(ctx, issue.ID); err != nil {
			slog.Warn("system1 routing override", "issue_id", uuidToString(issue.ID), "error", err)
		}
	}
}

type system1ClassReport struct {
	Class          string  `json:"class"`
	Decided        int64   `json:"decided"`
	Predicted      int64   `json:"predicted"`
	Agreed         int64   `json:"agreed"`
	Coverage       float64 `json:"coverage"`
	Precision      float64 `json:"precision"`
	OtherDecided   int64   `json:"other_decided"`
	OtherPredicted int64   `json:"other_predicted"`
	OtherAgreed    int64   `json:"other_agreed"`
	OtherPrecision float64 `json:"other_precision"`
	Reversed       int64   `json:"reversed"`
	MedianAnswerMs float64 `json:"median_answer_ms"`
}

func ratio(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// GetSystem1Report answers precision and coverage of the shadow predictions by
// class, for decisions and for routing. ready turns true at 100 decided.
func (h *Handler) GetSystem1Report(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	out := map[string]any{}
	for _, kind := range []string{"decision", "routing"} {
		rows, err := h.Queries.System1ReportByClass(r.Context(), db.System1ReportByClassParams{WorkspaceID: parseUUID(workspaceID), Kind: kind})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build report")
			return
		}
		classes, total := []system1ClassReport{}, int64(0)
		for _, row := range rows {
			total += row.Decided
			classes = append(classes, system1ClassReport{
				Class: row.Class, Decided: row.Decided, Predicted: row.Predicted, Agreed: row.Agreed,
				Coverage: ratio(row.Predicted, row.Decided), Precision: ratio(row.Agreed, row.Predicted),
				OtherDecided: row.OtherDecided, OtherPredicted: row.OtherPredicted, OtherAgreed: row.OtherAgreed,
				OtherPrecision: ratio(row.OtherAgreed, row.OtherPredicted), Reversed: row.Reversed, MedianAnswerMs: row.MedianAnswerMs,
			})
		}
		out[kind] = map[string]any{"decided": total, "ready": total >= 100, "classes": classes}
	}
	writeJSON(w, http.StatusOK, out)
}
