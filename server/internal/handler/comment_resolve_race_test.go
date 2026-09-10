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
)

// pauseResolveCountTxStarter and pauseResolveCountTx reuse the injected-pause
// pattern from issue_revision_test.go's pauseIssueAttachmentLinkTxStarter: a tx
// wrapper that blocks the resolve handler's in-flight transaction right after it
// runs CountThreadCommentsSince (the concurrent-feedback race guard in
// ResolveComment), so a test can deterministically land a concurrent reply
// insert inside the window the guard is supposed to close, instead of relying
// on goroutine scheduling luck to hit a real race.
type pauseResolveCountTxStarter struct {
	inner   txStarter
	reached chan<- struct{}
	release <-chan struct{}
}

func (s pauseResolveCountTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pauseResolveCountTx{Tx: tx, reached: s.reached, release: s.release}, nil
}

type pauseResolveCountTx struct {
	pgx.Tx
	reached chan<- struct{}
	release <-chan struct{}
}

func (tx *pauseResolveCountTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "CountThreadCommentsSince") {
		// Pause BEFORE running the query, not after: the test needs the
		// concurrent reply to land before CountThreadCommentsSince executes
		// against the database, otherwise the count is computed against
		// pre-race state and the guard has nothing to catch.
		tx.reached <- struct{}{}
		select {
		case <-tx.release:
		case <-ctx.Done():
			return tx.Tx.QueryRow(ctx, sql, args...)
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
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

// TestResolveComment_ConcurrentReplyRejectsStaleKnownAsOf is the core D2
// regression: an actor who loaded a thread, then resolves it while a new reply
// lands in that same thread from someone else before the resolve commits, must
// not have that reply silently folded away. The resolve is expected to be
// rejected with a typed conflict instead of succeeding over the new reply.
//
// The race is made deterministic (not scheduling-dependent) by pausing the
// resolve's transaction right after it evaluates CountThreadCommentsSince
// against the caller's known_as_of, inserting the concurrent reply while the
// resolve is paused there, then releasing the resolve to finish inside the
// same transaction. If the guard only checked once and then blindly committed
// without ever observing the reply, this reproduces the exact silent-discard
// failure mode; the fix must re-run inside a consistent snapshot such that the
// reply landing before commit still causes the reject.
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
	h.TxStarter = pauseResolveCountTxStarter{
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
		t.Fatal("resolve did not reach the count-thread-comments-since guard")
	}

	// While the resolve is paused inside its transaction, land a new reply in
	// the same thread — this is the concurrent feedback the guard must not let
	// the resolve silently fold away.
	newReplyID := insertResolveRaceReply(t, fx.IssueID, fx.Root1, "concurrent reply")

	close(release)
	resp := <-resolveDone

	if resp.Code != http.StatusConflict {
		t.Fatalf("resolve with a concurrently-added reply = %d: %s, want %d (thread_changed conflict)",
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

	// The root must NOT have been resolved — the new reply's content must stay
	// visible/unfolded rather than the resolve committing over it.
	if commentResolved(t, fx.Root1) {
		t.Fatalf("root1 must not be resolved when the resolve was rejected for a concurrent reply")
	}

	var stillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE id = $1)`, newReplyID).Scan(&stillPresent); err != nil {
		t.Fatalf("check concurrent reply survived: %v", err)
	}
	if !stillPresent {
		t.Fatalf("concurrent reply %s must still exist untouched", newReplyID)
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
