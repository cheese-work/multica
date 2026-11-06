package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// memWS stands for the workspace row: gone=true means it was deleted. locks
// counts the KEY SHARE locks taken.
type memWS struct {
	gone  bool
	locks int
}

func (m *memWS) LockWorkspaceForCIWrite(_ context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	m.locks++
	if m.gone {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return id, nil
}

func pipelineFor(ev *memEvents, ob *memObs, gh *fakeGH) Pipeline {
	return pipelineWithRuns(ev, ob, gh, fakeRuns{})
}

func pipelineWithRuns(ev *memEvents, ob *memObs, gh *fakeGH, runs Runs) Pipeline {
	c, _ := newCollector(gh, &fakeBudget{}, runs)
	return Pipeline{Collector: c, Tx: memTx(ev, ob, &memWS{}), Observer: Observer{W: ob}}
}

// memTx commits both memory stores or, when fn fails, restores them.
func memTx(ev *memEvents, ob *memObs, ws *memWS) InTx {
	return func(_ context.Context, fn func(Writers) error) error {
		evSnap, obSnap := map[string]bool{}, map[string]db.CreateSystem1CIObservationParams{}
		for k, v := range ev.seen {
			evSnap[k] = v
		}
		for k, v := range ob.rows {
			obSnap[k] = v
		}
		if err := fn(Writers{Workspace: ws, Events: ev, Observations: ob}); err != nil {
			ev.seen, ob.rows = evSnap, obSnap
			return err
		}
		return nil
	}
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
	if len(ev.seen) != 0 || len(ob.rows) != 0 {
		t.Fatalf("after failure: events=%d observations=%d, want 0 and 0 (one transaction)", len(ev.seen), len(ob.rows))
	}
	// Redelivery writes both together.
	ob.fail = nil
	if _, err := pipelineFor(ev, ob, failingRunGH()).Process(context.Background(), in, History{}); err != nil {
		t.Fatal(err)
	}
	if len(ev.seen) != 1 || len(ob.rows) != 1 {
		t.Fatalf("after redelivery: events=%d observations=%d, want 1 and 1", len(ev.seen), len(ob.rows))
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

// A failed write must not pin half a decision. The retry sees different state
// (a newer run has since succeeded); the event and the observation that end up
// stored must still agree: either both come from the first attempt, or the
// first attempt left nothing behind.
func TestProcessRetryNeverMixesTwoDecisions(t *testing.T) {
	ev, ob := &memEvents{}, &memObs{fail: errors.New("db down")}
	in := realInput()
	if _, err := pipelineFor(ev, ob, failingRunGH()).Process(context.Background(), in, History{}); err == nil {
		t.Fatal("a failed observation write must be returned")
	}
	ob.fail = nil
	retry := pipelineWithRuns(ev, ob, failingRunGH(), fakeRuns{exists: true, succeeded: true})
	if _, err := retry.Process(context.Background(), in, History{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range ob.rows {
		if len(ev.seen) == 1 && r.Class != "infra_rate_limit" {
			t.Errorf("stored event is a rate-limit failure but the observation says %q", r.Class)
		}
	}
	if len(ob.rows) != 1 {
		t.Fatalf("observations=%d, want 1", len(ob.rows))
	}
}

// The workspace can be deleted while the collector reads GitHub. Both inserts
// have no foreign key, so the pipeline must lock the workspace first and write
// nothing when it is gone.
func TestProcessWorkspaceDeletedDuringCollectionWritesNothing(t *testing.T) {
	ev, ob, ws := &memEvents{}, &memObs{}, &memWS{gone: true}
	p := pipelineFor(ev, ob, failingRunGH())
	p.Tx = memTx(ev, ob, ws)
	_, err := p.Process(context.Background(), realInput(), History{})
	if !errors.Is(err, ErrWorkspaceGone) {
		t.Fatalf("err=%v, want ErrWorkspaceGone", err)
	}
	if len(ev.seen) != 0 || len(ob.rows) != 0 {
		t.Fatalf("events=%d observations=%d, want 0 and 0", len(ev.seen), len(ob.rows))
	}
}

// A live workspace is locked once per write transaction.
func TestProcessLocksWorkspaceBeforeWriting(t *testing.T) {
	ev, ob, ws := &memEvents{}, &memObs{}, &memWS{}
	p := pipelineFor(ev, ob, failingRunGH())
	p.Tx = memTx(ev, ob, ws)
	if _, err := p.Process(context.Background(), realInput(), History{}); err != nil {
		t.Fatal(err)
	}
	if ws.locks != 1 || len(ev.seen) != 1 || len(ob.rows) != 1 {
		t.Fatalf("locks=%d events=%d observations=%d, want 1/1/1", ws.locks, len(ev.seen), len(ob.rows))
	}
}
