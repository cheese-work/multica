package pushnotify_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/pushnotify"
)

// CHE-1795: the push-session
// lifecycle and the teardown paths.

// recordingStarter wraps the pool and records the context every rollback gets.
type recordingStarter struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	// remaining is the time left on each rollback context; a negative value
	// means the context had no deadline at all.
	remaining []time.Duration
}

type recordingTx struct {
	pgx.Tx
	owner *recordingStarter
}

func (s *recordingStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return recordingTx{Tx: tx, owner: s}, nil
}

func (t recordingTx) Rollback(ctx context.Context) error {
	t.owner.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		t.owner.remaining = append(t.owner.remaining, time.Until(deadline))
	} else {
		t.owner.remaining = append(t.owner.remaining, -1)
	}
	t.owner.mu.Unlock()
	return t.Tx.Rollback(ctx)
}

func (s *recordingStarter) requireBounded(t *testing.T, what string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.remaining) == 0 {
		t.Fatalf("%s: no rollback was observed", what)
	}
	for _, left := range s.remaining {
		if left < 0 || left > 10*time.Second {
			t.Errorf("%s: a rollback ran with %v left; every teardown path needs its own short deadline", what, left)
		}
	}
}

func TestContestedDeviceRollbackIsBounded(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	rec := &recordingStarter{pool: testPool}
	release := lockDevice(t, w)

	if err := pushnotify.NewWorker(w.q, rec, w.sender).ProcessOnce(context.Background()); err != nil {
		t.Fatalf("process pass: %v", err)
	}
	release()

	rec.requireBounded(t, "contested device")
}

func TestCancelledAttemptRollsBackBoundedAndReleasesItsConnection(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	rec := &recordingStarter{pool: testPool}
	gate := newGateSender(201)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pushnotify.NewWorker(w.q, rec, gate).ProcessOnce(ctx) }()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never reached the send")
	}

	cancel() // shutdown in the middle of the send
	if err := <-done; err != nil {
		t.Fatalf("process pass: %v", err)
	}

	rec.requireBounded(t, "cancelled attempt")
	deadline := time.Now().Add(3 * time.Second)
	for testPool.Stat().AcquiredConns() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := testPool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after the cancelled attempt", n)
	}
}

// The worker takes a shared advisory lock on the registering session before it
// reads the context; logout takes the exclusive one after it commits the
// marker. The test plays logout by hand: it holds the exclusive lock, so the
// worker cannot pass the point where it reads the context, then commits the
// marker and lets go.
func TestMarkerCommittedBeforeTheContextReadStopsTheSend(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	session := "sid-" + w.endpoint
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, session)
	})

	logout, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := logout.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, session); err != nil {
		t.Fatalf("take the session lock: %v", err)
	}
	pass := goPass(w, w.sender)
	time.Sleep(400 * time.Millisecond) // the worker is as far as it can get
	w.fx.Exec(t, `INSERT INTO push_session_end (session_id) VALUES ($1)`, session)
	if err := logout.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-pass; err != nil {
		t.Fatalf("process pass: %v", err)
	}

	if w.sender.count() != 0 {
		t.Fatal("the worker read its context and sent although the session ended before it could")
	}
}

// Sliding renewal keeps the `sid` of a session alive for as long as it is used,
// so an ended session's marker is never old enough to drop.
func TestSessionMarkersOutliveAnyTokenLifetime(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	session := "sid-" + w.endpoint
	w.fx.Exec(t, `INSERT INTO push_session_end (session_id, ended_at) VALUES ($1, now() - interval '400 days')`, session)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, session)
	})

	w.run()

	if n := w.fx.Count(t, `SELECT count(*) FROM push_session_end WHERE session_id = $1`, session); n != 1 {
		t.Fatal("a pass dropped the marker of a session that could still be renewed")
	}
	if w.sender.count() != 0 {
		t.Fatal("sent to a device whose session ended long ago")
	}
}
