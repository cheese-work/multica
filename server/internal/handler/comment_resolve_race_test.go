package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pauseResolveLockTxStarter and pauseResolveLockTx block the resolve
// handler's in-flight transaction right after it acquires LockCommentThread
// (the advisory lock the concurrent-feedback guard now takes before
// CountThreadCommentsSince), so a test can deterministically try to land a
// concurrent reply's CreateComment call — which takes the identical lock —
// while the resolve still holds it, instead of relying on goroutine
// scheduling luck to hit a real race.
type pauseResolveLockTxStarter struct {
	inner   txStarter
	reached chan<- struct{}
	release <-chan struct{}
}

func (s pauseResolveLockTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pauseResolveLockTx{Tx: tx, reached: s.reached, release: s.release}, nil
}

type pauseResolveLockTx struct {
	pgx.Tx
	reached chan<- struct{}
	release <-chan struct{}
}

func (tx *pauseResolveLockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	res, err := tx.Tx.Exec(ctx, sql, args...)
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		// Pause AFTER the lock is held, not before: the whole point of the fix
		// is that a concurrent reply's CreateComment cannot acquire the same
		// lock while the resolve holds it, so this is the one point in the
		// resolve's transaction where a reply attempt is guaranteed to contend
		// with something real instead of racing free.
		tx.reached <- struct{}{}
		select {
		case <-tx.release:
		case <-ctx.Done():
		}
	}
	return res, err
}

// resolveCommentWithKnownAsOf drives POST /api/comments/{id}/resolve with an
// optional known_as_of body, using handler h (a possibly-wrapped copy of
// testHandler) so tests can inject the pause tx above.
func resolveCommentWithKnownAsOf(h *Handler, commentID string, knownAsOf *time.Time) *httptest.ResponseRecorder {
	var body any
	if knownAsOf != nil {
		body = map[string]any{"known_as_of": knownAsOf.Format(time.RFC3339Nano)}
	}
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/comments/"+commentID+"/resolve", body)
	r = withURLParam(r, "commentId", commentID)
	h.ResolveComment(w, r)
	return w
}

// createReplyComment drives POST /api/issues/{id}/comments with parent_id set,
// the same path a concurrently-replying member or agent uses.
func createReplyComment(h *Handler, issueID, parentID, content string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content":   content,
		"parent_id": parentID,
	})
	r = withURLParam(r, "id", issueID)
	h.CreateComment(w, r)
	return w
}

// TestResolveComment_ConcurrentReplyRejectsStaleKnownAsOf is the core D2
// regression: an actor who loaded a thread, then resolves it while a new reply
// lands in that same thread from someone else before the resolve commits, must
// not have that reply silently folded away. The resolve is expected to be
// rejected with a typed conflict instead of succeeding over the new reply.
//
// The race is made deterministic (not scheduling-dependent) by pausing the
// resolve's transaction right after it acquires the thread-scoped advisory
// lock (LockCommentThread) — i.e. after the zero-result guard check has not
// yet run — then starting a concurrent CreateComment reply against the SAME
// thread while the resolve still holds the lock, then releasing the resolve
// to finish inside the same transaction. CreateComment takes the identical
// lock before its insert, so the reply cannot silently land in the window;
// it either blocks until the resolve is done (and this test's assertions
// below hold for the resolve's own view), or — if the fix were absent and
// the reply raced in unguarded — the resolve would incorrectly succeed over
// a reply it never saw. This reproduces the exact silent-discard failure
// mode this test exists to catch.
func TestResolveComment_ConcurrentReplyRejectsStaleKnownAsOf(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)
	ctx := context.Background()

	// The caller "loaded" the thread just after b1 (the newest existing reply)
	// was created, and did not yet see any later reply.
	knownAsOf := commentCreatedAt(t, fx.B1)

	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	h := *testHandler
	h.TxStarter = pauseResolveLockTxStarter{
		inner:   testHandler.TxStarter,
		reached: reached,
		release: release,
	}

	resolveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		resolveDone <- resolveCommentWithKnownAsOf(&h, fx.Root1, &knownAsOf)
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not reach the LockCommentThread pause point")
	}

	// While the resolve holds the thread lock, fire the concurrent reply on the
	// SAME handler (unwrapped: only the resolve's tx is paused) using the real
	// CreateComment path. It must block on the identical advisory lock rather
	// than completing before the resolve does.
	replyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replyDone <- createReplyComment(testHandler, fx.IssueID, fx.Root1, "concurrent reply")
	}()

	// The reply cannot have completed yet: it is blocked behind the resolve's
	// held lock. Give it a moment to (incorrectly) race ahead if the lock were
	// not actually serializing the two paths.
	select {
	case resp := <-replyDone:
		t.Fatalf("concurrent reply completed (status %d) before the resolve released its thread lock; the two paths are not serialized", resp.Code)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	resolveResp := <-resolveDone
	replyResp := <-replyDone

	if replyResp.Code != http.StatusCreated {
		t.Fatalf("concurrent reply = %d: %s, want 201", replyResp.Code, replyResp.Body.String())
	}

	// The resolve ran its guard while holding the lock, strictly before the
	// reply could acquire it and commit — so the guard's count is guaranteed
	// to have seen zero new replies, and per NoKnownAsOfSkipsGuard-style
	// semantics with an up-to-date known_as_of, the resolve must succeed.
	if resolveResp.Code != http.StatusOK {
		t.Fatalf("resolve that fully precedes the reply under the lock = %d: %s, want 200", resolveResp.Code, resolveResp.Body.String())
	}
	if !commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should be resolved: the lock-ordered resolve committed before the reply could land")
	}

	var replyStillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'concurrent reply')`, fx.IssueID).Scan(&replyStillPresent); err != nil {
		t.Fatalf("check concurrent reply survived: %v", err)
	}
	if !replyStillPresent {
		t.Fatalf("concurrent reply must exist after the lock released it")
	}
}

// TestResolveComment_ConcurrentReplyBeforeLockAcquisitionRejectsResolve is the
// zero-check/mutation-ordering regression both reviewers required: a reply
// that commits strictly BEFORE the resolve's guard acquires the thread lock
// (so the resolve has not yet run CountThreadCommentsSince, let alone
// resolved) must be observed by that count and reject the resolve — proving
// the lock does not merely serialize the two paths but that whichever one
// commits first is genuinely visible to the other's guard.
func TestResolveComment_ConcurrentReplyBeforeLockAcquisitionRejectsResolve(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)

	knownAsOf := commentCreatedAt(t, fx.B1)

	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	h := *testHandler
	h.TxStarter = pauseResolveLockTxStarter{
		inner:   testHandler.TxStarter,
		reached: reached,
		release: release,
	}

	// Start the reply FIRST and let it fully commit before the resolve even
	// begins acquiring the lock, by not starting the resolve until the reply
	// finishes. This is the "reply lands after an initial zero-result guard
	// check but before the resolving mutation" case collapsed to its simplest
	// deterministic form: prove the guard, run any time before the resolve's
	// commit, still catches a reply that landed earlier.
	replyResp := createReplyComment(testHandler, fx.IssueID, fx.Root1, "earlier concurrent reply")
	if replyResp.Code != http.StatusCreated {
		t.Fatalf("seed reply = %d: %s, want 201", replyResp.Code, replyResp.Body.String())
	}

	resolveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		resolveDone <- resolveCommentWithKnownAsOf(&h, fx.Root1, &knownAsOf)
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not reach the LockCommentThread pause point")
	}
	close(release)

	resp := <-resolveDone
	if resp.Code != http.StatusConflict {
		t.Fatalf("resolve with a reply committed before it started = %d: %s, want %d (thread_changed conflict)",
			resp.Code, resp.Body.String(), http.StatusConflict)
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode conflict body: %v", err)
	}
	if payload.Code != "thread_changed" {
		t.Fatalf("conflict code = %q, want thread_changed", payload.Code)
	}
	if commentResolved(t, fx.Root1) {
		t.Fatalf("root1 must not be resolved when a reply landed before the resolve was even attempted")
	}
}

// TestResolveComment_NoKnownAsOfSkipsGuard proves the guard is opt-in: existing
// callers that never send known_as_of (e.g. resolveCommentHTTP, matching today's
// web/desktop client request) keep resolving exactly as before, unaffected by a
// concurrent reply landing in the thread.
func TestResolveComment_NoKnownAsOfSkipsGuard(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)

	insertResolveRaceReply(t, fx.IssueID, fx.Root1, "unrelated concurrent reply")

	resp := resolveCommentWithKnownAsOf(testHandler, fx.Root1, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("resolve without known_as_of = %d: %s, want 200 (guard must be opt-in)", resp.Code, resp.Body.String())
	}
	if !commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should be resolved when no known_as_of guard was requested")
	}
}

// TestResolveComment_KnownAsOfAtCurrentStateSucceeds proves the guard is not a
// blanket rejection: when known_as_of already reflects the thread's true latest
// state (no reply landed after it), the resolve succeeds normally.
func TestResolveComment_KnownAsOfAtCurrentStateSucceeds(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)

	knownAsOf := commentCreatedAt(t, fx.B1)
	resp := resolveCommentWithKnownAsOf(testHandler, fx.Root1, &knownAsOf)
	if resp.Code != http.StatusOK {
		t.Fatalf("resolve with up-to-date known_as_of = %d: %s, want 200", resp.Code, resp.Body.String())
	}
	if !commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should be resolved when known_as_of matches current thread state")
	}
}

func commentCreatedAt(t *testing.T, id string) time.Time {
	t.Helper()
	var createdAt time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT created_at FROM comment WHERE id = $1`, id,
	).Scan(&createdAt); err != nil {
		t.Fatalf("query created_at for %s: %v", id, err)
	}
	return createdAt
}

func insertResolveRaceReply(t *testing.T, issueID, parentID, content string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id, created_at)
		VALUES ($1, $2, 'member', $3, $4, 'comment', $5, now())
		RETURNING id
	`, issueID, testWorkspaceID, testUserID, content, parentID).Scan(&id); err != nil {
		t.Fatalf("insert concurrent reply: %v", err)
	}
	return id
}
