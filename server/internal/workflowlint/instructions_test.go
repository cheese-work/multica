package workflowlint

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// catalog is the workspace catalog the instruction rules read.
var catalog = map[string]string{
	"backlog": "Backlog", "todo": "Todo", "in_progress": "In progress", "in_review": "In review",
	"blocked": "Blocked", "done": "Done", "cancelled": "Cancelled",
	"agent_accepted": "Agent accepted", "merged": "Code Accepted", "need_decision": "Need Decision",
}

// withText returns clean() with the instructions of step key replaced.
func withText(key, text string) (d struct{ lint func() []Finding }) {
	def := clean()
	for i := range def.Steps {
		if def.Steps[i].StatusKey == key {
			def.Steps[i].Instructions = text
		}
	}
	d.lint = func() []Finding { return LintWith(def, Env{Statuses: catalog}) }
	return d
}

func TestWF009UndefinedStatusInInstructions(t *testing.T) {
	cases := map[string]string{
		"display name":     "Ready: move this issue to Agent accepted.",
		"key":              "Ready: move the issue to agent_accepted, stop.",
		"back to":          "Move it back to Agent accepted.",
		"cli form":         "Run: multica issue status <this issue> agent_accepted",
		"custom name":      "Then move this issue to Code Accepted.",
		"prohibition also": "Never move this issue to Agent accepted yourself.",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			fs := withText("in_progress", text).lint()
			if !has(fs, "WF009", "in_progress") {
				t.Fatalf("want WF009@in_progress, got %v", rules(fs))
			}
			for _, f := range fs {
				if f.Rule == "WF009" && f.Level != LevelWarning {
					t.Fatalf("WF009 must be a warning, got %s", f.Level)
				}
			}
		})
	}
}

func TestWF009QuietCases(t *testing.T) {
	cases := map[string]string{
		"defined status":      "Ready: move this issue to In review.",
		"not a status":        "Move to the next step when the plan is ready.",
		"word boundary":       "Move this issue to doneness checks.",
		"no catalog hit":      "Move this issue to Narnia.",
		"the step's own text": "Post one comment, then stop.",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if fs := withText("in_progress", text).lint(); countRule(fs, "WF009") != 0 {
				t.Fatalf("want no WF009, got %v", rules(fs))
			}
		})
	}
	t.Run("no catalog skips both rules", func(t *testing.T) {
		def := clean()
		def.Steps[2].Instructions = "move this issue to Agent accepted, then to Blocked"
		fs := Lint(def)
		if countRule(fs, "WF009")+countRule(fs, "WF010") != 0 {
			t.Fatalf("nil Statuses: want no WF009/WF010, got %v", rules(fs))
		}
	})
}

func TestWF010MoveWithoutEdge(t *testing.T) {
	// clean() in_review has edges to in_progress and done only.
	cases := map[string]string{
		"display name": "PASS: move this issue to Blocked.",
		"key":          "Then move the issue to blocked and say why.",
		"straight":     "Move straight to Cancelled when it is a duplicate.",
		"cli form":     "Run: multica issue status <this issue> blocked",
		// Two moves on one line: the second must not hide behind the first.
		"second move on the line":  "PASS: move this issue to Done; move to Blocked.",
		"second move after a stop": "Move this issue to Done. Otherwise move it to Cancelled.",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			fs := withText("in_review", text).lint()
			if !has(fs, "WF010", "in_review") {
				t.Fatalf("want WF010@in_review, got %v", rules(fs))
			}
			if has(fs, "WF009", "in_review") {
				t.Fatalf("status is defined, want no WF009: %v", rules(fs))
			}
		})
	}
}

func TestWF010QuietCases(t *testing.T) {
	cases := map[string]string{
		"edge exists":   "CHANGES REQUESTED: move this issue back to In progress; PASS: move it to Done.",
		"prohibition":   "Never move this issue to Blocked yourself.",
		"do not":        "Do not move the issue to Cancelled.",
		"own status":    "Stay: move this issue to In review only once.",
		"undefined one": "Move this issue to Agent accepted.", // WF009, not WF010
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if fs := withText("in_review", text).lint(); countRule(fs, "WF010") != 0 {
				t.Fatalf("want no WF010, got %v", rules(fs))
			}
		})
	}
}

func TestInstructionRulesHonourWaiver(t *testing.T) {
	def := clean()
	def.Steps[3].Instructions = "Move this issue to Blocked."
	def.Steps[3].LintWaive = []issueworkflow.LintWaiver{{Rule: "WF010", Reason: "human moves it by hand"}}
	var got *Finding
	for _, f := range LintWith(def, Env{Statuses: catalog}) {
		if f.Rule == "WF010" {
			got = &f
		}
	}
	if got == nil || !got.Waived || got.WaiveReason == "" {
		t.Fatalf("want a waived WF010 with its reason, got %+v", got)
	}
}
