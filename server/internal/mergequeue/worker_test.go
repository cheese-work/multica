package mergequeue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeSource answers only for its own workspace: a sweep reads every pending
// row, and rows other tests left in a shared database must see an empty
// timeline.
type fakeSource struct {
	ws    uuid.UUID
	items []TimelineItem
	runs  []BuildRun
	calls int
}

func (f *fakeSource) Timeline(_ context.Context, r Repo, _ int) ([]TimelineItem, error) {
	if r.WS != f.ws {
		return nil, nil
	}
	f.calls++
	return f.items, nil
}
func (f *fakeSource) QueueRuns(_ context.Context, r Repo, _ string) ([]BuildRun, error) {
	if r.WS != f.ws {
		return nil, nil
	}
	return f.runs, nil
}

type fakeEvidence struct{ calls int }

func (f *fakeEvidence) QueueBuild(context.Context, Repo, BuildRun) (Collected, error) {
	f.calls++
	return Collected{Signature: "sig", Failing: []TestID{{Package: "example.com/p", Name: "TestA"}}}, nil
}

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func removal(node string, at time.Time) TimelineItem {
	return TimelineItem{Kind: ItemRemoval, NodeID: node, BeforeOID: "oid-" + node, Reason: "CI_FAILURE", At: at}
}

func failedRun(at time.Time) BuildRun {
	return BuildRun{ID: 99, Attempt: 1, Completed: true, Conclusion: "failure", CreatedAt: at, CompletedAt: at.Add(10 * time.Minute)}
}

func newWorker(p *pgxpool.Pool, src *fakeSource, now time.Time) (*Worker, *fakeEvidence) {
	ev := &fakeEvidence{}
	return &Worker{Pool: p, Src: src, Ev: ev, Now: func() time.Time { return now }}, ev
}

func pending(t *testing.T, p *pgxpool.Pool, ws uuid.UUID, delivery string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTx(t, p, func(tx pgx.Tx) {
		var err error
		if id, _, err = InsertIntake(context.Background(), tx, input(ws, delivery, "warn")); err != nil {
			t.Fatal(err)
		}
	})
	return id
}

type rowView struct {
	Status, Node, Before, Delivery, Routing string
	Run                                     *int64
}

func view(t *testing.T, p *pgxpool.Pool, id uuid.UUID) rowView {
	t.Helper()
	var v rowView
	err := p.QueryRow(context.Background(), `SELECT status, coalesce(removal_node_id,''), coalesce(before_commit_oid,''), coalesce(delivery_id,''), routing, build_run_id FROM merge_queue_intake WHERE id = $1`, id).
		Scan(&v.Status, &v.Node, &v.Before, &v.Delivery, &v.Routing, &v.Run)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkerBindsTheNewestRemovalToItsQueueBuild(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, _ := newWorker(p, src, t0.Add(2*time.Hour))
	if n, err := w.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	v := view(t, p, id)
	if v.Status != "processed" || v.Node != "n2" || v.Before != "oid-n2" || v.Run == nil || *v.Run != 99 {
		t.Errorf("row = %+v; want processed, bound to n2 / oid-n2 / run 99", v)
	}
	var tests []byte
	var sig string
	if err := p.QueryRow(context.Background(), `SELECT signature, failing_tests FROM merge_queue_evidence WHERE intake_id = $1 AND kind = 'queue_build'`, id).Scan(&sig, &tests); err != nil {
		t.Fatalf("evidence row: %v", err)
	}
	var got []TestID
	_ = json.Unmarshal(tests, &got)
	if sig != "sig" || len(got) != 1 || got[0].Package != "example.com/p" || got[0].Name != "TestA" {
		t.Errorf("evidence = %q %v; want sig with example.com/p TestA", sig, got)
	}
}

func TestWorkerAttachesADeliveryToARemovalTheRecordAlreadyStored(t *testing.T) {
	p, ws := pool(t)
	inTx(t, p, func(tx pgx.Tx) {
		if _, _, err := EnsureIntakeForRemoval(context.Background(), tx, input(ws, "", "warn"), "n2", t0); err != nil {
			t.Fatal(err)
		}
	})
	pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n2", t0)}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(time.Hour))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, p, "merge_queue_intake", ws); n != 1 {
		t.Fatalf("intake rows = %d; want 1", n)
	}
	if n := count(t, p, "merge_queue_evidence", ws); n != 1 || ev.calls != 1 {
		t.Errorf("evidence rows = %d, collected %d times; want 1 and 1", n, ev.calls)
	}
	var delivery, status string
	if err := p.QueryRow(context.Background(), `SELECT coalesce(delivery_id,''), status FROM merge_queue_intake WHERE workspace_id = $1`, ws).Scan(&delivery, &status); err != nil {
		t.Fatal(err)
	}
	if delivery != "d1" || status != "processed" {
		t.Errorf("row delivery %q status %q; want d1 processed", delivery, status)
	}
}

func TestWorkerNeverLetsAnOlderRemovalReplaceNewerEvidence(t *testing.T) {
	p, ws := pool(t)
	newer := pending(t, p, ws, "d2")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, _ := newWorker(p, src, t0.Add(2*time.Hour))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The record stores the earlier removal afterwards, and a read that does not
	// show the newer one must still not write its evidence.
	var older uuid.UUID
	inTx(t, p, func(tx pgx.Tx) {
		var err error
		if older, _, err = EnsureIntakeForRemoval(context.Background(), tx, input(ws, "", "warn"), "n1", t0); err != nil {
			t.Fatal(err)
		}
	})
	src.items = []TimelineItem{removal("n1", t0)}
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, older); v.Status != "stale" {
		t.Errorf("older row status = %q; want stale", v.Status)
	}
	if v := view(t, p, newer); v.Status != "processed" {
		t.Errorf("newer row status = %q; want processed", v.Status)
	}
	if n := count(t, p, "merge_queue_evidence", ws); n != 1 {
		t.Errorf("evidence rows = %d; want only the newer removal's", n)
	}
}

func TestWorkerMarksARemovalStaleWhenAnotherRemovalFollows(t *testing.T) {
	p, ws := pool(t)
	var older uuid.UUID
	inTx(t, p, func(tx pgx.Tx) {
		older, _, _ = EnsureIntakeForRemoval(context.Background(), tx, input(ws, "", "warn"), "n1", t0)
	})
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(2*time.Hour))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, older); v.Status != "stale" || ev.calls != 0 {
		t.Errorf("status %q, collected %d times; want stale and no collection", v.Status, ev.calls)
	}
}

func TestWorkerMarksARemovalStaleAfterALaterPush(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	push := TimelineItem{Kind: ItemCommit, NodeID: "c1", At: t0.Add(time.Minute)}
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), push}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(time.Hour))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, id); v.Status != "stale" || v.Node != "n1" || ev.calls != 0 {
		t.Errorf("row %+v, collected %d times; want stale, bound to n1, no collection", v, ev.calls)
	}
}

func TestWorkerGivesInconclusiveAfter45MinutesWithNoBuild(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0)}}
	w, _ := newWorker(p, src, t0.Add(44*time.Minute))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, id); v.Status != "pending" || v.Node != "n1" {
		t.Fatalf("at 44 min: %+v; want pending, bound to n1", v)
	}
	w.Now = func() time.Time { return t0.Add(45 * time.Minute) }
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, id); v.Status != "inconclusive" {
		t.Errorf("at 45 min: status %q; want inconclusive", v.Status)
	}
}

func TestWorkerKeepsARunningBuildPending(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0)}, runs: []BuildRun{{ID: 5, Completed: false}}}
	w, _ := newWorker(p, src, t0.Add(10*time.Minute))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, id); v.Status != "pending" {
		t.Errorf("status %q; want pending", v.Status)
	}
}

func TestWorkerSettlesAnotherReasonWithoutABuildRead(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	manual := removal("n1", t0)
	manual.Reason = "MANUAL"
	src := &fakeSource{ws: ws, items: []TimelineItem{manual}}
	w, ev := newWorker(p, src, t0.Add(time.Minute))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	_ = p.QueryRow(context.Background(), `SELECT coalesce(reason,'') FROM merge_queue_intake WHERE id = $1`, id).Scan(&reason)
	if v := view(t, p, id); v.Status != "processed" || reason != "MANUAL" || ev.calls != 0 {
		t.Errorf("status %q reason %q, collected %d; want processed MANUAL 0", v.Status, reason, ev.calls)
	}
}

func TestWorkerKeepsTheRoutingOfTheIntakeCommit(t *testing.T) {
	p, ws := pool(t)
	id := pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0)}, runs: []BuildRun{failedRun(t0)}}
	w, _ := newWorker(p, src, t0.Add(time.Hour))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, id); v.Routing != "warn" {
		t.Errorf("routing %q; want warn, as at the intake commit", v.Routing)
	}
}

func TestWorkerSweepTwiceCollectsOnce(t *testing.T) {
	p, ws := pool(t)
	pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0)}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(time.Hour))
	for i := 0; i < 2; i++ {
		if _, err := w.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, p, "merge_queue_evidence", ws); n != 1 || ev.calls != 1 {
		t.Errorf("evidence rows %d, collections %d; want 1 and 1", n, ev.calls)
	}
}
