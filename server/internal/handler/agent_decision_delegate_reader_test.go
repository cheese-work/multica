package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// storeDelegateDecider rewrites a proposed scope to carry the decider value "delegate", as a
// later release will store it. Input still refuses "delegate" and the scope table is insert-only,
// so the test lifts the immutability trigger for the one rewrite and restores it in cleanup.
func storeDelegateDecider(t *testing.T, f agentDecisionFixture) {
	t.Helper()
	dbfx.Exec(t, `ALTER TABLE decision_agent_scope DISABLE TRIGGER agent_decision_scope_immutable`)
	restored := false
	restore := func() {
		if !restored {
			restored = true
			dbfx.Exec(t, `ALTER TABLE decision_agent_scope ENABLE TRIGGER agent_decision_scope_immutable`)
		}
	}
	t.Cleanup(restore)
	dbfx.Exec(t, `UPDATE decision_agent_scope SET scope=jsonb_set(scope,'{Proposal,decider}','"delegate"') WHERE instance_id=$1`, f.instance)
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_scope WHERE instance_id=$1 AND scope->'Proposal'->>'decider'='delegate'`, f.instance); n != 1 {
		t.Fatalf("stored scope rewrite touched %d rows, want 1", n)
	}
	restore()
}

// S4a: readers accept a stored "delegate" exactly like the legacy label.
func TestAgentDecisionStoredDelegateSettlesInLiveMode(t *testing.T) {
	enableAgentDecisions(t, true)
	before := readMetrics(t)
	f := jevFixture(t)
	setJev(t, f.writerID, "live")
	f.propose(t)
	storeDelegateDecider(t, f)
	settleJev(t, f)
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("stored delegate settled %d times in live mode, want 1", n)
	}
	after := readMetrics(t)
	if after.Jev.Decisions != before.Jev.Decisions+1 {
		t.Fatalf("metrics counted %d delegate decisions, want 1: before=%+v after=%+v", after.Jev.Decisions-before.Jev.Decisions, before.Jev, after.Jev)
	}
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	var out briefResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Decisions {
		if d.InstanceID == f.instance {
			if d.Decider != "delegate" || d.Confidence == nil || *d.Confidence != 0.8 {
				t.Fatalf("brief record: %+v, want decider delegate, confidence 0.8", d)
			}
			return
		}
	}
	t.Fatal("stored delegate record missing from the brief")
}

func TestAgentDecisionStoredDelegateDoesNotSettleInShadowMode(t *testing.T) {
	enableAgentDecisions(t, true)
	f := jevFixture(t)
	setJev(t, f.writerID, "shadow")
	f.propose(t)
	storeDelegateDecider(t, f)
	settleJev(t, f)
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("stored delegate settled %d times in shadow mode, want 0", n)
	}
}

// R1 changes readers only: the input value "delegate" stays refused until R2.
func TestAgentDecisionDelegateInputStillRefused(t *testing.T) {
	enableAgentDecisions(t, true)
	f := newAgentDecisionFixture(t)
	setJev(t, f.writerID, "live")
	f.scope["decider"] = "delegate"
	f.scope["confidence"] = 0.8
	f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
}
