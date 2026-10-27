package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

const askOptionShapeError = "ask.options[0] must be an object with label, what_happens, pros, cons, cost_or_time"

func (fixture requestCoreFixture) newShapeAsk() map[string]any {
	return map[string]any{
		"context_eli10": "This feature adds a chat archive. The code is ready. Some proof is still missing. You decide whether to collect it first.",
		"agent_did":     "Ran the scoped checks.", "decision": "Collect the proof first?",
		"stakes_if_wrong": "A bug could ship to phones.",
		"options": []any{
			map[string]any{"label": "KEEP", "what_happens": "Agents collect proof.", "pros": []string{"Safer"}, "cons": []string{"Slower"}, "cost_or_time": "1 day"},
			map[string]any{"label": "WAIVE", "what_happens": "Agents skip proof.", "pros": []string{"Faster"}, "cons": []string{"Riskier"}, "cost_or_time": "No extra time"},
		},
		"recommendation": "KEEP", "default_if_no_reply": "KEEP",
		"source_issue_id": fixture.issueID, "source_comment_id": fixture.tagID,
	}
}

func (fixture requestCoreFixture) askRequest(test *testing.T, ask any) *http.Request {
	request := fixture.apiRequest()
	request.Body = ioBody(mustDecisionJSON(test, map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": ask}))
	return request
}

func TestDecisionAskSchemaErrorsNameTheField(test *testing.T) {
	deleteKey := func(key string) func(map[string]any) {
		return func(ask map[string]any) { delete(ask, key) }
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"missing context_eli10", deleteKey("context_eli10"), "ask.context_eli10 is required"},
		{"missing stakes_if_wrong", deleteKey("stakes_if_wrong"), "ask.stakes_if_wrong is required"},
		{"legacy string options", func(ask map[string]any) { ask["options"] = []string{"Proceed", "Hold"} }, askOptionShapeError},
		{"option without what_happens", func(ask map[string]any) { delete(ask["options"].([]any)[1].(map[string]any), "what_happens") }, "ask.options[1].what_happens is required"},
		{"pros as a string", func(ask map[string]any) { ask["options"].([]any)[0].(map[string]any)["pros"] = "Safer" }, "ask.options[0].pros must be a list of strings"},
		{"unknown key", func(ask map[string]any) { ask["extra_field"] = "x" }, "ask.extra_field is not a known field"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			ask := fixture.newShapeAsk()
			tc.mutate(ask)
			response := testutil.Call(test, testHandler.RequestDecision, fixture.askRequest(test, ask)).Want(http.StatusBadRequest)
			if got := strings.TrimSpace(response.Body.String()); !strings.Contains(got, tc.want) {
				test.Fatalf("body = %s, want it to contain %q", got, tc.want)
			}
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionAskMalformedHidesDecoderText(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	response := testutil.Call(test, testHandler.RequestDecision, fixture.askRequest(test, "{")).Want(http.StatusBadRequest)
	body := response.Body.String()
	if !strings.Contains(body, "ask must be one JSON object") || strings.Contains(body, "json:") || strings.Contains(body, "decisiontx") {
		test.Fatalf("body = %s", body)
	}
}

func TestDecisionAskDecodeErrorsAfterAuthChecks(test *testing.T) {
	legacy := func(fixture requestCoreFixture) any {
		ask := fixture.newShapeAsk()
		ask["options"] = []string{"Proceed", "Hold"}
		return ask
	}
	test.Run("no task token", func(test *testing.T) {
		fixture := newRequestCoreFixture(test, false)
		request := fixture.askRequest(test, legacy(fixture))
		request.Header.Del("X-Actor-Source")
		response := testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusForbidden)
		if strings.Contains(response.Body.String(), "ask.") {
			test.Fatalf("auth failure leaked ask errors: %s", response.Body.String())
		}
	})
	test.Run("non-member with task token", func(test *testing.T) {
		fixture := newRequestCoreFixture(test, false)
		outsider := dbfx.User(test, "Ask Outsider", "ask-outsider-"+fixture.issueID+"@multica.test")
		request := fixture.askRequest(test, legacy(fixture))
		request.Header.Set("X-User-ID", outsider)
		response := testutil.Call(test, testHandler.RequestDecision, request)
		if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
			test.Fatalf("status = %d, want 403 or 404: %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "ask.") {
			test.Fatalf("auth failure leaked ask errors: %s", response.Body.String())
		}
		fixture.assertCounts(test, 0)
	})
	test.Run("unknown issue", func(test *testing.T) {
		fixture := newRequestCoreFixture(test, false)
		request := fixture.apiRequest()
		request.Body = ioBody(mustDecisionJSON(test, map[string]any{"issue_id": "00000000-0000-4000-8000-000000000000", "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": legacy(fixture)}))
		response := testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusNotFound)
		if strings.Contains(response.Body.String(), "ask.") {
			test.Fatalf("auth failure leaked ask errors: %s", response.Body.String())
		}
	})
}

func TestDecisionAskLintWarnsButAccepts(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	ask := fixture.newShapeAsk()
	ask["context_eli10"] = "Commit 554b9fc4244aaf9fd0bd464f5840975f5f17f086 has gap L2. The work is ready. You decide whether to wait. See [CHE-1](mention://issue/" + fixture.issueID + ")."
	var created struct {
		Hints []string `json:"hints"`
	}
	testutil.Call(test, testHandler.RequestDecision, fixture.askRequest(test, ask)).Want(http.StatusCreated).JSON(&created)
	joined := strings.Join(created.Hints, "\n")
	if !strings.Contains(joined, "554b9fc") || !strings.Contains(joined, "L2") {
		test.Fatalf("hints = %q, want SHA and L2 named", created.Hints)
	}
	if strings.Contains(joined, fixture.issueID[:8]) {
		test.Fatalf("mention link UUID flagged: %q", created.Hints)
	}
}
