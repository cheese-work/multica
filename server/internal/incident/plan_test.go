package incident

import (
	"reflect"
	"testing"
	"time"
)

const ccErr = "Concurrency limit exceeded for account"

func ccRuns() []FailedRun {
	return []FailedRun{run("1", "a", "x", 0, ccErr), run("2", "b", "y", 2, ccErr), run("3", "c", "z", 5, ccErr)}
}

func kinds(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Kind+":"+s.Class)
	}
	return out
}

func TestPlan(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	newest := t0.Add(5 * time.Minute)
	cases := []struct {
		name string
		runs []FailedRun
		open map[string]Open
		now  time.Time
		want []string
	}{
		{"first hit opens", ccRuns(), nil, now, []string{"open:concurrency_limit"}},
		{"open class, nothing new: quiet", ccRuns(), map[string]Open{ClassConcurrency: {LastFailure: newest}}, now, nil},
		{"open class, newer failure: comment", ccRuns(), map[string]Open{ClassConcurrency: {LastFailure: newest.Add(-time.Minute)}}, now, []string{"comment:concurrency_limit"}},
		{"open class, last failure 29 min ago: stays open", nil, map[string]Open{ClassConcurrency: {LastFailure: newest}}, newest.Add(29 * time.Minute), nil},
		{"open class, no failure for 30 min: close", nil, map[string]Open{ClassConcurrency: {LastFailure: newest}}, newest.Add(Window + time.Second), []string{"close:concurrency_limit"}},
		{"below threshold but still failing: stays open", ccRuns()[:1], map[string]Open{ClassConcurrency: {LastFailure: t0}}, now, nil},
		{"other class does not keep it open", []FailedRun{run("1", "a", "x", 40, "429 too many requests")}, map[string]Open{ClassConcurrency: {LastFailure: newest}}, t0.Add(41 * time.Minute), []string{"close:concurrency_limit"}},
	}
	for _, c := range cases {
		if got := kinds(Plan(c.runs, c.open, c.now)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlanCarriesIncidentAndClosesOnLastFailure(t *testing.T) {
	steps := Plan(ccRuns(), nil, t0.Add(10*time.Minute))
	if len(steps) != 1 || steps[0].Incident.Class != ClassConcurrency || !steps[0].LastFailure.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("bad step: %+v", steps)
	}
}
