package decisionaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	agentA = "33333333-3333-3333-3333-333333333333"
	agentB = "44444444-4444-4444-4444-444444444444"
	rt     = "55555555-5555-5555-5555-555555555555"
)

func slotFor(agent, key string) SecretSlot {
	return SecretSlot{Slot: key, Target: SlotTarget{AgentID: agent, EnvKey: key}}
}

func TestNormalizeRefusals(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		code string
	}{
		{"unknown type", Request{Type: "agent.delete", Version: 1, Params: json.RawMessage(`{}`)}, "unknown_action"},
		{"unknown version", Request{Type: "agent.copy", Version: 2, Params: json.RawMessage(`{}`)}, "unknown_action"},
		{"copy extra field", Request{Type: "agent.copy", Version: 1, Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","name":"n","runtime_id":"` + rt + `","admin":true}`)}, "invalid_params"},
		{"copy missing name", Request{Type: "agent.copy", Version: 1, Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","runtime_id":"` + rt + `"}`)}, "invalid_params"},
		{"copy bad uuid", Request{Type: "agent.copy", Version: 1, Params: json.RawMessage(`{"source_agent_id":"nope","name":"n","runtime_id":"` + rt + `"}`)}, "invalid_params"},
		{"copy takes no secret slots", Request{Type: "agent.copy", Version: 1, Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","name":"n","runtime_id":"` + rt + `"}`), SecretSlots: []SecretSlot{slotFor(agentA, "K")}}, "invalid_params"},
		{"copy max_concurrent_tasks out of range", Request{Type: "agent.copy", Version: 1, Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","name":"n","runtime_id":"` + rt + `","max_concurrent_tasks":51}`)}, "invalid_params"},
		{"env empty", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`)}, "invalid_params"},
		{"env set and unset same key", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","set":{"A":"1"},"unset":["A"]}`)}, "invalid_params"},
		{"env masked marker", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","set":{"A":"****"}}`)}, "invalid_params"},
		{"env value for slot key in params", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","set":{"K":"hunter2"}}`), SecretSlots: []SecretSlot{slotFor(agentA, "K")}}, "secret_in_params"},
		{"env slot key also unset", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","unset":["K"]}`), SecretSlots: []SecretSlot{slotFor(agentA, "K")}}, "invalid_params"},
		{"env slot for another agent", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`), SecretSlots: []SecretSlot{slotFor(agentB, "K")}}, "invalid_params"},
		{"env duplicate slot", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`), SecretSlots: []SecretSlot{slotFor(agentA, "K"), slotFor(agentA, "K")}}, "invalid_params"},
		{"env slot name differs from env key", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`), SecretSlots: []SecretSlot{{Slot: "S", Target: SlotTarget{AgentID: agentA, EnvKey: "K"}}}}, "invalid_params"},
		{"env bad key", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`), SecretSlots: []SecretSlot{slotFor(agentA, "1 BAD")}}, "invalid_params"},
		{"top-up missing window", Request{Type: "issue.run_budget.top_up", Version: 1, Params: json.RawMessage(`{"issue_id":"` + agentA + `"}`)}, "invalid_params"},
		{"top-up bad refusal id", Request{Type: "issue.run_budget.top_up", Version: 1, Params: json.RawMessage(`{"issue_id":"` + agentA + `","window_id":"w","refusal_id":"nope"}`)}, "invalid_params"},
		{"top-up extra field", Request{Type: "issue.run_budget.top_up", Version: 1, Params: json.RawMessage(`{"issue_id":"` + agentA + `","window_id":"w","new_cap":9}`)}, "invalid_params"},
		{"trailing json", Request{Type: "agent.env.patch", Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","unset":["A"]} {}`)}, "invalid_params"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize(tc.req)
			var actionErr *Error
			if !errors.As(err, &actionErr) || actionErr.Code != tc.code {
				t.Fatalf("want code %q, got %v", tc.code, err)
			}
		})
	}
}

func TestNormalizeSecretInParamsNeverEchoesValue(t *testing.T) {
	_, err := Normalize(Request{Type: "agent.env.patch", Version: 1,
		Params:      json.RawMessage(`{"agent_id":"` + agentA + `","set":{"K":"sentinel-value-123"}}`),
		SecretSlots: []SecretSlot{slotFor(agentA, "K")}})
	if err == nil || strings.Contains(err.Error(), "sentinel-value-123") {
		t.Fatalf("error must exist and not echo the value, got %v", err)
	}
}

func TestNormalizeIsStable(t *testing.T) {
	a, err := Normalize(Request{Type: "agent.env.patch", Version: 1,
		Params:      json.RawMessage(`{"unset":["Z","A"],"agent_id":"` + agentA + `","set":{"B":"2"}}`),
		SecretSlots: []SecretSlot{slotFor(agentA, "Y"), slotFor(agentA, "K")}})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	b, err := Normalize(Request{Type: "agent.env.patch", Version: 1,
		Params:      json.RawMessage(`{"agent_id":"` + agentA + `","set":{"B":"2"},"unset":["A","Z"]}`),
		SecretSlots: []SecretSlot{slotFor(agentA, "K"), slotFor(agentA, "Y")}})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if string(a.Params) != string(b.Params) {
		t.Fatalf("params differ:\n%s\n%s", a.Params, b.Params)
	}
	if len(a.SecretSlots) != 2 || a.SecretSlots[0].Slot != "K" || a.SecretSlots[1].Slot != "Y" {
		t.Fatalf("slots not sorted by name: %+v", a.SecretSlots)
	}
}

func TestNormalizeCopy(t *testing.T) {
	verbatim, err := Normalize(Request{Type: "agent.copy", Version: 1,
		Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","name":"Doop","runtime_id":"` + rt + `"}`)})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	explicit, err := Normalize(Request{Type: "agent.copy", Version: 1,
		Params: json.RawMessage(`{"runtime_id":"` + rt + `","name":"Doop","source_agent_id":"` + agentA + `","instructions":null,"custom_args":[]}`)})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if string(verbatim.Params) != string(explicit.Params) {
		t.Fatalf("omitted and explicit-default params must normalize alike:\n%s\n%s", verbatim.Params, explicit.Params)
	}
	replaced, err := Normalize(Request{Type: "agent.copy", Version: 1,
		Params: json.RawMessage(`{"source_agent_id":"` + agentA + `","name":"Doop","runtime_id":"` + rt + `","instructions":"new text"}`)})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if string(replaced.Params) == string(verbatim.Params) {
		t.Fatalf("replacement instructions must change the normalized params")
	}
	if verbatim.Definition.MinClass != "reversible" {
		t.Fatalf("min class = %q", verbatim.Definition.MinClass)
	}
}

func TestDefinitionTTL(t *testing.T) {
	def := mustLookup(t, "agent.copy", 1)
	for name, tc := range map[string]struct {
		in      time.Duration
		want    time.Duration
		wantErr bool
	}{
		"default":  {0, 24 * time.Hour, false},
		"explicit": {2 * time.Hour, 2 * time.Hour, false},
		"at max":   {72 * time.Hour, 72 * time.Hour, false},
		"past max": {72*time.Hour + time.Second, 0, true},
		"negative": {-time.Hour, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := def.TTL(tc.in)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("TTL(%v) = %v, %v", tc.in, got, err)
			}
		})
	}
}

func mustLookup(t *testing.T, typ string, version int) *Definition {
	t.Helper()
	def, ok := Lookup(typ, version)
	if !ok {
		t.Fatalf("%s v%d not registered", typ, version)
	}
	return def
}

const (
	budgetIssue  = "66666666-6666-6666-6666-666666666666"
	budgetWindow = "2026-11-01T05:22:39.123456Z"
)

func raiseRequest(params string) Request {
	return Request{Type: "issue.run_budget.raise", Version: 1, Params: json.RawMessage(params)}
}

func TestRunBudgetRaiseIsASpendAction(t *testing.T) {
	def, ok := Lookup("issue.run_budget.raise", 1)
	if !ok || def.MinClass != "spend" || def.Target != TargetIssue {
		t.Fatalf("definition = %+v, found %v; want a spend-class issue-target action", def, ok)
	}
}

func TestRunBudgetRaiseNormalizesAndCoversRefusalIDInTheHash(t *testing.T) {
	base := `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"` + budgetWindow + `"}`
	withRefusal := `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"` + budgetWindow + `","refusal_id":"` + agentA + `"}`
	hash := func(params string) string {
		t.Helper()
		normalized, err := Normalize(raiseRequest(params))
		if err != nil {
			t.Fatal(err)
		}
		return string(normalized.Params)
	}
	if got := hash(base); strings.Contains(got, "refusal_id") {
		t.Fatalf("params %s carry a refusal_id nobody sent", got)
	}
	if got := hash(withRefusal); !strings.Contains(got, `"refusal_id":"`+agentA+`"`) {
		t.Fatalf("params %s lack the refusal_id", got)
	}
}

func TestRunBudgetRaiseRefusals(t *testing.T) {
	for name, params := range map[string]string{
		"extra field":      `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"` + budgetWindow + `","admin":true}`,
		"bad issue id":     `{"issue_id":"x","new_cap":40,"window_id":"` + budgetWindow + `"}`,
		"cap below one":    `{"issue_id":"` + budgetIssue + `","new_cap":0,"window_id":"` + budgetWindow + `"}`,
		"cap over maximum": `{"issue_id":"` + budgetIssue + `","new_cap":1001,"window_id":"` + budgetWindow + `"}`,
		"missing window":   `{"issue_id":"` + budgetIssue + `","new_cap":40}`,
		"window not rfc":   `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"yesterday"}`,
		"window not exact": `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"2026-11-01T12:22:39.100+07:00"}`,
		"bad refusal id":   `{"issue_id":"` + budgetIssue + `","new_cap":40,"window_id":"` + budgetWindow + `","refusal_id":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(raiseRequest(params))
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeInvalidParams {
				t.Fatalf("err = %v; want invalid_params", err)
			}
		})
	}
}
