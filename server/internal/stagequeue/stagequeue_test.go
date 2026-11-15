package stagequeue

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

func prs(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.PR)
	}
	return out
}

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	if l.For(StageReview) != 2 || l.For(StageDevice) != 1 || l.For(StageBrowser) != 1 {
		t.Fatalf("defaults = %v", l)
	}
}

func TestLimitsFromEnvOverridesAndIgnoresBadValues(t *testing.T) {
	env := map[string]string{
		"MULTICA_STAGE_LIMIT_REVIEW":  "5",
		"MULTICA_STAGE_LIMIT_DEVICE":  "0",   // below 1: keep default
		"MULTICA_STAGE_LIMIT_BROWSER": "abc", // not a number: keep default
	}
	l := LimitsFromEnv(func(k string) string { return env[k] })
	if l.For(StageReview) != 5 || l.For(StageDevice) != 1 || l.For(StageBrowser) != 1 {
		t.Fatalf("limits = %v", l)
	}
}

// The (N+1)th PR for a stage is refused and listed as queued.
func TestPlanRefusesNPlusOneAndListsItQueued(t *testing.T) {
	es := []Entry{
		{PR: "a", Active: true, Created: t0},
		{PR: "b", Active: true, Created: t0.Add(time.Minute)},
		{PR: "c", Created: t0.Add(2 * time.Minute)},
	}
	d := Plan(es, 2)
	if !reflect.DeepEqual(prs(d.Admitted), []string{"a", "b"}) || !reflect.DeepEqual(prs(d.Queued), []string{"c"}) {
		t.Fatalf("admitted=%v queued=%v", prs(d.Admitted), prs(d.Queued))
	}
}

// Order: fewest open gates first, then oldest PR.
func TestPlanOrdersByOpenGatesThenAge(t *testing.T) {
	es := []Entry{
		{PR: "old-3-gates", OpenGates: 3, Created: t0},
		{PR: "new-1-gate", OpenGates: 1, Created: t0.Add(3 * time.Hour)},
		{PR: "old-1-gate", OpenGates: 1, Created: t0.Add(time.Hour)},
		{PR: "mid-2-gates", OpenGates: 2, Created: t0.Add(2 * time.Hour)},
	}
	d := Plan(es, 1)
	if !reflect.DeepEqual(prs(d.Admitted), []string{"old-1-gate"}) {
		t.Fatalf("admitted = %v", prs(d.Admitted))
	}
	want := []string{"new-1-gate", "mid-2-gates", "old-3-gates"}
	if !reflect.DeepEqual(prs(d.Queued), want) {
		t.Fatalf("queued = %v, want %v", prs(d.Queued), want)
	}
}

// A run that is already active keeps its slot, even over a closer-to-merge PR.
func TestPlanNeverEvictsActiveRun(t *testing.T) {
	es := []Entry{
		{PR: "running", OpenGates: 4, Active: true, Created: t0},
		{PR: "closer", OpenGates: 0, Created: t0.Add(time.Hour)},
	}
	d := Plan(es, 1)
	if !reflect.DeepEqual(prs(d.Admitted), []string{"running"}) || !reflect.DeepEqual(prs(d.Queued), []string{"closer"}) {
		t.Fatalf("admitted=%v queued=%v", prs(d.Admitted), prs(d.Queued))
	}
}

// Two active runs of one PR hold one slot, not two.
func TestPlanCountsAPROnce(t *testing.T) {
	es := []Entry{
		{PR: "a", Active: true, Created: t0},
		{PR: "a", Active: true, Created: t0},
		{PR: "b", Created: t0.Add(time.Minute)},
	}
	d := Plan(es, 2)
	if !reflect.DeepEqual(prs(d.Admitted), []string{"a", "b"}) || len(d.Queued) != 0 {
		t.Fatalf("admitted=%v queued=%v", prs(d.Admitted), prs(d.Queued))
	}
}

// Stack, N=1: a waiting PR holds no slot, so predecessor and successor both
// progress whichever one the stage serves first.
func TestStackedPredecessorAndSuccessorBothProgress(t *testing.T) {
	pred := Entry{PR: "pred", OpenGates: 1, Created: t0}
	succ := Entry{PR: "succ", OpenGates: 1, Created: t0.Add(time.Hour)}

	// 1. Successor's run is active; predecessor waits, holding nothing.
	succActive := succ
	succActive.Active = true
	d := Plan([]Entry{pred, succActive}, 1)
	if prs(d.Admitted)[0] != "succ" || prs(d.Queued)[0] != "pred" {
		t.Fatalf("step1 admitted=%v queued=%v", prs(d.Admitted), prs(d.Queued))
	}
	// 2. Successor's run ends (leaves the stage); predecessor is admitted.
	d = Plan([]Entry{pred}, 1)
	if prs(d.Admitted)[0] != "pred" || len(d.Queued) != 0 {
		t.Fatalf("step2 admitted=%v queued=%v", prs(d.Admitted), prs(d.Queued))
	}
	// 3. Predecessor's run ends; a waiting successor is admitted in turn.
	d = Plan([]Entry{succ}, 1)
	if prs(d.Admitted)[0] != "succ" {
		t.Fatalf("step3 admitted=%v", prs(d.Admitted))
	}
}

func TestAdmit(t *testing.T) {
	es := []Entry{
		{PR: "a", Active: true, Created: t0},
		{PR: "b", Active: true, Created: t0},
	}
	cand := Entry{PR: "c", OpenGates: 1, Created: t0}
	waiting := Entry{PR: "w", OpenGates: 0, Created: t0}
	ok, pos := Admit(append(es, waiting), cand, 2)
	if ok || pos != 1 { // behind w, which has fewer open gates
		t.Fatalf("full stage: ok=%v pos=%d", ok, pos)
	}
	ok, pos = Admit(es[:1], cand, 2)
	if !ok || pos != 0 {
		t.Fatalf("free slot: ok=%v pos=%d", ok, pos)
	}
	// A PR already holding a slot is always admitted again.
	ok, _ = Admit(es, Entry{PR: "a", Active: true}, 2)
	if !ok {
		t.Fatal("active PR refused")
	}
}

func TestGateStage(t *testing.T) {
	cases := []struct {
		title string
		want  Stage
		ok    bool
	}{
		{"device gate: CHE-3611", StageDevice, true},
		{"Browser Gate: CHE-12", StageBrowser, true},
		{"  device gate: x", StageDevice, true},
		{"security gate: CHE-3611", "", false},
		{"feat: device gate handling", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := GateStage(c.title)
		if got != c.want || ok != c.ok {
			t.Errorf("GateStage(%q) = %q, %v; want %q, %v", c.title, got, ok, c.want, c.ok)
		}
	}
}
