package mergequeue

import (
	"strings"
	"time"
)

// ActorClass is who enqueued a PR (plan 4.5).
type ActorClass string

const (
	ActorAgent   ActorClass = "agent"
	ActorPerson  ActorClass = "person"
	ActorUnknown ActorClass = "unknown"
)

// EventKind names what happened to a PR or an epoch.
type EventKind string

const (
	EventSuspectedSplit EventKind = "suspected_split"
	EventPRClosed       EventKind = "pr_closed"
	EventQueueEntry     EventKind = "queue_entry"
	EventEpochResolved  EventKind = "epoch_resolved"
	EventEpochOpened    EventKind = "epoch_opened"
	EventRoutingChanged EventKind = "routing_changed"
)

// AssociationEvent is one fact the association rule may react to.
type AssociationEvent struct {
	Kind  EventKind
	Actor ActorClass
}

// EndsAssociation reports whether the event ends an active (PR, key)
// association (plan 4.4). Only a split of its suspected epoch or a PR close
// does. A queue entry, by any actor, ends nothing: the actor is a fact to
// store, never a trigger.
func EndsAssociation(e AssociationEvent) bool {
	return e.Kind == EventSuspectedSplit || e.Kind == EventPRClosed
}

// KeyForSignature is the cause key of a test-failure signature (plan 4.1).
// The repo is case-insensitive.
func KeyForSignature(repo, signature string) string {
	return "sig:" + strings.ToLower(repo) + ":" + signature
}

// KeyForTimeout is the cause key of a timed-out check. Its prefix keeps it
// apart from a signature key with the same text.
func KeyForTimeout(repo, checkKey string) string {
	return "timeout:" + strings.ToLower(repo) + ":" + checkKey
}

// TestID names a Go test with its package.
type TestID struct{ Package, Name string }

// JobRun is one job of a main-evidence run.
type JobRun struct {
	Check    string
	Skipped  bool
	Passed   map[TestID]bool // tests that show --- PASS in the job log
	Wait     time.Duration   // runner wait
	Duration time.Duration
}

// TimeoutBudget is the 30 min check_response_timeout_minutes minus a 5 min margin.
const TimeoutBudget = 25 * time.Minute

// neverComparable is the check whose result says nothing about a test or a
// timeout.
const neverComparable = "migration-checks"

// Comparable reports whether a main-evidence job can prove a cause (plan 4.4).
// A skipped job and migration-checks never can. A test cause needs every
// failing test, by package and name, to show PASS in the job log. A timeout
// cause needs runner wait plus duration to fit the budget.
func Comparable(job JobRun, failing []TestID, timeout bool) bool {
	if job.Skipped || job.Check == neverComparable {
		return false
	}
	if timeout {
		return job.Wait+job.Duration <= TimeoutBudget
	}
	if len(failing) == 0 {
		return false
	}
	for _, id := range failing {
		if !job.Passed[id] {
			return false
		}
	}
	return true
}
