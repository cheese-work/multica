package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// S4b (CHE-8821): the brief shows "<agent> for <human in charge>" computed from
// the registration and the human-in-charge resolver, never a constant.

// settledJevRecord settles a live Jev record whose issue is created by a known
// member, and returns that member's name with the fixture.
func settledJevRecord(t *testing.T) (agentDecisionFixture, string) {
	t.Helper()
	enableAgentDecisions(t, true)
	f := jevFixture(t)
	setJev(t, f.writerID, "live")
	creator := hicMember(t, "member")
	dbfx.Exec(t, `UPDATE issue SET creator_type='member', creator_id=$2 WHERE id=$1`, f.issueID, creator)
	f.propose(t)
	settleJev(t, f)
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("live Jev record settled %d times, want 1", n)
	}
	var name string
	dbfx.QueryRow(t, `SELECT name FROM "user" WHERE id=$1`, creator).Scan(&name)
	return f, name
}

// briefDecider reads the brief and returns the raw record map for the instance.
func briefDecider(t *testing.T, instance string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("brief status=%d: %s", w.Code, w.Body)
	}
	var out struct {
		Decisions []map[string]any `json:"decisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Decisions {
		if d["instance_id"] == instance {
			return d
		}
	}
	t.Fatalf("record %s missing from brief", instance)
	return nil
}

func TestAgentDecisionBriefDeciderDisplayFollowsAgentName(t *testing.T) {
	f, human := settledJevRecord(t)
	dbfx.Exec(t, `UPDATE agent SET name=$2 WHERE id=$1`, f.writerID, "Delegate One")
	if got, want := briefDecider(t, f.instance)["decider_display"], "Delegate One for "+human; got != want {
		t.Fatalf("decider_display = %v, want %q", got, want)
	}
	dbfx.Exec(t, `UPDATE agent SET name=$2 WHERE id=$1`, f.writerID, "Delegate Renamed")
	if got, want := briefDecider(t, f.instance)["decider_display"], "Delegate Renamed for "+human; got != want {
		t.Fatalf("after rename decider_display = %v, want %q", got, want)
	}
	if got := briefDecider(t, f.instance)["decider"]; got != "Jev for Cheese" {
		t.Fatalf("raw decider = %v, must stay the stored value", got)
	}
}

func TestAgentDecisionBriefDeciderDisplayFallbacks(t *testing.T) {
	t.Run("missing_human_in_charge", func(t *testing.T) {
		f, _ := settledJevRecord(t)
		dbfx.Exec(t, `UPDATE agent SET name='Delegate Two' WHERE id=$1`, f.writerID)
		// Nobody may invoke the agent, so no human in charge resolves.
		other := hicMember(t, "member")
		dbfx.Exec(t, `UPDATE agent SET permission_mode='private', owner_id=$2 WHERE id=$1`, f.writerID, other)
		if got, want := briefDecider(t, f.instance)["decider_display"], "Delegate Two for the workspace owner"; got != want {
			t.Fatalf("decider_display = %v, want %q", got, want)
		}
	})
	t.Run("missing_agent", func(t *testing.T) {
		f, _ := settledJevRecord(t)
		// The registration names an agent that no longer exists.
		setJev(t, uuid.NewString(), "live")
		if got, want := briefDecider(t, f.instance)["decider_display"], "delegate agent for the workspace owner"; got != want {
			t.Fatalf("decider_display = %v, want %q", got, want)
		}
	})
	t.Run("no_registration", func(t *testing.T) {
		f, _ := settledJevRecord(t)
		dbfx.Exec(t, `UPDATE workspace SET settings=settings-'agent_decisions_jev' WHERE id=$1`, testWorkspaceID)
		if got, want := briefDecider(t, f.instance)["decider_display"], "delegate agent for the workspace owner"; got != want {
			t.Fatalf("decider_display = %v, want %q", got, want)
		}
	})
}

// A record with no decider has no display: existing gates print unchanged.
func TestAgentDecisionBriefNoDeciderNoDisplay(t *testing.T) {
	enableAgentDecisions(t, true)
	f := newAgentDecisionFixture(t)
	f.propose(t)
	settleJev(t, f)
	if _, ok := briefDecider(t, f.instance)["decider_display"]; ok {
		t.Fatal("a record without a decider must not carry decider_display")
	}
}
