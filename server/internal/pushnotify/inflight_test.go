package pushnotify_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/pushnotify"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1795: the ownership check and
// the send were two steps, so a device could change hands in between. These
// tests hold a send open at the push-service boundary and move the device while
// it is in flight.

// gateSender blocks inside Send until released, so a test controls exactly when
// the push leaves the process.
type gateSender struct {
	entered chan struct{}
	release chan struct{}
	status  int
	sends   atomic.Int32
}

func newGateSender(status int) *gateSender {
	return &gateSender{entered: make(chan struct{}, 4), release: make(chan struct{}), status: status}
}

func (g *gateSender) Send(ctx context.Context, _ pushnotify.Subscription, _ []byte) (int, error) {
	g.sends.Add(1)
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return g.status, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// inFlightTransfer starts a pass whose send is held open, then registers the
// same endpoint for another user on a second connection. It returns once the
// transfer either finished or has been seen waiting, and reports which.
func inFlightTransfer(t *testing.T, w *world, gate *gateSender) (newOwner string, transferredEarly bool, finish func()) {
	t.Helper()
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	newOwner = w.fx.User(t, "Push New Owner", fmt.Sprintf("push-new-owner-%d@multica.ai", time.Now().UnixNano()))

	worker := pushnotify.NewWorker(w.q, testPool, gate)
	pass := make(chan error, 1)
	go func() { pass <- worker.ProcessOnce(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never reached the send")
	}

	transfer := make(chan error, 1)
	go func() {
		_, err := w.q.UpsertPushSubscription(context.Background(), db.UpsertPushSubscriptionParams{
			ID: dbid.NewV7(), UserID: w.uuid(newOwner), Endpoint: w.endpoint, P256dh: "p", Auth: "a",
			SessionID: "sid-new-owner", SessionValidUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		transfer <- err
	}()
	select {
	case err := <-transfer:
		if err != nil {
			t.Fatalf("transfer: %v", err)
		}
		transferredEarly = true
	case <-time.After(500 * time.Millisecond):
	}
	return newOwner, transferredEarly, func() {
		t.Helper()
		close(gate.release)
		if err := <-pass; err != nil {
			t.Fatalf("process pass: %v", err)
		}
		if !transferredEarly {
			select {
			case err := <-transfer:
				if err != nil {
					t.Fatalf("transfer: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the transfer never finished after the send ended")
			}
		}
	}
}

func TestDeviceCannotChangeHandsWhileAPushIsInFlight(t *testing.T) {
	w := newWorld(t)
	gate := newGateSender(201)

	newOwner, early, finish := inFlightTransfer(t, w, gate)
	defer func() {
		if early {
			finish()
		}
	}()
	if early {
		t.Fatal("the device was handed to another account while the previous owner's push was still being sent")
	}
	finish()

	var owner string
	if err := testPool.QueryRow(context.Background(), `SELECT user_id::text FROM push_subscription WHERE endpoint = $1`, w.endpoint).Scan(&owner); err != nil || owner != newOwner {
		t.Fatalf("owner = %q (err %v), want the new account once the send ended", owner, err)
	}
	if got := gate.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want the old owner's single push", got)
	}
}

func TestStaleGoneDoesNotRemoveTheNewOwnersSubscription(t *testing.T) {
	w := newWorld(t)
	gate := newGateSender(410)

	newOwner, _, finish := inFlightTransfer(t, w, gate)
	finish()

	var owner string
	err := testPool.QueryRow(context.Background(), `SELECT user_id::text FROM push_subscription WHERE endpoint = $1`, w.endpoint).Scan(&owner)
	if err != nil {
		t.Fatalf("the new owner's subscription is gone after a stale 410 for the old owner's send: %v", err)
	}
	if owner != newOwner {
		t.Fatalf("owner = %q, want %q", owner, newOwner)
	}
}

func TestTransferBeforeTheSendStartsSendsNothing(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	other := w.fx.User(t, "Push Early Owner", fmt.Sprintf("push-early-owner-%d@multica.ai", time.Now().UnixNano()))
	if _, err := w.q.UpsertPushSubscription(context.Background(), db.UpsertPushSubscriptionParams{
		ID: dbid.NewV7(), UserID: w.uuid(other), Endpoint: w.endpoint, P256dh: "p", Auth: "a",
		SessionID: "sid-early", SessionValidUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	w.run()

	if w.sender.count() != 0 {
		t.Fatalf("sent the previous owner's record (%s) to the device's new owner", util.UUIDToString(w.uuid(w.userID)))
	}
}
