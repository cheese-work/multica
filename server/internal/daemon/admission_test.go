package daemon

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

type alertLog struct {
	mu    sync.Mutex
	kinds []string
}

func (l *alertLog) add(kind, _ string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kinds = append(l.kinds, kind)
}

func (l *alertLog) count(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, k := range l.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// ref and refs build first-generation attempt references for tests.
func ref(taskID string) AttemptRef { return AttemptRef{TaskID: taskID, Generation: "g1"} }

func refs(taskIDs ...string) []AttemptRef {
	out := make([]AttemptRef, 0, len(taskIDs))
	for _, id := range taskIDs {
		out = append(out, ref(id))
	}
	return out
}

func newTestAdmission(budget, r int64) (*Admission, *alertLog) {
	al := &alertLog{}
	return NewAdmission(AdmissionConfig{Budget: budget, RDefault: r, Alert: al.add}, nil), al
}

// n = min(free slots, floor((budget - charged) / R)); the charge is n x R at
// claim time, before the request is sent (plan r13 3.4 item 2).
func TestAdmissionChargesNTimesRAtClaim(t *testing.T) {
	a, _ := newTestAdmission(10, 2) // room for 5
	b, ok := a.Reserve(8)
	if !ok || b.N != 5 {
		t.Fatalf("Reserve(8) = %+v ok=%v, want n=5 (budget-limited)", b, ok)
	}
	if got := a.Charged(); got != 10 {
		t.Fatalf("charged = %d, want 10 (5 x R)", got)
	}
	if _, ok := a.Reserve(3); ok {
		t.Fatalf("Reserve with no budget left must not claim")
	}
	if got := a.BatchState(b.ID); got != batchPending {
		t.Fatalf("batch state = %q, want pending", got)
	}
}

// A late WS reply after abandonment: dropped before any accept (prep or
// StartTask would follow accept); the charge was refunded once at abandonment.
func TestAdmissionLateReplyAfterAbandonmentIsDropped(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(3)
	a.Abandon(b.ID) // ambiguous reply
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged after abandon = %d, want 0 (refund n)", got)
	}
	acc, dropped := a.Accept(b.ID, refs("t1", "t2"))
	if !dropped || len(acc) != 0 {
		t.Fatalf("late reply: dropped=%v accepted=%v, want dropped and none accepted", dropped, acc)
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("late reply changed the charge to %d", got)
	}
	a.Abandon(b.ID) // a second abandon must not refund twice
	if got := a.Charged(); got != 0 {
		t.Fatalf("second abandon refunded again: charged=%d", got)
	}
	// An unknown batch is dropped too.
	if _, dropped := a.Accept("nope", refs("t9")); !dropped {
		t.Fatalf("reply for an unknown batch must be dropped")
	}
}

// A reply for a settled batch (duplicate delivery of the reply) is dropped.
func TestAdmissionSecondReplyForSettledBatchIsDropped(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(2)
	if _, dropped := a.Accept(b.ID, refs("t1", "t2")); dropped {
		t.Fatalf("first reply dropped")
	}
	acc, dropped := a.Accept(b.ID, refs("t1", "t2"))
	if !dropped || len(acc) != 0 {
		t.Fatalf("second reply: dropped=%v accepted=%v", dropped, acc)
	}
	if got := a.Charged(); got != 4 {
		t.Fatalf("charged = %d, want 4 (2 accepted tasks)", got)
	}
}

// Crash between server commit and accept: the batch was in memory only, so a
// new instance has no record of it, no charge, and a reply it never receives.
// The re-delivered task is a new charged claim and is charged once.
func TestAdmissionCrashBetweenServerCommitAndAccept(t *testing.T) {
	old, _ := newTestAdmission(10, 2)
	b, _ := old.Reserve(1) // server commits, daemon dies before Accept
	_ = b

	fresh, _ := newTestAdmission(10, 2) // restart: batches are in memory only
	if got := fresh.Charged(); got != 0 {
		t.Fatalf("restarted instance charged = %d, want 0", got)
	}
	if _, dropped := fresh.Accept(b.ID, refs("t1")); !dropped {
		t.Fatalf("a reply for a batch the new instance never sent must be dropped")
	}
	// Re-delivery with a new generation arrives as a new charged claim.
	nb, ok := fresh.Reserve(1)
	if !ok {
		t.Fatalf("re-delivery claim not allowed")
	}
	acc, dropped := fresh.Accept(nb.ID, refs("t1"))
	if dropped || len(acc) != 1 || acc[0].Disposition != dispositionBound {
		t.Fatalf("re-delivered task: dropped=%v accepted=%v", dropped, acc)
	}
	if got := fresh.Charged(); got != 2 {
		t.Fatalf("charged = %d, want exactly one R after re-delivery", got)
	}
}

// Restart rebuilds held charges from durable attempt records only.
func TestAdmissionRestartRebuildsHeldChargesFromRecords(t *testing.T) {
	al := &alertLog{}
	a := NewAdmission(AdmissionConfig{Budget: 10, RDefault: 2, Alert: al.add}, map[AttemptRef]int64{ref("old1"): 2, ref("old2"): 2})
	if got := a.Charged(); got != 4 {
		t.Fatalf("charged = %d, want 4 from two durable records", got)
	}
	if err := a.Release(ref("old1")); err != nil {
		t.Fatalf("release of a rebuilt entry: %v", err)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2", got)
	}
}

// Partial batch: n = 3, the server returns k = 1; refund n-k = 2 at the reply;
// the one accepted task keeps its charge until release.
func TestAdmissionPartialBatchRefundsUnused(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(3)
	if got := a.Charged(); got != 6 {
		t.Fatalf("charged = %d, want 6", got)
	}
	acc, dropped := a.Accept(b.ID, refs("t1"))
	if dropped || len(acc) != 1 {
		t.Fatalf("accept: dropped=%v accepted=%v", dropped, acc)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2 (n-k refunded)", got)
	}
	if got := a.BatchState(b.ID); got != batchSettled {
		t.Fatalf("batch state = %q, want settled", got)
	}
	if err := a.Release(ref("t1")); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0 after release", got)
	}
}

// An empty definitive reply (k = 0) refunds the whole charge.
func TestAdmissionEmptyReplyRefundsAll(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(4)
	if _, dropped := a.Accept(b.ID, nil); dropped {
		t.Fatalf("empty reply dropped")
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0", got)
	}
}

// A server that returns more tasks than were requested cannot push the
// charge past what was reserved: extras are not accepted and an alert fires.
func TestAdmissionOverdeliveryIsCappedAtN(t *testing.T) {
	a, al := newTestAdmission(10, 2)
	b, _ := a.Reserve(1)
	acc, _ := a.Accept(b.ID, refs("t1", "t2", "t3"))
	if len(acc) != 1 {
		t.Fatalf("accepted %d tasks, want 1 (n)", len(acc))
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2", got)
	}
	if al.count("claim_overdelivery") != 1 {
		t.Fatalf("overdelivery alerts = %d, want 1", al.count("claim_overdelivery"))
	}
}

// Last-budget race: a claim reservation and a script task both want the last
// unit of budget. Exactly one wins; charged never exceeds the budget.
func TestAdmissionLastBudgetRaceWithScriptTask(t *testing.T) {
	for i := 0; i < 500; i++ {
		a, _ := newTestAdmission(2, 2) // one R of room
		var wg sync.WaitGroup
		var claimWon, scriptWon bool
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, claimWon = a.Reserve(1)
		}()
		go func() {
			defer wg.Done()
			<-start
			scriptWon = a.Charge(ref("script-1")) == nil
		}()
		close(start)
		wg.Wait()
		if claimWon == scriptWon {
			t.Fatalf("iter %d: claimWon=%v scriptWon=%v, want exactly one winner", i, claimWon, scriptWon)
		}
		if got := a.Charged(); got != 2 {
			t.Fatalf("iter %d: charged = %d, want 2 (budget)", i, got)
		}
	}
}

func TestAdmissionChargeRefusedWhenBudgetExhausted(t *testing.T) {
	a, _ := newTestAdmission(2, 2)
	if err := a.Charge(ref("s1")); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	if err := a.Charge(ref("s2")); !errors.Is(err, errBudgetExhausted) {
		t.Fatalf("second charge err = %v, want errBudgetExhausted", err)
	}
}

// Cancel in claim: the caller's context ends while the claim is in flight, so
// the outcome is ambiguous; the batch is abandoned and the charge is refunded
// once, even if the cancel path and the error path both call Abandon.
func TestAdmissionCancelInClaimRefundsOnce(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(2)
	a.Abandon(b.ID)
	a.Abandon(b.ID)
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0", got)
	}
	// A third party's charge is untouched by the double abandon.
	if _, ok := a.Reserve(1); !ok {
		t.Fatalf("budget was lost to an over-refund or leak")
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2", got)
	}
}

// Cancel in launch: the task was accepted (bound) and is cancelled before the
// fork; the cancel path and the terminal path both release; the charge is
// returned once and the second release is refused and alerted.
func TestAdmissionCancelInLaunchReleasesOnce(t *testing.T) {
	a, al := newTestAdmission(10, 2)
	b, _ := a.Reserve(1)
	if _, dropped := a.Accept(b.ID, refs("t1")); dropped {
		t.Fatalf("dropped")
	}
	if err := a.Release(ref("t1")); err != nil { // cancel before fork
		t.Fatalf("release: %v", err)
	}
	if err := a.Release(ref("t1")); !errors.Is(err, errDoubleRelease) { // terminal path
		t.Fatalf("second release err = %v, want errDoubleRelease", err)
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0", got)
	}
	if al.count("double_release") != 1 {
		t.Fatalf("double_release alerts = %d, want 1", al.count("double_release"))
	}
}

// Release of a task that was never charged is also a refused release: it must
// not drive the charge negative.
func TestAdmissionReleaseUnknownIsRefused(t *testing.T) {
	a, al := newTestAdmission(10, 2)
	if err := a.Release(ref("ghost")); !errors.Is(err, errDoubleRelease) {
		t.Fatalf("err = %v, want errDoubleRelease", err)
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0", got)
	}
	if al.count("double_release") != 1 {
		t.Fatalf("alerts = %d", al.count("double_release"))
	}
}

// Hold latched: no claim. A pending batch marked held, or any reply while the
// hold is latched, is accepted as handing_back and never bound (3.4 item 3).
func TestAdmissionHoldRoutesAcceptToHandingBack(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b, _ := a.Reserve(2)
	a.MarkHeld()
	if got := a.BatchState(b.ID); got != batchPendingHeld {
		t.Fatalf("batch state = %q, want pending_held", got)
	}
	acc, dropped := a.Accept(b.ID, refs("t1"))
	if dropped || len(acc) != 1 || acc[0].Disposition != dispositionHandingBack {
		t.Fatalf("accept under hold: dropped=%v accepted=%v", dropped, acc)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2 (n-k refunded, 1 held)", got)
	}
	a.LatchHold(true)
	if _, ok := a.Reserve(1); ok {
		t.Fatalf("Reserve while hold latched must not claim")
	}
	a.LatchHold(false)
	b2, ok := a.Reserve(1)
	if !ok {
		t.Fatalf("reserve after hold release")
	}
	a.LatchHold(true)
	acc, _ = a.Accept(b2.ID, refs("t2")) // reply with hold latched, batch still pending
	if len(acc) != 1 || acc[0].Disposition != dispositionHandingBack {
		t.Fatalf("reply while hold latched: %v", acc)
	}
}

// Concurrent soak: random reserve/accept/abandon/release interleavings never
// leave the ledger inconsistent: at quiescence the charge is zero and there
// are zero double_release alerts.
func TestAdmissionConcurrentSoakQuiescesAtZero(t *testing.T) {
	a, al := newTestAdmission(40, 2)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				b, ok := a.Reserve(3)
				if !ok {
					continue
				}
				switch (w + i) % 3 {
				case 0:
					a.Abandon(b.ID)
					a.Accept(b.ID, refs("late")) // late reply
				case 1:
					a.Accept(b.ID, nil)
				default:
					ids := refs(fmt.Sprintf("w%d-%d-a", w, i), fmt.Sprintf("w%d-%d-b", w, i))
					acc, _ := a.Accept(b.ID, ids)
					for _, t := range acc {
						_ = a.Release(t.Ref)
					}
				}
				if c := a.Charged(); c > 40 {
					panic(fmt.Sprintf("charged %d over budget", c))
				}
			}
		}(w)
	}
	wg.Wait()
	if got := a.Charged(); got != 0 {
		t.Fatalf("quiescent charge = %d, want 0", got)
	}
	if al.count("double_release") != 0 {
		t.Fatalf("double_release alerts = %d", al.count("double_release"))
	}
}

// Settled and abandoned claims must not pile up (empty polls settle a batch
// every few seconds, forever), while a late or duplicate reply for any of them
// is still dropped.
func TestAdmissionBatchStorageIsBounded(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	var firstSettled, firstAbandoned string
	for i := 0; i < 5000; i++ {
		b, ok := a.Reserve(1)
		if !ok {
			t.Fatalf("iter %d: reserve refused; charge leaked (charged=%d)", i, a.Charged())
		}
		if i%2 == 0 {
			a.Accept(b.ID, nil) // empty poll
			if firstSettled == "" {
				firstSettled = b.ID
			}
		} else {
			a.Abandon(b.ID)
			if firstAbandoned == "" {
				firstAbandoned = b.ID
			}
		}
	}
	if got := a.retainedBatches(); got > 512 {
		t.Fatalf("retained %d batches after 5000 polls, want a bounded number", got)
	}
	for _, id := range []string{firstSettled, firstAbandoned} {
		if acc, dropped := a.Accept(id, refs("late")); !dropped || len(acc) != 0 {
			t.Fatalf("late reply for old batch %s: dropped=%v accepted=%v", id, dropped, acc)
		}
	}
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged = %d, want 0", got)
	}
}

// A re-delivery has a new claim generation. It must get its own charge: it
// cannot reuse an earlier attempt's unreconciled charge, and releasing it must
// not free that earlier charge.
func TestAdmissionRedeliveryHasItsOwnCharge(t *testing.T) {
	a, al := newTestAdmission(10, 2)
	first := AttemptRef{TaskID: "t1", Generation: "gen-1"}
	again := AttemptRef{TaskID: "t1", Generation: "gen-2"}

	b1, _ := a.Reserve(1)
	acc, _ := a.Accept(b1.ID, []AttemptRef{first})
	if len(acc) != 1 || !acc[0].Fresh {
		t.Fatalf("first delivery: %v", acc)
	}
	b2, _ := a.Reserve(1)
	acc, _ = a.Accept(b2.ID, []AttemptRef{again}) // earlier attempt not torn down yet
	if len(acc) != 1 || !acc[0].Fresh {
		t.Fatalf("re-delivery must be a fresh charge: %v", acc)
	}
	if got := a.Charged(); got != 4 {
		t.Fatalf("charged = %d, want 4 (one R per attempt)", got)
	}
	if err := a.Release(again); err != nil {
		t.Fatalf("release re-delivery: %v", err)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2: the earlier attempt keeps its charge", got)
	}
	if err := a.Release(first); err != nil {
		t.Fatalf("release first attempt: %v", err)
	}
	if got := a.Charged(); got != 0 || al.count("double_release") != 0 {
		t.Fatalf("charged = %d, double_release = %d", got, al.count("double_release"))
	}
}

// The same attempt (same generation) delivered twice is one charge.
func TestAdmissionSameAttemptDeliveredTwiceChargesOnce(t *testing.T) {
	a, _ := newTestAdmission(10, 2)
	b1, _ := a.Reserve(1)
	a.Accept(b1.ID, refs("t1"))
	b2, _ := a.Reserve(1)
	acc, _ := a.Accept(b2.ID, refs("t1"))
	if len(acc) != 1 || acc[0].Fresh {
		t.Fatalf("second delivery of one attempt: %v, want not fresh", acc)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2", got)
	}
}
