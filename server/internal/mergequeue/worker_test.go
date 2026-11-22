package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// multiSource serves several workspaces, each from its own fake.
type multiSource map[uuid.UUID]*fakeSource

func (m multiSource) Timeline(ctx context.Context, r Repo, pr int) ([]TimelineItem, error) {
	if f := m[r.WS]; f != nil {
		return f.Timeline(ctx, r, pr)
	}
	return nil, nil
}
func (m multiSource) QueueRuns(ctx context.Context, r Repo, sha string) ([]BuildRun, error) {
	if f := m[r.WS]; f != nil {
		return f.QueueRuns(ctx, r, sha)
	}
	return nil, nil
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
	inTx(t, p, func(tx pgx.Tx) { // n1 already belongs to an earlier delivery
		if _, _, err := EnsureIntakeForRemoval(context.Background(), tx, input(ws, "d0", "warn"), "n1", t0); err != nil {
			t.Fatal(err)
		}
	})
	id := pending(t, p, ws, "d1")
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, _ := newWorker(p, src, t0.Add(2*time.Hour))
	if n, err := w.Sweep(context.Background()); err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want 2 (n1's row goes stale, the delivery's row is processed)", n, err)
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

// pendingAt stores a pending delivery row that arrived at a chosen time with a
// chosen routing value.
func pendingAt(t *testing.T, p *pgxpool.Pool, ws uuid.UUID, delivery, routing string, arrived time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTx(t, p, func(tx pgx.Tx) {
		var err error
		if id, _, err = InsertIntake(context.Background(), tx, input(ws, delivery, routing)); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := p.Exec(context.Background(), `UPDATE merge_queue_intake SET created_at = $2 WHERE id = $1`, id, arrived); err != nil {
		t.Fatal(err)
	}
	return id
}

// Each delivery binds to its own removal (plan 4.5): two removals before one
// sweep must not collapse both deliveries onto the newer one, and a delivery
// keeps the routing of its own commit.
func TestWorkerBindsEachDeliveryToItsOwnRemoval(t *testing.T) {
	for _, routes := range [][2]string{{"block", "warn"}, {"warn", "block"}} {
		p, ws := pool(t)
		first := pendingAt(t, p, ws, "d1", routes[0], t0.Add(5*time.Minute))
		second := pendingAt(t, p, ws, "d2", routes[1], t0.Add(time.Hour+5*time.Minute))
		src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
		w, _ := newWorker(p, src, t0.Add(2*time.Hour))
		if _, err := w.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := count(t, p, "merge_queue_intake", ws); n != 2 {
			t.Fatalf("routes %v: intake rows = %d; want 2, one per removal", routes, n)
		}
		a, b := view(t, p, first), view(t, p, second)
		if a.Node != "n1" || a.Delivery != "d1" || a.Routing != routes[0] || a.Status != "stale" {
			t.Errorf("routes %v: first delivery row = %+v; want stale, n1, d1, routing %s", routes, a, routes[0])
		}
		if b.Node != "n2" || b.Delivery != "d2" || b.Routing != routes[1] || b.Status != "processed" {
			t.Errorf("routes %v: second delivery row = %+v; want processed, n2, d2, routing %s", routes, b, routes[1])
		}
	}
}

// When two deliveries point at the same removal there is no evidence which one
// belongs to it, so neither is bound by a guess; after the grace they settle
// inconclusive and write no evidence.
func TestWorkerLeavesAmbiguousDeliveriesUnboundThenInconclusive(t *testing.T) {
	p, ws := pool(t)
	first := pendingAt(t, p, ws, "d1", "block", t0.Add(2*time.Hour))
	second := pendingAt(t, p, ws, "d2", "warn", t0.Add(2*time.Hour+time.Minute))
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(2*time.Hour+2*time.Minute))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{first, second} {
		if v := view(t, p, id); v.Status != "pending" || v.Node != "" {
			t.Errorf("inside the grace: %+v; want pending and unbound", v)
		}
	}
	w.Now = func() time.Time { return t0.Add(2*time.Hour + 10*time.Minute) }
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{first, second} {
		if v := view(t, p, id); v.Status != "inconclusive" || v.Node != "" {
			t.Errorf("after the grace: %+v; want inconclusive and unbound", v)
		}
	}
	if n := count(t, p, "merge_queue_evidence", ws); n != 0 || ev.calls != 0 {
		t.Errorf("evidence rows %d, collections %d; want none", n, ev.calls)
	}
}

// The same repository and removal in two workspaces stay independent: own row,
// own routing, own evidence (workspace isolation).
func TestWorkerKeepsTheSameRemovalOfTwoWorkspacesApart(t *testing.T) {
	p, ws1 := pool(t)
	ws2 := uuid.New()
	t.Cleanup(func() {
		for _, table := range []string{"merge_queue_evidence", "merge_queue_intake"} {
			_, _ = p.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", ws2)
		}
	})
	var recorded uuid.UUID
	inTx(t, p, func(tx pgx.Tx) {
		var err error
		if recorded, _, err = EnsureIntakeForRemoval(context.Background(), tx, input(ws1, "", "block"), "n1", t0); err != nil {
			t.Fatal(err)
		}
	})
	delivered := pendingAt(t, p, ws2, "d2", "warn", t0.Add(time.Minute))
	items := []TimelineItem{removal("n1", t0)}
	src := multiSource{
		ws1: {ws: ws1, items: items, runs: []BuildRun{failedRun(t0)}},
		ws2: {ws: ws2, items: items, runs: []BuildRun{failedRun(t0)}},
	}
	ev := &fakeEvidence{}
	w := &Worker{Pool: p, Src: src, Ev: ev, Now: func() time.Time { return t0.Add(time.Hour) }}
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, delivered); v.Status != "processed" || v.Node != "n1" || v.Routing != "warn" || v.Delivery != "d2" {
		t.Errorf("workspace 2 row = %+v; want its own processed row, n1, warn, d2", v)
	}
	if v := view(t, p, recorded); v.Status != "processed" || v.Routing != "block" || v.Delivery != "" {
		t.Errorf("workspace 1 row = %+v; want its own processed row, block, no delivery attached from workspace 2", v)
	}
	if n1, n2 := count(t, p, "merge_queue_intake", ws1), count(t, p, "merge_queue_intake", ws2); n1 != 1 || n2 != 1 {
		t.Errorf("intake rows %d/%d; want 1 each", n1, n2)
	}
	if n1, n2 := count(t, p, "merge_queue_evidence", ws1), count(t, p, "merge_queue_evidence", ws2); n1 != 1 || n2 != 1 || ev.calls != 2 {
		t.Errorf("evidence rows %d/%d, collections %d; want 1 each and 2", n1, n2, ev.calls)
	}
}

// Arrival order does not identify the event. R1's webhook arrives after R2
// exists and is swept before R2's webhook: it must not bind to R2, take R2's
// build evidence or leave with the wrong routing.
func TestWorkerDoesNotBindALateDeliveryOfAnOlderRemovalToTheNewerOne(t *testing.T) {
	p, ws := pool(t)
	late := pendingAt(t, p, ws, "d1", "block", t0.Add(time.Hour+30*time.Minute)) // reports R1, arrives after R2
	src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0), removal("n2", t0.Add(time.Hour))}, runs: []BuildRun{failedRun(t0)}}
	w, ev := newWorker(p, src, t0.Add(time.Hour+31*time.Minute))
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, late); v.Status != "pending" || v.Node != "" || v.Routing != "block" {
		t.Errorf("inside the grace: %+v; want pending, unbound, routing untouched", v)
	}
	w.Now = func() time.Time { return t0.Add(time.Hour + 40*time.Minute) }
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, late); v.Status != "inconclusive" || v.Node != "" || v.Run != nil {
		t.Errorf("after the grace: %+v; want inconclusive, unbound, no build", v)
	}
	// R2's own webhook then arrives; with R1 still unclaimed it cannot be told apart either.
	own := pendingAt(t, p, ws, "d2", "warn", t0.Add(time.Hour+45*time.Minute))
	w.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	if _, err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := view(t, p, own); v.Node != "" || v.Run != nil || v.Status == "processed" {
		t.Errorf("R2's delivery = %+v; want no binding to a guessed removal", v)
	}
	if n := count(t, p, "merge_queue_evidence", ws); n != 0 || ev.calls != 0 {
		t.Errorf("evidence rows %d, collections %d; want none", n, ev.calls)
	}
}

// A delivery that attaches to a row the record stored keeps the routing of its
// own intake commit, in both orders of the two values and when the stored row is
// already settled (plan 4.3).
func TestWorkerAttachKeepsTheRoutingOfTheDeliveryIntakeCommit(t *testing.T) {
	for _, routes := range [][2]string{{"warn", "block"}, {"block", "warn"}} { // {stored row, delivery}
		for _, settled := range []bool{false, true} {
			p, ws := pool(t)
			var stored uuid.UUID
			inTx(t, p, func(tx pgx.Tx) {
				var err error
				if stored, _, err = EnsureIntakeForRemoval(context.Background(), tx, input(ws, "", routes[0]), "n1", t0); err != nil {
					t.Fatal(err)
				}
			})
			if settled {
				if _, err := p.Exec(context.Background(), `UPDATE merge_queue_intake SET status = 'processed' WHERE id = $1`, stored); err != nil {
					t.Fatal(err)
				}
			}
			pendingAt(t, p, ws, "d1", routes[1], t0.Add(time.Minute))
			src := &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0)}, runs: []BuildRun{failedRun(t0)}}
			w, _ := newWorker(p, src, t0.Add(time.Hour))
			if _, err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := count(t, p, "merge_queue_intake", ws); n != 1 {
				t.Fatalf("routes %v settled %v: intake rows = %d; want 1", routes, settled, n)
			}
			if v := view(t, p, stored); v.Routing != routes[1] || v.Delivery != "d1" {
				t.Errorf("routes %v settled %v: row = %+v; want routing %s and delivery d1", routes, settled, v, routes[1])
			}
		}
	}
}

// failingSource fails the timeline read of every PR but one.
type failingSource struct {
	*fakeSource
	healthyPR int
	failed    map[int]int
}

func (f *failingSource) Timeline(ctx context.Context, r Repo, pr int) ([]TimelineItem, error) {
	if r.WS == f.ws && pr != f.healthyPR {
		f.failed[pr]++
		return nil, errors.New("github unavailable")
	}
	return f.fakeSource.Timeline(ctx, r, pr)
}

// Rows that fail on every sweep must not hide the rows behind them: the sweep
// reads past the first page (sweepLimit rows).
func TestWorkerSweepReachesAHealthyRowBehindPersistentFailures(t *testing.T) {
	p, ws := pool(t)
	for i := 0; i < sweepLimit; i++ {
		var id uuid.UUID
		inTx(t, p, func(tx pgx.Tx) {
			var err error
			in := input(ws, fmt.Sprintf("bad-%d", i), "warn")
			in.PR = 100 + i
			if id, _, err = InsertIntake(context.Background(), tx, in); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := p.Exec(context.Background(), `UPDATE merge_queue_intake SET created_at = $2 WHERE id = $1`, id, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	healthy := pendingAt(t, p, ws, "good", "warn", t0.Add(time.Hour)) // PR 7, behind the failing rows
	src := &failingSource{fakeSource: &fakeSource{ws: ws, items: []TimelineItem{removal("n1", t0.Add(55*time.Minute))}, runs: []BuildRun{failedRun(t0)}}, healthyPR: 7, failed: map[int]int{}}
	w := &Worker{Pool: p, Src: src, Ev: &fakeEvidence{}, Now: func() time.Time { return t0.Add(3 * time.Hour) }}
	n, err := w.Sweep(context.Background())
	if err == nil {
		t.Error("sweep error = nil; want the failing rows reported")
	}
	if v := view(t, p, healthy); n != 1 || v.Status != "processed" {
		t.Errorf("settled %d, healthy row %+v; want 1 and processed", n, v)
	}
	if len(src.failed) != sweepLimit {
		t.Errorf("failing PRs read %d; want %d", len(src.failed), sweepLimit)
	}
}
