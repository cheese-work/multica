// Package workflowlint checks a workflow definition for structural mistakes
// (CHE-3658): dangling targets, dead-end statuses, unreachable statuses.
// One entry point, Lint; the UI, the CLI and the nightly job all call it.
package workflowlint

import (
	"fmt"
	"slices"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Level is how serious a finding is.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
)

// Finding is one problem in a workflow. Step is empty for a workflow-wide
// finding. A waived finding is still returned, with the waiver's reason.
type Finding struct {
	Rule        string `json:"rule"`
	Level       Level  `json:"level"`
	Workflow    string `json:"workflow"`
	Step        string `json:"step,omitempty"`
	Message     string `json:"message"`
	Hint        string `json:"hint"`
	Waived      bool   `json:"waived,omitempty"`
	WaiveReason string `json:"waive_reason,omitempty"`
}

// Lint returns the structural findings for def. LintWith adds the agent,
// runtime and usage rules.
func Lint(def issueworkflow.Definition) []Finding { return LintWith(def, Env{}) }

type linter struct {
	def      issueworkflow.Definition
	reached  map[string]bool
	findings []Finding
}

// add records a finding, marking it waived when the step carries a waiver
// for the rule.
func (l *linter) add(rule string, level Level, s *issueworkflow.Step, message, hint string) {
	f := Finding{Rule: rule, Level: level, Workflow: l.def.Name, Message: message, Hint: hint}
	if s != nil {
		f.Step = s.StatusKey
		for _, w := range s.LintWaive {
			if w.Rule == rule {
				f.Waived, f.WaiveReason = true, w.Reason
				break
			}
		}
	}
	l.findings = append(l.findings, f)
}

func isTerminal(key string) bool { return key == issuestatus.Done || key == issuestatus.Cancelled }

func (l *linter) lintStep(s issueworkflow.Step) {
	key := s.StatusKey
	edges := l.def.EdgesOf(key)

	l.checkTargets(s)

	if !isTerminal(key) && key != issuestatus.Backlog {
		if !s.HandsOff() && !s.DynamicAssignment {
			l.add("WF002", LevelError, &s, fmt.Sprintf("%q has no handler.", key),
				"Give the step a handler, or set dynamic_assignment if the assignee is chosen at run time.")
		}
		if !s.HandsOff() && len(edges) == 0 {
			l.add("WF003", LevelError, &s, fmt.Sprintf("%q has no handler and no exit, so an issue that enters it is stuck.", key),
				"Give the step a handler or add an edge to another status.")
		}
	}

	seen := map[edgeKey]bool{}
	for _, e := range edges {
		k := edgeKey{e.To, e.Guard}
		if seen[k] {
			l.add("WF007", LevelWarning, &s, fmt.Sprintf("%q lists the edge to %q with guard %q more than once.", key, e.To, e.Guard),
				"Remove the repeated edge.")
		}
		seen[k] = true
	}

	switch key {
	case issuestatus.Backlog, issuestatus.Todo, issuestatus.Blocked:
	default:
		if !l.reached[key] {
			l.add("WF008", LevelWarning, &s, fmt.Sprintf("No edge reaches %q.", key),
				"Add an edge from another status to it, or remove the status.")
		}
	}

	if s.Handler.Type == issueworkflow.HandlerHumanByRole && !slices.ContainsFunc(edges, func(e issueworkflow.Edge) bool { return e.To == issuestatus.InProgress }) {
		l.add("WF011", LevelWarning, &s, fmt.Sprintf("%q is handled by a person but has no edge back to in_progress.", key),
			"Add an edge to in_progress so a rejected issue can return to the author.")
	}
}

type edgeKey struct{ to, guard string }

// checkTargets reports each next, back, edge and on_timeout target the
// workflow does not define (WF001).
func (l *linter) checkTargets(s issueworkflow.Step) {
	check := func(field, target string) {
		if target != "" && !l.def.Has(target) {
			l.add("WF001", LevelError, &s, fmt.Sprintf("%s of %q points to %q, which this workflow does not define.", field, s.StatusKey, target),
				"Point it at a status of this workflow, or add the status.")
		}
	}
	check("next_status_key", s.NextStatusKey)
	check("back_status_key", s.BackStatusKey)
	for _, e := range s.Edges {
		check("An edge", e.To)
		check("An edge on_timeout", e.OnTimeout)
	}
}

// reachedStatuses collects every status some edge leads to, on timeout too.
func reachedStatuses(def issueworkflow.Definition) map[string]bool {
	reached := map[string]bool{}
	for _, s := range def.Steps {
		for _, e := range def.EdgesOf(s.StatusKey) {
			reached[e.To] = true
			if e.OnTimeout != "" {
				reached[e.OnTimeout] = true
			}
		}
	}
	return reached
}

// lintRunBudget is WF014: the budget must cover at least one run per agent step.
func (l *linter) lintRunBudget() {
	agents := 0
	for _, s := range l.def.Steps {
		if s.Handler.Type == issueworkflow.HandlerAgent {
			agents++
		}
	}
	switch {
	case l.def.MaxRunsPerIssue == 0:
		l.add("WF014", LevelWarning, nil, "max_runs_per_issue is not set, so the platform default applies.",
			"Set max_runs_per_issue to at least the number of agent steps.")
	case l.def.MaxRunsPerIssue < agents:
		l.add("WF014", LevelWarning, nil, fmt.Sprintf("max_runs_per_issue is %d but the workflow has %d agent steps.", l.def.MaxRunsPerIssue, agents),
			"Raise max_runs_per_issue to at least the number of agent steps.")
	}
}
