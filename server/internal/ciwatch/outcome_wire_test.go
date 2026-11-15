package ciwatch

import (
	"context"
	"errors"
	"io"
	"testing"
)

// The production path (Watcher -> Pipeline) must apply the self_healed label
// to a failure whose newer run already went green.
func TestProcessAppliesSelfHealedLabel(t *testing.T) {
	ev, ob, set := &memEvents{}, &memObs{}, &fakeSetter{}
	p := pipelineWithRuns(ev, ob, failingRunGH(), fakeRuns{exists: true, succeeded: true})
	p.Observer.Labels = set
	res, err := p.Process(context.Background(), realInput(), History{})
	if err != nil || res.Kind != KindP0 || res.Label != LabelSelfHealed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if set.label != LabelSelfHealed {
		t.Fatalf("label set = %q, want self_healed", set.label)
	}
}

func codeFailGH() *fakeGH {
	return &fakeGH{jobsByPage: [][]Job{{failedJob(1, "test", t0)}},
		logs: map[int64]func(context.Context) io.Reader{1: text("--- FAIL: TestX (0.01s)\nFAIL\n")}}
}

// Third flake in 7 days: the next failure with that signature is flaky.
func TestProcessLoadsFlakeCountForTheSignature(t *testing.T) {
	cases := []struct {
		count int
		want  string
	}{{0, "code_or_test"}, {2, "code_or_test"}, {3, "flaky"}}
	for _, c := range cases {
		ev, ob := &memEvents{}, &memObs{}
		p := pipelineFor(ev, ob, codeFailGH())
		var gotSig string
		p.Observer.Flake = func(_ context.Context, _ Run, sig string) (int, error) { gotSig = sig; return c.count, nil }
		if _, err := p.Process(context.Background(), realInput(), History{}); err != nil {
			t.Fatal(err)
		}
		if gotSig == "" {
			t.Fatal("flake counter not asked for a signature")
		}
		for _, r := range ob.rows {
			if r.Class != c.want {
				t.Errorf("count %d: class = %q, want %q", c.count, r.Class, c.want)
			}
		}
	}
}

func TestProcessFlakeLookupErrorFailsTheDelivery(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{}
	p := pipelineFor(ev, ob, codeFailGH())
	p.Observer.Flake = func(context.Context, Run, string) (int, error) { return 0, errors.New("db down") }
	if _, err := p.Process(context.Background(), realInput(), History{}); err == nil {
		t.Fatal("a failed flake lookup must fail the delivery so it is redelivered")
	}
	if len(ev.seen) != 0 || len(ob.rows) != 0 {
		t.Fatalf("events=%d observations=%d, want none", len(ev.seen), len(ob.rows))
	}
}
