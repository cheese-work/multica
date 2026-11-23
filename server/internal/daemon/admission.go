package daemon

import (
	"errors"
	"fmt"
	"sync"
)

// Admission is the daemon's admission mutex and charge ledger (CHE-7168 plan
// r13 section 3.4, step 7 "S3a").
//
// What the charge protects is X99 memory. A task uses host memory only after
// the daemon accepts its payload, preps it and forks it, so the charge covers
// accepted payloads, not server rows:
//
//  1. Reserve charges n x RDefault before the claim is sent; the batch is
//     "pending". The network call itself happens outside the mutex.
//  2. A definitive reply with k tasks (Accept) refunds n-k, settles the batch
//     and writes one ledger entry per accepted task.
//  3. An ambiguous reply (error, timeout, cancel) calls Abandon: the batch is
//     "abandoned" and the whole charge is refunded. A reply that arrives later
//     for an abandoned or settled batch is dropped before any prep or
//     StartTask. Rows the server committed for it can start only through a
//     later delivery with a new generation, which is a new charged claim.
//  4. Batches live in memory only. After a restart held charges are rebuilt
//     from durable attempt records (NewAdmission's held argument).
//  5. Every ledger entry is released once (Release); a second release is
//     refused, logged and alerted as double_release.
//
// A nil *Admission admits everything and charges nothing: that is the flag-off
// path, byte-for-byte today's behaviour.
//
// Hold behaviour (latch, hand-back, recovery) is step 10a. This type only
// carries the two switches the charge transaction reads: LatchHold and
// MarkHeld (batch state "pending_held").
type Admission struct {
	mu       sync.Mutex
	budget   int64
	rDefault int64
	charged  int64
	held     bool // hold latched: no claim, accepts go to handing_back
	seq      uint64
	batches  map[string]*admissionBatch
	ledger   map[string]*ledgerEntry
	alert    func(kind, detail string)
}

type batchState string

const (
	batchPending     batchState = "pending"
	batchPendingHeld batchState = "pending_held"
	batchAbandoned   batchState = "abandoned"
	batchSettled     batchState = "settled"
)

type attemptDisposition string

const (
	// dispositionBound: the attempt may prep and start.
	dispositionBound attemptDisposition = "bound"
	// dispositionHandingBack: accepted under hold; it never passes through
	// bound, so no prep permit, leaf, fork or StartTask can follow.
	dispositionHandingBack attemptDisposition = "handing_back"
)

type ledgerState string

const (
	ledgerHeld     ledgerState = "held"
	ledgerReleased ledgerState = "released"
)

var (
	errBudgetExhausted = errors.New("admission: budget exhausted")
	errDoubleRelease   = errors.New("admission: double release")
)

type admissionBatch struct {
	id     string
	n      int
	charge int64
	state  batchState
}

type ledgerEntry struct {
	amount int64
	state  ledgerState
}

// AdmissionConfig sizes the charge. Budget is B_runs and RDefault is
// R_default, in the same unit (bytes). Alert receives containment alerts
// (kind is a stable slug such as "double_release"); it may be nil and is called
// with the admission mutex held, so it must not block or call back in.
type AdmissionConfig struct {
	Budget   int64
	RDefault int64
	Alert    func(kind, detail string)
}

// AdmissionBatch is the handle Reserve returns for one claim request.
type AdmissionBatch struct {
	ID string
	N  int
}

// AdmittedTask is one accepted task and what the caller must do with it.
type AdmittedTask struct {
	TaskID      string
	Disposition attemptDisposition
}

// NewAdmission builds the mutex and ledger. held maps task ID to charge for
// every earlier-instance attempt that is still charged (rebuilt from durable
// attempt records by the caller); nothing else survives a restart.
func NewAdmission(cfg AdmissionConfig, held map[string]int64) *Admission {
	a := &Admission{
		budget:   cfg.Budget,
		rDefault: cfg.RDefault,
		batches:  map[string]*admissionBatch{},
		ledger:   map[string]*ledgerEntry{},
		alert:    cfg.Alert,
	}
	for id, amount := range held {
		a.ledger[id] = &ledgerEntry{amount: amount, state: ledgerHeld}
		a.charged += amount
	}
	return a
}

func (a *Admission) raise(kind, detail string) {
	if a.alert != nil {
		a.alert(kind, detail)
	}
}

// Reserve decides n = min(freeSlots, floor((budget - charged) / RDefault)),
// charges n x RDefault and opens a pending batch. It returns false when the
// hold is latched or nothing fits, in which case no claim must be sent.
func (a *Admission) Reserve(freeSlots int) (*AdmissionBatch, bool) {
	if a == nil {
		return &AdmissionBatch{N: freeSlots}, freeSlots > 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held || freeSlots <= 0 || a.rDefault <= 0 {
		return nil, false
	}
	room := (a.budget - a.charged) / a.rDefault
	if room <= 0 {
		return nil, false
	}
	n := int64(freeSlots)
	if room < n {
		n = room
	}
	a.seq++
	b := &admissionBatch{
		id:     fmt.Sprintf("batch-%d", a.seq),
		n:      int(n),
		charge: n * a.rDefault,
		state:  batchPending,
	}
	a.batches[b.id] = b
	a.charged += b.charge
	return &AdmissionBatch{ID: b.id, N: b.n}, true
}

// Accept handles a definitive reply carrying taskIDs. dropped is true when the
// batch is unknown, abandoned or already settled: the caller must not prep or
// start anything from that reply. Otherwise n-k is refunded, the batch is
// settled and each accepted task gets a ledger entry; the disposition is
// handing_back when the hold is latched or the batch was marked pending_held.
func (a *Admission) Accept(batchID string, taskIDs []string) (accepted []AdmittedTask, dropped bool) {
	if a == nil {
		out := make([]AdmittedTask, 0, len(taskIDs))
		for _, id := range taskIDs {
			out = append(out, AdmittedTask{TaskID: id, Disposition: dispositionBound})
		}
		return out, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.batches[batchID]
	if !ok || (b.state != batchPending && b.state != batchPendingHeld) {
		return nil, true
	}
	disposition := dispositionBound
	if a.held || b.state == batchPendingHeld {
		disposition = dispositionHandingBack
	}
	k := len(taskIDs)
	if k > b.n {
		a.raise("claim_overdelivery", fmt.Sprintf("batch %s asked %d, server returned %d; extras not accepted", b.id, b.n, k))
		taskIDs = taskIDs[:b.n]
		k = b.n
	}
	a.charged -= b.charge // refund the whole reservation ...
	b.state = batchSettled
	for _, id := range taskIDs {
		if e := a.ledger[id]; e != nil && e.state == ledgerHeld {
			// Re-delivery of a task this instance still holds: its existing
			// entry is the one charge.
			accepted = append(accepted, AdmittedTask{TaskID: id, Disposition: disposition})
			continue
		}
		a.ledger[id] = &ledgerEntry{amount: a.rDefault, state: ledgerHeld}
		a.charged += a.rDefault // ... then charge the k accepted tasks.
		accepted = append(accepted, AdmittedTask{TaskID: id, Disposition: disposition})
	}
	return accepted, false
}

// Abandon handles an ambiguous reply (transport error, timeout, cancel while
// the claim was in flight): the batch is abandoned and n is refunded, once.
func (a *Admission) Abandon(batchID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.batches[batchID]
	if !ok || (b.state != batchPending && b.state != batchPendingHeld) {
		return
	}
	b.state = batchAbandoned
	a.charged -= b.charge
}

// MarkHeld is called by the latch (step 10a): every pending batch becomes
// pending_held, so its reply is accepted as handing_back whatever happens to
// the hold afterwards.
func (a *Admission) MarkHeld() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range a.batches {
		if b.state == batchPending {
			b.state = batchPendingHeld
		}
	}
}

// LatchHold sets or clears the hold flag read by Reserve and Accept.
func (a *Admission) LatchHold(on bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held = on
}

// Charge takes one RDefault for a task that did not come through a batch
// (for example a script task started locally). It competes with Reserve for
// the same budget under the same mutex.
func (a *Admission) Charge(taskID string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[taskID]; ok && e.state == ledgerHeld {
		return nil // already charged: one charge per task
	}
	if a.budget-a.charged < a.rDefault {
		return errBudgetExhausted
	}
	a.ledger[taskID] = &ledgerEntry{amount: a.rDefault, state: ledgerHeld}
	a.charged += a.rDefault
	return nil
}

// Release returns a task's charge once. Releasing an entry that is already
// released, or was never charged, is refused with errDoubleRelease and alerted.
func (a *Admission) Release(taskID string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[taskID]
	if !ok || e.state != ledgerHeld {
		a.raise("double_release", "task "+taskID)
		return errDoubleRelease
	}
	e.state = ledgerReleased
	a.charged -= e.amount
	return nil
}

// Charged is the total currently charged (held entries plus pending batches).
func (a *Admission) Charged() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.charged
}

// BatchState reports a batch's state, or "" for an unknown batch.
func (a *Admission) BatchState(id string) batchState {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.batches[id]; ok {
		return b.state
	}
	return ""
}
