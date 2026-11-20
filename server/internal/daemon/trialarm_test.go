package daemon

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// handArm recomputes the arm the slow way: sha256 digest read as a big-endian
// integer, mod 2. For a big-endian integer that is the parity of the last byte.
func handArm(key string) int {
	sum := sha256.Sum256([]byte(key))
	return int(sum[len(sum)-1] % 2)
}

func TestTrialArmMatchesHandComputation(t *testing.T) {
	// Pinned from `printf %s KEY | sha256sum`, last hex byte mod 2, by hand
	// (not from this code). CHE-6532 is the only odd one.
	for key, want := range map[string]int{
		"CHE-6498": 0, // ...1d2a
		"CHE-6537": 0, // ...d930
		"CHE-1":    0, // ...856e
		"CHE-6532": 1, // ...edb5
		"CHE-6539": 0, // ...0fca
	} {
		if got := TrialArm(key); got != want {
			t.Errorf("TrialArm(%q) = %d, want %d", key, got, want)
		}
		if got := handArm(key); got != want {
			t.Errorf("handArm(%q) = %d, want %d", key, got, want)
		}
	}
	// Both arms must occur, or the split is broken.
	seen := map[int]bool{}
	for i := 0; i < 64; i++ {
		seen[TrialArm(fmt.Sprintf("CHE-%d", 1000+i))] = true
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("arm split is one-sided: %v", seen)
	}
}

func TestTaskRootKeyAndArmForRootChildAndRerun(t *testing.T) {
	root := Task{IssueIdentifier: "CHE-6498", RootIssueIdentifier: "CHE-6498"}
	child := Task{IssueIdentifier: "CHE-6537", ParentIssue: &ParentIssueData{Identifier: "CHE-6498"}, RootIssueIdentifier: "CHE-6498"}
	rerun := Task{ID: "second-run", IssueIdentifier: "CHE-6537", ParentIssue: &ParentIssueData{Identifier: "CHE-6498"}, RootIssueIdentifier: "CHE-6498"}
	grandchild := Task{IssueIdentifier: "CHE-9000", ParentIssue: &ParentIssueData{Identifier: "CHE-6537"}, RootIssueIdentifier: "CHE-6498"}

	for name, task := range map[string]Task{"root": root, "child": child, "rerun": rerun, "grandchild": grandchild} {
		key, ok := task.RootIssueKey()
		if !ok || key != "CHE-6498" {
			t.Errorf("%s: root key = %q ok=%v, want CHE-6498", name, key, ok)
		}
	}
}

func TestTaskRootKeyUnknownForChildFromOldServer(t *testing.T) {
	// Old server: the parent is known but the chain is not, so the root is
	// unknown. Guessing the parent would put a grandchild in the wrong arm.
	task := Task{IssueIdentifier: "CHE-9000", ParentIssue: &ParentIssueData{Identifier: "CHE-6537"}}
	if key, ok := task.RootIssueKey(); ok {
		t.Fatalf("root key = %q, want unknown", key)
	}
	if _, ok := (Task{}).RootIssueKey(); ok {
		t.Fatal("a task with no issue must have no root key")
	}
}

// A sub-issue whose parent read failed at claim arrives with no ParentIssue
// (loadClaimParentIssue drops it) and no RootIssueIdentifier (the chain walk
// fails too). It must stay unknown, not become its own root.
func TestTaskRootKeyUnknownWhenParentLookupFailed(t *testing.T) {
	task := Task{IssueIdentifier: "CHE-6537"}
	if key, ok := task.RootIssueKey(); ok {
		t.Fatalf("root key = %q, want unknown", key)
	}
	var buf bytes.Buffer
	logTrialArm(slog.New(slog.NewTextHandler(&buf, nil)), task)
	for _, want := range []string{"issue=CHE-6537", "root=unknown", "arm=unknown"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q missing %q", buf.String(), want)
		}
	}
}

func TestLogTrialArmOneLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	logTrialArm(log, Task{IssueIdentifier: "CHE-6537", RootIssueIdentifier: "CHE-6498", ParentIssue: &ParentIssueData{Identifier: "CHE-6498"}})
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want one line, got %q", out)
	}
	for _, want := range []string{"trial arm", "issue=CHE-6537", "root=CHE-6498", "arm="} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
	buf.Reset()
	logTrialArm(log, Task{ChatSessionID: "c1"})
	if buf.Len() != 0 {
		t.Fatalf("non-issue task must not log an arm, got %q", buf.String())
	}
}
