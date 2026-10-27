package decisionaction

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func basePayload() Payload {
	return Payload{
		ActionType:        "agent.env.patch",
		ActionVersion:     1,
		WorkspaceID:       "11111111-1111-1111-1111-111111111111",
		DecisionRequestID: "22222222-2222-2222-2222-222222222222",
		Params:            json.RawMessage(`{"set":{"B":"2","A":"1"},"agent_id":"33333333-3333-3333-3333-333333333333","unset":[]}`),
		Preconditions:     json.RawMessage(`{"custom_env_version":4}`),
		SecretSlots: []SecretSlot{{
			Slot:   "DOOP_AGENT_KEY",
			Target: SlotTarget{AgentID: "33333333-3333-3333-3333-333333333333", EnvKey: "DOOP_AGENT_KEY"},
		}},
		ExpiresAt: time.Date(2026, 10, 28, 9, 0, 0, 123456000, time.FixedZone("ICT", 7*3600)),
	}
}

func TestPayloadCanonicalGolden(t *testing.T) {
	got, err := basePayload().CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"action_type":"agent.env.patch","action_version":1,"decision_request_id":"22222222-2222-2222-2222-222222222222",` +
		`"expires_at":"2026-10-28T02:00:00.123456Z","params":{"agent_id":"33333333-3333-3333-3333-333333333333","set":{"A":"1","B":"2"},"unset":[]},` +
		`"preconditions":{"custom_env_version":4},"secret_slots":[{"slot":"DOOP_AGENT_KEY","target":{"agent_id":"33333333-3333-3333-3333-333333333333","env_key":"DOOP_AGENT_KEY"}}],` +
		`"workspace_id":"11111111-1111-1111-1111-111111111111"}`
	if string(got) != want {
		t.Fatalf("canonical payload\n got %s\nwant %s", got, want)
	}
}

func TestPayloadHash(t *testing.T) {
	base, err := basePayload().Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(base) != 64 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("hash %q is not 64 lowercase hex", base)
	}

	reordered := basePayload()
	reordered.Params = json.RawMessage(`{ "unset": [], "agent_id": "33333333-3333-3333-3333-333333333333", "set": {"A":"1","B":"2"} }`)
	if got, _ := reordered.Hash(); got != base {
		t.Fatalf("key order or spacing changed the hash: %s vs %s", got, base)
	}

	utc := basePayload()
	utc.ExpiresAt = utc.ExpiresAt.UTC()
	if got, _ := utc.Hash(); got != base {
		t.Fatalf("same instant in another zone changed the hash")
	}

	mutations := map[string]func(*Payload){
		"action type":    func(p *Payload) { p.ActionType = "agent.copy" },
		"action version": func(p *Payload) { p.ActionVersion = 2 },
		"workspace":      func(p *Payload) { p.WorkspaceID = "99999999-9999-9999-9999-999999999999" },
		"request":        func(p *Payload) { p.DecisionRequestID = "99999999-9999-9999-9999-999999999999" },
		"params":         func(p *Payload) { p.Params = json.RawMessage(`{"agent_id":"x","set":{},"unset":[]}`) },
		"preconditions":  func(p *Payload) { p.Preconditions = json.RawMessage(`{"custom_env_version":5}`) },
		"slot name":      func(p *Payload) { p.SecretSlots[0].Slot = "OTHER" },
		"slot target":    func(p *Payload) { p.SecretSlots[0].Target.EnvKey = "OTHER" },
		"no slots":       func(p *Payload) { p.SecretSlots = nil },
		"expiry":         func(p *Payload) { p.ExpiresAt = p.ExpiresAt.Add(time.Microsecond) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := basePayload()
			p.SecretSlots = append([]SecretSlot(nil), p.SecretSlots...)
			mutate(&p)
			got, err := p.Hash()
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if got == base {
				t.Fatalf("changing %s left the hash unchanged", name)
			}
		})
	}
}

func TestPayloadNilSlotsHashLikeEmpty(t *testing.T) {
	nilSlots, empty := basePayload(), basePayload()
	nilSlots.SecretSlots, empty.SecretSlots = nil, []SecretSlot{}
	a, _ := nilSlots.Hash()
	b, _ := empty.Hash()
	if a != b || a == "" {
		t.Fatalf("nil and empty slots must hash alike, got %q and %q", a, b)
	}
}
