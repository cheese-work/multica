package issueworkflow

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func edgeDefinition(edges []Edge) Definition {
	def := devDefinition()
	for i, s := range def.Steps {
		if s.StatusKey == "code_review" {
			def.Steps[i].Edges = edges
		}
	}
	return def
}

func TestParseGuard(t *testing.T) {
	cases := []struct {
		in   string
		want Guard
	}{
		{"agent_done", Guard{Kind: "agent_done"}},
		{"agent_verdict(PASS)", Guard{Kind: "agent_verdict", Args: []string{"PASS"}}},
		{"agent_verdict(CHANGES)", Guard{Kind: "agent_verdict", Args: []string{"CHANGES"}}},
		{"pr.checks_green", Guard{Kind: "pr.checks_green"}},
		{"pr.closed_unmerged", Guard{Kind: "pr.closed_unmerged"}},
		{"pr.review_changes_requested", Guard{Kind: "pr.review_changes_requested"}},
		{"pr.review_approved", Guard{Kind: "pr.review_approved"}},
		{"children_done", Guard{Kind: "children_done"}},
		{" issue_status(CHE-1, done) ", Guard{Kind: "issue_status", Args: []string{"CHE-1", "done"}}},
	}
	for _, c := range cases {
		got, err := ParseGuard(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseGuard(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "pr.checks_blue", "agent_verdict(MAYBE)", "agent_verdict", "issue_status(CHE-1)", "pr.merged(1)", "agent_done)"} {
		if _, err := ParseGuard(bad); err == nil {
			t.Errorf("ParseGuard(%q) accepted", bad)
		}
	}
}

func TestEdgesOfDerivesLegacyFields(t *testing.T) {
	def := devDefinition()
	got := def.EdgesOf("code_review")
	want := []Edge{
		{To: "done", Guard: "agent_done"},
		{To: "implement", Guard: "agent_verdict(CHANGES)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy edges = %+v, want %+v", got, want)
	}
	// advance_on_ci_green is a pr.checks_green edge to the next status.
	for i, s := range def.Steps {
		if s.StatusKey == "implement" {
			def.Steps[i].AdvanceOnCIGreen = true
		}
	}
	got = def.EdgesOf("implement")
	if want := []Edge{{To: "code_review", Guard: "pr.checks_green"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("implement edges = %+v, want %+v", got, want)
	}
}

func TestExplicitEdgesReplaceLegacyFields(t *testing.T) {
	edges := []Edge{{To: "done", Guard: "pr.merged", Timeout: "72h", OnTimeout: "blocked"}}
	def := edgeDefinition(edges)
	if got := def.EdgesOf("code_review"); !reflect.DeepEqual(got, edges) {
		t.Fatalf("edges = %+v, want explicit %+v", got, edges)
	}
}

func TestValidateEdges(t *testing.T) {
	ok := Edge{To: "done", Guard: "pr.merged", Timeout: "72h", OnTimeout: "blocked"}
	cases := []struct {
		name  string
		edges []Edge
		def   func() Definition
		want  string // substring of the error; empty means valid
	}{
		{"valid", []Edge{ok}, nil, ""},
		{"agent edge needs no timeout", []Edge{{To: "implement", Guard: "agent_verdict(CHANGES)"}}, nil, ""},
		{"unknown target", []Edge{{To: "nowhere", Guard: "agent_done"}}, nil, "nowhere"},
		{"self loop", []Edge{{To: "code_review", Guard: "agent_done"}}, nil, "code_review"},
		{"bad guard", []Edge{{To: "done", Guard: "pr.checks_blue"}}, nil, "pr.checks_blue"},
		{"pr guard needs timeout", []Edge{{To: "done", Guard: "pr.merged", OnTimeout: "blocked"}}, nil, "timeout"},
		{"timeout must parse", []Edge{{To: "done", Guard: "pr.merged", Timeout: "soon", OnTimeout: "blocked"}}, nil, "timeout"},
		{"timeout has a ceiling", []Edge{{To: "done", Guard: "pr.merged", Timeout: "9999h", OnTimeout: "blocked"}}, nil, "timeout"},
		{"on_timeout must be a status", []Edge{{To: "done", Guard: "pr.merged", Timeout: "1h", OnTimeout: "nowhere"}}, nil, "nowhere"},
		{"on_timeout defaults to blocked", []Edge{{To: "done", Guard: "pr.merged", Timeout: "1h"}}, nil, ""},
		{"no blocked, no default", []Edge{{To: "done", Guard: "pr.merged", Timeout: "1h"}}, func() Definition {
			return Definition{Steps: stepsFor(ArrangeKeys([]string{"code_review"}))}
		}, "on_timeout"},
		{"review guard needs timeout", []Edge{{To: "implement", Guard: "pr.review_changes_requested", OnTimeout: "blocked"}}, nil, "timeout"},
		{"review guard valid", []Edge{{To: "implement", Guard: "pr.review_changes_requested", Timeout: "72h", OnTimeout: "blocked"}}, nil, ""},
		{"too many edges", make([]Edge, MaxEdgesPerStep+1), nil, "at most"},
	}
	for _, c := range cases {
		def := edgeDefinition(c.edges)
		if c.def != nil {
			def = c.def()
			for i, s := range def.Steps {
				if s.StatusKey == "code_review" {
					def.Steps[i].Edges = c.edges
				}
			}
		}
		step, _ := def.Step("code_review")
		err := ValidateHandoff(step, def)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestEdgesRoundTripAndNormalize(t *testing.T) {
	steps := stepsFor([]string{"todo", "code_review", "done", "cancelled"})
	steps[1].Edges = []Edge{{To: " done ", Guard: " pr.merged ", Timeout: " 72h ", OnTimeout: ""}}
	_, _, normalized := Normalize("Lean", "", steps)
	raw, err := EncodeSteps(normalized)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []Edge{{To: "done", Guard: "pr.merged", Timeout: "72h"}}
	for _, s := range got {
		if s.StatusKey == "code_review" && !reflect.DeepEqual(s.Edges, want) {
			t.Fatalf("edges after round trip = %+v, want %+v", s.Edges, want)
		}
		if s.StatusKey != "code_review" && s.Edges != nil {
			t.Fatalf("%s grew edges %+v", s.StatusKey, s.Edges)
		}
	}
}

func TestFirstMatchIsLevelTriggeredAndOrdered(t *testing.T) {
	merged := PRFact{State: "merged"}
	green := PRFact{State: "open", ChecksRollup: "SUCCESS", HeadFresh: true}
	edges := []Edge{
		{To: "blocked", Guard: "pr.checks_red"},
		{To: "code_review", Guard: "pr.checks_green"},
		{To: "done", Guard: "pr.merged"},
		{To: "implement", Guard: "agent_verdict(CHANGES)"},
	}
	cases := []struct {
		name  string
		facts Facts
		want  string
	}{
		{"no facts, nothing fires", Facts{}, ""},
		{"agent guards never fire from facts", Facts{PRs: nil}, ""},
		{"green fresh head", Facts{PRs: []PRFact{green}}, "code_review"},
		{"green on a stale head is ignored", Facts{PRs: []PRFact{{State: "open", ChecksRollup: "SUCCESS"}}}, ""},
		{"red wins by list order", Facts{PRs: []PRFact{{State: "open", ChecksRollup: "FAILURE", HeadFresh: true}, green}}, "blocked"},
		{"merged before entry fires now", Facts{PRs: []PRFact{merged}}, "done"},
		{"merged with another PR still open waits", Facts{PRs: []PRFact{merged, {State: "draft"}}}, ""},
	}
	for _, c := range cases {
		got, ok := FirstMatch(edges, c.facts)
		if c.want == "" && ok || c.want != "" && (!ok || got.To != c.want) {
			t.Errorf("%s: FirstMatch = %+v, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestFirstMatchPRReadyClosedChildrenAndIssueGuards(t *testing.T) {
	cases := []struct {
		guard string
		facts Facts
		want  bool
	}{
		{"pr.ready", Facts{PRs: []PRFact{{State: "open"}}}, true},
		{"pr.ready", Facts{PRs: []PRFact{{State: "draft"}}}, false},
		{"pr.closed_unmerged", Facts{PRs: []PRFact{{State: "closed"}}}, true},
		{"pr.closed_unmerged", Facts{PRs: []PRFact{{State: "closed"}, {State: "open"}}}, false},
		{"pr.merged", Facts{PRs: []PRFact{{State: "closed"}}}, false},
		{"pr.review_changes_requested", Facts{PRs: []PRFact{{State: "open", Review: "changes_requested"}}}, true},
		{"pr.review_changes_requested", Facts{PRs: []PRFact{{State: "open", Review: "approved"}}}, false},
		{"pr.review_changes_requested", Facts{PRs: []PRFact{{State: "open"}}}, false},
		{"pr.review_changes_requested", Facts{PRs: []PRFact{{State: "merged", Review: "changes_requested"}}}, false},
		{"pr.review_approved", Facts{PRs: []PRFact{{State: "draft", Review: "approved"}}}, true},
		{"pr.review_approved", Facts{PRs: []PRFact{{State: "open", Review: "approved"}, {State: "open", Review: "changes_requested"}}}, false},
		{"children_done", Facts{ChildrenDone: true}, true},
		{"children_done", Facts{}, false},
		{"issue_status(CHE-9, done)", Facts{IssueStatuses: map[string]string{"CHE-9": "done"}}, true},
		{"issue_status(CHE-9, done)", Facts{IssueStatuses: map[string]string{"CHE-9": "todo"}}, false},
	}
	for _, c := range cases {
		_, ok := FirstMatch([]Edge{{To: "done", Guard: c.guard}}, c.facts)
		if ok != c.want {
			t.Errorf("%s with %+v = %v, want %v", c.guard, c.facts, ok, c.want)
		}
	}
}

// A pr.checks_green edge is advance_on_ci_green under another name, so it
// keeps that rule: CI green never moves an issue out of a closed, parked or
// blocked status.
func TestChecksGreenEdgeKeepsTheCIAdvanceRule(t *testing.T) {
	def := devDefinition()
	for _, key := range []string{"backlog", "blocked", "done", "cancelled"} {
		step := Step{StatusKey: key, Handler: Handler{Type: HandlerNone}, Edges: []Edge{{To: "implement", Guard: GuardPRChecksGreen, Timeout: "72h", OnTimeout: "code_review"}}}
		if err := ValidateHandoff(step, def); err == nil || !strings.Contains(err.Error(), "CI green") {
			t.Errorf("%s: pr.checks_green edge error = %v, want a CI green refusal", key, err)
		}
	}
	step := Step{StatusKey: "code_review", Handler: Handler{Type: HandlerNone}, Edges: []Edge{{To: "done", Guard: GuardPRChecksGreen, Timeout: "72h"}}}
	if err := ValidateHandoff(step, def); err != nil {
		t.Fatalf("code_review pr.checks_green edge: %v", err)
	}
}

func TestFactEdgesKeepOnlyGuardsOnStoredFacts(t *testing.T) {
	edges := []Edge{
		{To: "implement", Guard: "agent_verdict(CHANGES)"},
		{To: "done", Guard: "pr.merged", Timeout: "1h"},
		{To: "done", Guard: "agent_done"},
		{To: "done", Guard: "children_done"},
		{To: "done", Guard: "issue_status(CHE-1, done)"},
		{To: "done", Guard: "bogus"},
	}
	got := FactEdges(edges)
	want := []Edge{edges[1], edges[3], edges[4]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FactEdges = %+v, want %+v", got, want)
	}
	if HasFactEdges(edges[:1]) || !HasFactEdges(edges[1:2]) {
		t.Fatal("HasFactEdges disagrees with FactEdges")
	}
}

func TestDecide(t *testing.T) {
	entered := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	merged := Facts{PRs: []PRFact{{State: "merged"}}}
	open := Facts{PRs: []PRFact{{State: "open"}}}
	closed := Facts{PRs: []PRFact{{State: "closed"}}}
	mergeEdge := Edge{To: "done", Guard: "pr.merged", Timeout: "72h", OnTimeout: "implement"}
	cases := []struct {
		name        string
		edges       []Edge
		facts       Facts
		elapsed     time.Duration
		noBlocked   bool
		want        Decision
		move        bool
		nextCheckIn time.Duration // 0 means the zero time
	}{
		{"nothing to watch", []Edge{{To: "done", Guard: "agent_done"}}, merged, 0, false, Decision{}, false, 0},
		{"merged before entry moves at once", []Edge{mergeEdge}, merged, 0, false, Decision{To: "done", Reason: "edge", Guard: "pr.merged"}, true, 0},
		{"list order breaks a tie between holding edges",
			[]Edge{{To: "implement", Guard: "children_done"}, {To: "done", Guard: "pr.merged", Timeout: "1h"}},
			Facts{ChildrenDone: true, PRs: merged.PRs}, 0, false, Decision{To: "implement", Reason: "edge", Guard: "children_done"}, true, 0},
		{"waiting polls within the interval", []Edge{mergeEdge}, open, time.Hour, false, Decision{}, false, EdgePollInterval},
		{"a near deadline shortens the poll", []Edge{mergeEdge}, open, 72*time.Hour - 10*time.Second, false, Decision{}, false, 10 * time.Second},
		{"timeout moves to on_timeout", []Edge{mergeEdge}, open, 72 * time.Hour, false, Decision{To: "implement", Reason: "timeout", Guard: "pr.merged"}, true, 0},
		{"timeout defaults to blocked", []Edge{{To: "done", Guard: "pr.merged", Timeout: "1h"}}, open, 2 * time.Hour, false, Decision{To: "blocked", Reason: "timeout", Guard: "pr.merged"}, true, 0},
		{"no blocked and no on_timeout stays put", []Edge{{To: "done", Guard: "pr.merged", Timeout: "1h"}}, open, 2 * time.Hour, true, Decision{}, false, EdgePollInterval},
		{"earliest deadline wins",
			[]Edge{{To: "done", Guard: "pr.merged", Timeout: "48h", OnTimeout: "implement"}, {To: "done", Guard: "pr.ready", Timeout: "24h", OnTimeout: "todo"}},
			Facts{}, 49 * time.Hour, false, Decision{To: "todo", Reason: "timeout", Guard: "pr.ready"}, true, 0},
		{"equal deadlines go by list order",
			[]Edge{{To: "done", Guard: "pr.merged", Timeout: "1h", OnTimeout: "implement"}, {To: "done", Guard: "pr.ready", Timeout: "1h", OnTimeout: "todo"}},
			Facts{}, 2 * time.Hour, false, Decision{To: "implement", Reason: "timeout", Guard: "pr.merged"}, true, 0},
		{"a holding edge beats an expired timeout",
			[]Edge{{To: "implement", Guard: "pr.merged", Timeout: "1h", OnTimeout: "todo"}}, merged, 2 * time.Hour, false, Decision{To: "implement", Reason: "edge", Guard: "pr.merged"}, true, 0},
		{"closed PR with no closed_unmerged edge blocks", []Edge{mergeEdge}, closed, time.Minute, false, Decision{To: "blocked", Reason: "closed_unmerged", Guard: "pr.closed_unmerged"}, true, 0},
		{"closed PR without blocked is left to the timeout", []Edge{mergeEdge}, closed, time.Minute, true, Decision{}, false, EdgePollInterval},
		{"an explicit closed_unmerged edge replaces the fallback",
			[]Edge{mergeEdge, {To: "todo", Guard: "pr.closed_unmerged", Timeout: "1h"}}, closed, time.Minute, false, Decision{To: "todo", Reason: "edge", Guard: "pr.closed_unmerged"}, true, 0},
		{"the fallback needs a pr edge", []Edge{{To: "done", Guard: "children_done"}}, closed, 0, false, Decision{}, false, EdgePollInterval},
	}
	now := entered
	for _, c := range cases {
		now = entered.Add(c.elapsed)
		got, move, next := Decide(c.edges, c.facts, entered, now, !c.noBlocked)
		var wantNext time.Time
		if c.nextCheckIn > 0 {
			wantNext = now.Add(c.nextCheckIn)
		}
		if got != c.want || move != c.move || !next.Equal(wantNext) {
			t.Errorf("%s: Decide = %+v, %v, %v; want %+v, %v, %v", c.name, got, move, next, c.want, c.move, wantNext)
		}
	}
}

func TestEffectiveMaxRunsDefaultsWhenUnset(t *testing.T) {
	if got := (Definition{}).EffectiveMaxRuns(); got != 15 {
		t.Fatalf("unset budget = %d, want 15", got)
	}
	if got := (Definition{MaxRunsPerIssue: 3}).EffectiveMaxRuns(); got != 3 {
		t.Fatalf("set budget = %d, want 3", got)
	}
}

const subWorkflowID = "0f0e0d0c-0b0a-4908-8706-050403020100"

func subWorkflowDefinition(sub *SubWorkflow, edges []Edge) Definition {
	def := devDefinition()
	for i, s := range def.Steps {
		if s.StatusKey == "implement" {
			def.Steps[i].SubWorkflow = sub
			def.Steps[i].Edges = edges
		}
	}
	return def
}

func TestSubWorkflowStepValidation(t *testing.T) {
	join := []Edge{{To: "code_review", Guard: "children_done"}}
	cases := []struct {
		name    string
		sub     *SubWorkflow
		edges   []Edge
		wantErr string
	}{
		{"valid", &SubWorkflow{WorkflowID: subWorkflowID, Stage: 1}, join, ""},
		{"no sub-workflow", nil, nil, ""},
		{"bad workflow id", &SubWorkflow{WorkflowID: "nope"}, join, "workflow id"},
		{"negative stage", &SubWorkflow{WorkflowID: subWorkflowID, Stage: -1}, join, "stage"},
		{"no children_done exit", &SubWorkflow{WorkflowID: subWorkflowID}, []Edge{{To: "code_review", Guard: "agent_done"}}, "children_done"},
		{"no edges at all", &SubWorkflow{WorkflowID: subWorkflowID}, nil, "children_done"},
	}
	for _, c := range cases {
		def := subWorkflowDefinition(c.sub, c.edges)
		step, _ := def.Step("implement")
		err := ValidateHandoff(NormalizeHandoff(step), def)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: error = %v, want it to mention %q", c.name, err, c.wantErr)
		}
	}
}

func TestSubWorkflowSurvivesStorage(t *testing.T) {
	in := []Step{{StatusKey: "in_progress", Handler: Handler{Type: HandlerNone}, SubWorkflow: &SubWorkflow{WorkflowID: "11111111-1111-4111-8111-111111111111", Stage: 3}}}
	raw, err := EncodeSteps(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].SubWorkflow == nil || *out[0].SubWorkflow != *in[0].SubWorkflow {
		t.Fatalf("stored and read back %+v, want %+v", out, in[0].SubWorkflow)
	}
}
