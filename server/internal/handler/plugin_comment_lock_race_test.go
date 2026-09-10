package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCreatePluginComment_ReplyTakesThreadLock is the fix #2 regression: before
// this fix, CreatePluginComment was one of four reply-insert call sites that
// bypassed LockCommentThread entirely (see commentguard.LockThreadForReply and
// its wiring into CreatePluginComment). A resolve running concurrently against
// the same thread had no way to serialize against a plugin-authored reply, so
// the exact TOCTOU window the lock exists to close stayed open for this path.
//
// This test proves CreatePluginComment now actually contends for the SAME
// advisory lock ResolveComment takes: it pauses ResolveComment's transaction
// right after it acquires LockCommentThread (the identical pauseResolveLockTxStarter/
// pausingTx machinery comment_resolve_race_test.go uses for the handler's own
// CreateComment path), then fires a concurrent plugin reply through
// CreatePluginComment against the same thread root. If CreatePluginComment
// still bypassed the lock, the plugin reply would complete immediately,
// independent of the resolve holding the lock. Instead it must block until the
// resolve's transaction releases the lock, then land afterward.
func TestCreatePluginComment_ReplyTakesThreadLock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newResolveTestFixture(t)
	ctx := context.Background()

	installationID := installPluginForAction(t, []string{"issues:read", "comments:read", "comments:write"})

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

	// While the resolve holds the thread lock, fire a concurrent plugin reply
	// against the SAME thread on the real (unwrapped) handler. If
	// CreatePluginComment takes LockCommentThread as required, it must block
	// behind the resolve's held lock rather than completing immediately.
	pluginReplyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		pluginReplyDone <- httptestRecordPluginComment(testHandler, fx.IssueID, fx.Root1, installationID, "plugin reply during held lock")
	}()

	select {
	case resp := <-pluginReplyDone:
		t.Fatalf("plugin reply completed (status %d) before the resolve released its thread lock; CreatePluginComment is not taking LockCommentThread", resp.Code)
	case <-time.After(200 * time.Millisecond):
	}

	// Confirm directly against the database that the plugin reply has not
	// committed yet, not merely that its HTTP call hasn't returned.
	var pluginReplyCommittedBeforeRelease bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'plugin reply during held lock')`, fx.IssueID).Scan(&pluginReplyCommittedBeforeRelease); err != nil {
		t.Fatalf("check plugin reply not yet committed: %v", err)
	}
	if pluginReplyCommittedBeforeRelease {
		t.Fatalf("plugin reply comment is visible in the database before the resolve transaction released its lock")
	}

	close(release)
	resolveResp := <-resolveDone
	pluginReplyResp := <-pluginReplyDone

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

	if pluginReplyResp.Code != http.StatusCreated {
		t.Fatalf("plugin reply queued behind the lock = %d: %s, want 201 once released", pluginReplyResp.Code, pluginReplyResp.Body.String())
	}
	var pluginReplyStillPresent bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id = $1 AND content = 'plugin reply during held lock')`, fx.IssueID).Scan(&pluginReplyStillPresent); err != nil {
		t.Fatalf("check plugin reply landed: %v", err)
	}
	if !pluginReplyStillPresent {
		t.Fatalf("plugin reply must exist after the lock released it")
	}

	// As in the CreateComment lock races: the reply committing after the
	// resolve legitimately reopens the thread via AutoUnresolveThreadOnReply.
	// The resolve response decoded above already proved root1 was resolved at
	// commit time, strictly before the plugin reply (still queued behind the
	// held lock at that point) could land.
	if commentResolved(t, fx.Root1) {
		t.Fatalf("root1 should have been auto-reopened by the plugin reply that landed after the resolve committed")
	}
}

// httptestRecordPluginComment drives POST /v1/issues/{issue_ref}/comments
// through CreatePluginComment with a parent_id set, the same reply path a
// concurrently-replying plugin uses.
func httptestRecordPluginComment(h *Handler, issueID, parentID, installationID, content string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := pluginActionRequest(http.MethodPost, "/v1/issues/"+issueID+"/comments", installationID,
		map[string]any{"content": content, "parent_id": parentID},
		map[string]string{"issue_ref": issueID})
	h.CreatePluginComment(w, r)
	return w
}
