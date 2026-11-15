package askguard

import (
	"slices"
	"strings"
)

// PurposeObserve marks a wakeup that never waits on a person.
const PurposeObserve = "observe"

// WakeupRule is the typed shape of a wakeup that the guard reads. It carries
// no instruction text: only typed fields decide.
type WakeupRule struct {
	Kind            string   `json:"kind"`
	EventTypes      []string `json:"event_types"`
	FilterActorType string   `json:"filter_actor_type"`
	FilterActorID   string   `json:"filter_actor_id"`
	Condition       bool     `json:"condition"`
}

// MemberEligible reports whether a member's action can fire the rule: an event
// rule for a comment, issue, reaction or attachment change whose actor filter
// does not exclude members. Timers, platform-evaluated conditions and task
// events never are.
func (r WakeupRule) MemberEligible() bool {
	if r.Kind != "event" || r.Condition || r.FilterActorType == "agent" {
		return false
	}
	return slices.ContainsFunc(r.EventTypes, func(e string) bool { return !strings.HasPrefix(e, "task.") })
}
