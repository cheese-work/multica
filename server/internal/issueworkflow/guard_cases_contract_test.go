package issueworkflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// testdata/guard_cases.json is read by this test and by
// packages/core/issue-workflows/graph-edit.guard-cases.test.ts. The web editor
// must accept exactly the guards the server accepts as an edge guard, or it
// flags a good edge as bad_guard and keeps Save disabled (the CHE-6242 shape).
type guardCase struct {
	Expr         string `json:"expr"`
	Valid        bool   `json:"valid"`
	NeedsTimeout bool   `json:"needs_timeout"`
	EntryOnly    bool   `json:"entry_only"`
}

func loadGuardCases(t *testing.T) []guardCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/guard_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []guardCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestGuardCasesMatchParseGuard(t *testing.T) {
	for _, p := range guardCaseProblems(loadGuardCases(t)) {
		t.Error(p)
	}
}

// guardCaseProblems compares the golden cases with ParseGuard and needsTimeout,
// and requires a valid case for every guardArity kind.
func guardCaseProblems(cases []guardCase) []string {
	var problems []string
	covered := map[string]bool{}
	for _, c := range cases {
		g, err := ParseGuard(c.Expr)
		if (err == nil) != c.Valid {
			problems = append(problems, fmt.Sprintf("ParseGuard(%q) error = %v, golden valid = %v: fix testdata/guard_cases.json or ParseGuard, and the web isValidGuard", c.Expr, err, c.Valid))
			continue
		}
		if !c.Valid {
			if c.NeedsTimeout || c.EntryOnly {
				problems = append(problems, fmt.Sprintf("invalid case %q must not set needs_timeout or entry_only", c.Expr))
			}
			continue
		}
		covered[g.Kind] = true
		if g.needsTimeout() != c.NeedsTimeout {
			problems = append(problems, fmt.Sprintf("needsTimeout(%q) = %v, golden needs_timeout = %v: fix the golden case, and the web guardNeedsTimeout", c.Expr, g.needsTimeout(), c.NeedsTimeout))
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(guardArity)) {
		if !covered[kind] {
			problems = append(problems, fmt.Sprintf("guardArity kind %q has no valid case in testdata/guard_cases.json: add one, and the web GUARD_ARITY entry if an edge may use it", kind))
		}
	}
	return problems
}

// A drifted fixture fails, and the message names the guard and the fix.
func TestGuardCaseProblemsCatchDrift(t *testing.T) {
	good := loadGuardCases(t)
	drift := func(mutate func([]guardCase) []guardCase) string {
		cases := mutate(slices.Clone(good))
		return strings.Join(guardCaseProblems(cases), "\n")
	}
	if got := drift(func(c []guardCase) []guardCase { return c }); got != "" {
		t.Fatalf("golden cases have problems on main: %s", got)
	}
	for name, tc := range map[string]struct {
		mutate func([]guardCase) []guardCase
		want   string
	}{
		"webhook timeout flipped": {func(c []guardCase) []guardCase {
			i := slices.IndexFunc(c, func(x guardCase) bool { return x.Expr == "webhook(ci)" })
			c[i].NeedsTimeout = false
			return c
		}, `needsTimeout("webhook(ci)")`},
		"invalid case made valid": {func(c []guardCase) []guardCase {
			i := slices.IndexFunc(c, func(x guardCase) bool { return x.Expr == "agent_verdict(MAYBE)" })
			c[i].Valid = true
			return c
		}, `ParseGuard("agent_verdict(MAYBE)")`},
		"kind loses its valid case": {func(c []guardCase) []guardCase {
			return slices.DeleteFunc(c, func(x guardCase) bool { return strings.HasPrefix(x.Expr, "webhook(") && x.Valid })
		}, `guardArity kind "webhook" has no valid case`},
	} {
		if got := drift(tc.mutate); !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems = %q, want one containing %q", name, got, tc.want)
		}
	}
}

// entry_only must mark exactly the valid guards that validateEdges refuses as
// edge guards; every other valid guard must pass it with a timeout supplied.
func TestGuardCasesEntryOnlyMatchesEdgeValidation(t *testing.T) {
	def := devDefinition()
	step, ok := def.Step("code_review")
	if !ok {
		t.Fatal("devDefinition has no code_review step")
	}
	for _, c := range loadGuardCases(t) {
		if !c.Valid {
			continue
		}
		step.Edges = []Edge{{To: "done", Guard: c.Expr, Timeout: "72h", OnTimeout: "implement"}}
		err := validateEdges(step, def)
		refused := err != nil && strings.Contains(err.Error(), "entry gate")
		if refused != c.EntryOnly {
			t.Errorf("validateEdges(%q) entry-gate refusal = %v (err %v), golden entry_only = %v", c.Expr, refused, err, c.EntryOnly)
		} else if !c.EntryOnly && err != nil {
			t.Errorf("validateEdges(%q) = %v, want nil for a non-entry guard", c.Expr, err)
		}
	}
}
