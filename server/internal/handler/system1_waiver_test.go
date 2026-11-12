package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// waiverAsk is a test-first waiver ask as an agent sends it, as a plain map so the test pins the wire contract (CHE-4383 B2a).
func waiverAsk(t *testing.T, fixture requestCoreFixture, mutate func(ask map[string]any)) map[string]any {
	t.Helper()
	raw, err := json.Marshal(fixture.unorderedAsk())
	if err != nil {
		t.Fatal(err)
	}
	var ask map[string]any
	if err := json.Unmarshal(raw, &ask); err != nil {
		t.Fatal(err)
	}
	option := func(label, key string) map[string]any {
		return map[string]any{"label": label, "option_key": key, "what_happens": label + " happens.", "pros": []string{"A pro"}, "cons": []string{"A con"}, "cost_or_time": "No extra time"}
	}
	ask["options"] = []map[string]any{option("Waive the red", "waive"), option("Refuse the waiver", "refuse")}
	ask["recommended_option"] = "Refuse the waiver"
	ask["recommendation"] = "Refuse the waiver"
	ask["topic"] = "test_first_waiver"
	ask["waiver"] = map[string]any{
		"repo": "cheese-work/multica", "head_sha": strings.Repeat("a", 40), "red_impossible_category": "no_behavior_change",
		"weaker_evidence_kind": "characterization_test", "cross_lab_pass_on_head": false, "tests_retrospective": true, "change_kind": "docs",
	}
	if mutate != nil {
		mutate(ask)
	}
	return ask
}

func postRawAsk(t *testing.T, fixture requestCoreFixture, ask map[string]any) *testutil.Response {
	t.Helper()
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible"})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return testutil.Call(t, testHandler.RequestDecision, request)
}

// A waiver ask whose recommended option is refuse is stored refuse-first; the observation keeps the topic, the typed fields and the option keys in that stored order, and no free text.
func TestSystem1WaiverObservationStoresOptionKeysInStoredOrder(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	ask := waiverAsk(t, fixture, nil)
	var created map[string]any
	postRawAsk(t, fixture, ask).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)

	var inputs string
	dbfx.QueryRow(t, `SELECT inputs::text FROM system1_observation WHERE kind = 'decision' AND subject_id = $1`, requestID).Scan(&inputs)
	var stored struct {
		Topic      string            `json:"topic"`
		OptionKeys []string          `json:"option_keys"`
		Waiver     map[string]any    `json:"waiver"`
		Other      map[string]string `json:"-"`
	}
	if err := json.Unmarshal([]byte(inputs), &stored); err != nil {
		t.Fatalf("inputs = %s: %v", inputs, err)
	}
	if stored.Topic != "test_first_waiver" || len(stored.OptionKeys) != 2 || stored.OptionKeys[0] != "refuse" || stored.OptionKeys[1] != "waive" {
		t.Fatalf("topic=%q option_keys=%v, want test_first_waiver and [refuse waive]; inputs = %s", stored.Topic, stored.OptionKeys, inputs)
	}
	if stored.Waiver["repo"] != "cheese-work/multica" || stored.Waiver["change_kind"] != "docs" || stored.Waiver["tests_retrospective"] != true || stored.Waiver["cross_lab_pass_on_head"] != false {
		t.Fatalf("waiver fields = %v", stored.Waiver)
	}
	// Never ask text, option labels or a free-text reason.
	for _, text := range []string{"Refuse the waiver", "Waive the red", ask["decision"].(string), ask["context_eli10"].(string), "reason"} {
		if strings.Contains(inputs, text) {
			t.Fatalf("observation inputs hold %q: %s", text, inputs)
		}
	}
}

// The contract is closed: every deviation is refused with 400 and nothing is stored.
func TestSystem1WaiverAskContractRefusals(t *testing.T) {
	for name, mutate := range map[string]func(ask map[string]any){
		"unknown topic":        func(a map[string]any) { a["topic"] = "free_lunch" },
		"missing waiver block": func(a map[string]any) { delete(a, "waiver") },
		"waiver without topic": func(a map[string]any) { delete(a, "topic") },
		"unknown waiver field": func(a map[string]any) { a["waiver"].(map[string]any)["reason"] = "because" },
		"bad head sha":         func(a map[string]any) { a["waiver"].(map[string]any)["head_sha"] = strings.Repeat("A", 40) },
		"bad repo":             func(a map[string]any) { a["waiver"].(map[string]any)["repo"] = "not a repo" },
		"unknown category":     func(a map[string]any) { a["waiver"].(map[string]any)["red_impossible_category"] = "lazy" },
		"unknown evidence":     func(a map[string]any) { a["waiver"].(map[string]any)["weaker_evidence_kind"] = "vibes" },
		"unknown change kind":  func(a map[string]any) { a["waiver"].(map[string]any)["change_kind"] = "magic" },
		"missing boolean":      func(a map[string]any) { delete(a["waiver"].(map[string]any), "tests_retrospective") },
		"non-boolean":          func(a map[string]any) { a["waiver"].(map[string]any)["cross_lab_pass_on_head"] = "yes" },
		"missing option key": func(a map[string]any) {
			delete(a["options"].([]map[string]any)[0], "option_key")
		},
		"duplicate option key": func(a map[string]any) { a["options"].([]map[string]any)[0]["option_key"] = "refuse" },
		"unknown option key":   func(a map[string]any) { a["options"].([]map[string]any)[0]["option_key"] = "maybe" },
		"third option": func(a map[string]any) {
			options := a["options"].([]map[string]any)
			a["options"] = append(options, map[string]any{"label": "Defer", "option_key": "defer", "what_happens": "Defer.", "pros": []string{"p"}, "cons": []string{"c"}, "cost_or_time": "none"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRequestCoreFixture(t, false)
			postRawAsk(t, fixture, waiverAsk(t, fixture, mutate)).Want(http.StatusBadRequest)
			var count int
			dbfx.QueryRow(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID).Scan(&count)
			if count != 0 {
				t.Fatalf("refused ask stored %d request(s)", count)
			}
		})
	}
}

// Outside the waiver topic the new fields do not exist: an ordinary ask with option_key or a waiver block is refused.
func TestSystem1WaiverFieldsRefusedOutsideTopic(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	ask := waiverAsk(t, fixture, func(a map[string]any) { delete(a, "topic") })
	delete(ask, "waiver")
	postRawAsk(t, fixture, ask).Want(http.StatusBadRequest) // option_key without a topic
}

type labelRow struct {
	label, scope, kind, source string
	superseded                 bool
}

func loadLabels(t *testing.T, requestID string) []labelRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT l.label, l.scope, l.kind, l.source, l.superseded_at IS NOT NULL
		FROM system1_label l JOIN system1_observation o ON o.id = l.observation_id
		WHERE o.kind = 'decision' AND o.subject_id = $1 ORDER BY l.created_at`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []labelRow
	for rows.Next() {
		var r labelRow
		if err := rows.Scan(&r.label, &r.scope, &r.kind, &r.source, &r.superseded); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func askWaiver(t *testing.T) string {
	t.Helper()
	fixture := newRequestCoreFixture(t, false)
	var created map[string]any
	postRawAsk(t, fixture, waiverAsk(t, fixture, nil)).Want(http.StatusCreated).JSON(&created)
	return created["id"].(string)
}

// The waiver ask is stored [refuse, waive] (recommended first). Answering index 1 is `waive`, not the positional `other`.
func TestSystem1WaiverAnswerWritesMappedLabel(t *testing.T) {
	requestID := askWaiver(t)
	one := 1
	answerOnCardRow(t, requestID, &one)
	got := loadLabels(t, requestID)
	want := labelRow{label: "waive", scope: "test_first_waiver", kind: "burn_in", source: "human_answer"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("labels = %+v, want [%+v]", got, want)
	}
}

func TestSystem1WaiverFreeTextAnswerIsUnmapped(t *testing.T) {
	requestID := askWaiver(t)
	answerOnCardRow(t, requestID, nil)
	if got := loadLabels(t, requestID); len(got) != 1 || got[0].label != "unmapped" {
		t.Fatalf("labels = %+v, want one unmapped", got)
	}
}

func TestSystem1WaiverDisclaimSupersedesLabel(t *testing.T) {
	requestID := askWaiver(t)
	zero := 0
	answerOnCardRow(t, requestID, &zero)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`, requestID)
	got := loadLabels(t, requestID)
	if len(got) != 1 || got[0].label != "refuse" || !got[0].superseded {
		t.Fatalf("labels = %+v, want one superseded refuse", got)
	}
}

// Only a topic with a stored key mapping gets a label; an ordinary ask stays positional.
func TestSystem1OrdinaryAnswerWritesNoLabel(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	var created map[string]any
	postRawAsk(t, fixture, waiverAsk(t, fixture, func(ask map[string]any) {
		delete(ask, "topic")
		delete(ask, "waiver")
		for _, o := range ask["options"].([]map[string]any) {
			delete(o, "option_key")
		}
	})).Want(http.StatusCreated).JSON(&created)
	requestID := created["id"].(string)
	zero := 0
	answerOnCardRow(t, requestID, &zero)
	if got := loadLabels(t, requestID); len(got) != 0 {
		t.Fatalf("labels = %+v, want none", got)
	}
}

// Withdraw (disclaim) then answer again: the old label stays superseded and the replacement answer gets its own mapped label of record.
func TestSystem1WaiverReanswerAfterDisclaimWritesNewLabel(t *testing.T) {
	requestID := askWaiver(t)
	zero, one := 0, 1
	answerOnCardRow(t, requestID, &zero)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`, requestID)
	answerOnCardRow(t, requestID, &one)
	got := loadLabels(t, requestID)
	want := []labelRow{
		{label: "refuse", scope: "test_first_waiver", kind: "burn_in", source: "human_answer", superseded: true},
		{label: "waive", scope: "test_first_waiver", kind: "burn_in", source: "human_answer"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("labels = %+v, want %+v", got, want)
	}
}
