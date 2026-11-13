package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/system1"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type observationRow struct {
	id                                  string
	optionCount                         int
	recommended, chosen                 *int
	source, predicted, choice, outcome  *string
	predictedPrecedents                 *int
	confidence                          *float32
	hasPredictedAt, hasAnswered, hasRev bool
}

func loadObservation(t *testing.T, kind, subject string) observationRow {
	t.Helper()
	var row observationRow
	dbfx.QueryRow(t, `SELECT id::text, option_count, recommended_index, chosen_index, decision_source, predicted_key, choice_key, outcome, predicted_precedents, agent_confidence,
		predicted_at IS NOT NULL, answered_at IS NOT NULL, reversed_at IS NOT NULL FROM system1_observation WHERE kind = $1 AND subject_id = $2 ORDER BY created_at DESC LIMIT 1`, kind, subject).
		Scan(&row.id, &row.optionCount, &row.recommended, &row.chosen, &row.source, &row.predicted, &row.choice, &row.outcome, &row.predictedPrecedents, &row.confidence, &row.hasPredictedAt, &row.hasAnswered, &row.hasRev)
	return row
}

func seedPrecedents(t *testing.T, class, key string, count int) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM system1_observation WHERE workspace_id = $1 AND class = $2`, testWorkspaceID, class)
	dbfx.Exec(t, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, choice_key, outcome)
		SELECT $1, 'decision', gen_random_uuid(), $2, $3, 'answered' FROM generate_series(1, $4)`, testWorkspaceID, class, key, count)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM system1_observation WHERE workspace_id = $1 AND class = $2`, testWorkspaceID, class)
	})
}

func answerOnCardRow(t *testing.T, requestID string, option *int) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO decision_answer (decision_request_id, answered_by_user_id, answer_source, chat_message_ids, content_snapshot, effective_at, chosen_option_index)
		VALUES ($1, $2, 'card', '{}', 'x', clock_timestamp(), $3)`, requestID, testUserID, option)
}

func TestSystem1DecisionObservationLifecycle(t *testing.T) {
	seedPrecedents(t, "reversible", system1.KeyRecommended, 6)
	fixture := newRequestCoreFixture(t, false)
	var created map[string]any
	ask := fixture.unorderedAsk() // options Proceed, Hold; recommendation Hold
	ask.Confidence = ptr(0.75)
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible"})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)

	// UI rule: the recommended option is first in the stored card and in the observation.
	var storedOptions []string
	var raw []byte
	dbfx.QueryRow(t, `SELECT jsonb_path_query_array(ask_json, '$.options[*].label') FROM decision_request WHERE id = $1`, requestID).Scan(&raw)
	_ = json.Unmarshal(raw, &storedOptions)
	if len(storedOptions) != 2 || storedOptions[0] != "Hold" {
		t.Fatalf("stored options = %v, want Hold first", storedOptions)
	}
	row := loadObservation(t, "decision", requestID)
	if row.optionCount != 2 || row.recommended == nil || *row.recommended != 0 {
		t.Fatalf("observation option_count=%d recommended=%v", row.optionCount, row.recommended)
	}
	// Shadow prediction exists before any answer, and nothing is decided yet.
	if !row.hasPredictedAt || row.predicted == nil || *row.predicted != system1.KeyRecommended || row.predictedPrecedents == nil || *row.predictedPrecedents != 6 || row.confidence == nil || *row.confidence != 0.75 {
		t.Fatalf("prediction/confidence not captured: %+v", row)
	}
	if row.hasAnswered || row.source != nil || row.choice != nil {
		t.Fatalf("open request already decided: %+v", row)
	}

	// The human picks the second option (not the recommendation).
	answerOnCardRow(t, requestID, ptr(1))
	row = loadObservation(t, "decision", requestID)
	if row.source == nil || *row.source != "human" || row.choice == nil || *row.choice != "other" || row.chosen == nil || *row.chosen != 1 || !row.hasAnswered || row.outcome == nil || *row.outcome != "answered" {
		t.Fatalf("answer not captured: %+v", row)
	}
	var ms *int64
	dbfx.QueryRow(t, `SELECT time_to_answer_ms FROM system1_observation WHERE id = $1`, row.id).Scan(&ms)
	if ms == nil || *ms < 0 {
		t.Fatalf("time_to_answer_ms = %v", ms)
	}

	// Disclaiming the answer records the reversal.
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`, requestID)
	if row = loadObservation(t, "decision", requestID); !row.hasRev {
		t.Fatal("disclaimed answer not marked reversed")
	}

	// The report counts the disagreement in the audit slice.
	var report map[string]struct {
		Decided int64                `json:"decided"`
		Classes []system1ClassReport `json:"classes"`
	}
	testutil.Call(t, testHandler.GetSystem1Report, newRequest(http.MethodGet, "/api/decisions/shadow-report", nil)).Want(http.StatusOK).JSON(&report)
	var got *system1ClassReport
	for i, class := range report["decision"].Classes {
		if class.Class == "reversible" {
			got = &report["decision"].Classes[i]
		}
	}
	// 6 seeded precedents (recommended, no prediction) + this answer (predicted recommended, human chose other).
	if got == nil || got.Decided != 7 || got.Predicted != 1 || got.Agreed != 0 || got.OtherDecided != 1 || got.OtherPredicted != 1 || got.OtherAgreed != 0 || got.Reversed != 1 {
		t.Fatalf("report = %+v", got)
	}
}

func TestSystem1ObservationHoldsNoAskText(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	ask := fixture.unorderedAsk()
	ask.Options = []decisiontx.AskOption{testAskOption("Proceed"), testAskOption("Hold sk-live-SECRET99")}
	ask.Recommendation = "Hold sk-live-SECRET99"
	ask.RecommendedOption = "Hold sk-live-SECRET99"
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible"})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	var created map[string]any
	testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)
	answerOnCardRow(t, requestID, ptr(1))
	var row string
	dbfx.QueryRow(t, `SELECT to_jsonb(o)::text FROM system1_observation o WHERE subject_id = $1`, requestID).Scan(&row)
	for _, text := range []string{"SECRET99", "Proceed", "Hold", ask.Decision, ask.ContextELI10} {
		if strings.Contains(row, text) {
			t.Fatalf("observation row holds ask text %q: %s", text, row)
		}
	}
	var count, recommended int
	dbfx.QueryRow(t, `SELECT option_count, recommended_index FROM system1_observation WHERE subject_id = $1`, requestID).Scan(&count, &recommended)
	if count != 2 || recommended != 0 {
		t.Fatalf("option_count=%d recommended_index=%d, want 2 and 0", count, recommended)
	}
}

func postOrderedAsk(t *testing.T, fixture requestCoreFixture, ask decisiontx.Ask) *testutil.Response {
	t.Helper()
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible"})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return testutil.Call(t, testHandler.RequestDecision, request)
}

func firstOption(t *testing.T, requestID string) string {
	t.Helper()
	var first string
	dbfx.QueryRow(t, `SELECT ask_json->'options'->0->>'label' FROM decision_request WHERE id = $1`, requestID).Scan(&first)
	return first
}

// recommended_option is the only source of order and of the shadow's recommended index; the prose is never read.
func TestSystem1StructuredOptionIsAuthoritative(t *testing.T) {
	seedPrecedents(t, "reversible", system1.KeyRecommended, 6)
	fixture := newRequestCoreFixture(t, false)
	ask := fixture.unorderedAsk() // options Proceed, Hold
	ask.RecommendedOption = "hold"
	ask.Recommendation = "Proceed is fine too"
	var created map[string]any
	postOrderedAsk(t, fixture, ask).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)
	if got := firstOption(t, requestID); got != "Hold" {
		t.Fatalf("first option = %s, want Hold", got)
	}
	row := loadObservation(t, "decision", requestID)
	if row.recommended == nil || *row.recommended != 0 || row.predicted == nil || *row.predicted != system1.KeyRecommended {
		t.Fatalf("recommended_index=%v predicted=%v, want 0 and recommended", row.recommended, row.predicted)
	}
	answerOnCardRow(t, requestID, ptr(0)) // the human takes the first card option, Hold
	if row = loadObservation(t, "decision", requestID); row.choice == nil || *row.choice != "recommended" {
		t.Fatalf("choice = %v, want recommended", row.choice)
	}
	var report map[string]struct {
		Classes []system1ClassReport `json:"classes"`
	}
	testutil.Call(t, testHandler.GetSystem1Report, newRequest(http.MethodGet, "/api/decisions/shadow-report", nil)).Want(http.StatusOK).JSON(&report)
	for _, class := range report["decision"].Classes {
		if class.Class == "reversible" && (class.Predicted < 1 || class.Agreed < 1) {
			t.Fatalf("report does not count the agreement: %+v", class)
		}
	}
}

// An ask whose recommended option is not named by recommended_option is refused, whatever the prose says.
func TestSystem1UnstructuredRecommendationIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*decisiontx.Ask){
		"prose names one option": func(a *decisiontx.Ask) { a.RecommendedOption, a.Recommendation = "", "Hold" },
		"prose misleads": func(a *decisiontx.Ask) {
			a.RecommendedOption, a.Recommendation = "", "Proceed is risky; I recommend Hold"
		},
		"prose uses a contrast": func(a *decisiontx.Ask) {
			a.RecommendedOption, a.Recommendation = "", "I recommend Hold rather than Proceed"
		},
		"prose names no option":      func(a *decisiontx.Ask) { a.RecommendedOption, a.Recommendation = "", "Whichever is cheaper." },
		"blank recommended_option":   func(a *decisiontx.Ask) { a.RecommendedOption = "  " },
		"unknown recommended_option": func(a *decisiontx.Ask) { a.RecommendedOption = "Sometime" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRequestCoreFixture(t, false) // one open intent per request
			ask := fixture.unorderedAsk()
			mutate(&ask)
			postOrderedAsk(t, fixture, ask).Want(http.StatusBadRequest)
		})
	}
}

func TestSystem1RoutingObservationAndOverride(t *testing.T) {
	reviewer := seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "System1 "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"}, {"status_key": "in_progress"},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review."},
	})
	projectID := createWorkflowTestProject(t, "System1 project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issueID := createIssueForTimeline(t, "system1 routing "+workflowTestSuffix())
	dbfx.Exec(t, `UPDATE issue SET project_id = $2, status = 'in_progress' WHERE id = $1`, issueID, projectID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM system1_observation WHERE subject_id = $1`, issueID) })

	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": "in_review", "suppress_run": true}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	row := loadObservation(t, "routing", issueID)
	if row.source == nil || *row.source != "rule" || row.choice == nil || *row.choice != "agent:"+reviewer || row.outcome == nil || *row.outcome != "applied" || row.hasRev || !row.hasPredictedAt {
		t.Fatalf("routing observation = %+v", row)
	}

	// A person replacing the handler marks the observation overridden.
	req = withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"assignee_type": "member", "assignee_id": testUserID}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if row = loadObservation(t, "routing", issueID); !row.hasRev {
		t.Fatal("manual reassignment not recorded as an override")
	}
	_ = context.Background
}
