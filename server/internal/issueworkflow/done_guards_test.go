package issueworkflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// CHE-9259: an agent reaches done from a started step by a human's done
// decision or by every linked PR being merged.

func doneExitDefinition() Definition {
	return withHandoffs(deliveryExits(), []Step{
		{StatusKey: "in_progress", Handler: Handler{Type: HandlerAgent, ID: testAgentID}, Edges: []Edge{
			{To: "agent_review", Guard: "agent_done"},
			{To: "blocked", Guard: "agent_done"},
			{To: "done", Guard: "human_done"},
			{To: "done", Guard: "pr.all_merged"},
		}},
	})
}

func TestParseDoneGuards(t *testing.T) {
	for _, in := range []string{"human_done", "pr.all_merged"} {
		if g, err := ParseGuard(in); err != nil || g.Kind != in {
			t.Errorf("ParseGuard(%q) = %+v, %v", in, g, err)
		}
	}
	for _, bad := range []string{"human_done(1)", "pr.all_merged(x)"} {
		if _, err := ParseGuard(bad); err == nil {
			t.Errorf("ParseGuard(%q) accepted", bad)
		}
	}
}

func TestPRAllMergedGuard(t *testing.T) {
	merged, open, draft, closed := PRFact{State: "merged"}, PRFact{State: "open"}, PRFact{State: "draft"}, PRFact{State: "closed"}
	for name, tc := range map[string]struct {
		prs  []PRFact
		want bool
	}{
		"no PR":                     {nil, false},
		"one merged":                {[]PRFact{merged}, true},
		"two merged":                {[]PRFact{merged, merged}, true},
		"one open":                  {[]PRFact{open}, false},
		"one draft":                 {[]PRFact{draft}, false},
		"merged and open":           {[]PRFact{merged, open}, false},
		"one closed unmerged":       {[]PRFact{closed}, false},
		"merged and closed unmerge": {[]PRFact{merged, closed}, false},
	} {
		if got := (Facts{PRs: tc.prs}).holds(Guard{Kind: GuardPRAllMerged}); got != tc.want {
			t.Errorf("%s: pr.all_merged = %v, want %v", name, got, tc.want)
		}
	}
}

func TestHumanDoneGuardReadsTheFact(t *testing.T) {
	if (Facts{}).holds(Guard{Kind: GuardHumanDone}) {
		t.Error("human_done holds with no decision")
	}
	if !(Facts{HumanDone: true}).holds(Guard{Kind: GuardHumanDone}) {
		t.Error("human_done does not hold after a done decision")
	}
}

// The CHE-7689 replay: the person chose done, the agent was refused.
func TestAgentMayCloseAfterAHumanDoneDecision(t *testing.T) {
	def := doneExitDefinition()
	if err := CheckAgentMoveFacts(def, "in_progress", "done", Facts{}); err == nil {
		t.Fatal("in_progress → done allowed with no decision and no merged PR")
	}
	if err := CheckAgentMoveFacts(def, "in_progress", "done", Facts{HumanDone: true}); err != nil {
		t.Errorf("in_progress → done refused after a done decision: %v", err)
	}
}

func TestAgentMayCloseWhenEveryPRMerged(t *testing.T) {
	def := doneExitDefinition()
	for name, tc := range map[string]struct {
		prs     []PRFact
		allowed bool
	}{
		"no PR":         {nil, false},
		"one open":      {[]PRFact{{State: "open"}}, false},
		"closed":        {[]PRFact{{State: "closed"}}, false},
		"merged + open": {[]PRFact{{State: "merged"}, {State: "open"}}, false},
		"all merged":    {[]PRFact{{State: "merged"}, {State: "merged"}}, true},
	} {
		err := CheckAgentMoveFacts(def, "in_progress", "done", Facts{PRs: tc.prs})
		if (err == nil) != tc.allowed {
			t.Errorf("%s: err = %v, allowed = %v", name, err, tc.allowed)
		}
	}
}

func TestRefusalNamesTheFailedGuardAndThePathThatWorks(t *testing.T) {
	err := CheckAgentMoveFacts(doneExitDefinition(), "in_progress", "done", Facts{PRs: []PRFact{{State: "open", Ref: "o/r#1"}}})
	var refused *MoveNotAllowedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want MoveNotAllowedError", err)
	}
	for _, want := range []string{"human_done", "pr.all_merged", "agent_review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if len(refused.Allowed) == 0 || refused.Allowed[0] != "agent_review" {
		t.Errorf("Allowed = %v, want the unconditional exits", refused.Allowed)
	}
}

// Without facts the plain check never opens a conditional exit.
func TestPlainCheckAgentMoveKeepsConditionalExitsClosed(t *testing.T) {
	if err := CheckAgentMove(doneExitDefinition(), "in_progress", "done"); err == nil {
		t.Error("CheckAgentMove opened a conditional exit without facts")
	}
}

// Other paths still go through review: no done edge, no done move.
func TestStepWithoutDoneEdgeStillRefusesDone(t *testing.T) {
	def := doneExitDefinition()
	if err := CheckAgentMoveFacts(def, "agent_review", "done", Facts{HumanDone: true, PRs: []PRFact{{State: "merged"}}}); err == nil {
		t.Error("agent_review → done allowed")
	}
}

func TestDoneGuardsInEdgeValidationAndWatch(t *testing.T) {
	def := doneExitDefinition()
	step, _ := def.Step("in_progress")
	if err := validateEdges(step, def); err != nil {
		t.Fatalf("validateEdges: %v", err)
	}
	// human_done is level-triggered: the engine applies the decision. pr.all_merged
	// waits for the agent's own request and needs no timeout.
	watched := FactEdges(step.Edges)
	if len(watched) != 1 || watched[0].Guard != "human_done" {
		t.Errorf("FactEdges = %+v, want only the human_done edge", watched)
	}
	if d, move, _ := Decide(step.Edges, Facts{HumanDone: true}, testNow, testNow, true); !move || d.To != "done" {
		t.Errorf("Decide with a done decision = %+v, %v; want a move to done", d, move)
	}
	if _, move, _ := Decide(step.Edges, Facts{PRs: []PRFact{{State: "merged"}}}, testNow, testNow, true); move {
		t.Error("Decide moved on merged PRs: that exit is the agent's to take")
	}
}
