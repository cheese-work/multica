// Package system1 is the shadow "System 1" predictor (CHE-3628): it guesses a
// decision from precedent and is never applied. A fine-tuned model replaces
// Predict behind the same signature (vendor-swap layer).
package system1

// Choice keys stored in system1_observation.predicted_key. Decisions use the
// first three; a routing observation uses "<handler_type>:<handler_id>".
const (
	KeyRecommended = "recommended"
	KeyOther       = "other"
	KeyFreeText    = "free_text"
)

// MinPrecedents is the fewest precedents a prediction needs; fewer abstains.
const MinPrecedents = 5

// Prediction is the guessed choice key. An empty Key is an abstention.
type Prediction struct {
	Key        string
	Confidence float64
	Precedents int
}

// Predict returns the most common precedent key with a Laplace-smoothed share
// as confidence. Too few precedents, or a tie for first, abstain.
func Predict(precedents []string) Prediction {
	out := Prediction{Precedents: len(precedents)}
	if len(precedents) < MinPrecedents {
		return out
	}
	counts := map[string]int{}
	for _, key := range precedents {
		counts[key]++
	}
	best, bestCount, tied := "", 0, false
	for key, count := range counts {
		switch {
		case count > bestCount:
			best, bestCount, tied = key, count, false
		case count == bestCount:
			tied = true
		}
	}
	if tied {
		return out
	}
	out.Key, out.Confidence = best, float64(bestCount+1)/float64(len(precedents)+2)
	return out
}

// Predictors of the test-first waiver scope (CHE-4383 B3). The version is part
// of the stored identity: editing a rule means a new version, never an edit.
const (
	ScopeTestFirstWaiver = "test_first_waiver"
	PredictorWaiverRule  = "waiver_rule"
	PredictorPrecedent   = "precedent"
	WaiverRuleVersion    = "1"
	PrecedentVersion     = "1"
	KeyWaive             = "waive"
	KeyRefuse            = "refuse"
)

// WaiverRule is waiver_rule@1: refuse without a cross-lab PASS on the head;
// waive with one unless the weaker evidence is review only; else abstain ("").
func WaiverRule(crossLabPassOnHead bool, weakerEvidenceKind string) string {
	switch {
	case !crossLabPassOnHead:
		return KeyRefuse
	case weakerEvidenceKind != "review_only":
		return KeyWaive
	}
	return ""
}
