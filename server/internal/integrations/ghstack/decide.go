// Package ghstack decides how to link dependent PRs into GitHub stacks.
package ghstack

// State is a PR's lifecycle state.
type State int

const (
	Open State = iota
	Closed
	Merged
)

// PR is one pull request of the repo. Head and Base are branch names.
// Fork is true when the head branch lives in a fork.
type PR struct {
	Number     int
	Head, Base string
	State      State
	Fork       bool
}

// Stack is an existing GitHub stack. PRs are numbers, bottom first.
type Stack struct {
	ID  int
	PRs []int
}

// Facts is everything Decide reads. PRs must contain N and every PR whose
// head ref could be an ancestor of N's base, and every stack member that
// matters; Decide does no I/O.
type Facts struct {
	N       int
	Default string
	PRs     []PR
	Stacks  []Stack
}

type Kind int

const (
	None Kind = iota
	Create
	Add
	Report
)

// Code names why a report was chosen.
type Code string

const (
	InOtherStack   Code = "in_other_stack"
	BranchingChain Code = "branching_chain"
	AmbiguousBase  Code = "ambiguous_base"
	StaleBase      Code = "stale_base"
	ClosedAncestor Code = "closed_ancestor"
	Cycle          Code = "cycle"
	DepthLimit     Code = "depth_limit"
)

// Action is the result. PRs is bottom first. Report carries no write.
type Action struct {
	Kind    Kind
	StackID int // Add only
	PRs     []int
	Code    Code // Report only
}

// maxAncestors is the deepest chain walk allowed below N.
const maxAncestors = 10

func report(c Code) Action { return Action{Kind: Report, Code: c} }

// Decide is a pure function; see the CHE-4143 plan r2, Decision rules.
// Every report carries no write, and a chain is never truncated.
func Decide(f Facts) Action {
	byNum := make(map[int]PR, len(f.PRs))
	for _, p := range f.PRs {
		byNum[p.Number] = p
	}
	n, ok := byNum[f.N]
	if !ok || n.State != Open || n.Fork || n.Base == f.Default {
		return Action{}
	}
	stackOf := func(num int) (s Stack, pos int, ok bool) {
		for _, s := range f.Stacks {
			for i, m := range s.PRs {
				if m == num {
					return s, i, true
				}
			}
		}
		return Stack{}, 0, false
	}
	if s, pos, in := stackOf(n.Number); in {
		if pos > 0 && byNum[s.PRs[pos-1]].Head == n.Base {
			// Same per-ancestor checks as the walk: a stacked N whose
			// base PR is closed, merged or ambiguous is still reported.
			if base, code := baseOf(f.PRs, n); code != "" {
				return report(code)
			} else if base.Number == s.PRs[pos-1] {
				return Action{}
			}
		}
		return report(InOtherStack)
	}

	chain := []int{n.Number} // top first while walking
	seen := map[string]bool{n.Head: true}
	for cur := n; ; {
		if cur.Base == f.Default {
			return Action{Kind: Create, PRs: reversed(chain)}
		}
		a, code := baseOf(f.PRs, cur)
		if code != "" {
			return report(code)
		}
		if seen[a.Head] {
			return report(Cycle)
		}
		seen[a.Head] = true
		if len(chain) > maxAncestors {
			return report(DepthLimit)
		}
		if s, pos, in := stackOf(a.Number); in {
			if pos != len(s.PRs)-1 {
				return report(BranchingChain)
			}
			return Action{Kind: Add, StackID: s.ID, PRs: reversed(chain)}
		}
		chain = append(chain, a.Number)
		cur = a
	}
}

// baseOf finds the single open same-repo PR whose head is cur's base.
// A non-empty code means the base is not usable: closed_ancestor,
// ambiguous_base, or stale_base (merged, fork-only or no PR).
func baseOf(prs []PR, cur PR) (PR, Code) {
	var open []PR
	closed := false
	for _, p := range prs {
		if p.Head != cur.Base || p.Fork {
			continue
		}
		if p.State == Open {
			open = append(open, p)
		} else if p.State == Closed {
			closed = true
		}
	}
	switch {
	case len(open) > 1:
		return PR{}, AmbiguousBase
	case len(open) == 0 && closed:
		return PR{}, ClosedAncestor
	case len(open) == 0:
		return PR{}, StaleBase
	}
	return open[0], ""
}

func reversed(s []int) []int {
	r := make([]int, len(s))
	for i, v := range s {
		r[len(s)-1-i] = v
	}
	return r
}
