package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
	dm := newAutoCompactDM(t, legacyPct80, `{}`)
	var root string
	dbfx.QueryRow(t, `SELECT chain_root_session_id FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&root)
	dbfx.Cleanup(t, `DELETE FROM chat_attention_item WHERE chain_root_session_id = $1`, root)
	dbfx.Cleanup(t, `DELETE FROM chat_turn_outcome WHERE chain_root_session_id = $1`, root)
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

// B4: a stored send is confirmed before any readiness refusal, so a retry after the agent was
// archived (or the DM archived) still recovers the first attempt instead of a 409 / 400.
func TestStoredSendReplaysEvenWhenAgentOrSessionIsNoLongerSendable(t *testing.T) {
	for name, breakIt := range map[string]func(dm attentionDM){
		"agent archived": func(dm attentionDM) { dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, dm.agentID) },
		"session archived": func(dm attentionDM) {
			dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, dm.sessionID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dm := newAttentionDM(t)
			extra := map[string]any{"client_message_id": "attempt-1"}
			first := dm.mustSend(t, "hello", extra)
			breakIt(dm)

			if second := dm.mustSend(t, "hello", extra); second.MessageID != first.MessageID || second.TaskID != first.TaskID {
				t.Fatalf("retry returned message %s task %s, want %s / %s", second.MessageID, second.TaskID, first.MessageID, first.TaskID)
			}
			// A new id is still refused: the refusal is only skipped for a stored send.
			if w := dm.sendWith(t, "new", map[string]any{"client_message_id": "attempt-2"}); w.Code < 400 {
				t.Fatalf("a new send was accepted (%d)", w.Code)
			}
		})
	}
}

// B4 for decision replies: the reply message and its decision record replay together, and a
// retry after the request is answered is not refused as "cannot accept another answer".
func TestDecisionReplyWithClientMessageIDReplaysOnce(t *testing.T) {
	dm := newAttentionDM(t)
	requestID, _, _ := seedDMDecision(t, dm.agentID, dm.sessionID, "Ask", "open", "1 minute")
	later := dm.item(t, "later")
	post := func() *httptest.ResponseRecorder {
		req := newRequest(http.MethodPost, "/api/chat/sessions/"+dm.sessionID+"/messages",
			map[string]any{"content": "> Ask\n\n1", "decision_request_id": requestID, "client_message_id": "attempt-1"})
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		w := httptest.NewRecorder()
		testHandler.SendChatMessage(w, withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", dm.sessionID)))
		return w
	}

	w := post()
	if w.Code != http.StatusCreated {
		t.Fatalf("decision reply with client_message_id: %d %s, want 201", w.Code, w.Body.String())
	}
	var first, second SendChatMessageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	st := dm.state(t, later)

	w = post()
	if w.Code != http.StatusOK {
		t.Fatalf("retry of an answered decision reply: %d %s, want 200", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if second.MessageID != first.MessageID || second.TaskID != first.TaskID {
		t.Fatalf("retry returned message %s task %s, want %s / %s", second.MessageID, second.TaskID, first.MessageID, first.TaskID)
	}
	if n := dm.userMessageCount(t); n != 1 {
		t.Fatalf("%d user messages, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID); n != 1 {
		t.Fatalf("%d decision answers, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, dm.sessionID); n != 1 {
		t.Fatalf("%d tasks, want 1", n)
	}
	if again := dm.state(t, later); *again.ResolvedAt != *st.ResolvedAt {
		t.Fatalf("retry rewrote resolved_at %s -> %s", *st.ResolvedAt, *again.ResolvedAt)
	}
}

// T2b single-ask fallback: with no ask_ids, exactly one open ask in the chain waits on the send;
// zero asks change nothing; several asks are ambiguous and none waits.
func TestSendWithoutAskIDsWaitsOnTheOnlyOpenAsk(t *testing.T) {
	t.Run("one ask", func(t *testing.T) {
		dm := newAttentionDM(t)
		ask := dm.item(t, "ask")
		res := dm.mustSend(t, "answer", nil)
		st := dm.state(t, ask)
		if st.WaitingMsg == nil || *st.WaitingMsg != res.MessageID || st.WaitingTsk == nil || *st.WaitingTsk != res.TaskID {
			t.Fatalf("only ask waiting = %v / %v, want message %s task %s", st.WaitingMsg, st.WaitingTsk, res.MessageID, res.TaskID)
		}
		if st.ResolvedBy != nil {
			t.Fatalf("the send resolved the ask (%s); only the agent's next outcome may", *st.ResolvedBy)
		}
	})
	t.Run("several asks", func(t *testing.T) {
		dm := newAttentionDM(t)
		a, b := dm.item(t, "ask"), dm.item(t, "ask")
		dm.mustSend(t, "answer", nil)
		for _, id := range []string{a, b} {
			if st := dm.state(t, id); st.WaitingMsg != nil || st.WaitingTsk != nil {
				t.Fatalf("ambiguous ask %s was marked waiting", id)
			}
		}
	})
	t.Run("no ask", func(t *testing.T) {
		dm := newAttentionDM(t)
		later := dm.item(t, "later")
		dm.mustSend(t, "hello", nil)
		if st := dm.state(t, later); st.ResolvedBy == nil || *st.ResolvedBy != "human_send" {
			t.Fatalf("later resolved_by = %v, want human_send", st.ResolvedBy)
		}
	})
}

// Security: T2 / T2b are the human's own transitions. A send made with an agent task token
// (a permitted agent writing into its human's DM) creates the message and task but must not
// resolve the human's later / unseen items or mark an ask waiting.
func TestAgentTokenSendLeavesHumanAttentionItemsAlone(t *testing.T) {
	dm := newAttentionDM(t)
	taskID := dm.mustSend(t, "hello", nil).TaskID // a task of the DM agent, to authenticate as
	later, unseen, ask := dm.item(t, "later"), dm.item(t, "unseen"), dm.item(t, "ask")

	body := map[string]any{"content": "from the agent", "client_message_id": "agent-1"}
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+dm.sessionID+"/messages", body), "sessionId", dm.sessionID))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	req.Header.Set("X-Agent-ID", dm.agentID)
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("agent send: %d %s, want 201", w.Code, w.Body.String())
	}

	for _, id := range []string{later, unseen} {
		if st := dm.state(t, id); st.ResolvedBy != nil {
			t.Fatalf("agent send resolved human item %s (resolved_by=%s)", id, *st.ResolvedBy)
		}
	}
	if st := dm.state(t, ask); st.WaitingMsg != nil || st.WaitingTsk != nil {
		t.Fatalf("agent send marked the human's ask waiting: %v / %v", st.WaitingMsg, st.WaitingTsk)
	}
}

// B4 race: a retry that arrives while the first send is still committing must wait for it, not
// miss it and be refused (409) as "agent archived" by a send that was in fact delivered.
// The first send is held open here: its client_message_id is hidden, the agent is already
// archived, and the session lock is held until the id is restored and committed.
func TestRetryOverlappingFirstSendWaitsForItInsteadOfRefusing(t *testing.T) {
	dm := newAttentionDM(t)
	extra := map[string]any{"client_message_id": "attempt-1"}
	first := dm.mustSend(t, "hello", extra)

	ctx := context.Background()
	dbfx.Exec(t, `UPDATE chat_message SET client_message_id = NULL WHERE id = $1`, first.MessageID)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, dm.agentID)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, dm.sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE chat_message SET client_message_id = 'attempt-1' WHERE id = $1`, first.MessageID); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- dm.sendWith(t, "hello", extra) }()
	time.Sleep(300 * time.Millisecond) // the retry has run its lookup against the uncommitted first send
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case w := <-done:
		var res SendChatMessageResponse
		json.Unmarshal(w.Body.Bytes(), &res)
		if w.Code != http.StatusOK || res.MessageID != first.MessageID {
			t.Fatalf("overlapping retry: %d %s, want 200 with message %s", w.Code, w.Body.String(), first.MessageID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retry never returned")
	}
}

// A keyed send must never hold one pool connection while it waits for another: concurrent
// sends would fill the pool, each waiting on a second connection nobody can release. On a pool
// of ONE connection that is a stall on the first request. Retries of a stored send and refused
// new sends both finish.
func TestKeyedSendNeverHoldsTwoPoolConnections(t *testing.T) {
	dm := newAttentionDM(t)
	dm.mustSend(t, "hello", map[string]any{"client_message_id": "attempt-1"})
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, dm.agentID) // every send now reaches a refusal

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica_test?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	h := *testHandler
	h.Queries, h.TxStarter = db.New(one), one

	send := func(id string) (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body := map[string]any{"content": "hello", "client_message_id": id}
		req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+dm.sessionID+"/messages", body).WithContext(ctx), "sessionId", dm.sessionID))
		w := httptest.NewRecorder()
		h.SendChatMessage(w, req)
		return w.Code, w.Body.String()
	}
	var wg sync.WaitGroup
	for i, id := range []string{"attempt-1", "new-1", "attempt-1", "new-2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := send(id)
			want := http.StatusConflict
			if id == "attempt-1" {
				want = http.StatusOK
			}
			if code != want {
				t.Errorf("send %d (%s) on a one-connection pool: %d %s, want %d", i, id, code, body, want)
			}
		}()
	}
	wg.Wait()
}
