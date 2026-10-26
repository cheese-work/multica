package issueworkflow

import (
	"reflect"
	"strings"
	"testing"
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
