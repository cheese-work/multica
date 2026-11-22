package browserlease

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4744 (W3-15): every lease transition writes its audit row inside the transition's
// own transaction. These tests perform the transitions and read what the audit hook saw.

type auditEntry struct {
	kind  string
	lease db.BrowserLease
}

type auditLog struct {
	mu      sync.Mutex
	entries []auditEntry
	failOn  string // kind that returns an error; "" fails nothing
}

func (a *auditLog) fn(_ context.Context, _ *db.Queries, kind string, l db.BrowserLease) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failOn == kind {
		return errors.New("audit write failed")
	}
	a.entries = append(a.entries, auditEntry{kind, l})
	return nil
}

func (a *auditLog) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.kind)
	}
	return out
}

func wantKinds(t *testing.T, a *auditLog, want ...string) {
	t.Helper()
	got := a.kinds()
	if len(got) != len(want) {
		t.Fatalf("audit kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit kinds = %v, want %v", got, want)
		}
	}
}

func TestGrantAuditsStart(t *testing.T) {
	e := newEnv(t, "x99-1")
	log := &auditLog{}
	e.svc.Audit = log.fn
	res, _ := e.mint(e.profile(ClassS0))
	wantKinds(t, log, AuditStart)
	got := log.entries[0].lease
	if got.ID != res.Lease.ID || got.State != "active" || !got.GrantedAt.Valid || got.EndedAt.Valid {
		t.Fatalf("start row lease = %s granted=%v ended=%v, want the granted lease", got.State, got.GrantedAt.Valid, got.EndedAt.Valid)
	}
}

func TestQueuedLeaseWritesNoAuditUntilGranted(t *testing.T) {
	e := newEnv(t, "x99-1")
	log := &auditLog{}
	e.svc.Audit = log.fn
	p := e.profile(ClassS0)
	a, _ := e.mint(p)
	b, _ := e.mint(p) // queued behind a
	if b.Lease.State != "queued" {
		t.Fatalf("second lease = %s, want queued", b.Lease.State)
	}
	wantKinds(t, log, AuditStart)
	if err := e.svc.Release(context.Background(), e.ws, b.Lease.TaskID, b.Lease.ID); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, log, AuditStart) // a lease that never started has nothing to end
	_ = a
}

func TestEndsAuditEndWithTheReason(t *testing.T) {
	t.Run("release", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{}
		e.svc.Audit = log.fn
		a, r := e.mint(e.profile(ClassS0))
		if err := e.svc.Release(context.Background(), e.ws, r.TaskID, a.Lease.ID); err != nil {
			t.Fatal(err)
		}
		wantKinds(t, log, AuditStart, AuditEnd)
		end := log.entries[1].lease
		if end.ID != a.Lease.ID || end.State != "ended" || end.EndReason.String != "released" || !end.EndedAt.Valid {
			t.Fatalf("end row lease = %s/%q ended=%v", end.State, end.EndReason.String, end.EndedAt.Valid)
		}
	})
	t.Run("expired at the hard limit", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{}
		e.svc.Audit = log.fn
		a, r := e.mint(e.profile(ClassS0))
		e.now = e.now.Add(MaxLeaseDuration)
		if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrLeaseEnded) {
			t.Fatalf("err = %v, want ErrLeaseEnded", err)
		}
		wantKinds(t, log, AuditStart, AuditEnd)
		if got := log.entries[1].lease.EndReason.String; got != "expired" {
			t.Fatalf("end reason = %q, want expired", got)
		}
	})
	t.Run("revalidation failed", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{}
		e.svc.Audit = log.fn
		a, r := e.mint(e.profile(ClassS0))
		e.hooks.gateErr[uuid.UUID(r.TaskID.Bytes).String()] = ErrGateDenied
		if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrGateDenied) {
			t.Fatalf("err = %v, want ErrGateDenied", err)
		}
		wantKinds(t, log, AuditStart, AuditEnd)
		if got := log.entries[1].lease.EndReason.String; got != "revalidation_failed" {
			t.Fatalf("end reason = %q, want revalidation_failed", got)
		}
	})
	t.Run("already ended writes nothing more", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{}
		e.svc.Audit = log.fn
		a, r := e.mint(e.profile(ClassS0))
		for i := 0; i < 2; i++ {
			if err := e.svc.Release(context.Background(), e.ws, r.TaskID, a.Lease.ID); err != nil {
				t.Fatal(err)
			}
		}
		wantKinds(t, log, AuditStart, AuditEnd)
	})
}

func TestAuditFailureRollsTheTransitionBack(t *testing.T) {
	t.Run("grant", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{failOn: AuditStart}
		e.svc.Audit = log.fn
		if _, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0))); err == nil {
			t.Fatal("mint succeeded although its audit row could not be written")
		}
		var active int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM browser_lease WHERE workspace_id = $1 AND state = 'active'`, e.ws).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 0 {
			t.Fatalf("%d active leases after a failed audit, want 0", active)
		}
	})
	t.Run("end", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		log := &auditLog{failOn: AuditEnd}
		e.svc.Audit = log.fn
		a, r := e.mint(e.profile(ClassS0))
		if err := e.svc.Release(context.Background(), e.ws, r.TaskID, a.Lease.ID); err == nil {
			t.Fatal("release succeeded although its audit row could not be written")
		}
		if l := e.lease(a.Lease.ID); l.State != "active" {
			t.Fatalf("lease = %s after a failed audit, want active", l.State)
		}
	})
}

func newAuditedRevoker(t *testing.T, w *world, a *auditLog) (*Revoker, *recorder) {
	t.Helper()
	r, rec := newRevoker(t, w)
	r.Pool = w.pool
	r.Audit = a.fn
	return r, rec
}

func TestRevokeAuditsRevokeWithTheReason(t *testing.T) {
	w := newWorld(t)
	log := &auditLog{}
	r, _ := newAuditedRevoker(t, w, log)
	target := w.lease()
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, LeaseID: target.ID}, ReasonPluginDisabled); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, log, AuditRevoke)
	got := log.entries[0].lease
	if got.ID != target.ID || got.State != "ended" || got.EndReason.String != ReasonPluginDisabled || !got.EndedAt.Valid || !got.GrantedAt.Valid {
		t.Fatalf("revoke row lease = %s/%q ended=%v granted=%v", got.State, got.EndReason.String, got.EndedAt.Valid, got.GrantedAt.Valid)
	}
}

func TestRevokeOfAQueuedLeaseAuditsRevokeWithoutStartOrEnd(t *testing.T) {
	w := newWorld(t)
	log := &auditLog{}
	r, _ := newAuditedRevoker(t, w, log)
	q := w.lease(queued())
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, TaskID: q.TaskID}, ReasonTaskCancelled); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, log, AuditRevoke)
	got := log.entries[0].lease
	if got.GrantedAt.Valid || got.EndedAt.Valid || got.EndReason.String != ReasonTaskCancelled {
		t.Fatalf("queued revoke row: granted=%v ended=%v reason=%q, want no times and the revoke reason",
			got.GrantedAt.Valid, got.EndedAt.Valid, got.EndReason.String)
	}
}

func TestRevokeIdempotentWritesNoSecondAudit(t *testing.T) {
	w := newWorld(t)
	log := &auditLog{}
	r, _ := newAuditedRevoker(t, w, log)
	done := w.lease(ended())
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, LeaseID: done.ID}, ReasonRelease); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, log)
}

func TestRevokeAuditFailureRollsBackAndPublishesNothing(t *testing.T) {
	w := newWorld(t)
	log := &auditLog{failOn: AuditRevoke}
	r, rec := newAuditedRevoker(t, w, log)
	target := w.lease()
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace, LeaseID: target.ID}, ReasonRevokeAll); err == nil {
		t.Fatal("revoke succeeded although its audit row could not be written")
	}
	if row, _ := w.reload(target); row.State != "active" {
		t.Fatalf("lease = %s after a failed audit, want active", row.State)
	}
	if len(rec.got) != 0 {
		t.Fatalf("published %d events for a rolled-back revoke", len(rec.got))
	}
}

func TestRevokerWithAuditNeedsAPool(t *testing.T) {
	w := newWorld(t)
	r, _ := newRevoker(t, w)
	r.Audit = (&auditLog{}).fn // no Pool
	w.lease()
	if _, err := r.Revoke(context.Background(), Filter{WorkspaceID: w.workspace}, ReasonRevokeAll); err == nil {
		t.Fatal("a Revoker with Audit and no Pool must refuse rather than skip the audit row")
	}
}

// CHE-4742 and CHE-4782 added revoke by decision (withdrawn answer, expired decision). Each
// lease it ends or drops must write one revoke row that names the decision and the reason.
func TestRevokeDecisionAuditsEachLeaseWithDecisionAndReason(t *testing.T) {
	for _, reason := range []string{ReasonDecisionWithdrawn, ReasonDecisionExpired} {
		t.Run(reason, func(t *testing.T) {
			w := newWorld(t)
			log := &auditLog{}
			r, _ := newAuditedRevoker(t, w, log)
			decision := newID()
			w.lease(withDecision(decision))
			w.lease(withDecision(decision), queued())
			w.lease(withDecision(newID())) // another decision: stays live, no row
			w.lease()                      // no decision: stays live, no row

			if _, err := r.RevokeDecision(context.Background(), w.workspace, decision, reason); err != nil {
				t.Fatal(err)
			}
			wantKinds(t, log, AuditRevoke, AuditRevoke)
			for _, e := range log.entries {
				if e.lease.DecisionID != decision || e.lease.EndReason.String != reason {
					t.Fatalf("revoke row decision=%v reason=%q, want %v %q", e.lease.DecisionID, e.lease.EndReason.String, decision, reason)
				}
			}
		})
	}
}

func TestRevokeDecisionAuditFailureLeavesLeasesLive(t *testing.T) {
	w := newWorld(t)
	log := &auditLog{failOn: AuditRevoke}
	r, rec := newAuditedRevoker(t, w, log)
	decision := newID()
	active := w.lease(withDecision(decision))
	if _, err := r.RevokeDecision(context.Background(), w.workspace, decision, ReasonDecisionWithdrawn); err == nil {
		t.Fatal("audit failure was swallowed")
	}
	if row, _ := w.reload(active); row.State != "active" {
		t.Fatalf("lease state = %q, want active after the rolled-back revoke", row.State)
	}
	if len(rec.got) != 0 {
		t.Fatalf("published %d events for a rolled-back revoke", len(rec.got))
	}
}
