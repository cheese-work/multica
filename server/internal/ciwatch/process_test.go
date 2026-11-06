package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// memEvents mimics the ci_failure_event unique key.
type memEvents struct{ seen map[string]bool }

func (m *memEvents) InsertCIFailureEvent(_ context.Context, a db.InsertCIFailureEventParams) (int64, error) {
	k := fmt.Sprint(a.WorkspaceID, a.RepoOwner, a.RepoName, a.RunID, a.RunAttempt)
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	if m.seen[k] {
		return 0, nil
	}
	m.seen[k] = true
	return 1, nil
}

// memObs mimics the (workspace, subject) unique key and can fail on demand.
type memObs struct {
	mu   sync.Mutex
	rows map[string]db.CreateSystem1CIObservationParams
	fail error
}

func (m *memObs) CreateSystem1CIObservation(_ context.Context, a db.CreateSystem1CIObservationParams) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	if m.rows == nil {
		m.rows = map[string]db.CreateSystem1CIObservationParams{}
	}
	k := uuid.UUID(a.WorkspaceID.Bytes).String() + "/" + uuid.UUID(a.SubjectID.Bytes).String()
	if _, dup := m.rows[k]; dup {
		return 0, nil
	}
	m.rows[k] = a
	return 1, nil
}

func pipelineFor(ev *memEvents, ob *memObs, gh *fakeGH) Pipeline {
	c, _ := newCollector(gh, &fakeBudget{}, fakeRuns{})
	return Pipeline{Collector: c, Events: ev, Observer: Observer{W: ob}}
}

func failingRunGH() *fakeGH {
	return &fakeGH{jobsByPage: [][]Job{{failedJob(1, "build", t0)}},
		logs: map[int64]func(context.Context) io.Reader{1: text("API rate limit exceeded\n")}}
}

func realInput() Input {
	in := baseInput()
	in.Run.WorkspaceID = uuid.NewString()
	return in
}

func TestProcessStoresEventThenObserves(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{}
	res, err := pipelineFor(ev, ob, failingRunGH()).Process(context.Background(), realInput(), History{})
	if err != nil || res.Kind != KindEvidence {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(ev.seen) != 1 || len(ob.rows) != 1 {
		t.Fatalf("events=%d observations=%d, want 1 and 1", len(ev.seen), len(ob.rows))
	}
	for _, r := range ob.rows {
		if r.Class != "infra_rate_limit" || r.ChoiceKey != "P1" {
			t.Errorf("row = %+v", r)
		}
	}
}

func TestProcessObservationFailureIsRecoveredByRedelivery(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{fail: errors.New("db down")}
	in := realInput()
	p := pipelineFor(ev, ob, failingRunGH())
	if _, err := p.Process(context.Background(), in, History{}); err == nil {
		t.Fatal("a failed observation write must be returned so the delivery is retried")
	}
	if len(ev.seen) != 1 || len(ob.rows) != 0 {
		t.Fatalf("after failure: events=%d observations=%d, want 1 and 0", len(ev.seen), len(ob.rows))
	}
	// Redelivery: the event is already stored, the observation is still written.
	ob.fail = nil
	if _, err := pipelineFor(ev, ob, failingRunGH()).Process(context.Background(), in, History{}); err != nil {
		t.Fatal(err)
	}
	if len(ob.rows) != 1 {
		t.Fatalf("after redelivery: observations=%d, want 1", len(ob.rows))
	}
	// A further redelivery adds nothing.
	if _, err := pipelineFor(ev, ob, failingRunGH()).Process(context.Background(), in, History{}); err != nil || len(ob.rows) != 1 {
		t.Fatalf("second redelivery: err=%v observations=%d, want 1", err, len(ob.rows))
	}
}

func TestProcessWritesP0RowWithoutStoringAnEvent(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{}
	in := realInput()
	in.Run.Conclusion = "cancelled"
	res, err := pipelineFor(ev, ob, &fakeGH{}).Process(context.Background(), in, History{})
	if err != nil || res.Kind != KindP0 || res.Reason != ReasonCancelled {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(ev.seen) != 0 || len(ob.rows) != 1 {
		t.Fatalf("events=%d observations=%d, want 0 and 1", len(ev.seen), len(ob.rows))
	}
}

func TestProcessDroppedRunWritesNothing(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{}
	in := realInput()
	in.FlagOn = false
	if _, err := pipelineFor(ev, ob, &fakeGH{}).Process(context.Background(), in, History{}); err != nil {
		t.Fatal(err)
	}
	if len(ev.seen) != 0 || len(ob.rows) != 0 {
		t.Fatalf("events=%d observations=%d, want none", len(ev.seen), len(ob.rows))
	}
}

func TestSubjectIDIsPerWorkspace(t *testing.T) {
	a, b := obsRun(), obsRun()
	b.WorkspaceID = uuid.NewString()
	if subjectID(a) == subjectID(b) {
		t.Error("two workspaces watching one repo must not share a subject id")
	}
}
