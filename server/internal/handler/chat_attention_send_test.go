package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4146 (L2 of CHE-2769): a DM send is idempotent per client_message_id and applies the
// attention transitions T2 / T2b in the message transaction. Every assertion goes through the
// send handler and raw SQL, so the file builds against a tree without the feature.

type attentionDM struct {
	autoCompactDM
	root string
}

func newAttentionDM(t *testing.T) attentionDM {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	var root string
	dbfx.QueryRow(t, `SELECT chain_root_session_id FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&root)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_item WHERE chain_root_session_id = $1`, root)
	return attentionDM{dm, root}
}

// item opens one attention item of kind (ask | later | unseen) and returns its id.
func (dm attentionDM) item(t *testing.T, kind string) string {
	t.Helper()
	cols := testutil.Cols{
		"workspace_id": testWorkspaceID, "chain_root_session_id": dm.root, "chat_session_id": dm.sessionID,
		"user_id": testUserID, "kind": kind,
	}
	switch kind {
	case "ask":
		cols["agent_id"] = dm.agentID
	case "later":
		cols["due_at"] = "2099-01-01T00:00:00Z"
	}
	return dbfx.Insert(t, "chat_attention_item", cols)
}

// sendWith posts a message with extra request fields.
func (dm attentionDM) sendWith(t *testing.T, content string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"content": content}
	for k, v := range extra {
		body[k] = v
	}
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+dm.sessionID+"/messages", body), "sessionId", dm.sessionID))
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, req)
	return w
}

func (dm attentionDM) mustSend(t *testing.T, content string, extra map[string]any) SendChatMessageResponse {
	t.Helper()
	w := dm.sendWith(t, content, extra)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("send %q: %d %s, want success", content, w.Code, w.Body.String())
	}
	var res SendChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

type itemState struct {
	ResolvedBy *string
	ResolvedAt *string
	WaitingMsg *string
	WaitingTsk *string
}

func (dm attentionDM) state(t *testing.T, id string) itemState {
	t.Helper()
	var s itemState
	dbfx.QueryRow(t, `SELECT resolved_by, resolved_at::text, waiting_message_id::text, waiting_task_id::text FROM chat_attention_item WHERE id = $1`, id).
		Scan(&s.ResolvedBy, &s.ResolvedAt, &s.WaitingMsg, &s.WaitingTsk)
	return s
}

// A22, A23: a retry after a timeout carries the same client_message_id. One message, one task,
// T2 applied once (the second send must not touch the rows it already resolved).
func TestSendWithSameClientMessageIDStoresOneMessageAndAppliesT2Once(t *testing.T) {
	dm := newAttentionDM(t)
	later, unseen := dm.item(t, "later"), dm.item(t, "unseen")
	extra := map[string]any{"client_message_id": "attempt-1"}

	first := dm.mustSend(t, "hello", extra)
	st := dm.state(t, later)
	if st.ResolvedBy == nil || *st.ResolvedBy != "human_send" {
		t.Fatalf("later after send: resolved_by = %v, want human_send", st.ResolvedBy)
	}
	if st := dm.state(t, unseen); st.ResolvedBy == nil || *st.ResolvedBy != "human_send" {
		t.Fatalf("unseen after send: resolved_by = %v, want human_send", st.ResolvedBy)
	}

	second := dm.mustSend(t, "hello", extra)
	if second.MessageID != first.MessageID || second.TaskID != first.TaskID {
		t.Fatalf("retry returned message %s task %s, want %s / %s", second.MessageID, second.TaskID, first.MessageID, first.TaskID)
	}
	if n := dm.userMessageCount(t); n != 1 {
		t.Fatalf("%d user messages after a retried send, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, dm.sessionID); n != 1 {
		t.Fatalf("%d tasks after a retried send, want 1", n)
	}
	if again := dm.state(t, later); *again.ResolvedAt != *st.ResolvedAt {
		t.Fatalf("retry rewrote resolved_at %s -> %s", *st.ResolvedAt, *again.ResolvedAt)
	}

	// A different id is a different send.
	dm.mustSend(t, "hello again", map[string]any{"client_message_id": "attempt-2"})
	if n := dm.userMessageCount(t); n != 2 {
		t.Fatalf("%d user messages after a send with a new id, want 2", n)
	}
}

// A12: a refused send (409 chat_compacting) leaves no message and no attention change.
func TestRefusedSendLeavesNoMessageAndNoAttentionChange(t *testing.T) {
	dm := newAttentionDM(t)
	dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
	if len(dm.compactionTasks(t)) != 1 {
		t.Fatal("setup: want one compaction task")
	}
	later, ask := dm.item(t, "later"), dm.item(t, "ask")
	before := dm.userMessageCount(t)

	w := dm.sendWith(t, "are you there?", map[string]any{"client_message_id": "attempt-1", "ask_ids": []string{ask}})
	if w.Code != http.StatusConflict {
		t.Fatalf("send while compacting: %d %s, want 409", w.Code, w.Body.String())
	}
	var body struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != "chat_compacting" {
		t.Fatalf("409 body = %s, want code chat_compacting", w.Body.String())
	}
	if after := dm.userMessageCount(t); after != before {
		t.Fatalf("a refused send stored %d new user messages", after-before)
	}
	if st := dm.state(t, later); st.ResolvedBy != nil {
		t.Fatalf("a refused send resolved the later item (%s)", *st.ResolvedBy)
	}
	if st := dm.state(t, ask); st.WaitingMsg != nil || st.WaitingTsk != nil {
		t.Fatal("a refused send marked an ask as waiting")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND client_message_id = 'attempt-1'`, dm.sessionID); n != 0 {
		t.Fatalf("%d messages stored the refused client_message_id", n)
	}
}

// A16: a reply that targets ask B changes only B, and only by marking it waiting.
func TestReplyTargetingOneAskChangesOnlyThatAsk(t *testing.T) {
	dm := newAttentionDM(t)
	askA, askB := dm.item(t, "ask"), dm.item(t, "ask")

	res := dm.mustSend(t, "answer for B", map[string]any{"ask_ids": []string{askB}})
	b := dm.state(t, askB)
	if b.WaitingMsg == nil || *b.WaitingMsg != res.MessageID || b.WaitingTsk == nil || *b.WaitingTsk != res.TaskID {
		t.Fatalf("targeted ask waiting = %v / %v, want message %s task %s", b.WaitingMsg, b.WaitingTsk, res.MessageID, res.TaskID)
	}
	if b.ResolvedBy != nil {
		t.Fatalf("a targeted reply resolved the ask (%s); only the agent's next outcome may", *b.ResolvedBy)
	}
	if a := dm.state(t, askA); a.WaitingMsg != nil || a.WaitingTsk != nil || a.ResolvedBy != nil {
		t.Fatalf("untargeted ask changed: %+v", a)
	}

	// A plain send targets nothing.
	dm.mustSend(t, "unrelated", nil)
	if a := dm.state(t, askA); a.WaitingMsg != nil {
		t.Fatal("a send without ask_ids marked an ask as waiting")
	}
}

// ask_ids that are not this user's open asks in this DM's chain are refused before any write.
func TestSendRejectsAskIDsOutsideTheChain(t *testing.T) {
	dm := newAttentionDM(t)
	otherDM := dbfx.ChatSession(t, dm.agentID)
	other := attentionDM{dm.autoCompactDM, otherDM}
	other.sessionID = otherDM
	foreign := other.item(t, "ask")
	before := dm.userMessageCount(t)

	for name, ids := range map[string][]string{"foreign": {foreign}, "malformed": {"nope"}} {
		if w := dm.sendWith(t, "x", map[string]any{"ask_ids": ids}); w.Code != http.StatusBadRequest {
			t.Fatalf("%s ask id: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if after := dm.userMessageCount(t); after != before {
		t.Fatalf("a rejected send stored %d new user messages", after-before)
	}
	if st := other.state(t, foreign); st.WaitingMsg != nil {
		t.Fatal("a foreign ask was marked waiting")
	}
}

// A9, chain-root part: a continued DM joins its predecessor's chain.
func TestContinuationSourceCopiesChainRoot(t *testing.T) {
	dm := newAttentionDM(t)
	next := dbfx.ChatSession(t, dm.agentID)
	var own string
	dbfx.QueryRow(t, `SELECT chain_root_session_id FROM chat_session WHERE id = $1`, next).Scan(&own)
	if own != next {
		t.Fatalf("setup: new DM root = %s, want itself", own)
	}

	if _, err := testHandler.Queries.SetChatContinuationSource(t.Context(), db.SetChatContinuationSourceParams{
		ID: parseUUID(next), ContinuedFromSessionID: parseUUID(dm.sessionID),
		SummaryCreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	var got string
	dbfx.QueryRow(t, `SELECT chain_root_session_id FROM chat_session WHERE id = $1`, next).Scan(&got)
	if got != dm.root {
		t.Fatalf("continued DM chain root = %s, want predecessor's %s", got, dm.root)
	}
}
