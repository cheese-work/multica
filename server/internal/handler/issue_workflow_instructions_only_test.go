package handler

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// CHE-5843: the instructions-only action is named only when instructions alone
// differ. An explicit entry or waits_on_open_decision that differs from the
// live step is a structural edit; an omitted one keeps the live value.
func TestOnlyInstructionsDiffer(t *testing.T) {
	gate := issueworkflow.EntryGate{Guard: "pr_open", Mode: "block"}
	other := issueworkflow.EntryGate{Guard: "ci_green", Mode: "warn"}
	live := []issueworkflow.Step{
		{StatusKey: "todo", Handler: issueworkflow.Handler{Type: "agent", ID: "a"}, Instructions: "a"},
		{StatusKey: "in_progress", Handler: issueworkflow.Handler{Type: "agent", ID: "a"}, Instructions: "b", Entry: []issueworkflow.EntryGate{gate}, WaitsOnOpenDecision: true},
	}
	// request copies live, sets new instructions on in_progress, then applies edit.
	request := func(edit func(*IssueWorkflowStepInput)) []IssueWorkflowStepInput {
		inputs := make([]IssueWorkflowStepInput, len(live))
		for i, step := range live {
			raw, _ := json.Marshal(stepToPayload(step))
			_ = json.Unmarshal(raw, &inputs[i])
			// A write that omits the fields keeps the stored ones.
			inputs[i].Entry, inputs[i].WaitsOnOpenDecision = nil, nil
		}
		inputs[1].Instructions = "b2"
		if edit != nil {
			edit(&inputs[1])
		}
		return inputs
	}
	truth, falsity := true, false
	cases := map[string]struct {
		edit func(*IssueWorkflowStepInput)
		want bool
	}{
		"omitted entry and wait keep the live values": {nil, true},
		"explicit same entry":                         {func(s *IssueWorkflowStepInput) { s.Entry = []issueworkflow.EntryGate{gate} }, true},
		"explicit same wait":                          {func(s *IssueWorkflowStepInput) { s.WaitsOnOpenDecision = &truth }, true},
		"explicit different entry":                    {func(s *IssueWorkflowStepInput) { s.Entry = []issueworkflow.EntryGate{other} }, false},
		"explicit empty entry clears the gate":        {func(s *IssueWorkflowStepInput) { s.Entry = []issueworkflow.EntryGate{} }, false},
		"explicit extra entry":                        {func(s *IssueWorkflowStepInput) { s.Entry = []issueworkflow.EntryGate{gate, other} }, false},
		"explicit false wait clears the flag":         {func(s *IssueWorkflowStepInput) { s.WaitsOnOpenDecision = &falsity }, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			key, ok := onlyInstructionsDiffer(live, request(c.edit))
			if ok != c.want || (ok && key != "in_progress") {
				t.Fatalf("onlyInstructionsDiffer = %q, %v; want ok=%v", key, ok, c.want)
			}
		})
	}
	t.Run("explicit wait true on a step that does not wait", func(t *testing.T) {
		inputs := request(nil)
		inputs[0].Instructions = "a"
		inputs[0].WaitsOnOpenDecision = &truth
		if key, ok := onlyInstructionsDiffer(live, inputs); ok {
			t.Fatalf("onlyInstructionsDiffer = %q, true; want false", key)
		}
	})
	t.Run("explicit gate on a step that has none", func(t *testing.T) {
		inputs := request(nil)
		inputs[0].Entry = []issueworkflow.EntryGate{gate}
		if key, ok := onlyInstructionsDiffer(live, inputs); ok {
			t.Fatalf("onlyInstructionsDiffer = %q, true; want false", key)
		}
	})
}
