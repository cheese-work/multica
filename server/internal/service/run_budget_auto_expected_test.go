package service

import (
	"slices"
	"testing"
)

func TestSplitExpectedChecks(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	names, apps, flows := splitExpectedChecks([]string{
		"go-test@15368@" + ci, // Actions with its workflow
		"go-test@15368",       // Actions without a workflow: never matches
		"lint@777",            // another App needs no workflow
		"deploy@777@" + ci,    // another App may still name one
		"bare",                // no app id
		"x@0@" + ci,           // app id must be positive
		"y@notanumber@" + ci,  // app id must be a number
		"a@b@15368@" + ci,     // an @ inside the name stays in the name
	})
	if want := []string{"go-test", "go-test", "lint", "deploy", "bare", "x@0@" + ci, "y@notanumber@" + ci, "a@b"}; !slices.Equal(names, want) {
		t.Errorf("names = %q, want %q", names, want)
	}
	if want := []int64{15368, 0, 777, 777, 0, 0, 0, 15368}; !slices.Equal(apps, want) {
		t.Errorf("app ids = %v, want %v", apps, want)
	}
	if want := []string{ci, "", "", ci, "", "", "", ci}; !slices.Equal(flows, want) {
		t.Errorf("workflows = %q, want %q", flows, want)
	}
}
