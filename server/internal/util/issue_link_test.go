package util

import "testing"

func TestOpenIssueFooter(t *testing.T) {
	got := OpenIssueFooter("CHE-10086", "01a215c1-9ea5-7097-8cf7-ea9b9748c4a4")
	want := "Open issue: [CHE-10086](mention://issue/01a215c1-9ea5-7097-8cf7-ea9b9748c4a4)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
