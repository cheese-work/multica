// Package designguard decides whether a design review may count for a
// decision card (CHE-7064 r3, D1.5).
package designguard

import "context"

// Refusal codes of the independence rules A1 to A6.
const (
	CodeAuthorUnknown      = "design_author_unknown"
	CodeAuthorLabUnknown   = "design_author_lab_unknown"
	CodeSelfReview         = "design_verdict_self_review"
	CodeReviewerLabUnknown = "design_reviewer_lab_unknown"
	CodeSameLab            = "design_verdict_same_lab"
	CodeSameAgentAsAsker   = "design_verdict_same_agent"
)

// Author is one row of doop's page author ledger, as the attestation view returns it.
type Author struct {
	Kind    string // agent, user, system or unknown
	AgentID string
	TaskID  string
}

// View is the part of a doop attestation view that independence reads.
type View struct {
	Authors            []Author
	ProvenanceComplete bool
}

// Reviewer is the agent that wrote the verdict and the task it wrote it from.
type Reviewer struct{ AgentID, TaskID string }

// Task is a Multica task row: the only source of an agent's lab.
type Task struct{ ID, AgentID, Provider, Model string }

// Deps are the lookups CheckIndependence needs. Tasks returns the rows it
// knows among ids; Lab maps an exact provider and model to a lab, or "".
type Deps struct {
	Tasks func(ctx context.Context, ids []string) ([]Task, error)
	Lab   func(provider, model string) string
}

// CheckIndependence returns "" when the reviewer is independent of every
// author, else the refusal code. asker is the agent that sends the card;
// pass "" at verdict-write time, when A6 does not apply.
func CheckIndependence(ctx context.Context, view View, reviewer Reviewer, asker string, deps Deps) (string, error) {
	// Every task id the rules read, in one lookup. Kind other than agent or
	// user (or an empty set) fails closed even when the view says complete.
	var ids []string
	var authors []Author
	ok := view.ProvenanceComplete && len(view.Authors) > 0
	for _, a := range view.Authors {
		switch a.Kind {
		case "user": // a person, not an agent: outside A3 to A5
		case "agent":
			ok = ok && a.AgentID != "" && a.TaskID != ""
			authors = append(authors, a)
			ids = append(ids, a.TaskID)
		default:
			ok = false
		}
	}
	if !ok {
		return CodeAuthorUnknown, nil
	}
	if reviewer.TaskID != "" {
		ids = append(ids, reviewer.TaskID)
	}
	rows, err := deps.Tasks(ctx, ids)
	if err != nil {
		return "", err
	}
	byID := make(map[string]Task, len(rows))
	for _, t := range rows {
		byID[t.ID] = t
	}

	// A1: each author task is a Multica task of that agent.
	for _, a := range authors {
		if t, found := byID[a.TaskID]; !found || t.AgentID != a.AgentID {
			return CodeAuthorUnknown, nil
		}
	}
	// A2: each author lab is known. Labs come from Multica's task rows only.
	authorLabs := map[string]bool{}
	for _, a := range authors {
		t := byID[a.TaskID]
		lab := deps.Lab(t.Provider, t.Model)
		if lab == "" {
			return CodeAuthorLabUnknown, nil
		}
		authorLabs[lab] = true
	}
	// A3: the reviewer is not an author.
	for _, a := range authors {
		if a.AgentID == reviewer.AgentID {
			return CodeSelfReview, nil
		}
	}
	// A4: the reviewer's lab is known, from the reviewer's own task.
	rt, found := byID[reviewer.TaskID]
	if reviewer.TaskID == "" || !found || rt.AgentID != reviewer.AgentID {
		return CodeReviewerLabUnknown, nil
	}
	lab := deps.Lab(rt.Provider, rt.Model)
	if lab == "" {
		return CodeReviewerLabUnknown, nil
	}
	// A5: the reviewer's lab differs from every author lab.
	if authorLabs[lab] {
		return CodeSameLab, nil
	}
	// A6: card time only.
	if asker != "" && asker == reviewer.AgentID {
		return CodeSameAgentAsAsker, nil
	}
	return "", nil
}
