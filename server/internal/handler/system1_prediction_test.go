package handler

import (
	"net/http"
	"testing"
	"time"
)

type predictionRow struct {
	predictor, version, scope string
	key                       *string
	precedents                *int
	predictedAt               time.Time
}

func loadPredictions(t *testing.T, requestID string) map[string]predictionRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT p.predictor_id, p.predictor_version, p.scope, p.predicted_key, p.predicted_precedents, p.predicted_at
		FROM system1_prediction p JOIN system1_observation o ON o.id = p.observation_id
		WHERE o.kind = 'decision' AND o.subject_id = $1`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]predictionRow{}
	for rows.Next() {
		var r predictionRow
		if err := rows.Scan(&r.predictor, &r.version, &r.scope, &r.key, &r.precedents, &r.predictedAt); err != nil {
			t.Fatal(err)
		}
		out[r.predictor] = r
	}
	return out
}

func seedWaiverLabels(t *testing.T, label string, count int) {
	t.Helper()
	clear := func() { dbfx.Exec(t, `DELETE FROM system1_label WHERE workspace_id = $1`, testWorkspaceID) }
	clear()
	t.Cleanup(clear)
	dbfx.Exec(t, `INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label)
		SELECT $1, gen_random_uuid(), 'test_first_waiver', 'burn_in', 'human_answer', $2 FROM generate_series(1, $3)`, testWorkspaceID, label, count)
}

// A new waiver ask writes two prediction rows (distinct predictors) in the ask transaction, before any answer.
func TestSystem1WaiverAskWritesTwoPredictionsBeforeAnswer(t *testing.T) {
	seedWaiverLabels(t, "waive", 0)
	requestID := askWaiver(t)
	got := loadPredictions(t, requestID)
	if len(got) != 2 {
		t.Fatalf("predictions = %+v, want waiver_rule and precedent", got)
	}
	rule, precedent := got["waiver_rule"], got["precedent"]
	if rule.version != "1" || precedent.version != "1" || rule.scope != "test_first_waiver" || precedent.scope != "test_first_waiver" {
		t.Fatalf("rule = %+v precedent = %+v, want version 1 in scope test_first_waiver", rule, precedent)
	}
	// cross_lab_pass_on_head is false in the fixture ask.
	if rule.key == nil || *rule.key != "refuse" {
		t.Fatalf("waiver_rule key = %v, want refuse", rule.key)
	}
	// No earlier labels: precedent abstains (NULL key), and records how many it saw.
	if precedent.key != nil || precedent.precedents == nil || *precedent.precedents != 0 {
		t.Fatalf("precedent = %+v, want abstention over 0 precedents", precedent)
	}
	zero := 0
	answerOnCardRow(t, requestID, &zero)
	var answeredAt time.Time
	dbfx.QueryRow(t, `SELECT answered_at FROM system1_observation WHERE kind = 'decision' AND subject_id = $1`, requestID).Scan(&answeredAt)
	if !rule.predictedAt.Before(answeredAt) || !precedent.predictedAt.Before(answeredAt) {
		t.Fatalf("predicted_at %v / %v not before answered_at %v", rule.predictedAt, precedent.predictedAt, answeredAt)
	}
}

// precedent@1 reads earlier mapped waiver labels (system1.Predict over them).
func TestSystem1WaiverPrecedentReadsEarlierLabels(t *testing.T) {
	seedWaiverLabels(t, "waive", 6)
	// An unmapped label is not a precedent.
	dbfx.Exec(t, `INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label) VALUES ($1, gen_random_uuid(), 'test_first_waiver', 'burn_in', 'human_answer', 'unmapped')`, testWorkspaceID)
	got := loadPredictions(t, askWaiver(t))["precedent"]
	if got.key == nil || *got.key != "waive" || got.precedents == nil || *got.precedents != 6 {
		t.Fatalf("precedent = %+v, want waive over 6 precedents", got)
	}
}

// An ordinary (non-waiver) ask writes no prediction rows.
func TestSystem1OrdinaryAskWritesNoPredictionRows(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	var created map[string]any
	postRawAsk(t, fixture, waiverAsk(t, fixture, func(a map[string]any) {
		delete(a, "topic")
		delete(a, "waiver")
		for _, o := range a["options"].([]map[string]any) {
			delete(o, "option_key")
		}
	})).Want(http.StatusCreated).JSON(&created)
	if got := loadPredictions(t, created["id"].(string)); len(got) != 0 {
		t.Fatalf("predictions = %+v, want none", got)
	}
}
