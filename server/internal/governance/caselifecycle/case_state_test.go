package caselifecycle

import "testing"

func TestTransitionGraph(t *testing.T) {
	tests := []struct {
		from CaseState
		to   CaseState
		want bool
	}{
		{CaseCaptured, CaseEvidenceReady, true},
		{CaseEvidenceReady, CaseJevEvaluating, true},
		{CaseJevEvaluating, CaseCorrectionPending, true},
		{CaseJevEvaluating, CaseAgentEscalation, true},
		{CaseJevEvaluating, CaseAbstained, true},
		{CaseAgentEscalation, CaseAgentAttempt, true},
		{CaseAgentEscalation, CaseHumanReview, true},
		{CaseAgentAttempt, CaseCorrectionPending, true},
		{CaseAgentAttempt, CaseNextAttempt, true},
		{CaseNextAttempt, CaseAgentAttempt, true},
		{CaseNextAttempt, CaseHumanReview, true},
		{CaseCorrectionPending, CaseExecuting, true},
		{CaseCorrectionPending, CaseRefreshing, true},
		{CaseExecuting, CaseResolved, true},
		{CaseRefreshing, CaseEvidenceReady, true},
		{CaseRefreshing, CaseHumanReview, true},
		{CaseHumanReview, CaseCorrectionPending, true},
		{CaseHumanReview, CaseDismissed, true},
		{CaseCaptured, CaseInvalidated, true},
		{CaseParked, CaseInvalidated, true},
		{CaseCaptured, CaseParked, true},
		{CaseAgentAttempt, CaseParked, true},
		{CaseCaptured, CaseFailed, true},
		{CaseCaptured, CaseExecuting, false},
		{CaseParked, CaseCaptured, false},
		{CaseParked, CaseEvidenceReady, false},
		{CaseResolved, CaseCaptured, false},
		{CaseDismissed, CaseCorrectionPending, false},
		{CaseAbstained, CaseAgentEscalation, false},
		{CaseInvalidated, CaseEvidenceReady, false},
		{CaseFailed, CaseState("retrying"), false},
		{CaseState("unknown"), CaseCaptured, false},
	}

	for _, test := range tests {
		t.Run(string(test.from)+"_to_"+string(test.to), func(t *testing.T) {
			if got := CanTransition(test.from, test.to); got != test.want {
				t.Fatalf("CanTransition(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestTerminalStates(t *testing.T) {
	for _, state := range []CaseState{CaseAbstained, CaseResolved, CaseDismissed, CaseInvalidated, CaseFailed} {
		if !state.IsTerminal() {
			t.Errorf("%q should be terminal", state)
		}
	}
	for _, state := range []CaseState{CaseCaptured, CaseParked, CaseRefreshing, CaseHumanReview} {
		if state.IsTerminal() {
			t.Errorf("%q should not be terminal", state)
		}
	}
}

func TestTransitionReasons(t *testing.T) {
	tests := []struct {
		from   CaseState
		to     CaseState
		reason CaseReason
		want   bool
	}{
		{CaseCaptured, CaseParked, ReasonDisabled, true},
		{CaseCaptured, CaseParked, ReasonConfigMissing, true},
		{CaseAgentAttempt, CaseParked, ReasonDisabled, true},
		{CaseCaptured, CaseParked, ReasonMaterialChanged, false},
		{CaseParked, CaseEvidenceReady, ReasonFreshEvidence, false},
		{CaseCaptured, CaseInvalidated, ReasonMaterialChanged, true},
		{CaseCorrectionPending, CaseRefreshing, ReasonAgeOnlyExpiry, true},
		{CaseCorrectionPending, CaseInvalidated, ReasonAgeOnlyExpiry, false},
		{CaseAgentAttempt, CaseCorrectionPending, ReasonValidProposal, true},
		{CaseAgentAttempt, CaseCorrectionPending, ReasonUnqualifiedAttempt, false},
		{CaseExecuting, CaseResolved, ReasonCoveredMutation, true},
	}

	for _, test := range tests {
		if got := CanTransitionReason(test.from, test.to, test.reason); got != test.want {
			t.Errorf("CanTransitionReason(%q, %q, %q) = %t, want %t", test.from, test.to, test.reason, got, test.want)
		}
	}
	if validCaseReason(CaseReason("model invented this")) {
		t.Fatal("untyped model reason was accepted")
	}
}
