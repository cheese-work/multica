package mergequeue

import "testing"

var allStates = []State{Open, Split, Owned, Unowned, Verifying, Resolved, Stalled}

// The plan's epoch state diagram (section 5), one entry per arrow.
var legalEdges = map[[2]State]bool{
	{Open, Split}:         true,
	{Open, Owned}:         true,
	{Open, Stalled}:       true,
	{Owned, Verifying}:    true,
	{Owned, Unowned}:      true,
	{Unowned, Owned}:      true,
	{Verifying, Resolved}: true,
	{Stalled, Owned}:      true,
}

func TestEveryEdgeLegalOrIllegal(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			want := legalEdges[[2]State{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("%s -> %s: CanTransition = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesLeadNowhere(t *testing.T) {
	for _, from := range []State{Resolved, Split} {
		for _, to := range allStates {
			if CanTransition(from, to) {
				t.Errorf("%s -> %s must be illegal", from, to)
			}
		}
	}
}

func TestUnknownStateIsIllegal(t *testing.T) {
	if CanTransition("bogus", Open) || CanTransition(Open, "bogus") {
		t.Error("an unknown state must never transition")
	}
}
