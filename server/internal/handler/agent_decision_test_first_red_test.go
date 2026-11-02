package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const redImpossibleReason = "The change is a pure rename; no behavior exists that a failing test could observe."

// testFirstRedFixture proposes a test_first_red waiver. Author and peer run on
// the claude lab, the leader (reviewer) on codex.
func testFirstRedFixture(t *testing.T) agentDecisionFixture {
	t.Helper()
	f := newAgentDecisionFixture(t)
	f.scope["waived_gate"] = "test_first_red"
	f.scope["reviewer_agent_id"] = f.leaderID
	f.scope["red_impossible_reason"] = redImpossibleReason
	f.scope["weaker_evidence_kind"] = "characterization_test"
	f.scope["followup_check"] = "CHE-9999: add a regression test when the rename lands behind a flag"
	return f
}

func testFirstRedOpinion(f agentDecisionFixture, agent, reason string) string {
	opinion := strings.Replace(f.opinion(agent, "PASS"), "waived_gate: screenshot_evidence", "waived_gate: test_first_red", 1)
	if reason != "" {
		opinion = strings.Replace(opinion, "verdict: PASS\n", "verdict: PASS\nred_impossible_reason: "+reason+"\n", 1)
	}
	return opinion
}

func TestAgentDecisionTestFirstRedAcceptedAsWaived(t *testing.T) {
	enableAgentDecisions(t, true)
	f := testFirstRedFixture(t)
	f.propose(t)
	opinions := map[string]string{
		f.leaderID: testFirstRedOpinion(f, f.leaderID, redImpossibleReason),
		f.peerID:   testFirstRedOpinion(f, f.peerID, ""),
	}
	for agent, opinion := range opinions {
		f.commit(t, agent, opinion)
	}
	for agent, opinion := range opinions {
		f.reveal(t, agent, "PASS", opinion, http.StatusCreated)
	}
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("test_first_red settled %d records, want 1", n)
	}
	var out briefResponse
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Decisions {
		if d.InstanceID == f.instance {
			if d.Gate != "test_first_red" || d.Outcome != "WAIVED" {
				t.Fatalf("brief record: %+v, want gate test_first_red outcome WAIVED", d)
			}
			return
		}
	}
	t.Fatal("test_first_red record missing from brief")
}

func TestAgentDecisionTestFirstRedRefusals(t *testing.T) {
	const missing = "test_first_red needs"
	cases := map[string]struct {
		mutate func(t *testing.T, f *agentDecisionFixture)
		want   string
	}{
		"no_reviewer":      {func(t *testing.T, f *agentDecisionFixture) { delete(f.scope, "reviewer_agent_id") }, missing},
		"no_reason":        {func(t *testing.T, f *agentDecisionFixture) { delete(f.scope, "red_impossible_reason") }, missing},
		"no_evidence_kind": {func(t *testing.T, f *agentDecisionFixture) { delete(f.scope, "weaker_evidence_kind") }, missing},
		"unknown_kind":     {func(t *testing.T, f *agentDecisionFixture) { f.scope["weaker_evidence_kind"] = "trust_me" }, missing},
		"no_followup":      {func(t *testing.T, f *agentDecisionFixture) { delete(f.scope, "followup_check") }, missing},
		"reviewer_not_a_signer": {func(t *testing.T, f *agentDecisionFixture) {
			f.scope["reviewer_agent_id"] = f.writerID
		}, missing},
		"adjudicate_route": {func(t *testing.T, f *agentDecisionFixture) { f.scope["route"] = "adjudicate" }, missing},
		// peer shares the author's lab, so naming it reviewer has no different-lab agreement.
		"reviewer_same_lab_as_author": {func(t *testing.T, f *agentDecisionFixture) {
			f.scope["reviewer_agent_id"] = f.peerID
		}, "different lab than the author"},
		// leader moves to the author's lab, so no signer is a different-lab reviewer.
		"all_labs_equal": {func(t *testing.T, f *agentDecisionFixture) {
			f.retask(t, f.leaderID, f.runtimeOf(t, f.writerID), "claude-test")
		}, "signer labs must differ"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := testFirstRedFixture(t)
			tc.mutate(t, &f)
			w := f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("refused for the wrong reason: %s, want %q", w.Body.String(), tc.want)
			}
		})
	}
	t.Run("reviewer_opinion_without_reason", func(t *testing.T) {
		f := testFirstRedFixture(t)
		f.propose(t)
		opinion := testFirstRedOpinion(f, f.leaderID, "")
		f.commit(t, f.leaderID, opinion)
		f.commit(t, f.peerID, testFirstRedOpinion(f, f.peerID, ""))
		f.reveal(t, f.leaderID, "PASS", opinion, http.StatusBadRequest)
	})
	t.Run("reviewer_opinion_with_other_reason", func(t *testing.T) {
		f := testFirstRedFixture(t)
		f.propose(t)
		opinion := testFirstRedOpinion(f, f.leaderID, "A different reason.")
		f.commit(t, f.leaderID, opinion)
		f.commit(t, f.peerID, testFirstRedOpinion(f, f.peerID, ""))
		f.reveal(t, f.leaderID, "PASS", opinion, http.StatusBadRequest)
	})
}

func TestAgentDecisionTestFirstRedFieldsOnlyForThatGate(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.scope["followup_check"] = "CHE-9999"
	f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
}

func TestAgentDecisionExcludedClassesRefused(t *testing.T) {
	for _, class := range []string{"irreversible", "design_direction", "architecture", "high_level"} {
		t.Run("class_"+class, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.scope["decision_class"] = class
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
		t.Run("jev_class_"+class, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.scope["decider"] = "Jev for Cheese"
			f.scope["confidence"] = 0.9
			f.scope["decision_class"] = class
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
		t.Run("jev_gate_"+class, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.scope["decider"] = "Jev for Cheese"
			f.scope["confidence"] = 0.9
			f.scope["waived_gate"] = class
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
	for _, gate := range []string{"design_direction", "architecture", "high_level"} {
		t.Run("gate_"+gate, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			f.scope["waived_gate"] = gate
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
}

func TestAgentDecisionJevForCheeseShowsAsAgentDecision(t *testing.T) {
	enableAgentDecisions(t, true)
	f := newAgentDecisionFixture(t)
	f.scope["decider"] = "Jev for Cheese"
	f.scope["confidence"] = 0.8
	f.propose(t)
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.commit(t, agent, f.opinion(agent, "PASS"))
	}
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.reveal(t, agent, "PASS", f.opinion(agent, "PASS"), http.StatusCreated)
	}
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	var out briefResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Decisions {
		if d.InstanceID != f.instance {
			continue
		}
		if d.Decider != "Jev for Cheese" || d.Confidence == nil || *d.Confidence != 0.8 {
			t.Fatalf("brief record: %+v, want decider Jev for Cheese, confidence 0.8", d)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM decision_answer WHERE decision_request_id=$1`, f.instance); n != 0 {
			t.Fatalf("Jev record produced %d answers from Cheese", n)
		}
		if got := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id=$1 AND authority='agents'`, f.instance); got != 1 {
			t.Fatal("Jev record is not an agent-authority decision")
		}
		return
	}
	t.Fatal("Jev record missing from brief")
}

func TestAgentDecisionJevDeciderRefusals(t *testing.T) {
	cases := map[string]func(f *agentDecisionFixture){
		"other_decider":     func(f *agentDecisionFixture) { f.scope["decider"] = "Cheese"; f.scope["confidence"] = 0.9 },
		"no_confidence":     func(f *agentDecisionFixture) { f.scope["decider"] = "Jev for Cheese" },
		"confidence_zero":   func(f *agentDecisionFixture) { f.scope["decider"] = "Jev for Cheese"; f.scope["confidence"] = 0 },
		"confidence_over_1": func(f *agentDecisionFixture) { f.scope["decider"] = "Jev for Cheese"; f.scope["confidence"] = 1.5 },
		"confidence_alone":  func(f *agentDecisionFixture) { f.scope["confidence"] = 0.9 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAgentDecisionFixture(t)
			mutate(&f)
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
		})
	}
}

// A record made under allowlist version 1 stays valid; the new gate needs 2.
func TestAgentDecisionAllowlistVersions(t *testing.T) {
	for _, tc := range []struct {
		version int
		gate    string
		want    bool
	}{
		{1, "screenshot_evidence", true},
		{2, "screenshot_evidence", true},
		{1, "test_first_red", false},
		{2, "test_first_red", true},
		{3, "screenshot_evidence", false},
		{2, "security", false},
	} {
		if got := agentDecisionAllowedAt(tc.version, tc.gate); got != tc.want {
			t.Errorf("agentDecisionAllowedAt(%d, %q) = %v, want %v", tc.version, tc.gate, got, tc.want)
		}
	}
}
