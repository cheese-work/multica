package issueworkflow

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/issuestatus"
)

// Edges are the exits of a workflow step (CHE-2030). Each edge names the
// status an issue moves to and the guard that must hold. Guards on PR facts
// are level-triggered: on step entry every edge is checked against the stored
// facts at once, so a PR that merged before entry fires now.
const (
	MaxEdgesPerStep = 8
	MaxEdgeTimeout  = 30 * 24 * time.Hour
	minEdgeTimeout  = time.Minute
)

const (
	GuardAgentDone        = "agent_done"
	GuardAgentVerdict     = "agent_verdict"
	GuardPRChecksGreen    = "pr.checks_green"
	GuardPRChecksRed      = "pr.checks_red"
	GuardPRReady          = "pr.ready"
	GuardPRMerged         = "pr.merged"
	GuardPRClosedUnmerged = "pr.closed_unmerged"
	GuardChildrenDone     = "children_done"
	GuardIssueStatus      = "issue_status"
	GuardWebhook          = "webhook"
)

// Edge is one exit of a step. Timeout and OnTimeout are required for guards
// on PR facts: a PR that never changes must not park an issue forever.
type Edge struct {
	To        string `json:"to"`
	Guard     string `json:"guard"`
	OnTimeout string `json:"on_timeout,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
}

// Guard is a parsed guard expression.
type Guard struct {
	Kind string
	Args []string
}

var guardArity = map[string]int{
	GuardAgentDone: 0, GuardAgentVerdict: 1, GuardPRChecksGreen: 0, GuardPRChecksRed: 0, GuardPRReady: 0,
	GuardPRMerged: 0, GuardPRClosedUnmerged: 0, GuardChildrenDone: 0, GuardIssueStatus: 2,
	GuardWebhook: 1,
}

// webhookNameRe is the endpoint name a webhook guard may reference.
var webhookNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ParseGuard parses "kind" or "kind(arg, ...)".
func ParseGuard(s string) (Guard, error) {
	s = strings.TrimSpace(s)
	kind, rest, hasArgs := strings.Cut(s, "(")
	arity, known := guardArity[kind]
	if !known {
		return Guard{}, fmt.Errorf("unknown guard %q", s)
	}
	var args []string
	if hasArgs {
		inner, closed := strings.CutSuffix(rest, ")")
		if !closed {
			return Guard{}, fmt.Errorf("guard %q is missing its closing parenthesis", s)
		}
		for _, a := range strings.Split(inner, ",") {
			args = append(args, strings.TrimSpace(a))
		}
	}
	if len(args) != arity || strings.Contains(strings.Join(args, ""), ")") {
		return Guard{}, fmt.Errorf("guard %q takes %d argument(s)", kind, arity)
	}
	if kind == GuardAgentVerdict && args[0] != "PASS" && args[0] != "CHANGES" {
		return Guard{}, fmt.Errorf("agent_verdict takes PASS or CHANGES, not %q", args[0])
	}
	if kind == GuardWebhook && !webhookNameRe.MatchString(args[0]) {
		return Guard{}, fmt.Errorf("webhook takes an endpoint name of 1-32 chars from a-z, 0-9, _ and -, not %q", args[0])
	}
	for _, a := range args {
		if a == "" {
			return Guard{}, fmt.Errorf("guard %q has an empty argument", s)
		}
	}
	return Guard{Kind: kind, Args: args}, nil
}

// needsTimeout reports whether a guard waits on an outside fact.
func (g Guard) needsTimeout() bool {
	return strings.HasPrefix(g.Kind, "pr.") || g.Kind == GuardWebhook
}

// readsFacts reports whether stored facts decide the guard; the agent guards
// are decided by the agent's own status write.
func (g Guard) readsFacts() bool {
	return g.Kind != GuardAgentDone && g.Kind != GuardAgentVerdict
}

// EdgesOf returns a step's exits: its own edges, or else the ones its legacy
// fields stand for. Stored workflows are not rewritten, so in-flight issues
// and old clients keep working.
func (d Definition) EdgesOf(key string) []Edge {
	step, ok := d.Step(key)
	if !ok {
		return nil
	}
	if len(step.Edges) > 0 {
		return step.Edges
	}
	var edges []Edge
	if next := d.NextStatus(key); next != "" && step.NextStatusKey != "" {
		edges = append(edges, Edge{To: next, Guard: GuardAgentDone})
	}
	if step.BackStatusKey != "" {
		edges = append(edges, Edge{To: step.BackStatusKey, Guard: GuardAgentVerdict + "(CHANGES)"})
	}
	if next := d.NextStatus(key); step.AdvanceOnCIGreen && next != "" {
		edges = append(edges, Edge{To: next, Guard: GuardPRChecksGreen})
	}
	return edges
}

// normalizeEdges trims edge fields.
func normalizeEdges(edges []Edge) []Edge {
	if len(edges) == 0 {
		return nil
	}
	out := make([]Edge, len(edges))
	for i, e := range edges {
		out[i] = Edge{
			To: strings.TrimSpace(e.To), Guard: strings.TrimSpace(e.Guard),
			OnTimeout: strings.TrimSpace(e.OnTimeout), Timeout: strings.TrimSpace(e.Timeout),
		}
	}
	return out
}

// validateEdges checks a step's explicit edges against its workflow.
func validateEdges(s Step, def Definition) error {
	if len(s.Edges) > MaxEdgesPerStep {
		return invalid("a step can have at most %d edges", MaxEdgesPerStep)
	}
	for _, e := range s.Edges {
		if e.To == s.StatusKey || !def.Has(e.To) {
			return invalid("edge target %q is not another status of this workflow", e.To)
		}
		g, err := ParseGuard(e.Guard)
		if err != nil {
			return invalid("%v", err)
		}
		if g.Kind == GuardPRChecksGreen && !CanAdvanceOnCIGreen(s.StatusKey) {
			return invalid("issues in %q cannot advance on CI green", s.StatusKey)
		}
		if !g.needsTimeout() {
			continue
		}
		d, err := time.ParseDuration(e.Timeout)
		if err != nil || d < minEdgeTimeout || d > MaxEdgeTimeout {
			return invalid("edge %q needs a timeout between %s and %s, e.g. \"72h\"", e.Guard, minEdgeTimeout, MaxEdgeTimeout)
		}
		switch {
		case e.OnTimeout == "" && !def.Has(issuestatus.Blocked):
			return invalid("edge %q needs on_timeout: this workflow has no blocked status", e.Guard)
		case e.OnTimeout != "" && (e.OnTimeout == s.StatusKey || !def.Has(e.OnTimeout)):
			return invalid("on_timeout %q is not another status of this workflow", e.OnTimeout)
		}
	}
	return nil
}

// PRFact is what a guard needs to know about one linked PR. State is open,
// draft, merged or closed. HeadFresh is true when the checks snapshot belongs
// to the PR's newest head.
type PRFact struct {
	State        string
	ChecksRollup string
	HeadFresh    bool
	// HeadSHA is the head the checks snapshot belongs to when HeadFresh.
	HeadSHA string
}

// Facts are the stored facts guards are checked against. PRs cover the issue
// and its children. IssueStatuses is keyed by the issue identifier a guard
// names.
type Facts struct {
	PRs           []PRFact
	ChildrenDone  bool
	IssueStatuses map[string]string
	// Webhooks holds the names of this step's webhook endpoints that received
	// a success call at or after the issue entered the step.
	Webhooks map[string]bool
}

// FirstMatch returns the first edge, in list order, whose guard holds for the
// facts. Agent guards are decided by the agent's own status write, never by
// facts, so they do not match here.
func FirstMatch(edges []Edge, f Facts) (Edge, bool) {
	for _, e := range edges {
		if g, err := ParseGuard(e.Guard); err == nil && f.holds(g) {
			return e, true
		}
	}
	return Edge{}, false
}

func (f Facts) holds(g Guard) bool {
	switch g.Kind {
	case GuardPRChecksGreen:
		return f.anyPR(func(p PRFact) bool { return p.open() && p.HeadFresh && p.ChecksRollup == "SUCCESS" })
	case GuardPRChecksRed:
		return f.anyPR(func(p PRFact) bool {
			return p.open() && p.HeadFresh && (p.ChecksRollup == "FAILURE" || p.ChecksRollup == "ERROR")
		})
	case GuardPRReady:
		return f.anyPR(func(p PRFact) bool { return p.State == "open" })
	case GuardPRMerged:
		return f.anyPR(func(p PRFact) bool { return p.State == "merged" }) && !f.anyPR(PRFact.open)
	case GuardPRClosedUnmerged:
		return f.anyPR(func(p PRFact) bool { return p.State == "closed" }) && !f.anyPR(func(p PRFact) bool { return p.open() || p.State == "merged" })
	case GuardChildrenDone:
		return f.ChildrenDone
	case GuardIssueStatus:
		return f.IssueStatuses[g.Args[0]] == g.Args[1]
	case GuardWebhook:
		return f.Webhooks[g.Args[0]]
	}
	return false
}

func (p PRFact) open() bool { return p.State == "open" || p.State == "draft" }

func (f Facts) anyPR(pred func(PRFact) bool) bool {
	for _, p := range f.PRs {
		if pred(p) {
			return true
		}
	}
	return false
}

// EdgePollInterval is how often a watched step re-reads its facts: PR and
// children facts change in other tables, so the watch polls between deadlines.
const EdgePollInterval = time.Minute

// Why the engine moves an issue.
const (
	ReasonEdge           = "edge"
	ReasonTimeout        = "timeout"
	ReasonClosedUnmerged = "closed_unmerged"
)

// Decision is what the engine does for an issue sitting on a step. Guard is
// the guard of the edge that decided it.
type Decision struct {
	To, Reason, Guard string
}

// FactEdges returns the edges the engine watches: those whose guard reads
// stored facts. Agent guards are decided by the agent's own status write.
func FactEdges(edges []Edge) []Edge {
	var out []Edge
	for _, e := range edges {
		if g, err := ParseGuard(e.Guard); err == nil && g.readsFacts() {
			out = append(out, e)
		}
	}
	return out
}

// HasFactEdges reports whether any edge reads stored facts.
func HasFactEdges(edges []Edge) bool {
	return len(FactEdges(edges)) > 0
}

// Decide checks a step's edges against the stored facts for an issue that
// entered the step at entered. It moves the issue to the first edge, in list
// order, whose guard holds; else to blocked when a closed PR would otherwise
// strand an issue whose edges never mention it; else to the on_timeout of the
// edge with the earliest deadline that has passed (ties by list order).
//
// When nothing moves, nextCheck is when to look again: the earliest future
// deadline, at most EdgePollInterval away. It is zero when no edge reads facts.
func Decide(edges []Edge, f Facts, entered, now time.Time, hasBlocked bool) (d Decision, move bool, nextCheck time.Time) {
	watched := FactEdges(edges)
	if len(watched) == 0 {
		return Decision{}, false, time.Time{}
	}
	if e, ok := FirstMatch(watched, f); ok {
		return Decision{To: e.To, Reason: ReasonEdge, Guard: e.Guard}, true, time.Time{}
	}
	// A PR closed without merging would strand an issue whose edges only wait
	// for a merge or green checks, so it parks the issue unless an edge of
	// the step already names that case.
	if hasBlocked && hasPREdge(watched) && !hasGuard(watched, GuardPRClosedUnmerged) && f.holds(Guard{Kind: GuardPRClosedUnmerged}) {
		return Decision{To: issuestatus.Blocked, Reason: ReasonClosedUnmerged, Guard: GuardPRClosedUnmerged}, true, time.Time{}
	}
	var first *Edge
	var firstAt time.Time
	nextCheck = now.Add(EdgePollInterval)
	for i, e := range watched {
		timeout, err := time.ParseDuration(e.Timeout)
		if err != nil || timeout <= 0 {
			continue
		}
		deadline := entered.Add(timeout)
		if deadline.After(now) {
			if deadline.Before(nextCheck) {
				nextCheck = deadline
			}
			continue
		}
		if first == nil || deadline.Before(firstAt) {
			first, firstAt = &watched[i], deadline
		}
	}
	if first != nil {
		to := first.OnTimeout
		if to == "" && hasBlocked {
			to = issuestatus.Blocked
		}
		if to != "" {
			return Decision{To: to, Reason: ReasonTimeout, Guard: first.Guard}, true, time.Time{}
		}
	}
	return Decision{}, false, nextCheck
}

func hasPREdge(edges []Edge) bool {
	for _, e := range edges {
		if g, err := ParseGuard(e.Guard); err == nil && g.needsTimeout() {
			return true
		}
	}
	return false
}

func hasGuard(edges []Edge, kind string) bool {
	for _, e := range edges {
		if g, err := ParseGuard(e.Guard); err == nil && g.Kind == kind {
			return true
		}
	}
	return false
}
