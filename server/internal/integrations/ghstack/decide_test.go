package ghstack

import (
	"fmt"
	"reflect"
	"testing"
)

// o builds an open same-repo PR.
func o(n int, head, base string) PR { return PR{Number: n, Head: head, Base: base} }

func with(p PR, s State, fork bool) PR { p.State, p.Fork = s, fork; return p }

func stack(id int, prs ...int) Stack { return Stack{ID: id, PRs: prs} }

func rep(c Code) Action { return Action{Kind: Report, Code: c} }

func TestDecide(t *testing.T) {
	// chain of n open PRs: pr1 -> main, pr2 -> b1, ...
	chain := func(n int) []PR {
		prs := []PR{o(1, "b1", "main")}
		for i := 2; i <= n; i++ {
			prs = append(prs, o(i, fmt.Sprintf("b%d", i), fmt.Sprintf("b%d", i-1)))
		}
		return prs
	}
	tests := []struct {
		name string
		f    Facts
		want Action
	}{
		{"N missing", Facts{N: 9, Default: "main"}, Action{}},
		{"N base is default", Facts{N: 1, Default: "main", PRs: chain(1)}, Action{}},
		{"N closed", Facts{N: 2, Default: "main", PRs: []PR{o(1, "b1", "main"), with(o(2, "b2", "b1"), Closed, false)}}, Action{}},
		{"N from fork", Facts{N: 2, Default: "main", PRs: []PR{o(1, "b1", "main"), with(o(2, "b2", "b1"), Open, true)}}, Action{}},
		{"base is not a PR head", Facts{N: 1, Default: "main", PRs: []PR{o(1, "b1", "release")}}, rep(StaleBase)},
		{"create 2", Facts{N: 2, Default: "main", PRs: chain(2)}, Action{Kind: Create, PRs: []int{1, 2}}},
		{"create 4 bottom first", Facts{N: 4, Default: "main", PRs: chain(4)}, Action{Kind: Create, PRs: []int{1, 2, 3, 4}}},
		{"create ignores PRs above N", Facts{N: 2, Default: "main", PRs: chain(4)}, Action{Kind: Create, PRs: []int{1, 2}}},
		{"create ignores fork PR with same head", Facts{N: 2, Default: "main",
			PRs: []PR{o(1, "b1", "main"), with(o(7, "b1", "main"), Open, true), o(2, "b2", "b1")}}, Action{Kind: Create, PRs: []int{1, 2}}},
		{"create ignores closed PR with same head when an open one exists", Facts{N: 2, Default: "main",
			PRs: []PR{o(1, "b1", "main"), with(o(7, "b1", "main"), Closed, false), o(2, "b2", "b1")}}, Action{Kind: Create, PRs: []int{1, 2}}},
		{"depth 10 ancestors ok", Facts{N: 11, Default: "main", PRs: chain(11)},
			Action{Kind: Create, PRs: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}},
		{"depth 11 ancestors", Facts{N: 12, Default: "main", PRs: chain(12)}, rep(DepthLimit)},
		{"extend top of stack", Facts{N: 3, Default: "main", PRs: chain(3), Stacks: []Stack{stack(5, 1, 2)}},
			Action{Kind: Add, StackID: 5, PRs: []int{3}}},
		{"review case: D first adds C, D", Facts{N: 4, Default: "main", PRs: chain(4), Stacks: []Stack{stack(5, 1, 2)}},
			Action{Kind: Add, StackID: 5, PRs: []int{3, 4}}},
		{"review case: then C is none", Facts{N: 3, Default: "main", PRs: chain(4), Stacks: []Stack{stack(5, 1, 2, 3, 4)}}, Action{}},
		{"already linked, predecessor is base", Facts{N: 2, Default: "main", PRs: chain(2), Stacks: []Stack{stack(5, 1, 2)}}, Action{}},
		{"in stack, predecessor closed", Facts{N: 2, Default: "main", PRs: []PR{with(o(1, "b1", "main"), Closed, false), o(2, "b2", "b1")}, Stacks: []Stack{stack(5, 1, 2)}}, rep(ClosedAncestor)},
		{"in stack, predecessor merged", Facts{N: 2, Default: "main", PRs: []PR{with(o(1, "b1", "main"), Merged, false), o(2, "b2", "b1")}, Stacks: []Stack{stack(5, 1, 2)}}, rep(StaleBase)},
		{"in stack, predecessor head shared", Facts{N: 2, Default: "main", PRs: []PR{o(1, "b1", "main"), o(7, "b1", "main"), o(2, "b2", "b1")}, Stacks: []Stack{stack(5, 1, 2)}}, rep(AmbiguousBase)},
		{"in stack, predecessor is not base", Facts{N: 3, Default: "main", PRs: chain(3), Stacks: []Stack{stack(5, 1, 3)}}, rep(InOtherStack)},
		{"in stack as bottom with non-default base", Facts{N: 2, Default: "main", PRs: chain(2), Stacks: []Stack{stack(5, 2)}}, rep(InOtherStack)},
		{"ancestor not top of its stack", Facts{N: 3, Default: "main", PRs: chain(3), Stacks: []Stack{stack(5, 1, 2, 9)}}, rep(BranchingChain)},
		{"ancestor stacked, not top, deeper walk", Facts{N: 4, Default: "main", PRs: chain(4), Stacks: []Stack{stack(5, 1, 2, 9)}}, rep(BranchingChain)},
		{"ambiguous base", Facts{N: 2, Default: "main", PRs: []PR{o(1, "b1", "main"), o(7, "b1", "main"), o(2, "b2", "b1")}}, rep(AmbiguousBase)},
		{"ambiguous base deeper", Facts{N: 3, Default: "main",
			PRs: []PR{o(1, "b1", "main"), o(7, "b1", "main"), o(2, "b2", "b1"), o(3, "b3", "b2")}}, rep(AmbiguousBase)},
		{"closed ancestor", Facts{N: 2, Default: "main", PRs: []PR{with(o(1, "b1", "main"), Closed, false), o(2, "b2", "b1")}}, rep(ClosedAncestor)},
		{"closed ancestor deeper", Facts{N: 3, Default: "main",
			PRs: []PR{with(o(1, "b1", "main"), Closed, false), o(2, "b2", "b1"), o(3, "b3", "b2")}}, rep(ClosedAncestor)},
		{"merged ancestor is stale base", Facts{N: 2, Default: "main", PRs: []PR{with(o(1, "b1", "main"), Merged, false), o(2, "b2", "b1")}}, rep(StaleBase)},
		{"fork-only ancestor is stale base", Facts{N: 2, Default: "main", PRs: []PR{with(o(1, "b1", "main"), Open, true), o(2, "b2", "b1")}}, rep(StaleBase)},
		{"cycle", Facts{N: 1, Default: "main", PRs: []PR{o(1, "a", "b"), o(2, "b", "c"), o(3, "c", "a")}}, rep(Cycle)},
		{"self base", Facts{N: 1, Default: "main", PRs: []PR{o(1, "a", "a")}}, rep(Cycle)},
		{"cycle not through N", Facts{N: 1, Default: "main", PRs: []PR{o(1, "a", "b"), o(2, "b", "c"), o(3, "c", "b")}}, rep(Cycle)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.f); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
