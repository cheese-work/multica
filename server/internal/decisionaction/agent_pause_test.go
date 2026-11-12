package decisionaction

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestAgentPauseResumeNormalize(t *testing.T) {
	for _, typ := range []string{"agent.pause", "agent.resume"} {
		got, err := Normalize(Request{Type: typ, Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`)})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if string(got.Params) != `{"agent_id":"`+agentA+`"}` || got.Definition.MinClass != "reversible" || got.Definition.Target != TargetAgent {
			t.Fatalf("%s: unexpected %s %+v", typ, got.Params, got.Definition)
		}
		for name, req := range map[string]Request{
			"extra field":  {Type: typ, Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `","cancel_running":true}`)},
			"bad uuid":     {Type: typ, Version: 1, Params: json.RawMessage(`{"agent_id":"nope"}`)},
			"secret slots": {Type: typ, Version: 1, Params: json.RawMessage(`{"agent_id":"` + agentA + `"}`), SecretSlots: []SecretSlot{slotFor(agentA, "K")}},
		} {
			var actionErr *Error
			if _, err := Normalize(req); !errors.As(err, &actionErr) || actionErr.Code != CodeInvalidParams {
				t.Fatalf("%s %s: want invalid_params, got %v", typ, name, err)
			}
		}
	}
}
