package handler

import "testing"

func TestIssueWebPath(test *testing.T) {
	if got, want := issueWebPath("cheese-work", "01a140fb-3ecf"), "/cheese-work/issues/01a140fb-3ecf"; got != want {
		test.Fatalf("issueWebPath = %q, want %q", got, want)
	}
}
