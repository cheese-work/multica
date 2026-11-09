package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-4147 (plan r4 T3-T5, acceptance A2 A6 A11 A13 A21 A24, C1): human attention endpoints.

type attnFx struct {
	t         *testing.T
	agentID   string
	sessionID string
}

func newAttnFx(t *testing.T) *attnFx {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "attn-"+t.Name(), []byte("[]"))
	sessionID := chatSessionFor(t, agentID, testutil.Cols{})
	dbfx.Cleanup(t, `DELETE FROM chat_attention_mark WHERE user_id = $1`, testUserID)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_item WHERE chat_session_id = $1`, sessionID)
	return &attnFx{t: t, agentID: agentID, sessionID: sessionID}
}

func (f *attnFx) item(kind, user string, openedAgo time.Duration) string {
	f.t.Helper()
	cols := testutil.Cols{
		"workspace_id": testWorkspaceID, "chain_root_session_id": f.sessionID, "chat_session_id": f.sessionID,
		"user_id": user, "kind": kind,
		"opened_at": testutil.Raw(fmt.Sprintf("now() - interval '%d seconds'", int(openedAgo.Seconds()))),
	}
	if kind == "ask" {
		cols["agent_id"] = f.agentID
	}
	if kind == "later" {
		cols["due_at"] = testutil.Raw("now() + interval '1 hour'")
	}
	return dbfx.Insert(f.t, "chat_attention_item", cols)
}

func (f *attnFx) decision() string {
	f.t.Helper()
	return dbfx.Insert(f.t, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": dbfx.Issue(f.t, "Attn decision"), "asking_agent_id": f.agentID,
		"human_user_id": testUserID, "chat_session_id": f.sessionID, "escalation_class": "reversible", "origin": "asked",
	})
}

func (f *attnFx) turn() string {
	f.t.Helper()
	return dbfx.Insert(f.t, "agent_task_queue", testutil.Cols{
		"agent_id": f.agentID, "runtime_id": handlerTestRuntimeID(f.t), "status": "completed", "priority": 2, "chat_session_id": f.sessionID,
	})
}

func (f *attnFx) resolved(id string) (by *string) {
	f.t.Helper()
	dbfx.QueryRow(f.t, `SELECT resolved_by FROM chat_attention_item WHERE id = $1`, id).Scan(&by)
	return by
}

func (f *attnFx) call(h http.HandlerFunc, path string, body any, params ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := newRequest(http.MethodPost, path, body)
	for i := 0; i+1 < len(params); i += 2 {
		req = withURLParam(req, params[i], params[i+1])
	}
	rec := httptest.NewRecorder()
	h(rec, withChatTestWorkspaceCtx(f.t, req))
	return rec
}

func (f *attnFx) post(h http.HandlerFunc, verb string, body map[string]any) *httptest.ResponseRecorder {
	return f.call(h, "/api/chat/sessions/"+f.sessionID+"/attention/"+verb, body, "sessionId", f.sessionID)
}

type attnView struct {
	StaleLevel string   `json:"stale_level"`
	Keys       []string `json:"keys"`
}

func attnOne(t *testing.T, rec *httptest.ResponseRecorder) (attnView, string) {
	t.Helper()
	var out struct {
		BatchID   string   `json:"batch_id"`
		Attention attnView `json:"attention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out.Attention, out.BatchID
}

func wantAttentionCode(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d: %s", rec.Code, code, rec.Body.String())
	}
}

func has(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// A6 / E19: a delayed Xong carries old keys; the ask that arrived after is untouched.
func TestChatAttentionXong_NewAskSurvivesDelayedKeys(t *testing.T) {
	f := newAttnFx(t)
	old := f.item("ask", testUserID, 5*time.Hour)
	arrived := f.item("ask", testUserID, time.Minute)

	rec := f.post(testHandler.ChatAttentionXong, "xong", map[string]any{"subject_keys": []string{service.ItemKey(old)}})
	wantAttentionCode(t, rec, http.StatusOK)
	view, _ := attnOne(t, rec)
	if by := f.resolved(old); by == nil || *by != "xong" {
		t.Fatalf("old ask resolved_by = %v, want xong", by)
	}
	if f.resolved(arrived) != nil || !has(view.Keys, service.ItemKey(arrived)) || has(view.Keys, service.ItemKey(old)) {
		t.Fatalf("new ask must stay open and visible: %+v", view)
	}
	// Idempotent: a repeat of the same keys changes nothing.
	wantAttentionCode(t, f.post(testHandler.ChatAttentionXong, "xong", map[string]any{"subject_keys": []string{service.ItemKey(old)}}), http.StatusOK)
}

// A21: a decision key in Xong is refused; the whole request changes nothing.
func TestChatAttentionXong_DecisionKeyRefused(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, time.Hour)
	d := f.decision()
	rec := f.post(testHandler.ChatAttentionXong, "xong", map[string]any{"subject_keys": []string{service.ItemKey(ask), service.DecisionKey(d, 1)}})
	wantAttentionCode(t, rec, http.StatusUnprocessableEntity)
	if f.resolved(ask) != nil || dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, d) != 1 {
		t.Fatal("rejected request must not change the ask or the decision")
	}
}

// A24: another user's key is a 403 on Xong and Để sau; nothing changes.
func TestChatAttentionKeysOfAnotherUserForbidden(t *testing.T) {
	f := newAttnFx(t)
	other := dbfx.User(t, "Attn Other", fmt.Sprintf("attn-other-%d@multica.ai", time.Now().UnixNano()))
	foreign := f.item("ask", other, time.Hour)
	mine := f.item("ask", testUserID, time.Hour)
	keys := []string{service.ItemKey(mine), service.ItemKey(foreign)}

	wantAttentionCode(t, f.post(testHandler.ChatAttentionXong, "xong", map[string]any{"subject_keys": keys}), http.StatusForbidden)
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": keys, "request_id": uuid.NewString()}), http.StatusForbidden)
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": keys}), http.StatusForbidden)
	if f.resolved(mine) != nil || f.resolved(foreign) != nil ||
		dbfx.Count(t, `SELECT count(*) FROM chat_attention_mark WHERE user_id = $1`, testUserID) != 0 ||
		dbfx.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later'`, f.sessionID) != 0 {
		t.Fatal("a forbidden request must change nothing")
	}
}

// A2: /later 2h hides the ask for 2h, keeps it open, and it returns with its original age.
func TestChatAttentionLater_HidesThenReturnsWithOriginalAge(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, 5*time.Hour)
	var opened time.Time
	dbfx.QueryRow(t, `SELECT opened_at FROM chat_attention_item WHERE id = $1`, ask).Scan(&opened)

	rec := f.post(testHandler.ChatAttentionLater, "later", map[string]any{
		"subject_keys": []string{service.ItemKey(ask)}, "duration_seconds": 7200, "request_id": uuid.NewString(),
	})
	wantAttentionCode(t, rec, http.StatusOK)
	view, _ := attnOne(t, rec)
	if f.resolved(ask) != nil || has(view.Keys, service.ItemKey(ask)) {
		t.Fatalf("ask must stay open but hidden: %+v", view)
	}
	rows, err := service.LoadChatAttention(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(testUserID), time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.RootID == f.sessionID {
			if !has(r.Keys, service.ItemKey(ask)) || r.AttentionSince == nil || r.AttentionSince.After(opened.Add(time.Second)) {
				t.Fatalf("after 3h the ask returns with its original age: %+v", r)
			}
			return
		}
	}
	t.Fatal("chain missing from projection after 3h")
}

// C1: /later on a DM with no other obligation makes a later item that stays quiet until due.
func TestChatAttentionLater_QuietUntilDue(t *testing.T) {
	f := newAttnFx(t)
	rec := f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": []string{}, "duration_seconds": 7200, "request_id": uuid.NewString()})
	wantAttentionCode(t, rec, http.StatusOK)
	view, _ := attnOne(t, rec)
	if view.StaleLevel != "none" || len(view.Keys) != 0 {
		t.Fatalf("later item must be quiet before due: %+v", view)
	}
	var id string
	var due time.Time
	dbfx.QueryRow(t, `SELECT id, due_at FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later' AND resolved_at IS NULL`, f.sessionID).Scan(&id, &due)
	if d := time.Until(due); d < 119*time.Minute || d > 121*time.Minute {
		t.Fatalf("due_at = now+%v, want ~2h", d)
	}
}

// A13: out-of-range durations are refused with the range; a retried request does not extend.
func TestChatAttentionLater_RangeAndIdempotency(t *testing.T) {
	f := newAttnFx(t)
	for name, body := range map[string]map[string]any{
		"30d":  {"duration_seconds": 30 * 86400},
		"zero": {"duration_seconds": 0},
		"past": {"until": time.Now().Add(-time.Hour).Format(time.RFC3339)},
	} {
		body["subject_keys"], body["request_id"] = []string{}, uuid.NewString()
		rec := f.post(testHandler.ChatAttentionLater, "later", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "168h") {
			t.Fatalf("%s: status %d, body %s; want 422 naming the 168h range", name, rec.Code, rec.Body.String())
		}
	}
	if dbfx.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1`, f.sessionID) != 0 {
		t.Fatal("a rejected request must create nothing")
	}

	req := uuid.NewString()
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": []string{}, "duration_seconds": 3600, "request_id": req}), http.StatusOK)
	var first time.Time
	dbfx.QueryRow(t, `SELECT due_at FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later' AND resolved_at IS NULL`, f.sessionID).Scan(&first)
	time.Sleep(20 * time.Millisecond)
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": []string{}, "duration_seconds": 3600, "request_id": req}), http.StatusOK)
	var again time.Time
	dbfx.QueryRow(t, `SELECT due_at FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later' AND resolved_at IS NULL`, f.sessionID).Scan(&again)
	if !again.Equal(first) {
		t.Fatalf("retry extended the reminder: %v -> %v", first, again)
	}

	// A new request replaces the open later: still exactly one open.
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": []string{}, "duration_seconds": 7200, "request_id": uuid.NewString()}), http.StatusOK)
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later' AND resolved_at IS NULL`, f.sessionID); n != 1 {
		t.Fatalf("open later items = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'later' AND resolved_by = 'replaced'`, f.sessionID); n != 1 {
		t.Fatalf("replaced later items = %d, want 1", n)
	}
}

// A11: Xong tất cả, then undo. Arrival after the snapshot and decisions are untouched.
func TestChatAttentionXongAll_UndoRestoresOnlyItsBatch(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, 5*time.Hour)
	unseen := f.item("unseen", testUserID, time.Hour)
	d := f.decision()
	turn := f.turn()
	keys := []string{service.ItemKey(ask), service.ItemKey(unseen), service.DecisionKey(d, 1), service.TurnKey(turn)}
	arrived := f.item("ask", testUserID, time.Minute)

	rec := f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": keys})
	wantAttentionCode(t, rec, http.StatusOK)
	var done struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil || done.BatchID == "" {
		t.Fatalf("batch_id missing: %s", rec.Body.String())
	}
	acked := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM chat_attention_mark WHERE user_id = $1 AND subject_key = $2 AND acked_at IS NOT NULL`, testUserID, service.TurnKey(turn))
	}
	if f.resolved(ask) == nil || f.resolved(unseen) == nil || acked() != 1 {
		t.Fatal("ask, unseen and turn must be resolved or acknowledged")
	}
	if f.resolved(arrived) != nil || dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, d) != 1 {
		t.Fatal("arrival and decision must be untouched")
	}

	// A later Xong on another ask is a different batch; undoing the first must not reopen it.
	other := f.item("ask", testUserID, 2*time.Hour)
	wantAttentionCode(t, f.post(testHandler.ChatAttentionXong, "xong", map[string]any{"subject_keys": []string{service.ItemKey(other)}}), http.StatusOK)

	wantAttentionCode(t, f.call(testHandler.ChatAttentionUndoXongAll, "/api/chat/attention/xong-all/"+done.BatchID+"/undo", nil, "batchId", done.BatchID), http.StatusOK)
	if f.resolved(ask) != nil || f.resolved(unseen) != nil || acked() != 0 {
		t.Fatal("undo must reopen the batch's rows and clear its acknowledgement")
	}
	if f.resolved(other) == nil || f.resolved(arrived) != nil {
		t.Fatal("undo must not touch other batches or arrivals")
	}
}

func TestChatAttentionXongAll_MaxKeys(t *testing.T) {
	f := newAttnFx(t)
	keys := make([]string, 501)
	for i := range keys {
		keys[i] = service.ItemKey(uuid.NewString())
	}
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": keys}), http.StatusUnprocessableEntity)
}

// Mark-unread hook: explicit mark-unseen opens one unseen item; Để sau turns it into a later.
func TestMarkChatSessionUnread_OpensUnseenItem(t *testing.T) {
	f := newAttnFx(t)
	unread := func() *httptest.ResponseRecorder {
		req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+f.sessionID+"/unread", nil), "sessionId", f.sessionID))
		rec := httptest.NewRecorder()
		testHandler.MarkChatSessionUnread(rec, req)
		return rec
	}
	openUnseen := `SELECT count(*) FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'unseen' AND resolved_at IS NULL`

	wantAttentionCode(t, unread(), http.StatusNoContent)
	if dbfx.Count(t, openUnseen, f.sessionID) != 0 {
		t.Fatal("no assistant message: mark-unread stays a no-op")
	}
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": f.sessionID, "role": "assistant", "content": "hi"})
	wantAttentionCode(t, unread(), http.StatusNoContent)
	wantAttentionCode(t, unread(), http.StatusNoContent)
	if n := dbfx.Count(t, openUnseen, f.sessionID); n != 1 {
		t.Fatalf("open unseen items = %d, want 1", n)
	}

	var unseenID string
	dbfx.QueryRow(t, `SELECT id FROM chat_attention_item WHERE chat_session_id = $1 AND kind = 'unseen' AND resolved_at IS NULL`, f.sessionID).Scan(&unseenID)
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{"subject_keys": []string{service.ItemKey(unseenID)}, "request_id": uuid.NewString()}), http.StatusOK)
	if dbfx.Count(t, openUnseen, f.sessionID) != 0 {
		t.Fatal("Để sau must turn the unseen item into the later item")
	}
}

// Review finding P1 (E19 for Để sau): an unseen item marked after the displayed snapshot is not in
// the sent keys, so a delayed Để sau must leave it open and visible.
func TestChatAttentionLater_KeepsUnseenMarkedAfterSnapshot(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, 5*time.Hour)
	arrived := f.item("unseen", testUserID, time.Minute)

	rec := f.post(testHandler.ChatAttentionLater, "later", map[string]any{
		"subject_keys": []string{service.ItemKey(ask)}, "duration_seconds": 7200, "request_id": uuid.NewString(),
	})
	wantAttentionCode(t, rec, http.StatusOK)
	view, _ := attnOne(t, rec)
	if f.resolved(arrived) != nil || !has(view.Keys, service.ItemKey(arrived)) || has(view.Keys, service.ItemKey(ask)) {
		t.Fatalf("unseen marked after the snapshot must stay open and visible; ask hidden: %+v", view)
	}
}

func (f *attnFx) keysAt(at time.Time) []string {
	f.t.Helper()
	rows, err := service.LoadChatAttention(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(testUserID), at)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rows {
		if r.RootID == f.sessionID {
			return r.Keys
		}
	}
	return nil
}

// A19: Để sau snoozes the decision revision it displayed. A new decision and a reopened
// (answer_revision +1) decision are different subjects and show at once.
func TestChatAttentionLater_DecisionSnoozeIsPerRevision(t *testing.T) {
	f := newAttnFx(t)
	d1 := f.decision()
	wantAttentionCode(t, f.post(testHandler.ChatAttentionLater, "later", map[string]any{
		"subject_keys": []string{service.DecisionKey(d1, 1)}, "duration_seconds": 7200, "request_id": uuid.NewString(),
	}), http.StatusOK)
	if got := f.keysAt(time.Now()); has(got, service.DecisionKey(d1, 1)) {
		t.Fatalf("D1 must be hidden until due: %v", got)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, d1) != 1 {
		t.Fatal("Để sau must not resolve the decision")
	}
	d2 := f.decision()
	if got := f.keysAt(time.Now()); !has(got, service.DecisionKey(d2, 1)) || has(got, service.DecisionKey(d1, 1)) {
		t.Fatalf("D2 visible, D1 still hidden: %v", got)
	}
	dbfx.Exec(t, `UPDATE decision_request SET answer_revision = 2 WHERE id = $1`, d1)
	if got := f.keysAt(time.Now()); !has(got, service.DecisionKey(d1, 2)) || has(got, service.DecisionKey(d1, 1)) {
		t.Fatalf("reopened D1@2 visible: %v", got)
	}
	if got := f.keysAt(time.Now().Add(3 * time.Hour)); !has(got, service.DecisionKey(d2, 1)) {
		t.Fatalf("D2 still visible after due: %v", got)
	}
}

// A20: Xong tất cả acknowledges the displayed unclassified turn; the next turn is a new subject.
func TestChatAttentionXongAll_NextUnclassifiedTurnStaysVisible(t *testing.T) {
	t.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	f := newAttnFx(t)
	t1 := f.turn()
	if !has(f.keysAt(time.Now()), service.TurnKey(t1)) {
		t.Fatal("T1 must be listed before Xong tất cả")
	}
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": []string{service.TurnKey(t1)}}), http.StatusOK)
	t2 := f.turn()
	got := f.keysAt(time.Now())
	if has(got, service.TurnKey(t1)) || !has(got, service.TurnKey(t2)) {
		t.Fatalf("T1 acknowledged, T2 visible: %v", got)
	}
}

// Undo is scoped to the workspace of the request: the same batch id from another workspace changes
// nothing, and the batch can still be undone from its own workspace.
func TestChatAttentionUndo_ScopedToRequestWorkspace(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, time.Hour)
	turn := f.turn()
	rec := f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": []string{service.ItemKey(ask), service.TurnKey(turn)}})
	wantAttentionCode(t, rec, http.StatusOK)
	var done struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil || done.BatchID == "" {
		t.Fatalf("batch_id missing: %s", rec.Body.String())
	}
	batch := done.BatchID
	acked := `SELECT count(*) FROM chat_attention_mark WHERE user_id = $1 AND subject_key = $2 AND acked_at IS NOT NULL`

	req := withURLParam(newRequest(http.MethodPost, "/api/chat/attention/xong-all/"+batch+"/undo", nil), "batchId", batch)
	req = withChatTestWorkspaceCtx(t, req)
	member, _ := middleware.MemberFromContext(req.Context())
	req = req.WithContext(middleware.SetMemberContext(req.Context(), uuid.NewString(), member))
	out := httptest.NewRecorder()
	testHandler.ChatAttentionUndoXongAll(out, req)
	wantAttentionCode(t, out, http.StatusOK)
	if f.resolved(ask) == nil || dbfx.Count(t, acked, testUserID, service.TurnKey(turn)) != 1 {
		t.Fatal("undo from another workspace must change nothing")
	}

	wantAttentionCode(t, f.call(testHandler.ChatAttentionUndoXongAll, "/api/chat/attention/xong-all/"+batch+"/undo", nil, "batchId", batch), http.StatusOK)
	if f.resolved(ask) != nil || dbfx.Count(t, acked, testUserID, service.TurnKey(turn)) != 0 {
		t.Fatal("undo from the batch's own workspace must restore it")
	}
}

// A member who lost access to a private agent must neither read nor change the attention of that
// agent's DM: not through the response of Xong-all or undo, and not by naming its subject keys.
func TestChatAttention_RevokedPrivateAgentIsInvisibleAndImmutable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	privateAgent, _, memberID := privateAgentTestFixture(t)
	publicAgent := createHandlerTestAgent(t, "attn-revoked-public", []byte("[]"))
	privateSession := insertChatSessionAs(t, privateAgent, memberID)
	publicSession := insertChatSessionAs(t, publicAgent, memberID)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_mark WHERE user_id = $1`, memberID)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_item WHERE user_id = $1`, memberID)
	ask := func(session, agent string) string {
		return dbfx.Insert(t, "chat_attention_item", testutil.Cols{
			"workspace_id": testWorkspaceID, "chain_root_session_id": session, "chat_session_id": session,
			"user_id": memberID, "kind": "ask", "agent_id": agent,
		})
	}
	privateAsk, publicAsk := ask(privateSession, privateAgent), ask(publicSession, publicAgent)
	closedBatch := uuid.NewString()
	closed := dbfx.Insert(t, "chat_attention_item", testutil.Cols{
		"workspace_id": testWorkspaceID, "chain_root_session_id": privateSession, "chat_session_id": privateSession,
		"user_id": memberID, "kind": "ask", "agent_id": privateAgent,
		"resolved_at": testutil.Raw("now()"), "resolved_by": "xong", "resolve_batch_id": closedBatch,
	})

	asMember := func(h http.HandlerFunc, path string, body any, params ...string) *httptest.ResponseRecorder {
		req := newRequestAs(memberID, http.MethodPost, path, body)
		for i := 0; i+1 < len(params); i += 2 {
			req = withURLParam(req, params[i], params[i+1])
		}
		rec := httptest.NewRecorder()
		h(rec, chatPendingCtxAs(t, req, memberID))
		return rec
	}
	roots := func(rec *httptest.ResponseRecorder) []string {
		var out struct {
			Attention []struct {
				RootID string   `json:"root_id"`
				Keys   []string `json:"keys"`
			} `json:"attention"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		var ids []string
		for _, a := range out.Attention {
			ids = append(ids, a.RootID)
			if has(a.Keys, service.ItemKey(privateAsk)) {
				t.Fatalf("response leaks the private agent's keys: %s", rec.Body.String())
			}
		}
		return ids
	}
	openAsk := `SELECT count(*) FROM chat_attention_item WHERE id = $1 AND resolved_at IS NULL`

	// Naming a revoked subject key changes nothing, even beside an allowed one.
	for _, keys := range [][]string{{service.ItemKey(privateAsk)}, {service.ItemKey(publicAsk), service.ItemKey(privateAsk)}} {
		rec := asMember(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": keys})
		wantAttentionCode(t, rec, http.StatusForbidden)
	}
	if dbfx.Count(t, openAsk, privateAsk) != 1 || dbfx.Count(t, openAsk, publicAsk) != 1 {
		t.Fatal("a refused Xong-all must change nothing")
	}

	// The response of an allowed Xong-all carries the allowed chain only.
	rec := asMember(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": []string{service.ItemKey(publicAsk)}})
	wantAttentionCode(t, rec, http.StatusOK)
	for _, id := range roots(rec) {
		if id == privateSession {
			t.Fatalf("response lists the revoked chain: %s", rec.Body.String())
		}
	}

	// Undo neither reopens the revoked chain's row nor reads it back.
	rec = asMember(testHandler.ChatAttentionUndoXongAll, "/api/chat/attention/xong-all/"+closedBatch+"/undo", nil, "batchId", closedBatch)
	wantAttentionCode(t, rec, http.StatusOK)
	for _, id := range roots(rec) {
		if id == privateSession {
			t.Fatalf("undo response lists the revoked chain: %s", rec.Body.String())
		}
	}
	if dbfx.Count(t, openAsk, closed) != 0 {
		t.Fatal("undo must not reopen a row of a chain the member lost access to")
	}
}

// A body past the size limit is refused before it is decoded.
func TestChatAttentionXongAll_OversizedBodyRefused(t *testing.T) {
	f := newAttnFx(t)
	big := strings.Repeat("x", attentionMaxBody+1)
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": []string{big}}), http.StatusRequestEntityTooLarge)
}

// A subject key whose id is a valid but noncanonical UUID (uppercase) names the same subject as the
// canonical key. It must not panic, must dedupe with the canonical form, and the mark is stored under
// the canonical key so it matches the projection.
func TestChatAttentionXong_NoncanonicalIdsAreCanonicalized(t *testing.T) {
	f := newAttnFx(t)
	turn, decision, ask := f.turn(), f.decision(), f.item("ask", testUserID, time.Hour)
	// The reported panic: an uppercase turn id alone finds no root in the canonical-keyed map.
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": []string{service.TurnKey(strings.ToUpper(turn))}}), http.StatusOK)
	keys := []string{
		service.TurnKey(strings.ToUpper(turn)), service.TurnKey(turn),
		service.ItemKey(strings.ToUpper(ask)),
		service.DecisionKey(strings.ToUpper(decision), 1),
	}
	wantAttentionCode(t, f.call(testHandler.ChatAttentionXongAll, "/api/chat/attention/xong-all", map[string]any{"subject_keys": keys}), http.StatusOK)
	if dbfx.Count(t, `SELECT count(*) FROM chat_attention_mark WHERE user_id = $1 AND subject_key = $2 AND acked_at IS NOT NULL`, testUserID, service.TurnKey(turn)) != 1 {
		t.Fatal("the turn mark must exist once under the canonical key")
	}
	if dbfx.Count(t, `SELECT count(*) FROM chat_attention_mark WHERE user_id = $1 AND subject_key <> lower(subject_key)`, testUserID) != 0 {
		t.Fatal("no mark may be stored under a noncanonical key")
	}
	if f.resolved(ask) == nil {
		t.Fatal("the ask named by its uppercase id must be resolved")
	}
}

// CHE-4151 (L7): `multica chat later` reads the keys the server shows for the chain before it snoozes them.
func TestGetChatSessionAttention_ReturnsDisplayedKeys(t *testing.T) {
	f := newAttnFx(t)
	ask := f.item("ask", testUserID, 5*time.Hour)

	rec := f.call(testHandler.GetChatSessionAttention, "/api/chat/sessions/"+f.sessionID+"/attention", nil, "sessionId", f.sessionID)
	wantAttentionCode(t, rec, http.StatusOK)
	view, _ := attnOne(t, rec)
	if !has(view.Keys, service.ItemKey(ask)) || view.StaleLevel != "yellow" {
		t.Fatalf("open ask must be listed as yellow: %+v", view)
	}
}
