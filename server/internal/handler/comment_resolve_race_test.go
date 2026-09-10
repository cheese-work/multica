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

// lockCommentThreadSQLMarker is the substring unique to LockCommentThread's
// generated SQL (see pkg/db/queries/comment.sql / pkg/db/generated/comment.sql.go).
// Matching on it specifically — rather than on "pg_advisory_xact_lock" generally
// — matters because other advisory locks exist in this codebase sharing that
// generic substring (LockIssueDuplicateKey, for one); a wrapper that paused on
// any advisory-lock Exec would also pause unrelated locking paths and could fire
// more than once inside a single transaction, overflowing the size-1 "reached"
// channel below and deadlocking the test instead of failing it cleanly.
const lockCommentThreadSQLMarker = ":comment_thread"

// countThreadCommentsSinceSQLMarker is the substring unique to
// CountThreadCommentsSince's generated SQL, used by pausingTx.QueryRow to pause
// AFTER that specific statement returns rather than after the lock Exec — see
// TestResolveComment_ConcurrentReplyBlocksUntilResolveTransactionEnds below for
// why that distinction is the point of that test.
const countThreadCommentsSinceSQLMarker = "comment.created_at > "

// pauseResolveLockTxStarter and pausingTx block the resolve handler's
// in-flight transaction at a chosen point — either right after it acquires
// LockCommentThread, or right after CountThreadCommentsSince returns — so a
// test can deterministically try to land a concurrent reply's CreateComment
// call (which takes the identical lock) while the resolve still holds it,
// instead of relying on goroutine scheduling luck to hit a real race.
type pauseResolveLockTxStarter struct {
	inner txStarter
	// execMarker, if non-empty, pauses after the first Exec whose SQL contains
	// it. queryRowMarker, if non-empty, pauses after the first QueryRow whose
	// SQL contains it. At most one should be set per test.
	execMarker     string
	queryRowMarker string
	reached        chan<- struct{}
	release        <-chan struct{}
}

func (s pauseResolveLockTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pausingTx{
		Tx:             tx,
		execMarker:     s.execMarker,
		queryRowMarker: s.queryRowMarker,
		reached:        s.reached,
		release:        s.release,
	}, nil
}

type pausingTx struct {
	pgx.Tx
	execMarker     string
	queryRowMarker string
	reached        chan<- struct{}
	release        <-chan struct{}
}

func (tx *pausingTx) pause(ctx context.Context) {
	// Pause AFTER the matched statement completed, not before: the whole
	// point of the fix is that a concurrent reply's CreateComment cannot
	// acquire the same lock while the resolve holds it, so this is the one
	// point in the resolve's transaction where a reply attempt is guaranteed
	// to contend with something real instead of racing free.
	tx.reached <- struct{}{}
	select {
	case <-tx.release:
	case <-ctx.Done():
	}
}

func (tx *pausingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	res, err := tx.Tx.Exec(ctx, sql, args...)
	if tx.execMarker != "" && strings.Contains(sql, tx.execMarker) {
		tx.pause(ctx)
	}
	return res, err
}

// signalingTxStarter/signalingTx give the OTHER side of a lock-contention
// test (the reply, not the resolve) a positive "I am about to attempt this
// Exec" signal instead of relying on a sleep to infer it. Unlike
// pauseResolveLockTxStarter, this wrapper never blocks the statement it
// matches — it only sends (non-blocking, via a buffered channel) right BEFORE
// issuing the Exec, so the assertion that "the reply hasn't committed" can
// wait on proof the reply goroutine has actually reached its lock attempt
// rather than guessing from a fixed delay. A slow/unscheduled goroutine (GC
// pause, CI runner contention) can otherwise still be sitting before that
// Exec when a sleep-based assertion fires — a false pass that proves nothing
// about lock contention.
type signalingTxStarter struct {
	inner      txStarter
	execMarker string
	ready      chan<- struct{}
}

func (s signalingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &signalingTx{Tx: tx, execMarker: s.execMarker, ready: s.ready}, nil
}

type signalingTx struct {
	pgx.Tx
	execMarker string
	ready      chan<- struct{}
}

func (tx *signalingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.execMarker != "" && strings.Contains(sql, tx.execMarker) {
		select {
		case tx.ready <- struct{}{}:
		default:
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *pausingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if tx.queryRowMarker != "" && strings.Contains(sql, tx.queryRowMarker) {
		return &pausingRow{Row: row, tx: tx, ctx: ctx}
	}
	return row
}

// pausingRow defers the pause until after Scan is called, so the pause point
// is "CountThreadCommentsSince has returned its result" (the count is already
// decoded) rather than merely "the query was issued" — matching the
// requirement that the pause land after the zero-result guard check has been
// observed, while the transaction (and its held lock) is still open.
type pausingRow struct {
	pgx.Row
	tx  *pausingTx
	ctx context.Context
}

func (r *pausingRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.tx.pause(r.ctx)
	return err
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

// TestResolveComment_LockOrderedReplyLandsAfterResolveCommits is the core D2
// regression, restated accurately (this test used to be named
// TestResolveComment_ConcurrentReplyRejectsStaleKnownAsOf and its doc comment
// claimed the resolve gets REJECTED — that was wrong: read carefully, this
// scenario is the lock-ordered SUCCESS case, not a rejection case. The resolve
// acquires LockCommentThread strictly before the concurrent reply can even
// begin its own CreateComment insert, so by the time the reply is allowed to
// proceed, the resolve's guard has already run its zero-result count and the
// resolve is free to commit ahead of the reply. The reply then queues behind
// the lock, and lands (as a normal 201) only once the resolve's transaction
// has released it. This is the "whichever side commits first wins ordering"
// half of the fix; TestResolveComment_ConcurrentReplyBeforeLockAcquisitionRejectsResolve
// below is the other half, where the reply commits FIRST and the resolve is
// the one that has to react to it.
//
// The scenario is made deterministic (not scheduling-dependent) by pausing the
// resolve's transaction right after it acquires the thread-scoped advisory
// lock (LockCommentThread) — i.e. before the zero-result guard check has run —
// then starting a concurrent CreateComment reply against the SAME thread while
// the resolve still holds the lock, then releasing the resolve to finish
// inside the same transaction. CreateComment takes the identical lock before
// its insert, so the reply cannot silently land in the window: it blocks until
// the resolve is done.
func TestResolveComment_LockOrderedReplyLandsAfterResolveCommits(t *testing.T) {
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
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		reached:    reached,
		release:    release,
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

	// While the resolve holds the thread lock, fire the concurrent reply,
	// wrapped so it signals replyReady the instant it is about to issue its
	// own LockCommentThread Exec — a positive proof the reply goroutine
	// actually reached its lock attempt, not an inference from a fixed
	// sleep. It must block on the identical advisory lock rather than
	// completing before the resolve does.
	replyReady := make(chan struct{}, 1)
	replyHandler := *testHandler
	replyHandler.TxStarter = signalingTxStarter{
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		ready:      replyReady,
	}
	replyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replyDone <- createReplyComment(&replyHandler, fx.IssueID, fx.Root1, "concurrent reply")
	}()

	select {
	case <-replyReady:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reply did not reach its own LockCommentThread attempt — no positive signal of lock contention")
	}

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
	// semantics with an up-to-date known_as_of, the resolve must succeed (not
	// be rejected with a thread_changed conflict).
	if resolveResp.Code != http.StatusOK {
		t.Fatalf("resolve that fully precedes the reply under the lock = %d: %s, want 200", resolveResp.Code, resolveResp.Body.String())
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

	if replyResp.Code != http.StatusCreated {
		t.Fatalf("concurrent reply = %d: %s, want 201", replyResp.Code, replyResp.Body.String())
	}
	var replyStillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'concurrent reply')`, fx.IssueID).Scan(&replyStillPresent); err != nil {
		t.Fatalf("check concurrent reply survived: %v", err)
	}
	if !replyStillPresent {
		t.Fatalf("concurrent reply must exist after the lock released it")
	}

	// The reply committing AFTER the resolve legitimately re-opens the thread
	// (AutoUnresolveThreadOnReply — a reply in a resolved thread always
	// reopens it, by design). That is real product behavior, not a symptom of
	// the guard failing: the resolve response above already proved root1 was
	// resolved at commit time, strictly before the reply could land.
	if commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should have been auto-reopened by the reply that landed after the resolve committed")
	}
}

// TestResolveComment_ConcurrentReplyBlocksUntilResolveTransactionEnds is the
// mutual-exclusion regression: it proves the lock genuinely serializes
// commits, not merely that the guard's read happens to observe zero. The
// resolve's transaction is paused AFTER CountThreadCommentsSince has already
// returned a zero result — the guard has passed and the resolve is about to
// perform its resolving write — while the thread-scoped advisory lock is
// still held (the transaction has not committed). A real concurrent reply
// attempt is then started against the same thread: it must be UNABLE to
// commit — its CreateComment call must not even return — until the resolve's
// transaction finishes, proving the lock is held across the guard's entire
// read-then-write window, not just released the instant the count comes back.
// Once the resolve is allowed to finish and commits, the queued reply
// proceeds and lands afterward: this is the same commit-ordering outcome as
// TestResolveComment_LockOrderedReplyLandsAfterResolveCommits, but this test's
// pause point proves WHY it holds — the lock spans CountThreadCommentsSince
// through the resolve's own commit, not just the LockCommentThread call
// itself.
func TestResolveComment_ConcurrentReplyBlocksUntilResolveTransactionEnds(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)
	ctx := context.Background()

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
		inner:          testHandler.TxStarter,
		queryRowMarker: countThreadCommentsSinceSQLMarker,
		reached:        reached,
		release:        release,
	}

	resolveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		resolveDone <- resolveCommentWithKnownAsOf(&h, fx.Root1, &knownAsOf)
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not reach the post-CountThreadCommentsSince pause point")
	}

	// The guard has already read zero new replies and the resolve has not yet
	// committed (or even performed its resolving UPDATE) — the lock is still
	// held. Start a real concurrent reply now, wrapped so it signals
	// replyReady the instant it is about to issue its own LockCommentThread
	// Exec — a positive proof the reply goroutine has actually reached its
	// lock attempt, not an inference from a fixed sleep. Without this signal,
	// a slow/unscheduled goroutine (GC pause, CI runner contention) could
	// still be sitting before that Exec when the assertion below runs — a
	// false pass that proves nothing about lock contention.
	replyReady := make(chan struct{}, 1)
	replyHandler := *testHandler
	replyHandler.TxStarter = signalingTxStarter{
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		ready:      replyReady,
	}
	replyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replyDone <- createReplyComment(&replyHandler, fx.IssueID, fx.Root1, "reply during held lock")
	}()

	select {
	case <-replyReady:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reply did not reach its own LockCommentThread attempt — no positive signal of lock contention")
	}

	// The reply has now provably reached (or is provably about to enter) its
	// own LockCommentThread Exec while the resolve still holds the identical
	// advisory lock. Assert non-completion repeatedly across a window, not
	// just once: a flaky false pass (the reply happening to not have
	// returned yet purely by scheduling luck, rather than genuinely being
	// blocked on the lock) is the exact failure mode a pre-lock version of
	// this test would produce, so give it several opportunities to race
	// ahead if the exclusion were not real.
	for i := 0; i < 5; i++ {
		select {
		case resp := <-replyDone:
			t.Fatalf("concurrent reply completed (status %d) while the resolve transaction still held the thread lock post-count; mutual exclusion is not holding", resp.Code)
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Confirm directly against the database that the reply has not committed
	// yet, not merely that its HTTP call hasn't returned.
	var replyCommittedBeforeRelease bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'reply during held lock')`, fx.IssueID).Scan(&replyCommittedBeforeRelease); err != nil {
		t.Fatalf("check reply not yet committed: %v", err)
	}
	if replyCommittedBeforeRelease {
		t.Fatalf("reply comment is visible in the database before the resolve transaction released its lock")
	}

	close(release)
	resolveResp := <-resolveDone
	replyResp := <-replyDone

	if resolveResp.Code != http.StatusOK {
		t.Fatalf("resolve holding the lock through the guard = %d: %s, want 200", resolveResp.Code, resolveResp.Body.String())
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
	if replyResp.Code != http.StatusCreated {
		t.Fatalf("reply queued behind the lock = %d: %s, want 201 once released", replyResp.Code, replyResp.Body.String())
	}

	var replyStillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'reply during held lock')`, fx.IssueID).Scan(&replyStillPresent); err != nil {
		t.Fatalf("check concurrent reply landed: %v", err)
	}
	if !replyStillPresent {
		t.Fatalf("concurrent reply must exist after the lock released it")
	}

	// As in TestResolveComment_LockOrderedReplyLandsAfterResolveCommits: the
	// reply committing after the resolve legitimately reopens the thread via
	// AutoUnresolveThreadOnReply. The resolve response decoded above already
	// proved root1 was resolved at commit time, strictly before the reply
	// (still queued behind the held lock at that point) could land.
	if commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should have been auto-reopened by the reply that landed after the resolve committed")
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
		inner:      testHandler.TxStarter,
		execMarker: lockCommentThreadSQLMarker,
		reached:    reached,
		release:    release,
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
