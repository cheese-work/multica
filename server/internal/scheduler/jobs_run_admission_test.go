package scheduler

import (
	"context"
	"testing"
)

type fakeAdmission struct{ expire, reconcile, release int }

func (f *fakeAdmission) ExpireDueReservations(context.Context) (int, error) {
	f.expire++
	return 2, nil
}

func (f *fakeAdmission) ReleaseObligations(context.Context) (int, error) {
	f.release++
	return 3, nil
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
	if res.RowsAffected != 5 { // 2 expired + 3 released by the fake
		t.Fatalf("RowsAffected = %d, want 5", res.RowsAffected)
	}
}

// The tick also delivers the releases owed by ended runs (CHE-5265).
func TestRunAdmissionExpiryJobDeliversOwedReleases(t *testing.T) {
	f := &fakeAdmission{}
	res, err := RunAdmissionExpiryJob(f).Handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatal(err)
	}
	if f.release != 1 {
		t.Fatalf("tick ran ReleaseObligations %d times, want 1", f.release)
	}
	if res.RowsAffected != 5 {
		t.Fatalf("RowsAffected = %d, want 5 (2 expired + 3 released)", res.RowsAffected)
	}
}
