package pushnotify_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/pushnotify"
)

// CHE-1795: a claim could outlive
// its lease while queued behind a same-device send, and nothing tied the
// outcome writes to the claim that made them. Locks are held and released by
// the test, so no case sleeps through a production lease.

// lockDevice holds the device row the way an in-flight send does.
func lockDevice(t *testing.T, w *world) (release func()) {
	t.Helper()
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM push_subscription WHERE id = $1 FOR NO KEY UPDATE`, w.subID); err != nil {
		t.Fatalf("lock device: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Commit(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// waitBlocked waits until n backends are queued on a lock.
func waitBlocked(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := testPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatalf("read activity: %v", err)
		}
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d backends queued on the device lock", n)
}

func goPass(w *world, sender pushnotify.Sender) <-chan error {
	done := make(chan error, 1)
	worker := pushnotify.NewWorker(w.q, testPool, sender)
	go func() { done <- worker.ProcessOnce(context.Background()) }()
	return done
}

func (w *world) expireLease() {
	w.t.Helper()
	w.fx.Exec(w.t, `UPDATE push_delivery SET claimed_until = now() - interval '1 second' WHERE subscription_id = $1`, w.subID)
}

func TestStaleClaimantNeverSendsAfterAReclaim(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	a, c := &fakeSender{}, &fakeSender{}
	release := lockDevice(t, w)

	passA := goPass(w, a)
	waitBlocked(t, 1) // A holds the claim and queues behind the device lock
	w.expireLease()   // A's lease runs out while it waits
	passC := goPass(w, c)
	waitBlocked(t, 2) // C reclaims the row and queues too
	release()
	for _, pass := range []<-chan error{passA, passC} {
		if err := <-pass; err != nil {
			t.Fatalf("process pass: %v", err)
		}
	}

	if got := a.count() + c.count(); got != 1 {
		t.Fatalf("sends = %d (stale claimant %d, current claimant %d), want one from the current claim only", got, a.count(), c.count())
	}
	if a.count() != 0 {
		t.Fatal("the claimant whose lease expired still sent")
	}
}

func TestStaleOutcomeCannotTouchTheNewerClaim(t *testing.T) {
	for _, status := range []int{201, 503, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			w := newWorld(t)
			w.setPush("dm", true)
			w.dmReply()
			w.due()
			gateA, gateC := newGateSender(status), newGateSender(201)
			var closeA, closeC sync.Once
			t.Cleanup(func() { closeA.Do(func() { close(gateA.release) }); closeC.Do(func() { close(gateC.release) }) })

			passA := goPass(w, gateA)
			select {
			case <-gateA.entered: // A holds the device lock inside its send
			case <-time.After(10 * time.Second):
				t.Fatal("the first claimant never reached the send")
			}
			w.expireLease()
			passC := goPass(w, gateC)
			waitBlocked(t, 1) // C reclaims the row and queues behind A's send
			closeA.Do(func() { close(gateA.release) })
			if err := <-passA; err != nil {
				t.Fatalf("process pass: %v", err)
			}

			cInSend, cDone := false, false
			select {
			case <-gateC.entered:
				cInSend = true
			case err := <-passC:
				cDone = true
				if err != nil {
					t.Fatalf("process pass: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the newer claimant neither sent nor finished")
			}

			var sentSet bool
			var claimedUntil *time.Time
			if err := testPool.QueryRow(context.Background(),
				`SELECT sent_at IS NOT NULL, claimed_until FROM push_delivery WHERE subscription_id = $1`, w.subID).Scan(&sentSet, &claimedUntil); err != nil {
				t.Errorf("the newer claim's delivery row is gone after the stale outcome: %v", err)
			} else {
				if sentSet {
					t.Error("the stale outcome marked the newer claim as sent")
				}
				if claimedUntil == nil || time.Until(*claimedUntil) < time.Minute {
					t.Errorf("the stale outcome cleared or moved the newer claim's lease: %v", claimedUntil)
				}
			}
			if n := w.fx.Count(t, `SELECT count(*) FROM push_subscription WHERE id = $1`, w.subID); n != 1 {
				t.Error("the stale outcome removed the subscription")
			}

			closeC.Do(func() { close(gateC.release) })
			if cInSend && !cDone {
				if err := <-passC; err != nil {
					t.Fatalf("process pass: %v", err)
				}
			}
		})
	}
}

func TestContestedDeviceDefersInsteadOfQueueing(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	release := lockDevice(t, w) // another delivery's send holds the device

	pass := goPass(w, w.sender)
	select {
	case err := <-pass:
		if err != nil {
			t.Fatalf("process pass: %v", err)
		}
	case <-time.After(5 * time.Second):
		release()
		<-pass
		t.Fatal("the pass was still waiting on a contested device after 5 s: a delivery must give up the wait and reschedule")
	}
	release()

	if w.sender.count() != 0 {
		t.Fatal("sent while another send held the device")
	}
	var attempts int
	var next time.Time
	var claimed *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT attempts, next_attempt_at, claimed_until FROM push_delivery WHERE subscription_id = $1`, w.subID).Scan(&attempts, &next, &claimed); err != nil {
		t.Fatalf("a contested delivery must stay queued: %v", err)
	}
	if attempts != 0 || claimed != nil || time.Until(next) < time.Second {
		t.Fatalf("attempts=%d claimed=%v next in %v: want the attempt refunded, the lease released and a later retry", attempts, claimed, time.Until(next))
	}
}

// Ending a session is a marker row, not an update of the device row, so logout
// never waits behind a send.
func TestEndedSessionMeansNoSend(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.fx.Exec(t, `INSERT INTO push_session_end (session_id) VALUES ($1)`, "sid-"+w.endpoint)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, "sid-"+w.endpoint)
	})

	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent to a device whose registering session was ended")
	}
}
