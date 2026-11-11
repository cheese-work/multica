package issueworkflow

import "testing"

// CHE-4976: the declaration survives storage; a step without it stays false.
func TestWaitsOnOpenDecisionRoundTrips(t *testing.T) {
	raw, err := EncodeSteps([]Step{
		{StatusKey: "need_decision", Handler: Handler{Type: HandlerAgent, ID: "a"}, Instructions: "x", WaitsOnOpenDecision: true},
		{StatusKey: "blocked", Handler: Handler{Type: HandlerAgent, ID: "a"}, Instructions: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps, err := DecodeSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !steps[0].WaitsOnOpenDecision || steps[1].WaitsOnOpenDecision {
		t.Fatalf("round trip lost the declaration: %+v", steps)
	}
}
