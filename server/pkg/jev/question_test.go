package jev

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeAnswer round-trips raw JSON through Answer's custom UnmarshalJSON,
// mirroring how a real Response is decoded off the wire.
func decodeAnswer(t *testing.T, raw string) Answer {
	t.Helper()
	var a Answer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("Unmarshal(%s): %v", raw, err)
	}
	return a
}

func TestAsStrictChoiceAcceptsWellFormedAnswer(t *testing.T) {
	a := decodeAnswer(t, `{
		"type": "choice",
		"choice": "agent_work",
		"confidence": 0.97,
		"probabilities": {"agent_work": 0.97, "human_decision": 0.03}
	}`)

	choice, confidence, err := a.AsStrictChoice([]string{"agent_work", "human_decision"})
	if err != nil {
		t.Fatalf("AsStrictChoice: %v", err)
	}
	if choice != "agent_work" {
		t.Errorf("choice = %q, want agent_work", choice)
	}
	if confidence != 0.97 {
		t.Errorf("confidence = %v, want 0.97", confidence)
	}
}

func TestAsStrictChoiceAcceptsPresentZeroConfidence(t *testing.T) {
	// A genuinely-present zero is different from an omitted field: the key
	// exists on the wire, so it must be trusted, not rejected.
	a := decodeAnswer(t, `{
		"type": "choice",
		"choice": "no",
		"confidence": 0,
		"probabilities": {"yes": 0.5, "no": 0.5}
	}`)

	choice, confidence, err := a.AsStrictChoice([]string{"yes", "no"})
	if err != nil {
		t.Fatalf("AsStrictChoice: %v", err)
	}
	if choice != "no" || confidence != 0 {
		t.Errorf("got (%q, %v), want (no, 0)", choice, confidence)
	}
}

func TestAsStrictChoiceRejectsWrongType(t *testing.T) {
	a := Answer{Type: TypeNoul, Noul: 0.5}
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted a noul answer")
	}
}

func TestAsStrictChoiceRequiresOfferedLabels(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":1}}`)
	if _, _, err := a.AsStrictChoice(nil); err == nil {
		t.Fatal("AsStrictChoice accepted an empty offered set")
	}
}

func TestAsStrictChoiceRejectsMissingConfidence(t *testing.T) {
	// The key is genuinely absent from the payload, distinct from a
	// present-but-zero confidence (covered above).
	a := decodeAnswer(t, `{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4}}`)
	_, _, err := a.AsStrictChoice([]string{"a", "b"})
	if err == nil {
		t.Fatal("AsStrictChoice accepted an omitted confidence")
	}
	if !strings.Contains(err.Error(), "confidence") {
		t.Errorf("error %q does not mention confidence", err)
	}
}

func TestAsStrictChoiceRejectsNullConfidence(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":null,"probabilities":{"a":0.6,"b":0.4}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted a null confidence")
	}
}

func TestAsStrictChoiceRejectsOutOfRangeConfidence(t *testing.T) {
	for _, confidence := range []string{"1.5", "-0.1"} {
		a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":`+confidence+`,"probabilities":{"a":1}}`)
		if _, _, err := a.AsStrictChoice([]string{"a"}); err == nil {
			t.Errorf("confidence %s: AsStrictChoice accepted it", confidence)
		}
	}
}

// A non-numeric confidence is malformed at the decode boundary, before
// AsStrictChoice ever runs — the same "malformed responses" failure mode
// the spec asks for, just caught one layer earlier.
func TestDecodeRejectsNonNumericConfidence(t *testing.T) {
	var a Answer
	err := json.Unmarshal([]byte(`{"type":"choice","choice":"a","confidence":"high","probabilities":{"a":1}}`), &a)
	if err == nil {
		t.Fatal("Unmarshal accepted a string confidence")
	}
}

func TestAsStrictChoiceRejectsMissingProbabilities(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9}`)
	_, _, err := a.AsStrictChoice([]string{"a", "b"})
	if err == nil {
		t.Fatal("AsStrictChoice accepted omitted probabilities")
	}
	if !strings.Contains(err.Error(), "probabilities") {
		t.Errorf("error %q does not mention probabilities", err)
	}
}

func TestAsStrictChoiceRejectsLabelMismatch(t *testing.T) {
	tests := []struct {
		name    string
		offered []string
		raw     string
	}{
		{
			name:    "missing offered label",
			offered: []string{"a", "b", "c"},
			raw:     `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.6,"b":0.4}}`,
		},
		{
			name:    "extra unoffered label",
			offered: []string{"a", "b"},
			raw:     `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.5,"b":0.3,"c":0.2}}`,
		},
		{
			name:    "swapped label",
			offered: []string{"a", "b"},
			raw:     `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.6,"z":0.4}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := decodeAnswer(t, tc.raw)
			if _, _, err := a.AsStrictChoice(tc.offered); err == nil {
				t.Fatal("AsStrictChoice accepted a mismatched label set")
			}
		})
	}
}

func TestAsStrictChoiceRejectsNonFiniteProbability(t *testing.T) {
	// json.Unmarshal itself rejects NaN/Inf literals, so the overflow path
	// is exercised through an out-of-[0,1] value instead — the closest a
	// wire payload can get without failing to parse at all.
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":1.5,"b":-0.5}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted an out-of-range probability")
	}
}

func TestAsStrictChoiceRejectsBadSum(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.6,"b":0.6}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted probabilities summing to 1.2")
	}
}

func TestAsStrictChoiceAcceptsSumWithinEpsilon(t *testing.T) {
	// Floating point round trips off real providers rarely sum to exactly
	// 1; a hair under the documented epsilon must still pass.
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.6000001,"b":0.3999998}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err != nil {
		t.Fatalf("AsStrictChoice rejected a sum within epsilon: %v", err)
	}
}

func TestAsStrictChoiceRejectsSumJustOverEpsilon(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.60001,"b":0.4}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted a sum just over epsilon")
	}
}

func TestAsStrictChoiceRejectsWinnerNotOffered(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"z","confidence":0.9,"probabilities":{"a":0.6,"b":0.4}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted a choice outside the offered set")
	}
}

func TestAsStrictChoiceRejectsWinnerNotMaximum(t *testing.T) {
	// Injection / prompt-manipulation shape: the model (or a forged
	// response) names a winner whose own distribution disagrees with it.
	a := decodeAnswer(t, `{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.1,"b":0.9}}`)
	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice accepted a winner that was not the maximum")
	}
}

func TestAsStrictChoiceAcceptsExactTie(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"b","confidence":0.5,"probabilities":{"a":0.5,"b":0.5}}`)
	choice, _, err := a.AsStrictChoice([]string{"a", "b"})
	if err != nil {
		t.Fatalf("AsStrictChoice rejected a genuine tie resolved by the model: %v", err)
	}
	if choice != "b" {
		t.Errorf("choice = %q, want b", choice)
	}
}

func TestAsStrictChoiceRejectsMalformedJSON(t *testing.T) {
	var a Answer
	if err := json.Unmarshal([]byte(`{"type":"choice","choice":"a","confidence":"not a number"`), &a); err == nil {
		t.Fatal("Unmarshal accepted truncated/malformed JSON")
	}
}

// AsChoice is unchanged: it must still read a legacy, unvalidated answer,
// including one that AsStrictChoice would reject.
func TestAsChoiceCompatibilityWithOmittedConfidence(t *testing.T) {
	a := decodeAnswer(t, `{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4}}`)

	choice, confidence, err := a.AsChoice()
	if err != nil {
		t.Fatalf("AsChoice: %v", err)
	}
	if choice != "a" {
		t.Errorf("choice = %q, want a", choice)
	}
	if confidence != 0 {
		t.Errorf("confidence = %v, want 0 (legacy zero-value read)", confidence)
	}

	if _, _, err := a.AsStrictChoice([]string{"a", "b"}); err == nil {
		t.Fatal("AsStrictChoice should reject what AsChoice silently accepted")
	}
}
