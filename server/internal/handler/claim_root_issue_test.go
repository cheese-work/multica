package handler

import (
	"context"
	"errors"
	"testing"
)

// fakeIssue is one node of an in-memory parent chain: number and parent id.
type fakeIssue struct {
	number int32
	parent string
}

func fakeLookup(issues map[string]fakeIssue) func(context.Context, string) (int32, string, error) {
	return func(_ context.Context, id string) (int32, string, error) {
		i, ok := issues[id]
		if !ok {
			return 0, "", errors.New("not found")
		}
		return i.number, i.parent, nil
	}
}

func TestResolveRootIssueNumber(t *testing.T) {
	issues := map[string]fakeIssue{
		"root":       {number: 6498},
		"child":      {number: 6537, parent: "root"},
		"grandchild": {number: 9000, parent: "child"},
	}
	cases := []struct {
		name       string
		self       int32
		parent     string
		wantNumber int32
	}{
		{"root issue is its own root", 6498, "", 6498},
		{"child takes the parent as root", 6537, "root", 6498},
		{"grandchild walks two hops", 9000, "child", 6498},
	}
	for _, c := range cases {
		got := resolveRootIssueNumber(context.Background(), c.self, c.parent, fakeLookup(issues))
		if got != c.wantNumber {
			t.Errorf("%s: root = %d, want %d", c.name, got, c.wantNumber)
		}
	}
}

func TestResolveRootIssueNumberRerunSameRoot(t *testing.T) {
	// A rerun is another task on the same issue: same inputs, same root.
	issues := map[string]fakeIssue{"root": {number: 6498}}
	a := resolveRootIssueNumber(context.Background(), 6537, "root", fakeLookup(issues))
	b := resolveRootIssueNumber(context.Background(), 6537, "root", fakeLookup(issues))
	if a != 6498 || a != b {
		t.Fatalf("rerun root = %d/%d, want 6498 twice", a, b)
	}
}

func TestResolveRootIssueNumberBrokenChainIsUnknown(t *testing.T) {
	issues := map[string]fakeIssue{"child": {number: 9000, parent: "missing"}}
	if got := resolveRootIssueNumber(context.Background(), 6537, "child", fakeLookup(issues)); got != 0 {
		t.Fatalf("broken chain root = %d, want 0 (unknown)", got)
	}
}

func TestResolveRootIssueNumberCycleIsUnknown(t *testing.T) {
	issues := map[string]fakeIssue{"a": {number: 1, parent: "b"}, "b": {number: 2, parent: "a"}}
	if got := resolveRootIssueNumber(context.Background(), 3, "a", fakeLookup(issues)); got != 0 {
		t.Fatalf("cyclic chain root = %d, want 0 (unknown)", got)
	}
}
