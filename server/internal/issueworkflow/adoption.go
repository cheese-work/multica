package issueworkflow

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// AdoptionLink is one issue listed in metadata adoption_issues.
type AdoptionLink struct {
	ID       string
	Terminal bool
	// Missing: the issue does not exist or could not be read.
	Missing bool
}

// AdoptionFacts are what the adoption_linked gate reads (CHE-3487).
type AdoptionFacts struct {
	// Err is a failed lookup or unparsable metadata; the gate fails closed.
	Err string
	// SetupChange is "none", "yes" or "" (unset).
	SetupChange  string
	Links        []AdoptionLink
	Components   []string // metadata components
	Registry     []string // known registry component ids
	AdoptionNone string   // metadata adoption_none
	// CrossCheckHit names the registry component that the title or description
	// names together with a change word; set only for SetupChange none.
	CrossCheckHit string
}

// MinAdoptionNone is the shortest adoption_none reason that counts as naming
// the agent-facing changes checked.
const MinAdoptionNone = 40

// Refusal is empty when the gate holds, else the fact that refuses closure.
// Rule order (plan CHE-2948 section 4): lookup facts, then the link check for
// both values, then components, then the value's own allowance.
func (a *AdoptionFacts) Refusal() string {
	if a == nil {
		return "adoption facts were not loaded"
	}
	if a.Err != "" {
		return a.Err
	}
	if a.SetupChange != "none" && a.SetupChange != "yes" {
		if a.SetupChange == "" {
			return "Setup change is not set"
		}
		return fmt.Sprintf("Setup change is %q, not none or yes", a.SetupChange)
	}
	for _, l := range a.Links {
		switch {
		case l.Missing:
			return fmt.Sprintf("adoption issue %s was not found or could not be read", l.ID)
		case !l.Terminal:
			return fmt.Sprintf("adoption issue %s is not terminal", l.ID)
		}
	}
	if a.SetupChange == "none" {
		if a.CrossCheckHit != "" {
			return fmt.Sprintf("Setup change is none but the title or description names %s with a change word", a.CrossCheckHit)
		}
		return ""
	}
	if !a.knownComponent() {
		return "metadata components lists no registry component"
	}
	if len(a.Links) == 0 && utf8.RuneCountInString(strings.TrimSpace(a.AdoptionNone)) < MinAdoptionNone {
		return fmt.Sprintf("no adoption issue is linked and metadata adoption_none is under %d characters", MinAdoptionNone)
	}
	return ""
}

func (a *AdoptionFacts) knownComponent() bool {
	for _, c := range a.Components {
		if slices.Contains(a.Registry, c) {
			return true
		}
	}
	return false
}

func adoptionFix() string {
	return "set Setup change, list registry component ids in components, link adoption issues in adoption_issues and close them, or record adoption_none of 40+ characters"
}
