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
	batches  map[string]*admissionBatch // pending and pending_held only
	done     map[string]batchState      // bounded tombstones of settled and abandoned batches
	doneFIFO []string
	relFIFO  []AttemptRef // released ledger entries, oldest first, bounded
	ledger   map[AttemptRef]*ledgerEntry
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

// Retention caps. A reply for a batch older than the tombstone window is
// dropped as an unknown batch, which is the same outcome as a tombstone hit.
const (
	maxBatchTombstones = 256
	maxReleasedEntries = 1024
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

// AttemptRef names one delivery of a task: the task ID plus the claim
// generation the server stamped on it. A re-delivery has a new generation, so
// it gets its own ledger entry and cannot spend or release an earlier
// attempt's charge.
type AttemptRef struct {
	TaskID     string
	Generation string
}

// AdmittedTask is one accepted attempt and what the caller must do with it.
// Fresh is false when the same attempt was already held (the same generation
// delivered twice): its existing charge is the only one.
type AdmittedTask struct {
	Ref         AttemptRef
	Disposition attemptDisposition
	Fresh       bool
}

// NewAdmission builds the mutex and ledger. held maps attempt to charge for
// every earlier-instance attempt that is still charged (rebuilt from durable
// attempt records by the caller); nothing else survives a restart.
func NewAdmission(cfg AdmissionConfig, held map[AttemptRef]int64) *Admission {
	a := &Admission{
		budget:   cfg.Budget,
		rDefault: cfg.RDefault,
		batches:  map[string]*admissionBatch{},
		done:     map[string]batchState{},
		ledger:   map[AttemptRef]*ledgerEntry{},
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
func (a *Admission) Accept(batchID string, refs []AttemptRef) (accepted []AdmittedTask, dropped bool) {
	if a == nil {
		out := make([]AdmittedTask, 0, len(refs))
		for _, r := range refs {
			out = append(out, AdmittedTask{Ref: r, Disposition: dispositionBound, Fresh: true})
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
	if k := len(refs); k > b.n {
		a.raise("claim_overdelivery", fmt.Sprintf("batch %s asked %d, server returned %d; extras not accepted", b.id, b.n, k))
		refs = refs[:b.n]
	}
	a.charged -= b.charge // refund the whole reservation ...
	a.retire(b, batchSettled)
	for _, r := range refs {
		if e := a.ledger[r]; e != nil && e.state == ledgerHeld {
			// The same attempt delivered twice: its existing entry is the one charge.
			accepted = append(accepted, AdmittedTask{Ref: r, Disposition: disposition})
			continue
		}
		a.ledger[r] = &ledgerEntry{amount: a.rDefault, state: ledgerHeld}
		a.charged += a.rDefault // ... then charge the accepted attempts.
		accepted = append(accepted, AdmittedTask{Ref: r, Disposition: disposition, Fresh: true})
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
	a.charged -= b.charge
	a.retire(b, batchAbandoned)
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
func (a *Admission) Charge(ref AttemptRef) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[ref]; ok && e.state == ledgerHeld {
		return nil // already charged: one charge per attempt
	}
	if a.budget-a.charged < a.rDefault {
		return errBudgetExhausted
	}
	a.ledger[ref] = &ledgerEntry{amount: a.rDefault, state: ledgerHeld}
	a.charged += a.rDefault
	return nil
}

// Release returns an attempt's charge once. Releasing an entry that is already
// released, or was never charged, is refused with errDoubleRelease and alerted.
func (a *Admission) Release(ref AttemptRef) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[ref]
	if !ok || e.state != ledgerHeld {
		a.raise("double_release", "task "+ref.TaskID+" generation "+ref.Generation)
		return errDoubleRelease
	}
	// Released entries are kept (bounded) so a second release is recognised
	// and alerted instead of looking like a never-charged attempt.
	e.state = ledgerReleased
	a.charged -= e.amount
	a.noteReleased(ref)
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

// retire moves a finished batch out of the live map into the bounded
// tombstone window, so storage stays flat while a repeated poll runs forever.
func (a *Admission) retire(b *admissionBatch, state batchState) {
	delete(a.batches, b.id)
	a.done[b.id] = state
	a.doneFIFO = append(a.doneFIFO, b.id)
	if len(a.doneFIFO) > maxBatchTombstones {
		delete(a.done, a.doneFIFO[0])
		a.doneFIFO = a.doneFIFO[1:]
	}
}

// noteReleased bounds the released ledger entries the same way.
func (a *Admission) noteReleased(ref AttemptRef) {
	a.relFIFO = append(a.relFIFO, ref)
	if len(a.relFIFO) > maxReleasedEntries {
		delete(a.ledger, a.relFIFO[0])
		a.relFIFO = a.relFIFO[1:]
	}
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
	return a.done[id]
}

// retainedBatches is how many batch records (live plus tombstones) the ledger still holds.
func (a *Admission) retainedBatches() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.batches) + len(a.done)
}
