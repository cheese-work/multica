package handler

import (
	"encoding/json"
	"fmt"
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

// setJev writes the workspace Jev setting directly and restores the settings after the test.
func setJev(t *testing.T, agentID, mode string) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{agent_decisions_jev}',jsonb_build_object('agent_id',$2::text,'mode',$3::text)) WHERE id=$1`, testWorkspaceID, agentID, mode)
}

func jevFixture(t *testing.T) agentDecisionFixture {
	t.Helper()
	f := newAgentDecisionFixture(t)
	f.scope["decider"] = "Jev for Cheese"
	f.scope["confidence"] = 0.8
	return f
}

func settleJev(t *testing.T, f agentDecisionFixture) {
	t.Helper()
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.commit(t, agent, f.opinion(agent, "PASS"))
	}
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.reveal(t, agent, "PASS", f.opinion(agent, "PASS"), http.StatusCreated)
	}
}

func TestAgentDecisionJevForCheeseShowsAsAgentDecision(t *testing.T) {
	enableAgentDecisions(t, true)
	f := jevFixture(t)
	setJev(t, f.writerID, "live")
	f.propose(t)
	settleJev(t, f)
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("live Jev record settled %d times, want 1", n)
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

// The 7-day shadow runs independently of agent_decisions_enabled: with the main switch on,
// a shadow Jev record is recorded but never settles.
func TestAgentDecisionJevShadowRecordsButNeverSettles(t *testing.T) {
	enableAgentDecisions(t, true)
	f := jevFixture(t)
	setJev(t, f.writerID, "shadow")
	f.propose(t)
	settleJev(t, f)
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id=$1`, f.instance); n != 1 {
		t.Fatalf("shadow proposal not recorded: %d requests", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatalf("shadow Jev record settled %d times with agent_decisions_enabled on", n)
	}
}

func TestAgentDecisionJevProposalNeedsVerifiedJevAgent(t *testing.T) {
	cases := map[string]func(t *testing.T, f agentDecisionFixture){
		"no_setting":   func(t *testing.T, f agentDecisionFixture) {},
		"mode_off":     func(t *testing.T, f agentDecisionFixture) { setJev(t, f.writerID, "off") },
		"unknown_mode": func(t *testing.T, f agentDecisionFixture) { setJev(t, f.writerID, "yolo") },
		"other_agent":  func(t *testing.T, f agentDecisionFixture) { setJev(t, f.leaderID, "live") },
		"not_a_uuid":   func(t *testing.T, f agentDecisionFixture) { setJev(t, "jev", "live") },
		"shadow_other": func(t *testing.T, f agentDecisionFixture) { setJev(t, f.peerID, "shadow") },
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			enableAgentDecisions(t, true)
			f := jevFixture(t)
			configure(t, f)
			w := f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
			if !strings.Contains(w.Body.String(), "Jev") {
				t.Fatalf("refused for the wrong reason: %s", w.Body.String())
			}
		})
	}
}

func TestAgentDecisionJevSettingIsOwnerOnlyAndValidated(t *testing.T) {
	enableAgentDecisions(t, false)
	jev := createHandlerTestAgent(t, "jev-setting-agent", nil)
	set := func(userID string, body map[string]any) int {
		req := newRequest(http.MethodPut, "/api/agent-decisions/jev", body)
		req.Header.Set("X-User-ID", userID)
		w := httptest.NewRecorder()
		testHandler.SetAgentDecisionJev(w, req)
		return w.Code
	}
	if code := set("00000000-0000-0000-0000-000000000000", map[string]any{"agent_id": jev, "mode": "shadow"}); code == http.StatusOK {
		t.Fatal("non-owner changed the Jev setting")
	}
	for name, body := range map[string]map[string]any{
		"bad_mode":      {"agent_id": jev, "mode": "yolo"},
		"bad_agent":     {"agent_id": "jev", "mode": "shadow"},
		"unknown_agent": {"agent_id": "00000000-0000-0000-0000-000000000001", "mode": "shadow"},
		"no_mode":       {"agent_id": jev},
	} {
		if code := set(testUserID, body); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", name, code)
		}
	}
	if code := set(testUserID, map[string]any{"agent_id": jev, "mode": "shadow"}); code != http.StatusOK {
		t.Fatalf("owner set status=%d", code)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace WHERE id=$1 AND settings->'agent_decisions_jev'->>'agent_id'=$2 AND settings->'agent_decisions_jev'->>'mode'='shadow'`, testWorkspaceID, jev); got != 1 {
		t.Fatal("owner Jev setting did not stick")
	}
	if code := set(testUserID, map[string]any{"mode": "off"}); code != http.StatusOK {
		t.Fatalf("owner off status=%d", code)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace WHERE id=$1 AND settings ? 'agent_decisions_jev'`, testWorkspaceID); got != 0 {
		t.Fatal("off did not clear the Jev setting")
	}
}

// decision_class is required: a missing or unknown class fails closed, for agent and Jev proposals alike.
func TestAgentDecisionClassRequiredAndRecognised(t *testing.T) {
	for _, jev := range []bool{false, true} {
		for name, mutate := range map[string]func(f *agentDecisionFixture){
			"missing":    func(f *agentDecisionFixture) { delete(f.scope, "decision_class") },
			"empty":      func(f *agentDecisionFixture) { f.scope["decision_class"] = "" },
			"unknown":    func(f *agentDecisionFixture) { f.scope["decision_class"] = "something_new" },
			"misspelled": func(f *agentDecisionFixture) { f.scope["decision_class"] = "irreversable" },
			"cased":      func(f *agentDecisionFixture) { f.scope["decision_class"] = "Architecture" },
			"padded":     func(f *agentDecisionFixture) { f.scope["decision_class"] = " reversible_gate" },
		} {
			t.Run(fmt.Sprintf("jev=%v/%s", jev, name), func(t *testing.T) {
				enableAgentDecisions(t, true)
				f := newAgentDecisionFixture(t)
				if jev {
					f.scope["decider"] = "Jev for Cheese"
					f.scope["confidence"] = 0.9
					setJev(t, f.writerID, "live")
				}
				mutate(&f)
				w := f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
				if !strings.Contains(w.Body.String(), "decision_class") {
					t.Fatalf("refused for the wrong reason: %s", w.Body.String())
				}
				if brief := readBrief(t); !brief.Enabled {
					t.Fatalf("an unknown class tripped the stop rule: %+v", brief)
				}
			})
		}
	}
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
