// Package mergequeue holds the dequeue-cause rules of CHE-7148: the epoch
// state machine, the key, association and comparability rules, and the
// settings validator. Pure rules live here; storage is migration 999q.
package mergequeue

import (
	"errors"
	"fmt"
)

// State is a cause epoch's state (plan 4.1, S1).
type State string

const (
	Open      State = "open"
	Split     State = "split"
	Owned     State = "owned"
	Unowned   State = "unowned"
	Verifying State = "verifying"
	Resolved  State = "resolved"
	Stalled   State = "stalled"
)

// ErrIllegalTransition is returned for an edge the table does not list.
var ErrIllegalTransition = errors.New("mergequeue: illegal epoch transition")

// edges is the epoch transition table, one entry per arrow of the plan's state
// diagram. It is the only writer of epoch state: resolved and split are
// terminal, so they have no entry. Member-override edges arrive with B5.
var edges = map[State][]State{
	Open:      {Split, Owned, Stalled},
	Owned:     {Verifying, Unowned},
	Unowned:   {Owned},
	Verifying: {Resolved},
	Stalled:   {Owned},
}

// CanTransition reports whether the table lists the edge.
func CanTransition(from, to State) bool {
	for _, next := range edges[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Check returns ErrIllegalTransition (wrapped with the edge) when the table
// does not list from -> to.
func Check(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}
