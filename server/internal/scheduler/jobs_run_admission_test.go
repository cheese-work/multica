package scheduler

import (
	"context"
	"testing"
)

type fakeAdmission struct{ expire, reconcile int }

func (f *fakeAdmission) ExpireDueReservations(context.Context) (int, error) {
	f.expire++
	return 2, nil
}

func (f *fakeAdmission) ReconcileAdmission(context.Context) error {
	f.reconcile++
	return nil
}

// The tick delivers due lease-expiry events only; the full admission pass is a
// restart-time reconcile, not a periodic sweep (CHE-5213).
func TestRunAdmissionExpiryJobDeliversOnlyDueExpiryEvents(t *testing.T) {
	f := &fakeAdmission{}
	res, err := RunAdmissionExpiryJob(f).Handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatal(err)
	}
	if f.expire != 1 || f.reconcile != 0 {
		t.Fatalf("tick ran expire=%d reconcile=%d, want expire=1 reconcile=0", f.expire, f.reconcile)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("RowsAffected = %d, want 2", res.RowsAffected)
	}
}
