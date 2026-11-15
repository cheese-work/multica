package workflowlint

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

type (
	step = issueworkflow.Step
	edge = issueworkflow.Edge
)

var (
	agent = issueworkflow.Handler{Type: issueworkflow.HandlerAgent, ID: "a"}
	human = issueworkflow.Handler{Type: issueworkflow.HandlerHumanByRole}
	none  = issueworkflow.Handler{Type: issueworkflow.HandlerNone}
)

// clean is a workflow that must lint with no finding.
func clean() issueworkflow.Definition {
	return issueworkflow.Definition{
		Name:            "Clean",
		MaxRunsPerIssue: 10,
		Steps: []step{
			{StatusKey: "backlog", Handler: none},
			{StatusKey: "todo", Handler: agent, Edges: []edge{{To: "in_progress", Guard: "agent_done"}}},
			{StatusKey: "in_progress", Handler: agent, Edges: []edge{
				{To: "in_review", Guard: "agent_done"},
				{To: "blocked", Guard: "agent_verdict(CHANGES)"},
				{To: "cancelled", Guard: "agent_verdict(PASS)"},
			}},
			{StatusKey: "in_review", Handler: human, Edges: []edge{
				{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
				{To: "done", Guard: "agent_done"},
			}},
			{StatusKey: "blocked", Handler: agent, Edges: []edge{{To: "in_progress", Guard: "agent_done"}}},
			{StatusKey: "done", Handler: none},
			{StatusKey: "cancelled", Handler: none},
		},
	}
}

// with returns clean() with step s replaced, or appended when new.
func with(s step) issueworkflow.Definition {
	def := clean()
	for i := range def.Steps {
		if def.Steps[i].StatusKey == s.StatusKey {
			def.Steps[i] = s
			return def
		}
	}
	// Insert before done so the appended step keeps board order.
	n := len(def.Steps) - 2
	def.Steps = append(def.Steps[:n], append([]step{s}, def.Steps[n:]...)...)
	return def
}

// rules returns the sorted-by-appearance "RULE@step" of every finding.
func rules(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Rule + "@" + f.Step
	}
	return out
}

func has(fs []Finding, rule, stepKey string) bool {
	for _, f := range fs {
		if f.Rule == rule && f.Step == stepKey {
			return true
		}
	}
	return false
}

func countRule(fs []Finding, rule string) int {
	n := 0
	for _, f := range fs {
		if f.Rule == rule {
			n++
		}
	}
	return n
}

func TestCleanWorkflowHasNoFindings(t *testing.T) {
	if fs := Lint(clean()); len(fs) != 0 {
		t.Fatalf("clean workflow: want no findings, got %v", rules(fs))
	}
}

func TestWF001DanglingTargets(t *testing.T) {
	cases := map[string]step{
		"edge to":         {StatusKey: "in_progress", Handler: agent, Edges: []edge{{To: "ghost", Guard: "agent_done"}}},
		"edge on_timeout": {StatusKey: "in_progress", Handler: agent, Edges: []edge{{To: "done", Guard: "pr.merged", OnTimeout: "ghost"}}},
		"next":            {StatusKey: "in_progress", Handler: agent, NextStatusKey: "ghost"},
		"back":            {StatusKey: "in_progress", Handler: agent, NextStatusKey: "done", BackStatusKey: "ghost"},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			fs := Lint(with(s))
			if !has(fs, "WF001", "in_progress") {
				t.Fatalf("want WF001@in_progress, got %v", rules(fs))
			}
			for _, f := range fs {
				if f.Rule == "WF001" && f.Level != LevelError {
					t.Fatalf("WF001 must be an error, got %s", f.Level)
				}
			}
		})
	}
}

func TestWF002NoHandler(t *testing.T) {
	fs := Lint(with(step{StatusKey: "review", Handler: none, Edges: []edge{{To: "done", Guard: "agent_done"}}}))
	if !has(fs, "WF002", "review") {
		t.Fatalf("want WF002@review, got %v", rules(fs))
	}
	// An edge into the step keeps WF008 quiet; a step with an exit has no WF003.
	if has(fs, "WF003", "review") {
		t.Fatalf("step has an edge, want no WF003: %v", rules(fs))
	}
}

func TestWF003NoEffectiveExit(t *testing.T) {
	t.Run("no handler and no edges", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "review", Handler: none}))
		if !has(fs, "WF003", "review") {
			t.Fatalf("want WF003@review, got %v", rules(fs))
		}
	})
	t.Run("active handler is an exit", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "review", Handler: agent}))
		if has(fs, "WF003", "review") {
			t.Fatalf("active handler: want no WF003, got %v", rules(fs))
		}
	})
	t.Run("legacy next is an exit", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "review", Handler: none, NextStatusKey: "done"}))
		if has(fs, "WF003", "review") {
			t.Fatalf("legacy next: want no WF003, got %v", rules(fs))
		}
	})
	t.Run("done category is not terminal", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "agent_accepted", Handler: none}))
		if !has(fs, "WF002", "agent_accepted") || !has(fs, "WF003", "agent_accepted") {
			t.Fatalf("agent_accepted: want WF002 and WF003, got %v", rules(fs))
		}
	})
}

func TestWF007DuplicateEdges(t *testing.T) {
	dup := step{StatusKey: "in_review", Handler: human, Edges: []edge{
		{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
		{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
		{To: "done", Guard: "agent_done"},
	}}
	fs := Lint(with(dup))
	if got := countRule(fs, "WF007"); got != 1 {
		t.Fatalf("want one WF007 for the repeated pair, got %d: %v", got, rules(fs))
	}
	// Same target with a different guard is not a duplicate.
	dup.Edges[1].Guard = "pr.merged"
	if fs := Lint(with(dup)); countRule(fs, "WF007") != 0 {
		t.Fatalf("different guard: want no WF007, got %v", rules(fs))
	}
}

func TestWF008UnreachableStatus(t *testing.T) {
	fs := Lint(with(step{StatusKey: "island", Handler: agent}))
	if !has(fs, "WF008", "island") {
		t.Fatalf("want WF008@island, got %v", rules(fs))
	}
	for _, key := range []string{"backlog", "todo", "blocked"} {
		if has(fs, "WF008", key) {
			t.Fatalf("%s is exempt from WF008", key)
		}
	}
	// An on_timeout target counts as reached.
	def := with(step{StatusKey: "island", Handler: agent})
	def.Steps[2].Edges = append(def.Steps[2].Edges, edge{To: "done", Guard: "pr.merged", OnTimeout: "island"})
	if has(Lint(def), "WF008", "island") {
		t.Fatal("on_timeout target is reached, want no WF008")
	}
}

func TestWF011HumanStepNeedsWayBack(t *testing.T) {
	fs := Lint(with(step{StatusKey: "in_review", Handler: human, Edges: []edge{{To: "done", Guard: "agent_done"}}}))
	if !has(fs, "WF011", "in_review") {
		t.Fatalf("want WF011@in_review, got %v", rules(fs))
	}
}

func TestWF014RunBudget(t *testing.T) {
	def := clean()
	def.MaxRunsPerIssue = 0
	if fs := Lint(def); !has(fs, "WF014", "") {
		t.Fatalf("unset budget: want WF014, got %v", rules(fs))
	}
	def.MaxRunsPerIssue = 2 // clean() has 3 agent steps
	if fs := Lint(def); !has(fs, "WF014", "") {
		t.Fatalf("budget below agent steps: want WF014, got %v", rules(fs))
	}
	def.MaxRunsPerIssue = 3
	if fs := Lint(def); countRule(fs, "WF014") != 0 {
		t.Fatalf("budget equals agent steps: want no WF014, got %v", rules(fs))
	}
}

// Live shapes of 5 Nov 2026.
func TestLiveShapes(t *testing.T) {
	t.Run("agent_accepted without handler or edges", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "agent_accepted", Handler: none, Edges: []edge{}}))
		if !has(fs, "WF002", "agent_accepted") || !has(fs, "WF003", "agent_accepted") {
			t.Fatalf("want WF002 and WF003, got %v", rules(fs))
		}
	})
	t.Run("in_review repeats an edge", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "in_review", Handler: human, Edges: []edge{
			{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
			{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
			{To: "in_progress", Guard: "agent_verdict(CHANGES)"},
			{To: "done", Guard: "agent_done"},
		}}))
		if !has(fs, "WF007", "in_review") {
			t.Fatalf("want WF007@in_review, got %v", rules(fs))
		}
	})
	t.Run("backlog without handler or edges", func(t *testing.T) {
		for _, f := range Lint(clean()) {
			t.Fatalf("unexpected %s@%s", f.Rule, f.Step)
		}
		def := clean()
		def.Steps[0] = step{StatusKey: "backlog", Handler: none, Edges: []edge{}}
		if fs := Lint(def); len(fs) != 0 {
			t.Fatalf("backlog: want no finding, got %v", rules(fs))
		}
	})
	t.Run("Gate in_progress is dynamic", func(t *testing.T) {
		fs := Lint(with(step{StatusKey: "in_progress", Handler: none, DynamicAssignment: true, Edges: []edge{
			{To: "done", Guard: "agent_done"},
			{To: "blocked", Guard: "agent_verdict(CHANGES)"},
			{To: "cancelled", Guard: "agent_verdict(PASS)"},
		}}))
		for _, f := range fs {
			if f.Step == "in_progress" {
				t.Fatalf("dynamic step: want no finding, got %s", f.Rule)
			}
		}
	})
}

func TestWaiverKeepsFindingMarkedWaived(t *testing.T) {
	fs := Lint(with(step{
		StatusKey: "review", Handler: none, Edges: []edge{{To: "done", Guard: "agent_done"}},
		LintWaive: []issueworkflow.LintWaiver{{Rule: "WF002", Reason: "assigned by hand"}},
	}))
	for _, f := range fs {
		if f.Rule == "WF002" && f.Step == "review" {
			if !f.Waived || f.WaiveReason != "assigned by hand" {
				t.Fatalf("want waived with reason, got %+v", f)
			}
			return
		}
	}
	t.Fatalf("waived finding must still be returned, got %v", rules(fs))
}
