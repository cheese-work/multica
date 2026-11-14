package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type briefMetrics struct {
	Waivers []struct {
		AgentID string `json:"agent_id"`
		Agent   string `json:"agent"`
		Count   int    `json:"count"`
	} `json:"waivers"`
	Jev struct {
		Decisions       int     `json:"decisions"`
		Vetoed          int     `json:"vetoed"`
		VetoRate        float64 `json:"veto_rate"`
		ExcludedClasses int     `json:"excluded_class_decisions"`
	} `json:"jev"`
	JevAsks []struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
		Answer    string `json:"answer"`
	} `json:"jev_asks"`
}

func readMetrics(t *testing.T, asks ...string) briefMetrics {
	t.Helper()
	target := "/api/agent-decisions/brief"
	if len(asks) > 0 {
		q := url.Values{}
		for _, id := range asks {
			q.Add("jev_ask", id)
		}
		target += "?" + q.Encode()
	}
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, target, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("brief status=%d: %s", w.Code, w.Body)
	}
	var out briefMetrics
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waiverCount(m briefMetrics, agentID string) int {
	for _, w := range m.Waivers {
		if w.AgentID == agentID {
			return w.Count
		}
	}
	return 0
}

// Zero data: the new blocks are present, empty and zero, never null or missing.
func TestAgentDecisionBriefMetricsZeroData(t *testing.T) {
	enableAgentDecisions(t, true)
	dbfx.Exec(t, `DELETE FROM decision_agent_settlement WHERE workspace_id=$1`, testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["waivers"]) != "[]" || string(raw["jev_asks"]) != "[]" {
		t.Fatalf("waivers=%s jev_asks=%s, want empty arrays", raw["waivers"], raw["jev_asks"])
	}
	m := readMetrics(t)
	if m.Jev.Decisions != 0 || m.Jev.Vetoed != 0 || m.Jev.VetoRate != 0 || m.Jev.ExcludedClasses != 0 {
		t.Fatalf("jev with no data: %+v, want all zero", m.Jev)
	}
}

// A waiver counts for the agent whose work it waived; a plain gate record does not.
func TestAgentDecisionBriefCountsWaiversPerAgent(t *testing.T) {
	enableAgentDecisions(t, true)
	f := testFirstRedFixture(t)
	before := readMetrics(t)
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
	after := readMetrics(t)
	if got := waiverCount(after, f.writerID) - waiverCount(before, f.writerID); got != 1 {
		t.Fatalf("waivers for the author rose by %d, want 1: %+v", got, after.Waivers)
	}
	for _, w := range after.Waivers {
		if w.AgentID == f.writerID && w.Agent == "" {
			t.Fatalf("waiver row has no agent name: %+v", w)
		}
	}
}

func TestAgentDecisionBriefIgnoresNonWaiverGates(t *testing.T) {
	enableAgentDecisions(t, true)
	f := prepareBrief(t)
	effective(t, f)
	if got := waiverCount(readMetrics(t), f.writerID); got != 0 {
		t.Fatalf("a screenshot_evidence record counted as %d waivers", got)
	}
}

func TestAgentDecisionBriefJevDecisionsAndVetoRate(t *testing.T) {
	enableAgentDecisions(t, true)
	before := readMetrics(t)
	f := jevFixture(t)
	setJev(t, f.writerID, "live")
	f.propose(t)
	settleJev(t, f)
	mid := readMetrics(t)
	if mid.Jev.Decisions != before.Jev.Decisions+1 || mid.Jev.Vetoed != before.Jev.Vetoed {
		t.Fatalf("after a Jev decision: before=%+v after=%+v", before.Jev, mid.Jev)
	}
	f.post(t, f.leaderID, f.vetoBody("metrics"), http.StatusCreated)
	after := readMetrics(t)
	if after.Jev.Vetoed != mid.Jev.Vetoed+1 || after.Jev.Decisions != mid.Jev.Decisions {
		t.Fatalf("after a veto: mid=%+v after=%+v", mid.Jev, after.Jev)
	}
	want := float64(after.Jev.Vetoed) / float64(after.Jev.Decisions)
	if after.Jev.VetoRate != want {
		t.Fatalf("veto rate %v, want %v", after.Jev.VetoRate, want)
	}
	if after.Jev.ExcludedClasses != 0 {
		t.Fatalf("excluded-class decisions = %d, want 0", after.Jev.ExcludedClasses)
	}
}

// An excluded-class proposal is refused, so it leaves no decision behind: the count stays zero.
func TestAgentDecisionBriefJevExcludedClassStaysZero(t *testing.T) {
	enableAgentDecisions(t, true)
	f := jevFixture(t)
	setJev(t, f.writerID, "live")
	f.scope["decision_class"] = "irreversible"
	f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
	if got := readMetrics(t).Jev.ExcludedClasses; got != 0 {
		t.Fatalf("excluded-class decisions = %d, want 0", got)
	}
}

func insertJevAsk(t *testing.T, created time.Time, chosen *int) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, ask_json, escalation_class, origin, created_at)
		VALUES ($1, gen_random_uuid(), gen_random_uuid(), $2, gen_random_uuid(), 'Pick', '{"options":[{"label":"Proceed"},{"label":"Hold"}]}', 'reversible', 'asked', $3)
		RETURNING id::text`, testWorkspaceID, testUserID, created).Scan(&id)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM decision_answer WHERE decision_request_id=$1`, id)
		dbfx.Exec(t, `DELETE FROM decision_request WHERE id=$1`, id)
	})
	if chosen != nil {
		answerOnCardRow(t, id, chosen)
	}
	return id
}

// The CLI sends the ids from the shadow log; the server returns when each was asked and what Cheese chose.
func TestAgentDecisionBriefReturnsJevAsksWithCheeseAnswer(t *testing.T) {
	enableAgentDecisions(t, true)
	hold := 1
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	answered := insertJevAsk(t, created, &hold)
	open := insertJevAsk(t, created, nil)
	m := readMetrics(t, answered, open)
	if len(m.JevAsks) != 2 {
		t.Fatalf("jev_asks = %+v, want 2", m.JevAsks)
	}
	for _, a := range m.JevAsks {
		got, err := time.Parse(time.RFC3339, a.CreatedAt)
		if err != nil || !got.Equal(created) {
			t.Fatalf("ask %s created_at %q (%v), want %s", a.ID, a.CreatedAt, err, created)
		}
		switch a.ID {
		case answered:
			if a.Answer != "Hold" {
				t.Fatalf("answered ask: answer %q, want Hold", a.Answer)
			}
		case open:
			if a.Answer != "" {
				t.Fatalf("open ask: answer %q, want empty", a.Answer)
			}
		default:
			t.Fatalf("unexpected ask %s", a.ID)
		}
	}
}

func TestAgentDecisionBriefRejectsBadJevAskID(t *testing.T) {
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief?jev_ask=not-a-uuid", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}
