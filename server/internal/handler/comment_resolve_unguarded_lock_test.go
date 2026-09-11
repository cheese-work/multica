package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestResolveComment_UnguardedResolveTakesThreadLock is the fix #1 regression
// (Sol's round-5 finding): before this fix, ResolveComment only acquired
// LockCommentThread inside the `if req.KnownAsOf != nil` block, so an
// unguarded resolve (no known_as_of in the request body — the default shape
// every existing web/desktop caller sends today) never took the lock at all.
// A reply transaction holding LockCommentThread via
// commentguard.LockThreadForReplyAndClearResolution believes, by construction
// of that lock, that nothing else can observe or mutate the thread's
// resolution state while it holds it — but an unguarded resolve violated that
// belief by mutating resolution state (ClearOtherThreadResolutions +
// ResolveComment) without ever contending for the same lock.
//
// This test proves the fix: it drives a REPLY first, pausing that reply's
// transaction right after it acquires LockCommentThread (the identical
// pauseResolveLockTxStarter/pausingTx machinery
// comment_resolve_race_test.go's tests use, just applied to the reply side
// instead of the resolve side this time). While the reply holds the lock, it
// fires a concurrent UNGUARDED resolve (no known_as_of) against the very same
// thread. If ResolveComment still skipped lock acquisition for the unguarded
// path, that resolve would complete immediately, racing past the reply's held
// lock. With the fix, it must block until the reply's transaction releases
// the lock, then land afterward — whichever side commits first is the one
// the other observes, exactly as the guarded (known_as_of) path already
// requires.
func TestResolveComment_UnguardedResolveTakesThreadLock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)
	ctx := context.Background()

	// Root1 starts resolved so the concurrent unguarded resolve below has
	// real resolution state to mutate (ClearOtherThreadResolutions +
	// ResolveComment), not just a no-op path with nothing to clear.
	resolveCommentHTTP(t, fx.Root1)
	if !commentResolved(t, fx.Root1) {
		t.Fatalf("root1 must be resolved before the reply lands")
	}

	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	// Pause the REPLY's transaction right after it acquires LockCommentThread
	// via commentguard — the same pause point comment_resolve_race_test.go
	// uses to pause the resolve side, applied here to the reply side.
	replyHandler := *testHandler
	replyHandler.TxStarter = pauseResolveLockTxStarter{
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		reached:    reached,
		release:    release,
	}

	replyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replyDone <- createReplyComment(&replyHandler, fx.IssueID, fx.Root1, "reply holding lock for unguarded resolve test")
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("reply did not reach the LockCommentThread pause point")
	}

	// While the reply holds the thread lock, fire a concurrent UNGUARDED
	// resolve (no known_as_of) against the same thread, wrapped so it signals
	// resolveReady the instant it is about to issue its own LockCommentThread
	// Exec — a positive proof the resolve goroutine actually reached its lock
	// attempt, not an inference from a fixed sleep.
	resolveReady := make(chan struct{}, 1)
	resolveHandler := *testHandler
	resolveHandler.TxStarter = signalingTxStarter{
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		ready:      resolveReady,
	}
	resolveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		resolveDone <- resolveCommentWithKnownAsOf(&resolveHandler, fx.Root1, nil)
	}()

	select {
	case <-resolveReady:
	case <-time.After(5 * time.Second):
		t.Fatal("unguarded resolve did not reach its own LockCommentThread attempt — no positive signal of lock contention; ResolveComment may still be skipping lock acquisition for the unguarded path")
	}

	// The unguarded resolve cannot have completed yet: it is blocked behind
	// the reply's held lock. Assert non-completion repeatedly across a
	// window, not just once, to rule out a flaky false pass from scheduling
	// luck rather than genuine mutual exclusion.
	for i := 0; i < 5; i++ {
		select {
		case resp := <-resolveDone:
			t.Fatalf("unguarded resolve completed (status %d) while the reply transaction still held the thread lock; the unguarded resolve path is not taking LockCommentThread", resp.Code)
		case <-time.After(100 * time.Millisecond):
		}
	}

	close(release)
	replyResp := <-replyDone
	resolveResp := <-resolveDone

	if replyResp.Code != http.StatusCreated {
		t.Fatalf("reply = %d: %s, want 201", replyResp.Code, replyResp.Body.String())
	}
	if resolveResp.Code != http.StatusOK {
		t.Fatalf("unguarded resolve queued behind the lock = %d: %s, want 200 once released", resolveResp.Code, resolveResp.Body.String())
	}
	var resolvedPayload struct {
		ResolvedAt *string `json:"resolved_at"`
	}
	if err := json.Unmarshal(resolveResp.Body.Bytes(), &resolvedPayload); err != nil {
		t.Fatalf("decode resolve response: %v", err)
	}
	if resolvedPayload.ResolvedAt == nil {
		t.Fatalf("resolve response did not report root1 as resolved: %s", resolveResp.Body.String())
	}

	// The reply committed FIRST (it held the lock and released only when this
	// test explicitly released it, strictly before the resolve could
	// acquire the lock), so the reply's atomic in-transaction clear already
	// reopened root1 before the unguarded resolve even began its own write.
	// The unguarded resolve therefore observes an unresolved root1 and
	// legitimately re-resolves it — commit order is consistent, not
	// corrupted: whichever side committed first (the reply) is exactly what
	// the side that committed second (the resolve) observed and acted on.
	if !commentResolved(t, fx.Root1) {
		t.Fatalf("root1 must be resolved: the unguarded resolve committed last and its own resolution must be the final state")
	}

	var replyStillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'reply holding lock for unguarded resolve test')`, fx.IssueID).Scan(&replyStillPresent); err != nil {
		t.Fatalf("check reply landed: %v", err)
	}
	if !replyStillPresent {
		t.Fatalf("reply must exist after the lock released it")
	}
}
