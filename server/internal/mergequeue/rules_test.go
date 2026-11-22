package mergequeue

import (
	"testing"
	"time"
)

func TestAssociationEndsOnlyOnSplitOrPRClose(t *testing.T) {
	want := map[EventKind]bool{EventSuspectedSplit: true, EventPRClosed: true}
	kinds := []EventKind{EventSuspectedSplit, EventPRClosed, EventQueueEntry, EventEpochResolved, EventEpochOpened, EventRoutingChanged}
	for _, kind := range kinds {
		for _, actor := range []ActorClass{ActorAgent, ActorPerson, ActorUnknown} {
			if got := EndsAssociation(AssociationEvent{kind, actor}); got != want[kind] {
				t.Errorf("%s by %s: EndsAssociation = %v, want %v", kind, actor, got, want[kind])
			}
		}
	}
}

func TestKeys(t *testing.T) {
	sig := KeyForSignature("Cheese-Work/Multica", "abc")
	if sig == "" || sig != KeyForSignature("cheese-work/multica", "abc") {
		t.Errorf("signature key must be non-empty and repo-case-insensitive: %q", sig)
	}
	if sig == KeyForSignature("cheese-work/multica", "abd") || sig == KeyForSignature("cheese-work/other", "abc") {
		t.Error("different signature or repo must give a different key")
	}
	timeout := KeyForTimeout("cheese-work/multica", "abc")
	if timeout == "" || timeout == sig {
		t.Errorf("a timeout key must differ from a signature key with the same text: %q vs %q", timeout, sig)
	}
	if timeout == KeyForTimeout("cheese-work/multica", "abd") {
		t.Error("different check must give a different timeout key")
	}
}

func TestComparable(t *testing.T) {
	a := TestID{"pkg/a", "TestA"}
	b := TestID{"pkg/b", "TestB"}
	job := func(mut func(*JobRun)) JobRun {
		j := JobRun{Check: "backend-test-shards (rest)", Passed: map[TestID]bool{a: true, b: true}, Wait: 4 * time.Minute, Duration: 18 * time.Minute}
		if mut != nil {
			mut(&j)
		}
		return j
	}
	for _, tc := range []struct {
		name    string
		job     JobRun
		failing []TestID
		timeout bool
		want    bool
	}{
		{"both failing tests pass by name and package", job(nil), []TestID{a, b}, false, true},
		{"one named test missing from the log", job(func(j *JobRun) { delete(j.Passed, b) }), []TestID{a, b}, false, false},
		{"same name in another package", job(func(j *JobRun) { j.Passed = map[TestID]bool{{"pkg/x", "TestA"}: true, b: true} }), []TestID{a, b}, false, false},
		{"job was skipped", job(func(j *JobRun) { j.Skipped = true }), []TestID{a}, false, false},
		{"migration-checks never qualifies", job(func(j *JobRun) { j.Check = "migration-checks" }), []TestID{a}, false, false},
		{"nothing to prove", job(nil), nil, false, false},
		{"timeout at exactly 25 min", job(func(j *JobRun) { j.Wait, j.Duration = 5*time.Minute, 20*time.Minute }), nil, true, true},
		{"timeout one second over", job(func(j *JobRun) { j.Wait, j.Duration = 5*time.Minute, 20*time.Minute+time.Second }), nil, true, false},
		{"timeout probe at 27 min", job(func(j *JobRun) { j.Wait, j.Duration = 4*time.Minute, 23*time.Minute }), nil, true, false},
		{"timeout on a skipped job", job(func(j *JobRun) { j.Skipped = true }), nil, true, false},
	} {
		if got := Comparable(tc.job, tc.failing, tc.timeout); got != tc.want {
			t.Errorf("%s: Comparable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
