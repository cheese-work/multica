package issueworkflow

import (
	"fmt"
	"strings"
)

// Entry gates (CHE-2295): conditions a status write must meet to put an issue
// into a step. They reuse the guard catalog, evaluated at write time against
// the stored facts. Exits (edges) keep their own, looser semantics.
const (
	EntryModeOff   = "off"
	EntryModeWarn  = "warn"
	EntryModeBlock = "block"

	MaxEntryGatesPerStep = MaxEdgesPerStep
)

// EntryGate is one entry condition of a step. MemberOverride says whether a
// person may pass a failed block gate with a reason; nil means true.
type EntryGate struct {
	Guard          string `json:"guard"`
	Mode           string `json:"mode"`
	MemberOverride *bool  `json:"member_override,omitempty"`
}

// Overridable reports whether a person may override the gate.
func (g EntryGate) Overridable() bool { return g.MemberOverride == nil || *g.MemberOverride }

// EntryFailure is a gate that does not hold, with the fact behind it and the
// action that fixes it.
type EntryFailure struct {
	Guard          string `json:"guard"`
	Mode           string `json:"mode"`
	Fact           string `json:"fact"`
	Fix            string `json:"fix"`
	MemberOverride bool   `json:"member_override"`
}

// normalizeEntry trims gate fields and fills the stored default. It keeps nil
// (omitted: a save that does not know the field keeps the stored gates) apart
// from empty (cleared).
func normalizeEntry(gates []EntryGate) []EntryGate {
	if gates == nil {
		return nil
	}
	out := make([]EntryGate, len(gates))
	for i, g := range gates {
		override := g.Overridable()
		out[i] = EntryGate{Guard: strings.TrimSpace(g.Guard), Mode: strings.TrimSpace(g.Mode), MemberOverride: &override}
	}
	return out
}

// validateEntry checks a step's entry gates. The agent guards describe an
// agent's own write and the webhook guard waits for a call since the issue
// entered the step; none is a state a write can be checked against.
func validateEntry(s Step) error {
	if len(s.Entry) > MaxEntryGatesPerStep {
		return invalid("a step can have at most %d entry gates", MaxEntryGatesPerStep)
	}
	for _, e := range s.Entry {
		g, err := ParseGuard(e.Guard)
		if err != nil {
			return invalid("%v", err)
		}
		if !g.readsFacts() || g.Kind == GuardWebhook {
			return invalid("entry gate %q cannot be checked when a status is written", e.Guard)
		}
		switch e.Mode {
		case EntryModeOff, EntryModeWarn, EntryModeBlock:
		default:
			return invalid("entry gate %q needs mode off, warn or block, not %q", e.Guard, e.Mode)
		}
	}
	return nil
}

// entryFamily is how a guard is checked at entry.
type entryFamily int

const (
	familyOpenPR   entryFamily = iota // per PR: every open or draft PR must pass every such gate
	familyTerminal                    // merged, closed: Facts.holds as the exit catalog defines
	familyOther                       // children and issue status: Facts.holds
)

func familyOf(kind string) entryFamily {
	switch kind {
	case GuardPRReady, GuardPRChecksGreen, GuardPRChecksRed, GuardPRReviewApproved, GuardPRReviewChanges:
		return familyOpenPR
	case GuardPRMerged, GuardPRClosedUnmerged:
		return familyTerminal
	}
	return familyOther
}

// prHolds is the per-PR test of an open-PR gate.
func prHolds(kind string, p PRFact) bool {
	switch kind {
	case GuardPRReady:
		return p.State == "open"
	case GuardPRChecksGreen:
		return p.HeadFresh && p.ChecksRollup == "SUCCESS"
	case GuardPRChecksRed:
		return p.HeadFresh && (p.ChecksRollup == "FAILURE" || p.ChecksRollup == "ERROR")
	case GuardPRReviewApproved:
		return p.Review == "approved"
	case GuardPRReviewChanges:
		return p.Review == "changes_requested"
	}
	return false
}

// EvaluateEntry returns the gates that do not hold for the facts, in list
// order. Gates in mode off are skipped. The open-PR family is checked per PR:
// there must be at least one open or draft PR and each must pass every
// open-PR gate, so a red PR cannot hide behind a green draft one.
func EvaluateEntry(gates []EntryGate, f Facts) []EntryFailure {
	var open []PRFact
	for _, p := range f.PRs {
		if p.open() {
			open = append(open, p)
		}
	}
	var failed []EntryFailure
	for _, e := range gates {
		if e.Mode == EntryModeOff {
			continue
		}
		g, err := ParseGuard(e.Guard)
		if err != nil {
			continue
		}
		var fact string
		switch familyOf(g.Kind) {
		case familyOpenPR:
			fact = openPRFact(g.Kind, open)
		default:
			if !f.holds(g) {
				fact = factFor(g, f)
			}
		}
		if fact != "" {
			failed = append(failed, EntryFailure{Guard: e.Guard, Mode: e.Mode, Fact: fact, Fix: fixFor(g), MemberOverride: e.Overridable()})
		}
	}
	return failed
}

// openPRFact is empty when every open or draft PR passes the gate.
func openPRFact(kind string, open []PRFact) string {
	if len(open) == 0 {
		return "no open or draft PR is linked"
	}
	var facts []string
	for _, p := range open {
		if !prHolds(kind, p) {
			facts = append(facts, describePR(p))
		}
	}
	return strings.Join(facts, "; ")
}

func describePR(p PRFact) string {
	head := ""
	if p.HeadSHA != "" {
		head = " on head " + p.HeadSHA[:min(7, len(p.HeadSHA))]
	}
	switch {
	case p.State == "draft":
		return "a PR is draft" + head
	case !p.HeadFresh:
		return "checks of a PR are not reported for its newest head"
	case p.ChecksRollup != "":
		return fmt.Sprintf("checks %s%s", p.ChecksRollup, head)
	}
	return "a PR does not meet the gate" + head
}

func factFor(g Guard, f Facts) string {
	switch g.Kind {
	case GuardPRMerged:
		if f.anyPR(PRFact.open) {
			return "an open or draft PR is still linked"
		}
		return "no merged PR is linked"
	case GuardPRClosedUnmerged:
		if f.anyPR(func(p PRFact) bool { return p.open() || p.State == "merged" }) {
			return "an open, draft or merged PR is linked"
		}
		return "no closed PR is linked"
	case GuardChildrenDone:
		return "a child issue is not done"
	case GuardIssueStatus:
		if got := f.IssueStatuses[g.Args[0]]; got != "" {
			return fmt.Sprintf("%s is %s", g.Args[0], got)
		}
		return fmt.Sprintf("%s was not found", g.Args[0])
	}
	return "the condition does not hold"
}

func fixFor(g Guard) string {
	switch g.Kind {
	case GuardPRReady:
		return "mark the PR ready for review, or open one"
	case GuardPRChecksGreen:
		return "get the checks of every open PR green on its newest head"
	case GuardPRChecksRed:
		return "this gate needs failing checks on every open PR"
	case GuardPRReviewApproved:
		return "get an approving review on every open PR"
	case GuardPRReviewChanges:
		return "this gate needs a changes-requested review on every open PR"
	case GuardPRMerged:
		return "merge the PR and close or merge any other open PR"
	case GuardPRClosedUnmerged:
		return "close the PR without merging and leave no other open or merged PR"
	case GuardChildrenDone:
		return "finish or close every child issue"
	case GuardIssueStatus:
		return fmt.Sprintf("move %s to %s", g.Args[0], g.Args[1])
	}
	return "make the condition hold"
}
