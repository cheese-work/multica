package decisionaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	skillLow  = "11111111-1111-1111-1111-111111111111"
	skillHigh = "99999999-9999-9999-9999-999999999999"
)

var zeroDigest = strings.Repeat("0", 64)

func skillsRequest(skills string) Request {
	return Request{Type: "agent.skills.set", Version: 1, Params: json.RawMessage(
		`{"agent_id":"` + agentA + `","skill_ids":` + skills + `,"base_sha256":"` + zeroDigest + `","after_sha256":"` + zeroDigest + `"}`)}
}

func TestAgentSkillsSetIsAnOwnerOnlyPreviewedAgentAction(t *testing.T) {
	def, ok := Lookup("agent.skills.set", 1)
	if !ok {
		t.Fatal("agent.skills.set@1 is not registered")
	}
	if def.Target != TargetAgent || !def.OwnerOnly || !def.RequiresPreview || def.MinClass != "spend" {
		t.Fatalf("definition %+v; want agent target, owner only, requires preview, spend class", def)
	}
}

func TestAgentSkillsSetNormalizesToSortedDistinctLowercaseIDs(t *testing.T) {
	upper := strings.ToUpper(skillHigh)
	got, err := Normalize(skillsRequest(`["` + upper + `","` + skillLow + `","` + skillHigh + `"]`))
	if err != nil {
		t.Fatal(err)
	}
	var params AgentSkillsSetParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatal(err)
	}
	if len(params.SkillIDs) != 2 || params.SkillIDs[0] != skillLow || params.SkillIDs[1] != skillHigh {
		t.Fatalf("skill_ids %v; want [%s %s]", params.SkillIDs, skillLow, skillHigh)
	}
}

func TestAgentSkillsSetAcceptsAnEmptyList(t *testing.T) {
	if _, err := Normalize(skillsRequest(`[]`)); err != nil {
		t.Fatalf("an empty list removes every skill, got %v", err)
	}
}

func TestAgentSkillsSetNormalizeRefusals(t *testing.T) {
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf(`"00000000-0000-0000-0000-%012d"`, i)
	}
	fifty := many[:50]
	if _, err := Normalize(skillsRequest("[" + strings.Join(fifty, ",") + "]")); err != nil {
		t.Fatalf("50 skills are allowed, got %v", err)
	}
	withParams := func(body string) Request {
		return Request{Type: "agent.skills.set", Version: 1, Params: json.RawMessage(body)}
	}
	digests := `"base_sha256":"` + zeroDigest + `","after_sha256":"` + zeroDigest + `"`
	cases := map[string]Request{
		"51 skills":              skillsRequest("[" + strings.Join(many, ",") + "]"),
		"skill id not a uuid":    skillsRequest(`["nope"]`),
		"skill_ids is null":      skillsRequest(`null`),
		"skill_ids is absent":    withParams(`{"agent_id":"` + agentA + `",` + digests + `}`),
		"agent id not a uuid":    withParams(`{"agent_id":"nope","skill_ids":[],` + digests + `}`),
		"unknown field":          withParams(`{"agent_id":"` + agentA + `","skill_ids":[],"enabled":true,` + digests + `}`),
		"malformed base digest":  withParams(`{"agent_id":"` + agentA + `","skill_ids":[],"base_sha256":"abc","after_sha256":"` + zeroDigest + `"}`),
		"malformed after digest": withParams(`{"agent_id":"` + agentA + `","skill_ids":[],"base_sha256":"` + zeroDigest + `","after_sha256":"ABC"}`),
	}
	slotted := skillsRequest(`[]`)
	slotted.SecretSlots = []SecretSlot{slotFor(agentA, "K")}
	cases["secret slot"] = slotted
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(req)
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Code != CodeInvalidParams {
				t.Fatalf("got %v; want an invalid_params refusal", err)
			}
		})
	}
}
