package handler

import (
	"net/http"
	"strings"
	"testing"
)

const exclusionHeader = "Verdict: BLOCK\nDisputed-point: screenshot coverage\n"

// Prose never excludes a decision or turns the setting off: only typed fields do.
func TestAgentDecisionAdjudicationIgnoresProseWords(t *testing.T) {
	for name, prose := range map[string]string{
		"vietnamese":           "Cần ảnh chụp màn hình. Không liên quan đến deploy, security hay spend; thiếu missing evidence cũng không đúng.\n",
		"chinese":              "需要截图。这与 deploy、security、spend 无关，也不是 missing evidence。\n",
		"english":              "Screenshots are required. Not about deploy, security or spend.\n",
		"repeated untyped key": "Note: cần ảnh chụp màn hình\nNote: 需要截图\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdjudicationFixtureWithReview(t, exclusionHeader+"Gate: screenshot_evidence\nBlock-category: reversible_gate\n"+prose)
			enableAgentDecisions(t, true)
			f.propose(t)
			if !readBrief(t).Enabled {
				t.Fatal("prose turned the setting off")
			}
		})
	}
}

// A protected Gate or a missing, unknown or protected Block-category excludes the
// decision and turns the setting off, whatever language the prose is in.
func TestAgentDecisionAdjudicationExcludesByTypedFields(t *testing.T) {
	for name, tc := range map[string]struct{ fields, prose, reason string }{
		"protected gate":           {"Gate: security_review\nBlock-category: reversible_gate\n", "Cần ảnh chụp màn hình.\n", "security_review"},
		"protected category":       {"Gate: screenshot_evidence\nBlock-category: deploy\n", "需要截图。\n", "deploy"},
		"unknown category":         {"Gate: screenshot_evidence\nBlock-category: tuỳ_ý\n", "Cần ảnh chụp màn hình.\n", "tuỳ_ý"},
		"missing category":         {"Gate: screenshot_evidence\n", "需要截图。\n", `Block-category ""`},
		"duplicate category":       {"Gate: screenshot_evidence\nBlock-category: reversible_gate\nBlock-category: reversible_gate\n", "Cần ảnh.\n", "repeated"},
		"header-shaped prose line": {"Gate: screenshot_evidence\n", "Block-category: reversible_gate là sai.\n", "reversible_gate là sai"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdjudicationFixtureWithReview(t, exclusionHeader+tc.fields+tc.prose)
			enableAgentDecisions(t, true)
			f.post(t, f.writerID, f.body(t), http.StatusBadRequest)
			brief := readBrief(t)
			if brief.Enabled || brief.Stopped == nil || !strings.Contains(brief.Stopped.Reason, tc.reason) {
				t.Fatalf("not excluded by typed field: %+v", brief)
			}
		})
	}
}
